// Package cable is a minimal ActionCable client for collie's MachineChannel: it connects to wss://<server>/cable
// with the machine token, subscribes, and hands every dispatch it receives to a callback.
package cable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/collies-ai/collie-agent/internal/machine"
)

// Channel is the ActionCable channel collie pushes dispatches on.
const Channel = "MachineChannel"

// StaleAfter is how long the connection may stay silent before it is considered dead. ActionCable pings every 3 s.
const StaleAfter = 15 * time.Second

// ErrUnauthorized means collie refused the machine token on the cable (revoked or invalid).
var ErrUnauthorized = errors.New("collie refused the machine token on the cable")

// Client connects to one collie server.
type Client struct {
	Server string // https://collies.sh
	Token  string
	// HTTPClient is used for the websocket handshake (tests pass the test server's).
	HTTPClient *http.Client
	UserAgent  string
}

// URL is the cable endpoint: wss for https servers, ws for http (local development).
func (c *Client) URL() (string, error) {
	u, err := url.Parse(c.Server)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported server scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/cable"
	return u.String(), nil
}

// Origin is the server's origin, which collie's ActionCable accepts.
func (c *Client) Origin() string {
	u, err := url.Parse(c.Server)
	if err != nil {
		return c.Server
	}
	return u.Scheme + "://" + u.Host
}

type frame struct {
	Type       string          `json:"type,omitempty"`
	Identifier string          `json:"identifier,omitempty"`
	Message    json.RawMessage `json:"message,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Reconnect  *bool           `json:"reconnect,omitempty"`
}

// Run connects, subscribes and calls onDispatch for every dispatch until ctx is done (nil), the connection fails
// (an error), or collie refuses the token (ErrUnauthorized). onSubscribed is called once the subscription is
// confirmed. onCodebasesChanged (optional) is called with collie's codebases_version ("" when it sent none) when the
// codebases this machine may serve changed. Run does not reconnect: the caller does, with backoff, and long-polls
// meanwhile.
func (c *Client) Run(ctx context.Context, onSubscribed func(), onDispatch func(machine.Dispatch), onCodebasesChanged func(version string)) error {
	wsURL, err := c.URL()
	if err != nil {
		return err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.Token)
	h.Set("Origin", c.Origin())
	if c.UserAgent != "" {
		h.Set("User-Agent", c.UserAgent)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient:   c.HTTPClient,
		HTTPHeader:   h,
		Subprotocols: []string{"actioncable-v1-json"},
	})
	cancel()
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return ErrUnauthorized
		}
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("connecting to %s: %w", wsURL, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	identifier, _ := json.Marshal(map[string]string{"channel": Channel})
	subscribed := false
	for {
		readCtx, cancel := context.WithTimeout(ctx, StaleAfter)
		_, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil // the deferred CloseNow drops it: waiting for a close handshake would stall shutdown
			}
			return fmt.Errorf("cable: %w", err)
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.Type {
		case "welcome":
			sub, _ := json.Marshal(map[string]string{"command": "subscribe", "identifier": string(identifier)})
			if err := conn.Write(ctx, websocket.MessageText, sub); err != nil {
				return fmt.Errorf("cable: subscribing: %w", err)
			}
		case "ping":
		case "confirm_subscription":
			if !subscribed {
				subscribed = true
				if onSubscribed != nil {
					onSubscribed()
				}
			}
		case "reject_subscription":
			return ErrUnauthorized
		case "disconnect":
			if f.Reason == "unauthorized" {
				return ErrUnauthorized
			}
			return fmt.Errorf("cable: collie closed the connection (%s)", f.Reason)
		case "":
			if f.Identifier != string(identifier) || len(f.Message) == 0 {
				continue
			}
			var head struct {
				Type             string          `json:"type"`
				CodebasesVersion json.RawMessage `json:"codebases_version"`
			}
			if err := json.Unmarshal(f.Message, &head); err != nil {
				continue
			}
			switch head.Type {
			case "codebases_changed":
				if onCodebasesChanged != nil {
					onCodebasesChanged(machine.VersionString(head.CodebasesVersion))
				}
			case "dispatch":
				var d machine.Dispatch
				if err := json.Unmarshal(f.Message, &d); err != nil || d.DispatchID == 0 {
					continue
				}
				onDispatch(d)
			}
		}
	}
}
