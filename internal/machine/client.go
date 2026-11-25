// Package machine is the client of collie's machine API (/machine/v1), authenticated with the machine token.
package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrRevoked means collie answered 401: the machine was revoked in collie or its token is invalid. Retrying cannot
// fix it; the machine has to be paired again.
var ErrRevoked = errors.New("collie rejected the machine token (401): the machine was revoked or its token is invalid; run `collie-agent pair --force` to pair it again")

// Agent is a coding-agent CLI found on this machine.
type Agent struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Heartbeat is the body of POST /machine/v1/heartbeat.
type Heartbeat struct {
	Version        string   `json:"version"`
	OS             string   `json:"os"`
	MaxConcurrency int      `json:"max_concurrency"`
	Agents         []Agent  `json:"agents"`
	Running        []string `json:"running"`
	// RootsCount is how many root folders are configured. Always sent (no omitempty): 0 means "no roots", while
	// collie reads a missing value (an older agent) as unknown.
	RootsCount int `json:"roots_count"`
}

// HeartbeatResponse is collie's answer: the bindings version, so the agent knows when to fetch its bindings again,
// and the codebases version, so it knows when to look for repos again ("" when collie sends none).
type HeartbeatResponse struct {
	BindingsVersion  string
	CodebasesVersion string
}

// VersionString turns a version as collie sends it (a number, a string or null) into a string, "" for none.
func VersionString(raw json.RawMessage) string {
	v := strings.Trim(string(raw), `"`)
	if v == "null" {
		return ""
	}
	return v
}

// Client calls the machine API.
type Client struct {
	Server    string // no trailing slash
	Token     string
	UserAgent string
	HTTP      *http.Client
}

// NewClient returns a Client with a 30-second timeout.
func NewClient(server, token, userAgent string) *Client {
	return &Client{Server: server, Token: token, UserAgent: userAgent, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// StatusError is a non-2xx answer other than 401.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("collie answered HTTP %d: %s", e.Status, e.Body)
}

// Heartbeat tells collie the machine is online.
func (c *Client) Heartbeat(ctx context.Context, hb Heartbeat) (*HeartbeatResponse, error) {
	if hb.Agents == nil {
		hb.Agents = []Agent{}
	}
	if hb.Running == nil {
		hb.Running = []string{}
	}
	var raw struct {
		BindingsVersion  json.RawMessage `json:"bindings_version"`
		CodebasesVersion json.RawMessage `json:"codebases_version"`
	}
	if err := c.do(ctx, http.MethodPost, "/machine/v1/heartbeat", hb, &raw); err != nil {
		return nil, err
	}
	return &HeartbeatResponse{BindingsVersion: VersionString(raw.BindingsVersion), CodebasesVersion: VersionString(raw.CodebasesVersion)}, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The status alone says the token was rejected: decide before reading a body that may never arrive.
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrRevoked
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := strings.TrimSpace(string(b))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return &StatusError{Status: resp.StatusCode, Body: snippet}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("unexpected answer from collie: %w", err)
		}
	}
	return nil
}
