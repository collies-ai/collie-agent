package machine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnauthorizedIsRevokedEvenWhenTheBodyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Promise a body, send part of it and drop the connection.
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"inv`))
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "tok", "test").Heartbeat(context.Background(), Heartbeat{})
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("err = %v, want ErrRevoked", err)
	}
}

func TestServerErrorIsNotRevoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "tok", "test").Heartbeat(context.Background(), Heartbeat{})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 502 || errors.Is(err, ErrRevoked) {
		t.Fatalf("err = %v", err)
	}
}

func TestDispatchDecodesCollieMessage(t *testing.T) {
	// Exactly what collie's MachineSession#message sends: reference is the dispatch's bigint column.
	raw := `{"type":"dispatch","dispatch_id":91,"session_id":7,"runner_id":3,"kind":"work_goal","purpose":"code",
	"reference":148,"payload":"collie work_goal #148 in collie-agent","project":"collies","codebase":"collie-agent",
	"mcp_url":"https://collies.sh/p/collies/mcp","ack_deadline":"2026-11-25T10:00:30Z"}`
	var d Dispatch
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("decoding collie's message: %v", err)
	}
	if d.Reference != "148" || d.DispatchID != 91 || d.SessionID != 7 {
		t.Errorf("dispatch = %+v", d)
	}
	var s Dispatch
	if err := json.Unmarshal([]byte(`{"type":"dispatch","dispatch_id":1,"reference":"212"}`), &s); err != nil || s.Reference != "212" {
		t.Errorf("string reference: %+v, %v", s, err)
	}
}

func TestInventoryJSONShape(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", "test")
	err := c.PutInventory(context.Background(), []InventoryRepo{
		{CodebaseID: 1, Path: "/code/collie", Source: "claude", Remote: "origin", Alternatives: []InventoryClone{
			{Path: "/code/fork", Source: "root", Remote: "upstream"},
		}},
		{CodebaseID: 2, Path: "/code/agent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"repos":[{"codebase_id":1,"path":"/code/collie","source":"claude","remote":"origin","alternatives":[{"path":"/code/fork","source":"root","remote":"upstream"}]},{"codebase_id":2,"path":"/code/agent"}]}`
	if strings.TrimSpace(body) != want {
		t.Errorf("inventory body =\n%s\nwant\n%s", body, want)
	}
}

func TestHeartbeatReadsCodebasesVersion(t *testing.T) {
	for resp, want := range map[string]string{
		`{"bindings_version": 3, "codebases_version": 12}`:   "12",
		`{"bindings_version": 3, "codebases_version": null}`: "",
		`{"bindings_version": 3}`:                            "",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(resp)) }))
		got, err := NewClient(srv.URL, "tok", "test").Heartbeat(context.Background(), Heartbeat{})
		srv.Close()
		if err != nil || got.CodebasesVersion != want || got.BindingsVersion != "3" {
			t.Errorf("%s: %+v, %v", resp, got, err)
		}
	}
}

// The review an agent answered with travels with the finished event, as JSON; without one the field is left out.
func TestSessionFinishedSendsTheReview(t *testing.T) {
	var bodies []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", "test")
	review := json.RawMessage(`{"verdict":"approve","summary":"ok","reason":"","findings":[]}`)
	if err := c.SessionFinished(context.Background(), 7, 0, nil, review); err != nil {
		t.Fatal(err)
	}
	if err := c.SessionFinished(context.Background(), 8, 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	if string(bodies[0]["review"]) != string(review) {
		t.Errorf("review sent = %s", bodies[0]["review"])
	}
	if _, ok := bodies[1]["review"]; ok {
		t.Error("a finished event without a review sent the field")
	}
}
