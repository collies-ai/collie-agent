package machine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
