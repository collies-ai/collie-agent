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

// InventoryRepo is a repo found on this machine whose remote matches a codebase.
type InventoryRepo struct {
	CodebaseID int64  `json:"codebase_id"`
	Path       string `json:"path"`
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
	Reference   string          `json:"reference"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Project     string          `json:"project"`
	Codebase    string          `json:"codebase"`
	MCPURL      string          `json:"mcp_url"`
	AckDeadline time.Time       `json:"ack_deadline"`
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
