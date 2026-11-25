// Package pairing pairs a machine with collie through the OAuth 2.0 device authorization grant (RFC 8628):
// collie-agent asks for a device code, the person approves the user code in the browser, and collie-agent polls the
// token endpoint until it gets the machine's id and token.
package pairing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientID is the OAuth client collie knows collie-agent as.
const ClientID = "collie-agent"

// DeviceCodeGrant is the RFC 8628 grant type.
const DeviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"

// slowDownStep is how much RFC 8628 §3.5 says to add to the interval on every slow_down.
const slowDownStep = 5 * time.Second

// maxInterval caps the backoff after timeouts and server errors.
const maxInterval = 60 * time.Second

// defaultInterval is used when the server does not send one (RFC 8628 §3.2).
const defaultInterval = 5 * time.Second

var (
	// ErrDenied means the person denied the device on collie.
	ErrDenied = errors.New("the pairing was denied in collie")
	// ErrExpired means the device code expired before anyone approved it.
	ErrExpired = errors.New("the code expired before it was approved: run `collie-agent pair` again")
)

// Machine describes this machine to collie when it asks for a device code.
type Machine struct {
	Name     string
	OS       string
	Hostname string
}

// DeviceAuthorization is collie's answer to the device authorization request.
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// Result is a successful pairing.
type Result struct {
	MachineID    string
	MachineToken string
}

// Client runs the device flow against one collie server.
type Client struct {
	Server string // e.g. https://collies.sh, no trailing slash
	HTTP   *http.Client
	// Sleep waits d or until ctx is done. Tests replace it so they do not wait for real.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the clock; tests replace it together with Sleep.
	Now func() time.Time
	// OnPoll, when set, is called before every token request with the interval in force (tests and verbose output).
	OnPoll func(interval time.Duration)
}

// NewClient returns a Client with real time and a 30-second HTTP timeout.
func NewClient(server string) *Client {
	return &Client{Server: server, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// oauthError is the RFC 6749 §5.2 error body.
type oauthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *oauthError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

// Authorize asks collie for a device code (RFC 8628 §3.1).
func (c *Client) Authorize(ctx context.Context, m Machine) (*DeviceAuthorization, error) {
	form := url.Values{"client_id": {ClientID}, "name": {m.Name}, "os": {m.OS}, "hostname": {m.Hostname}}
	status, body, err := c.postForm(ctx, "/oauth/device_authorization", form)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("collie refused the pairing request (HTTP %d): %w", status, decodeError(body))
	}
	var da DeviceAuthorization
	if err := json.Unmarshal(body, &da); err != nil {
		return nil, fmt.Errorf("unexpected answer from collie: %w", err)
	}
	if da.DeviceCode == "" || da.UserCode == "" || da.VerificationURI == "" {
		return nil, errors.New("unexpected answer from collie: missing device_code, user_code or verification_uri")
	}
	if err := ValidateVerificationURI(da.VerificationURI); err != nil {
		return nil, err
	}
	if da.VerificationURIComplete != "" {
		if err := ValidateVerificationURI(da.VerificationURIComplete); err != nil {
			return nil, err
		}
	}
	return &da, nil
}

// Poll waits for the person to approve the device (RFC 8628 §3.4–3.5). It waits the interval before every request,
// adds 5 seconds on every slow_down, and gives up when the code expires. Network errors and 5xx are retried until then.
func (c *Client) Poll(ctx context.Context, da *DeviceAuthorization) (*Result, error) {
	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = defaultInterval
	}
	var deadline time.Time
	if da.ExpiresIn > 0 {
		deadline = c.now().Add(time.Duration(da.ExpiresIn) * time.Second)
	}
	form := url.Values{"grant_type": {DeviceCodeGrant}, "device_code": {da.DeviceCode}, "client_id": {ClientID}}

	for {
		if !deadline.IsZero() && !c.now().Add(interval).Before(deadline) {
			return nil, ErrExpired
		}
		if err := c.sleep(ctx, interval); err != nil {
			return nil, err
		}
		if c.OnPoll != nil {
			c.OnPoll(interval)
		}
		status, body, err := c.postForm(ctx, "/oauth/token", form)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// A timeout or a network error: RFC 8628 §3.5 says to poll less often before retrying.
			interval = backOff(interval)
			continue
		}
		if status == http.StatusOK {
			return parseToken(body)
		}
		if status >= 500 {
			interval = backOff(interval)
			continue
		}
		oe := decodeError(body)
		switch oe.Code {
		case "authorization_pending":
		case "slow_down":
			interval += slowDownStep
		case "access_denied":
			return nil, ErrDenied
		case "expired_token":
			return nil, ErrExpired
		default:
			return nil, fmt.Errorf("pairing failed (HTTP %d): %w", status, oe)
		}
	}
}

// backOff doubles the interval after a failed request, up to maxInterval. It never lowers the interval in force:
// one the server set above the cap, or slow_down increases past it, stay as they are.
func backOff(d time.Duration) time.Duration {
	if d >= maxInterval {
		return d
	}
	return min(d*2, maxInterval)
}

// ValidateVerificationURI checks a URL collie sent for the person to open: an absolute https URL (http only on a
// loopback host, for development), so a broken or hostile server cannot make the OS opener run a local file, an
// option or a custom scheme.
func ValidateVerificationURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil {
		return fmt.Errorf("collie sent an invalid verification URL %q", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	return fmt.Errorf("collie sent a verification URL that is not https: %q", raw)
}

func parseToken(body []byte) (*Result, error) {
	var raw struct {
		MachineID    json.RawMessage `json:"machine_id"`
		MachineToken string          `json:"machine_token"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unexpected answer from collie: %w", err)
	}
	// collie sends the id as a number; accept a string too.
	id := strings.Trim(string(raw.MachineID), `"`)
	if id == "null" {
		id = ""
	}
	if id == "" || raw.MachineToken == "" {
		return nil, errors.New("unexpected answer from collie: missing machine_id or machine_token")
	}
	return &Result{MachineID: id, MachineToken: raw.MachineToken}, nil
}

func (c *Client) postForm(ctx context.Context, path string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Server+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

func decodeError(body []byte) *oauthError {
	var oe oauthError
	if err := json.Unmarshal(body, &oe); err != nil || oe.Code == "" {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return &oauthError{Code: "unexpected_response", Description: snippet}
	}
	return &oe
}
