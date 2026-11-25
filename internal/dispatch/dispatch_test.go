package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/collies-ai/collie-agent/internal/bindings"
	"github.com/collies-ai/collie-agent/internal/machine"
)

type ack struct {
	DispatchID int64
	Accept     bool
	Reason     string
}

type fakeAPI struct {
	mu   sync.Mutex
	acks []ack
	err  map[int64]error
}

func (f *fakeAPI) Ack(_ context.Context, id int64, accept bool, reason string) (*machine.Accepted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, ack{id, accept, reason})
	if err := f.err[id]; err != nil {
		return nil, err
	}
	if !accept {
		return nil, nil
	}
	return &machine.Accepted{SessionID: id * 100, DispatchID: id, MCPURL: "https://collie.test/p/collies/mcp"}, nil
}

type fakeSessions struct {
	mu      sync.Mutex
	started []Session
	done    map[int64]func()
	refuse  string
}

func (f *fakeSessions) CanRun(agent string) error {
	if f.refuse != "" && agent == f.refuse {
		return fmt.Errorf("%s is not installed", agent)
	}
	return nil
}

func (f *fakeSessions) Start(_ context.Context, s Session, done func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, s)
	if f.done == nil {
		f.done = map[int64]func(){}
	}
	f.done[s.ID] = done
	return nil
}

func binding(runner int64, folder string, max int) bindings.Binding {
	return bindings.Binding{Binding: machine.Binding{RunnerID: runner, Project: "collies", Codebase: "collie", Purpose: "code",
		Folder: folder, Agents: map[string]string{"code": "claude", "reviewer": "codex"}, MaxConcurrency: max}}
}

func offer(id, runner int64) machine.Dispatch {
	return machine.Dispatch{Type: "dispatch", DispatchID: id, SessionID: id + 1000, RunnerID: runner, Kind: "work_goal", Purpose: "code", Reference: "collie work_goal #148"}
}

func TestAcceptsUpToCapacityThenDeclines(t *testing.T) {
	api := &fakeAPI{}
	sess := &fakeSessions{}
	d := &Dispatcher{API: api, Sessions: sess, MaxConcurrency: 2}
	d.SetBindings([]bindings.Binding{binding(1, "/code/collie", 1), binding(2, "/code/agent", 5)})
	ctx := context.Background()

	_ = d.Handle(ctx, offer(1, 1)) // accepted: binding 1 now full
	_ = d.Handle(ctx, offer(2, 1)) // declined: binding 1 at capacity
	_ = d.Handle(ctx, offer(3, 2)) // accepted: machine now full (2 of 2)
	_ = d.Handle(ctx, offer(4, 2)) // declined: machine at capacity

	want := []ack{
		{1, true, ""},
		{2, false, "binding at capacity (1 of 1 sessions)"},
		{3, true, ""},
		{4, false, "machine at capacity (2 of 2 sessions)"},
	}
	if fmt.Sprint(api.acks) != fmt.Sprint(want) {
		t.Fatalf("acks = %+v\nwant %+v", api.acks, want)
	}
	if len(sess.started) != 2 || sess.started[0].Agent != "claude" || sess.started[0].ID != 100 || sess.started[0].Binding.Folder != "/code/collie" {
		t.Errorf("started = %+v", sess.started)
	}
	if got := d.Running(); fmt.Sprint(got) != "[100 300]" {
		t.Errorf("Running = %v", got)
	}

	// A session ends: its slot frees up.
	sess.done[100]()
	_ = d.Handle(ctx, offer(5, 1))
	if last := api.acks[len(api.acks)-1]; !last.Accept {
		t.Errorf("after a session ended: %+v", last)
	}
}

func TestDeclineReasons(t *testing.T) {
	refused := binding(3, "/etc", 1)
	refused.Refused = `folder "/etc" is outside the root folders`
	cases := []struct {
		name   string
		offer  machine.Dispatch
		reason string
	}{
		{"unknown runner", offer(1, 9), "runner 9 is not bound on this machine"},
		{"refused binding", offer(2, 3), `binding refused by the machine: folder "/etc" is outside the root folders`},
		{"no agent for the purpose", func() machine.Dispatch { o := offer(3, 1); o.Purpose = "qa-browser"; return o }(), `no agent configured for purpose "qa-browser"`},
		{"agent not runnable", func() machine.Dispatch { o := offer(4, 1); o.Purpose = "reviewer"; return o }(), "codex is not installed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := &fakeAPI{}
			sess := &fakeSessions{refuse: "codex"}
			d := &Dispatcher{API: api, Sessions: sess, MaxConcurrency: 4}
			d.SetBindings([]bindings.Binding{binding(1, "/code/collie", 2), refused})
			if err := d.Handle(context.Background(), c.offer); err != nil {
				t.Fatal(err)
			}
			if len(api.acks) != 1 || api.acks[0].Accept || api.acks[0].Reason != c.reason {
				t.Errorf("acks = %+v, want a decline %q", api.acks, c.reason)
			}
			if len(sess.started) != 0 {
				t.Error("a declined offer started a session")
			}
		})
	}
}

func TestWithoutASessionRunnerEveryOfferIsDeclined(t *testing.T) {
	api := &fakeAPI{}
	d := &Dispatcher{API: api, Sessions: Unsupported{}, MaxConcurrency: 2}
	d.SetBindings([]bindings.Binding{binding(1, "/code/collie", 1)})
	_ = d.Handle(context.Background(), offer(1, 1))
	if len(api.acks) != 1 || api.acks[0].Accept || !strings.Contains(api.acks[0].Reason, "cannot run sessions yet") {
		t.Errorf("acks = %+v", api.acks)
	}
}

func TestTheSameOfferIsAckedOnce(t *testing.T) {
	// The cable replays pending offers on subscribe and the long-poll may serve them too.
	api := &fakeAPI{}
	d := &Dispatcher{API: api, Sessions: &fakeSessions{}, MaxConcurrency: 2}
	d.SetBindings([]bindings.Binding{binding(1, "/code/collie", 2)})
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = d.Handle(context.Background(), offer(1, 1)) }()
	}
	wg.Wait()
	if len(api.acks) != 1 {
		t.Errorf("acked %d times", len(api.acks))
	}
	// A re-offer of the same dispatch (a new session id) is a new offer.
	o := offer(1, 1)
	o.SessionID = 5555
	_ = d.Handle(context.Background(), o)
	if len(api.acks) != 2 {
		t.Errorf("re-offer: acked %d times in total", len(api.acks))
	}
}

func TestConcurrentOffersNeverExceedCapacity(t *testing.T) {
	api := &fakeAPI{}
	sess := &fakeSessions{}
	d := &Dispatcher{API: api, Sessions: sess, MaxConcurrency: 3}
	d.SetBindings([]bindings.Binding{binding(1, "/code/collie", 10)})
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(id int64) { defer wg.Done(); _ = d.Handle(context.Background(), offer(id, 1)) }(int64(i + 1))
	}
	wg.Wait()
	accepted := 0
	for _, a := range api.acks {
		if a.Accept {
			accepted++
		}
	}
	if accepted != 3 || len(sess.started) != 3 || len(api.acks) != 20 {
		t.Errorf("accepted %d, started %d, acks %d", accepted, len(sess.started), len(api.acks))
	}
}

func TestGoneAndRevoked(t *testing.T) {
	api := &fakeAPI{err: map[int64]error{1: fmt.Errorf("%w: expired", machine.ErrGone), 2: machine.ErrRevoked, 3: errors.New("network down")}}
	sess := &fakeSessions{}
	d := &Dispatcher{API: api, Sessions: sess, MaxConcurrency: 2}
	d.SetBindings([]bindings.Binding{binding(1, "/code/collie", 2)})
	ctx := context.Background()
	if err := d.Handle(ctx, offer(1, 1)); err != nil {
		t.Errorf("gone: %v", err)
	}
	if err := d.Handle(ctx, offer(2, 1)); !errors.Is(err, machine.ErrRevoked) {
		t.Errorf("revoked: %v", err)
	}
	if err := d.Handle(ctx, offer(3, 1)); err != nil {
		t.Errorf("network: %v", err)
	}
	if len(sess.started) != 0 || len(d.Running()) != 0 {
		t.Error("a failed ack started or kept a session")
	}
	// After a network error the same offer may be tried again when collie re-sends it.
	delete(api.err, 3)
	_ = d.Handle(ctx, offer(3, 1))
	if len(sess.started) != 1 {
		t.Error("the retried offer was not accepted")
	}
}
