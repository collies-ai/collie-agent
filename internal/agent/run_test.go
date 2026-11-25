package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/machine"
)

func testProfile(t *testing.T, name string) config.Profile {
	t.Helper()
	t.Setenv("COLLIE_AGENT_CONFIG_DIR", t.TempDir())
	p, err := config.Resolve(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeSleep records the waits and cancels the loop after max of them.
func fakeSleep(waits *[]time.Duration, max int, cancel func()) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		if len(*waits) >= max {
			cancel()
			return context.Canceled
		}
		return nil
	}
}

func TestLoopSendsHeartbeatsAndStopsOn401(t *testing.T) {
	p := testProfile(t, "")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Path != "/machine/v1/heartbeat" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		var hb map[string]any
		_ = json.NewDecoder(r.Body).Decode(&hb)
		for _, k := range []string{"version", "os", "max_concurrency", "agents", "running"} {
			if _, ok := hb[k]; !ok {
				t.Errorf("heartbeat lacks %q: %v", k, hb)
			}
		}
		switch n {
		case 1:
			_, _ = w.Write([]byte(`{"bindings_version": 3}`))
		case 2:
			w.WriteHeader(503)
		default:
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
		}
	}))
	defer srv.Close()

	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Runner{Profile: p, MachineID: "42", API: machine.NewClient(srv.URL, "tok", "test"), Sleep: fakeSleep(&waits, 100, cancel)}
	err := r.Loop(ctx)
	if !errors.Is(err, machine.ErrRevoked) {
		t.Fatalf("Loop = %v, want ErrRevoked", err)
	}
	if calls.Load() != 3 {
		t.Errorf("heartbeats = %d, want 3 (no retry after 401)", calls.Load())
	}
	if len(waits) != 2 || waits[0] != HeartbeatInterval || waits[1] != 5*time.Second {
		t.Errorf("waits = %v, want [30s 5s]", waits)
	}
	st, _ := p.LoadState()
	if !st.Revoked || st.MachineID != "42" || st.BindingsVersion != "3" || st.LastHeartbeatAt.IsZero() {
		t.Errorf("state = %+v", st)
	}
}

type scripted struct {
	errs []error
	n    int
}

func (s *scripted) Heartbeat(context.Context, machine.Heartbeat) (*machine.HeartbeatResponse, error) {
	i := s.n
	s.n++
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	return &machine.HeartbeatResponse{BindingsVersion: "1"}, nil
}

func TestLoopBacksOffAndRecovers(t *testing.T) {
	p := testProfile(t, "")
	boom := errors.New("connection refused")
	api := &scripted{errs: []error{boom, boom, boom, nil}}
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Runner{Profile: p, MachineID: "1", API: api, Sleep: fakeSleep(&waits, 5, cancel)}
	if err := r.Loop(ctx); err != nil {
		t.Fatalf("Loop = %v", err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, HeartbeatInterval, HeartbeatInterval}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("waits = %v, want %v", waits, want)
			break
		}
	}
	st, _ := p.LoadState()
	if st.LastError != "" || st.Revoked {
		t.Errorf("state after recovery = %+v", st)
	}
}

func TestBackoffIsCapped(t *testing.T) {
	d := time.Duration(0)
	for range 20 {
		d = nextBackoff(d)
	}
	if d != MaxBackoff {
		t.Errorf("backoff = %s", d)
	}
}

func TestOneInstancePerProfile(t *testing.T) {
	p := testProfile(t, "")
	unlock, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if !IsLocked(p) {
		t.Error("IsLocked = false while locked")
	}
	if _, err := Lock(p); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Lock = %v, want ErrAlreadyRunning", err)
	}
	// Another profile is another instance.
	other, _ := config.Resolve("work")
	unlockOther, err := Lock(other)
	if err != nil {
		t.Fatalf("Lock(work) = %v", err)
	}
	unlockOther()
	unlock()
	if IsLocked(p) {
		t.Error("IsLocked = true after unlock")
	}
	again, err := Lock(p)
	if err != nil {
		t.Fatalf("Lock after unlock = %v", err)
	}
	again()
}
