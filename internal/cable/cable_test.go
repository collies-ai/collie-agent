package cable

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/collies-ai/collie-agent/internal/machine"
)

// server is a fake ActionCable endpoint; script runs after the subscription is confirmed.
func server(t *testing.T, script func(ctx context.Context, c *websocket.Conn, ident string)) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cable" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Origin"); got != srv.URL {
			t.Errorf("Origin = %q, want %q", got, srv.URL)
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"actioncable-v1-json"}, InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		if c.Subprotocol() != "actioncable-v1-json" {
			t.Errorf("subprotocol = %q", c.Subprotocol())
		}
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"welcome"}`))
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var sub map[string]string
		_ = json.Unmarshal(data, &sub)
		if sub["command"] != "subscribe" || sub["identifier"] != `{"channel":"MachineChannel"}` {
			t.Errorf("subscribe = %s", data)
		}
		ident := sub["identifier"]
		confirm, _ := json.Marshal(map[string]string{"identifier": ident, "type": "confirm_subscription"})
		_ = c.Write(ctx, websocket.MessageText, confirm)
		script(ctx, c, ident)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func send(ctx context.Context, c *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	_ = c.Write(ctx, websocket.MessageText, b)
}

func TestReceivesDispatches(t *testing.T) {
	srv := server(t, func(ctx context.Context, c *websocket.Conn, ident string) {
		send(ctx, c, map[string]any{"type": "ping", "message": 1})
		send(ctx, c, map[string]any{"identifier": `{"channel":"OtherChannel"}`, "message": map[string]any{"type": "dispatch", "dispatch_id": 9}})
		send(ctx, c, map[string]any{"identifier": ident, "message": map[string]any{"type": "something_else"}})
		send(ctx, c, map[string]any{"identifier": ident, "message": map[string]any{
			"type": "dispatch", "dispatch_id": 7, "session_id": 70, "runner_id": 3, "kind": "work_goal", "purpose": "code",
			"reference": "collie work_goal #148 in collie-agent", "project": "collies", "codebase": "collie-agent",
			"mcp_url": "https://collie.test/p/collies/mcp", "ack_deadline": "2026-11-25T10:00:30Z"}})
		<-ctx.Done()
	})
	c := &Client{Server: srv.URL, Token: "tok"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	subscribed := false
	var got []machine.Dispatch
	err := c.Run(ctx, func() { subscribed = true }, func(d machine.Dispatch) {
		got = append(got, d)
		cancel()
	}, nil)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !subscribed {
		t.Error("onSubscribed not called")
	}
	if len(got) != 1 || got[0].DispatchID != 7 || got[0].SessionID != 70 || got[0].RunnerID != 3 || got[0].Reference != "collie work_goal #148 in collie-agent" {
		t.Errorf("dispatches = %+v", got)
	}
}

func TestUnauthorized(t *testing.T) {
	srv := server(t, func(ctx context.Context, c *websocket.Conn, ident string) {})
	c := &Client{Server: srv.URL, Token: "wrong"}
	if err := c.Run(context.Background(), nil, func(machine.Dispatch) {}, nil); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("bad token at the handshake: %v", err)
	}

	srv2 := server(t, func(ctx context.Context, c *websocket.Conn, ident string) {
		send(ctx, c, map[string]any{"type": "disconnect", "reason": "unauthorized", "reconnect": false})
		<-ctx.Done()
	})
	c2 := &Client{Server: srv2.URL, Token: "tok"}
	if err := c2.Run(context.Background(), nil, func(machine.Dispatch) {}, nil); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("disconnect unauthorized: %v", err)
	}
}

func TestConnectionFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := &Client{Server: srv.URL, Token: "tok"}
	err := c.Run(context.Background(), nil, func(machine.Dispatch) {}, nil)
	if err == nil || errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v", err)
	}
}

func TestURL(t *testing.T) {
	cases := map[string]string{"https://collies.sh": "wss://collies.sh/cable", "http://localhost:3000": "ws://localhost:3000/cable", "https://x.test/sub": "wss://x.test/sub/cable"}
	for in, want := range cases {
		got, err := (&Client{Server: in}).URL()
		if err != nil || got != want {
			t.Errorf("URL(%q) = %q, %v", in, got, err)
		}
	}
	if o := (&Client{Server: "https://collies.sh/sub"}).Origin(); o != "https://collies.sh" {
		t.Errorf("Origin = %q", o)
	}
}

func TestCodebasesChangedCallsItsCallback(t *testing.T) {
	srv := server(t, func(ctx context.Context, c *websocket.Conn, ident string) {
		send(ctx, c, map[string]any{"identifier": ident, "message": map[string]any{"type": "codebases_changed", "codebases_version": 7}})
		send(ctx, c, map[string]any{"identifier": `{"channel":"OtherChannel"}`, "message": map[string]any{"type": "codebases_changed", "codebases_version": 99}})
		send(ctx, c, map[string]any{"identifier": ident, "message": map[string]any{"type": "codebases_changed"}})
		send(ctx, c, map[string]any{"identifier": ident, "message": map[string]any{"type": "dispatch", "dispatch_id": 5, "session_id": 50}})
		<-ctx.Done()
	})
	c := &Client{Server: srv.URL, Token: "tok"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var versions []string
	var got []machine.Dispatch
	err := c.Run(ctx, nil, func(d machine.Dispatch) {
		got = append(got, d)
		cancel()
	}, func(v string) { versions = append(versions, v) })
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(versions) != 2 || versions[0] != "7" || versions[1] != "" {
		t.Errorf("codebases_changed versions = %q", versions)
	}
	if len(got) != 1 || got[0].DispatchID != 5 {
		t.Errorf("dispatches = %+v", got)
	}
}
