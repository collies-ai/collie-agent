package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Codebase is a codebase of a project granted to this machine (GET /machine/v1/codebases).
type Codebase struct {
	CodebaseID   int64  `json:"codebase_id"`
	Codebase     string `json:"codebase"`
	Project      string `json:"project"`
	RepoFullName string `json:"repo_full_name"`
}

// InventoryRepo is a codebase found on this machine: the clone collie-agent chose for it and the other clones
// (Alternatives, best first). Source is where the clone was found (root, claude, codex or scan) and Remote the name of
// the remote that matched (origin, or e.g. upstream for a fork). A collie that does not know the newer keys reads only
// codebase_id and path.
type InventoryRepo struct {
	CodebaseID   int64            `json:"codebase_id"`
	Path         string           `json:"path"`
	Source       string           `json:"source,omitempty"`
	Remote       string           `json:"remote,omitempty"`
	Alternatives []InventoryClone `json:"alternatives,omitempty"`
}

// InventoryClone is another clone of an InventoryRepo's codebase.
type InventoryClone struct {
	Path   string `json:"path"`
	Source string `json:"source,omitempty"`
	Remote string `json:"remote,omitempty"`
}

// Binding is a machine runner: a folder of this machine bound to a project's codebase (GET /machine/v1/bindings).
type Binding struct {
	RunnerID       int64             `json:"runner_id"`
	Project        string            `json:"project"`
	Codebase       string            `json:"codebase"`
	CodebaseID     int64             `json:"codebase_id"`
	RepoFullName   string            `json:"repo_full_name"`
	Purpose        string            `json:"purpose"`
	Folder         string            `json:"folder"`
	Agents         map[string]string `json:"agents"`
	PermissionMode string            `json:"permission_mode"`
	MaxConcurrency int               `json:"max_concurrency"`
}

// Dispatch is an offer of work to this machine, pushed over MachineChannel or served by the long-poll. It is only a
// job reference, never instructions.
type Dispatch struct {
	Type        string          `json:"type"`
	DispatchID  int64           `json:"dispatch_id"`
	SessionID   int64           `json:"session_id"`
	RunnerID    int64           `json:"runner_id"`
	Kind        string          `json:"kind"`
	Purpose     string          `json:"purpose"`
	Reference   Ref             `json:"reference"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Project     string          `json:"project"`
	Codebase    string          `json:"codebase"`
	MCPURL      string          `json:"mcp_url"`
	AckDeadline time.Time       `json:"ack_deadline"`
}

// Ref is a dispatch's reference: collie sends it as a number (a goal or task id), older or hand-made messages as a
// string. Both decode.
type Ref string

// UnmarshalJSON accepts a JSON number or string.
func (r *Ref) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*r = Ref(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("reference must be a number or a string: %w", err)
	}
	*r = Ref(n.String())
	return nil
}

// Accepted is collie's answer to an accepted offer.
type Accepted struct {
	SessionID    int64     `json:"session_id"`
	DispatchID   int64     `json:"dispatch_id"`
	MCPURL       string    `json:"mcp_url"`
	SessionToken string    `json:"session_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// ErrGone means the offer is no longer this machine's: it expired, was redelivered, or the work stopped being
// claimable (409).
var ErrGone = errors.New("the offer is gone")

// Codebases lists the codebases of the projects granted to this machine.
func (c *Client) Codebases(ctx context.Context) ([]Codebase, error) {
	var out []Codebase
	if err := c.do(ctx, http.MethodGet, "/machine/v1/codebases", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PutInventory replaces the machine's inventory with repos.
func (c *Client) PutInventory(ctx context.Context, repos []InventoryRepo) error {
	if repos == nil {
		repos = []InventoryRepo{}
	}
	return c.do(ctx, http.MethodPut, "/machine/v1/inventory", map[string]any{"repos": repos}, nil)
}

// Bindings lists the machine's bindings as collie sees them. collie-agent still checks each one locally.
func (c *Client) Bindings(ctx context.Context) ([]Binding, error) {
	var out []Binding
	if err := c.do(ctx, http.MethodGet, "/machine/v1/bindings", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PollDispatches is the long-poll fallback: it waits up to wait (at most 25 s) for offers.
func (c *Client) PollDispatches(ctx context.Context, wait time.Duration) ([]Dispatch, error) {
	secs := int(wait / time.Second)
	if secs < 0 {
		secs = 0
	}
	if secs > 25 {
		secs = 25
	}
	var out struct {
		Dispatches []Dispatch `json:"dispatches"`
	}
	q := url.Values{"wait": {strconv.Itoa(secs)}}
	if err := c.do(ctx, http.MethodGet, "/machine/v1/dispatches?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Dispatches, nil
}

// Ack accepts or declines an offer. Accepting returns the session; declining returns (nil, nil). ErrGone means the
// offer is no longer this machine's.
func (c *Client) Ack(ctx context.Context, dispatchID int64, accept bool, reason string) (*Accepted, error) {
	body := map[string]any{"accepted": accept}
	if reason != "" {
		body["reason"] = reason
	}
	var out Accepted
	path := fmt.Sprintf("/machine/v1/dispatches/%d/ack", dispatchID)
	if err := c.do(ctx, http.MethodPost, path, body, &out); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusConflict {
			return nil, fmt.Errorf("%w: %s", ErrGone, se.Body)
		}
		return nil, err
	}
	if !accept {
		return nil, nil
	}
	return &out, nil
}

// Usage is an agent session's cumulative usage, as collie charges it on the finished event.
type Usage struct {
	SessionID                string `json:"session_id,omitempty"`
	Model                    string `json:"model,omitempty"`
	InputTokens              int64  `json:"input_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	Billing                  string `json:"billing,omitempty"`
}

// ErrRetry means collie could not record a session event; send it again.
var ErrRetry = errors.New("collie could not record the event; send it again")

// RenewSessionToken pushes the session token's expiry. The token itself does not change. ErrGone means the session
// ended or its token already expired.
func (c *Client) RenewSessionToken(ctx context.Context, sessionID int64) (time.Time, error) {
	var out struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/machine/v1/sessions/%d/token", sessionID), map[string]any{}, &out)
	if err != nil {
		return time.Time{}, sessionError(err)
	}
	return out.ExpiresAt, nil
}

// SessionStarted reports that the agent process started, with the link to watch it when there is one.
func (c *Client) SessionStarted(ctx context.Context, sessionID int64, sessionURL string) error {
	body := map[string]any{"event": "started"}
	if sessionURL != "" {
		body["session_url"] = sessionURL
	}
	return sessionError(c.do(ctx, http.MethodPost, fmt.Sprintf("/machine/v1/sessions/%d/events", sessionID), body, nil))
}

// SessionFinished reports that the agent process ended, with its exit code and, when known, its usage and the
// review the agent answered with (a review session whose agent takes an output schema).
func (c *Client) SessionFinished(ctx context.Context, sessionID int64, exitCode int, usage *Usage, review json.RawMessage) error {
	body := map[string]any{"event": "finished", "exit_code": exitCode}
	if usage != nil {
		body["usage"] = usage
	}
	if len(review) > 0 {
		body["review"] = review
	}
	return sessionError(c.do(ctx, http.MethodPost, fmt.Sprintf("/machine/v1/sessions/%d/events", sessionID), body, nil))
}

func sessionError(err error) error {
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusConflict, http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrGone, se.Body)
		case http.StatusServiceUnavailable:
			return ErrRetry
		}
	}
	return err
}

// Reissued is a new session token for a session collie-agent resumes after a restart (the plaintext token is never
// kept on disk).
type Reissued struct {
	SessionID int64     `json:"session_id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	MCPURL    string    `json:"mcp_url"`
}

// ReissueSessionToken asks for a new token for a session of this machine that collie still holds as accepted
// (suspended or not), even when the old token expired; the old token stops working. ErrGone means the session is no
// longer this machine's to resume.
func (c *Client) ReissueSessionToken(ctx context.Context, sessionID int64) (*Reissued, error) {
	var out Reissued
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/machine/v1/sessions/%d/token/reissue", sessionID), map[string]any{}, &out)
	if err != nil {
		return nil, sessionError(err)
	}
	return &out, nil
}

// SessionSuspended reports a graceful shutdown: the agent process was stopped to be resumed when collie-agent runs
// again, and the session stays accepted.
func (c *Client) SessionSuspended(ctx context.Context, sessionID int64) error {
	return c.sessionEvent(ctx, sessionID, map[string]any{"event": "suspended"})
}

// SessionResumed reports that the agent process was resumed after a restart, with its new link when it has one.
func (c *Client) SessionResumed(ctx context.Context, sessionID int64, sessionURL string) error {
	body := map[string]any{"event": "resumed"}
	if sessionURL != "" {
		body["session_url"] = sessionURL
	}
	return c.sessionEvent(ctx, sessionID, body)
}

// ResumeReasonMax is the longest reason collie takes on a resume_failed event.
const ResumeReasonMax = 1000

// SessionResumeFailed reports that a session could not be resumed after a restart; collie finishes it with the reason.
func (c *Client) SessionResumeFailed(ctx context.Context, sessionID int64, reason string) error {
	if len(reason) > ResumeReasonMax {
		reason = reason[:ResumeReasonMax]
	}
	return c.sessionEvent(ctx, sessionID, map[string]any{"event": "resume_failed", "reason": reason})
}

func (c *Client) sessionEvent(ctx context.Context, sessionID int64, body map[string]any) error {
	return sessionError(c.do(ctx, http.MethodPost, fmt.Sprintf("/machine/v1/sessions/%d/events", sessionID), body, nil))
}
