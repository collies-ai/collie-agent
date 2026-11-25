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
