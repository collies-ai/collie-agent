package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/collies-ai/collie-agent/internal/cable"
	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/discovery"
	"github.com/collies-ai/collie-agent/internal/dispatch"
	"github.com/collies-ai/collie-agent/internal/machine"
)

type ackRec struct {
	DispatchID int64
	Accepted   bool
	Reason     string
}

// fakeCollie implements the machine API and MachineChannel well enough for collie-agent: offers stay pending until
// acked, the cable pushes them on subscribe, the long-poll serves them.
type fakeCollie struct {
	t           *testing.T
	srv         *httptest.Server
	cableDown   bool
	bindings    []machine.Binding
	codebases   []machine.Codebase
	revokeAfter int // heartbeats before answering 401 (0: never)

	mu           sync.Mutex
	offers       []machine.Dispatch
	acks         []ackRec
	inventory    [][]machine.InventoryRepo
	heartbeats   []machine.Heartbeat
	bindingsV    int
	bindingsGets int
	polls        int
	subscribed   int
}

func newFakeCollie(t *testing.T) *fakeCollie {
	f := &fakeCollie{t: t, bindingsV: 1}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCollie) offer(d machine.Dispatch) {
	f.mu.Lock()
	f.offers = append(f.offers, d)
	f.mu.Unlock()
}

func (f *fakeCollie) snapshotAcks() []ackRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ackRec(nil), f.acks...)
}

func (f *fakeCollie) pending() []machine.Dispatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]machine.Dispatch(nil), f.offers...)
}

func (f *fakeCollie) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/cable":
		f.serveCable(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/machine/v1/heartbeat":
		var hb machine.Heartbeat
		_ = json.NewDecoder(r.Body).Decode(&hb)
		f.mu.Lock()
		f.heartbeats = append(f.heartbeats, hb)
		n, v := len(f.heartbeats), f.bindingsV
		f.mu.Unlock()
		if f.revokeAfter > 0 && n > f.revokeAfter {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bindings_version": v})
	case r.Method == http.MethodGet && r.URL.Path == "/machine/v1/codebases":
		_ = json.NewEncoder(w).Encode(f.codebases)
	case r.Method == http.MethodPut && r.URL.Path == "/machine/v1/inventory":
		var body struct {
			Repos []machine.InventoryRepo `json:"repos"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.inventory = append(f.inventory, body.Repos)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(body)
	case r.Method == http.MethodGet && r.URL.Path == "/machine/v1/bindings":
		f.mu.Lock()
		f.bindingsGets++
		b := f.bindings
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(b)
	case r.Method == http.MethodGet && r.URL.Path == "/machine/v1/dispatches":
		f.mu.Lock()
		f.polls++
		f.mu.Unlock()
		deadline := time.Now().Add(200 * time.Millisecond)
		for {
			if p := f.pending(); len(p) > 0 || time.Now().After(deadline) {
				_ = json.NewEncoder(w).Encode(map[string]any{"dispatches": p})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/machine/v1/dispatches/") && strings.HasSuffix(r.URL.Path, "/ack"):
		var body struct {
			Accepted bool   `json:"accepted"`
			Reason   string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var id int64
		_, _ = fmtSscan(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/machine/v1/dispatches/"), "/ack"), &id)
		f.mu.Lock()
		defer f.mu.Unlock()
		idx := -1
		for i, o := range f.offers {
			if o.DispatchID == id {
				idx = i
			}
		}
		if idx < 0 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"gone"}`))
			return
		}
		o := f.offers[idx]
		f.offers = append(f.offers[:idx], f.offers[idx+1:]...)
		f.acks = append(f.acks, ackRec{id, body.Accepted, body.Reason})
		if body.Accepted {
			_ = json.NewEncoder(w).Encode(map[string]any{"session_id": o.SessionID, "dispatch_id": id, "mcp_url": f.srv.URL + "/p/collies/mcp"})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"declined": true})
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCollie) serveCable(w http.ResponseWriter, r *http.Request) {
	if f.cableDown {
		http.Error(w, "cable down", http.StatusServiceUnavailable)
		return
	}
	if got, want := r.Header.Get("Origin"), f.srv.URL; got != want {
		f.t.Errorf("cable Origin = %q, want %q", got, want)
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"actioncable-v1-json"}, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"welcome"}`))
	_, data, err := c.Read(ctx)
	if err != nil {
		return
	}
	var sub map[string]string
	_ = json.Unmarshal(data, &sub)
	ident := sub["identifier"]
	b, _ := json.Marshal(map[string]string{"identifier": ident, "type": "confirm_subscription"})
	_ = c.Write(ctx, websocket.MessageText, b)
	f.mu.Lock()
	f.subscribed++
	f.mu.Unlock()
	// Push pending offers as they appear, as MachineChannel does.
	sent := map[int64]bool{}
	for ctx.Err() == nil {
		for _, o := range f.pending() {
			if !sent[o.DispatchID] {
				sent[o.DispatchID] = true
				msg, _ := json.Marshal(map[string]any{"identifier": ident, "message": o})
				if c.Write(ctx, websocket.MessageText, msg) != nil {
					return
				}
			}
		}
		ping, _ := json.Marshal(map[string]any{"type": "ping", "message": time.Now().Unix()})
		if c.Write(ctx, websocket.MessageText, ping) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type recSessions struct {
	mu      sync.Mutex
	started []dispatch.Session
}

func (s *recSessions) CanRun(string) error { return nil }
func (s *recSessions) Start(_ context.Context, sess dispatch.Session, _ func()) error {
	s.mu.Lock()
	s.started = append(s.started, sess)
	s.mu.Unlock()
	return nil // the session keeps running: its slot stays taken
}

func shortSleep(ctx context.Context, d time.Duration) error {
	if d > 20*time.Millisecond {
		d = 20 * time.Millisecond
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

// setup builds a root folder with the bound repo and a runner against f.
func setup(t *testing.T, f *fakeCollie) (*Runner, *recSessions, string) {
	t.Helper()
	p := testProfile(t, "")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(root, "collie")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:jfromaniello/collie.git\n"), 0o644)
	other := filepath.Join(root, "other")
	_ = os.MkdirAll(filepath.Join(other, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(other, ".git", "config"), []byte("[remote \"origin\"]\n\turl = https://github.com/someone/else\n"), 0o644)

	f.codebases = []machine.Codebase{{CodebaseID: 1, Codebase: "collie", Project: "collies", RepoFullName: "jfromaniello/collie"}}
	f.bindings = []machine.Binding{
		{RunnerID: 11, Project: "collies", Codebase: "collie", CodebaseID: 1, Purpose: "code", Folder: repo, Agents: map[string]string{"code": "claude"}, MaxConcurrency: 1},
		{RunnerID: 12, Project: "collies", Codebase: "evil", Purpose: "code", Folder: "/etc", Agents: map[string]string{"code": "claude"}, MaxConcurrency: 5},
	}
	client := machine.NewClient(f.srv.URL, "tok", "test")
	sess := &recSessions{}
	r := &Runner{
		Profile: p, MachineID: "42", API: client, Remote: client,
		Cable:      &cable.Client{Server: f.srv.URL, Token: "tok"},
		Sessions:   sess,
		Settings:   &config.Settings{RootFolders: []string{root}, MaxConcurrency: 3},
		Sleep:      shortSleep,
		FindAgents: func(context.Context) []machine.Agent { return []machine.Agent{{Name: "claude", Version: "2.1.0"}} },
		ScanRepos:  discovery.ScanRepos,
	}
	return r, sess, repo
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func offerFor(id, runner int64) machine.Dispatch {
	return machine.Dispatch{Type: "dispatch", DispatchID: id, SessionID: id * 10, RunnerID: runner, Kind: "work_goal", Purpose: "code",
		Reference: "collie work_goal #148 in collie", Project: "collies", Codebase: "collie"}
}

func checkAcks(t *testing.T, f *fakeCollie, sess *recSessions, repo string) {
	t.Helper()
	acks := f.snapshotAcks()
	byID := map[int64]ackRec{}
	for _, a := range acks {
		byID[a.DispatchID] = a
	}
	accepted := 0
	for _, id := range []int64{1, 2} {
		if byID[id].Accepted {
			accepted++
		} else if byID[id].Reason != "binding at capacity (1 of 1 sessions)" {
			t.Errorf("dispatch %d declined with %q", id, byID[id].Reason)
		}
	}
	if accepted != 1 {
		t.Errorf("accepted %d of the two offers for a binding with max_concurrency 1: %+v", accepted, acks)
	}
	if a := byID[3]; a.Accepted || !strings.Contains(a.Reason, "outside the root folders") {
		t.Errorf("offer for the binding outside the roots: %+v", a)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.started) != 1 || sess.started[0].Binding.Folder != repo || sess.started[0].Agent != "claude" {
		t.Errorf("sessions started = %+v", sess.started)
	}
}

func TestDispatchesOverTheCable(t *testing.T) {
	f := newFakeCollie(t)
	r, sess, repo := setup(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Loop(ctx) }()

	waitFor(t, "the cable subscription", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.subscribed > 0 })
	f.offer(offerFor(1, 11))
	f.offer(offerFor(2, 11))
	f.offer(offerFor(3, 12))
	waitFor(t, "three acks", func() bool { return len(f.snapshotAcks()) == 3 })
	checkAcks(t, f, sess, repo)

	f.mu.Lock()
	polls := f.polls
	inv := f.inventory
	hbs := f.heartbeats
	f.mu.Unlock()
	if polls != 0 {
		t.Errorf("long-polled %d times while the cable was up", polls)
	}
	if len(inv) == 0 || len(inv[0]) != 1 || inv[0][0].CodebaseID != 1 || inv[0][0].Path != repo {
		t.Errorf("inventory = %+v", inv)
	}
	if hbs[0].MaxConcurrency != 3 || len(hbs[0].Agents) != 1 || hbs[0].Agents[0].Name != "claude" {
		t.Errorf("heartbeat = %+v", hbs[0])
	}
	// The accepted session shows up in the next heartbeat's running list.
	waitFor(t, "a heartbeat with the running session", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		last := f.heartbeats[len(f.heartbeats)-1]
		return len(last.Running) == 1
	})
	st, _ := r.Profile.LoadState()
	if st.Connection != "cable" || len(st.Bindings) != 2 || st.Bindings[1].Refused == "" || st.Repos != 1 {
		t.Errorf("state = %+v", st)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Loop = %v", err)
	}
}

func TestDispatchesOverLongPollWhenTheCableIsDown(t *testing.T) {
	f := newFakeCollie(t)
	f.cableDown = true
	r, sess, repo := setup(t, f)
	f.offer(offerFor(1, 11))
	f.offer(offerFor(2, 11))
	f.offer(offerFor(3, 12))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Loop(ctx) }()

	waitFor(t, "three acks", func() bool { return len(f.snapshotAcks()) == 3 })
	checkAcks(t, f, sess, repo)
	f.mu.Lock()
	polls := f.polls
	f.mu.Unlock()
	if polls == 0 {
		t.Error("never long-polled")
	}
	st, _ := r.Profile.LoadState()
	if st.Connection != "long-poll" {
		t.Errorf("connection = %q", st.Connection)
	}
	cancel()
	<-done
}

func TestBindingsResyncWhenTheVersionChanges(t *testing.T) {
	f := newFakeCollie(t)
	r, _, _ := setup(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Loop(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the first bindings sync", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.bindingsGets == 1 })
	time.Sleep(100 * time.Millisecond) // several heartbeats with the same version
	f.mu.Lock()
	if f.bindingsGets != 1 {
		t.Errorf("bindings fetched %d times without a version change", f.bindingsGets)
	}
	f.bindings = f.bindings[:1]
	f.bindingsV = 2
	f.mu.Unlock()
	waitFor(t, "a resync", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.bindingsGets == 2 })
	waitFor(t, "the state to show one binding", func() bool {
		st, _ := r.Profile.LoadState()
		return len(st.Bindings) == 1
	})
}

func TestRevocationStopsEverything(t *testing.T) {
	f := newFakeCollie(t)
	f.revokeAfter = 3
	r, _, _ := setup(t, f)
	done := make(chan error, 1)
	go func() { done <- r.Loop(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, machine.ErrRevoked) {
			t.Fatalf("Loop = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Loop did not stop after a 401")
	}
	st, _ := r.Profile.LoadState()
	if !st.Revoked || st.Connection != "" {
		t.Errorf("state = %+v", st)
	}
}

func fmtSscan(s string, id *int64) (int, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int64(c-'0')
	}
	*id = n
	return 1, nil
}

// dropCable delivers one offer and then drops the connection right away, before the ack comes back.
type dropCable struct{ offered sync.Once }

func (c *dropCable) Run(ctx context.Context, onSubscribed func(), onDispatch func(machine.Dispatch)) error {
	onSubscribed()
	c.offered.Do(func() { onDispatch(offerFor(1, 11)) })
	return errors.New("socket closed")
}

// slowRevokedAck is a Remote whose ack answers 401 after the cable has already moved on.
type slowRevokedAck struct {
	*machine.Client
}

func (s slowRevokedAck) Ack(ctx context.Context, id int64, accept bool, reason string) (*machine.Accepted, error) {
	select {
	case <-time.After(150 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, machine.ErrRevoked
}

func (s slowRevokedAck) PollDispatches(ctx context.Context, wait time.Duration) ([]machine.Dispatch, error) {
	select {
	case <-time.After(50 * time.Millisecond):
	case <-ctx.Done():
	}
	return nil, nil
}

func TestALateRevokedAckStopsTheRunnerAfterAReconnect(t *testing.T) {
	f := newFakeCollie(t)
	r, _, _ := setup(t, f)
	r.Remote = slowRevokedAck{machine.NewClient(f.srv.URL, "tok", "test")}
	r.Cable = &dropCable{}
	done := make(chan error, 1)
	go func() { done <- r.Loop(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, machine.ErrRevoked) {
			t.Fatalf("Loop = %v, want ErrRevoked", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a 401 from an ack that finished after the cable reconnected did not stop the runner")
	}
}

func TestACable401StopsTheRunnerWithoutPolling(t *testing.T) {
	for _, mode := range []string{"refused at the upgrade", "disconnect unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeCollie(t)
			r, _, _ := setup(t, f)
			// Only the cable rejects the token; the HTTP API still accepts it, so a fallback would keep polling.
			r.Cable = cableRejecting{mode: mode}
			done := make(chan error, 1)
			go func() { done <- r.Loop(context.Background()) }()
			select {
			case err := <-done:
				if !errors.Is(err, machine.ErrRevoked) {
					t.Fatalf("Loop = %v, want ErrRevoked", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a 401 on the cable did not stop the runner")
			}
			f.mu.Lock()
			polls := f.polls
			f.mu.Unlock()
			if polls != 0 {
				t.Errorf("long-polled %d times after the cable refused the token", polls)
			}
			st, _ := r.Profile.LoadState()
			if !st.Revoked {
				t.Errorf("state = %+v", st)
			}
		})
	}
}

// cableRejecting runs the real cable client against a server that refuses the token on the cable only.
type cableRejecting struct{ mode string }

func (c cableRejecting) Run(ctx context.Context, onSubscribed func(), onDispatch func(machine.Dispatch)) error {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.mode == "refused at the upgrade" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"actioncable-v1-json"}, InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"disconnect","reason":"unauthorized","reconnect":false}`))
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()
	return (&cable.Client{Server: srv.URL, Token: "tok"}).Run(ctx, onSubscribed, onDispatch)
}

// revokingSessions is a session runner whose session endpoint answers 401 as soon as a session starts.
type revokingSessions struct {
	recSessions
	onRevoked func(error)
}

func (s *revokingSessions) OnRevoked(f func(error)) { s.onRevoked = f }
func (s *revokingSessions) Start(ctx context.Context, sess dispatch.Session, done func()) error {
	_ = s.recSessions.Start(ctx, sess, done)
	go s.onRevoked(fmt.Errorf("renewing the session token: %w", machine.ErrRevoked))
	return nil
}

func TestA401OnASessionEndpointStopsTheRunner(t *testing.T) {
	f := newFakeCollie(t)
	r, _, _ := setup(t, f)
	sess := &revokingSessions{}
	r.Sessions = sess
	done := make(chan error, 1)
	go func() { done <- r.Loop(context.Background()) }()
	waitFor(t, "the cable subscription", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.subscribed > 0 })
	f.offer(offerFor(1, 11)) // the dispatch channel is connected and well
	select {
	case err := <-done:
		if !errors.Is(err, machine.ErrRevoked) {
			t.Fatalf("Loop = %v, want ErrRevoked", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a 401 from a session endpoint did not stop the runner")
	}
	if st, _ := r.Profile.LoadState(); !st.Revoked {
		t.Error("the state does not say the machine was revoked")
	}
}
