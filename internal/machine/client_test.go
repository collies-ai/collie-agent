package machine

import (
	"context"
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
