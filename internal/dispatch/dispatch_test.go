package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// setBindings creates each "/code/..." folder under a temporary root folder and gives the dispatcher the bindings
// with those real paths, plus that root. Other folders (e.g. /etc) are left as they are.
func setBindings(t *testing.T, d *Dispatcher, bs []bindings.Binding) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range bs {
		if rest, ok := strings.CutPrefix(bs[i].Folder, "/code/"); ok {
			bs[i].Folder = filepath.Join(root, rest)
			if err := os.MkdirAll(bs[i].Folder, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	d.SetBindings(bs, bindings.Allowed{Roots: []string{root}})
	return root
}

func offer(id, runner int64) machine.Dispatch {
	return machine.Dispatch{Type: "dispatch", DispatchID: id, SessionID: id + 1000, RunnerID: runner, Kind: "work_goal", Purpose: "code", Reference: "collie work_goal #148"}
}

func TestAcceptsUpToCapacityThenDeclines(t *testing.T) {
	api := &fakeAPI{}
	sess := &fakeSessions{}
	d := &Dispatcher{API: api, Sessions: sess, MaxConcurrency: 2}
	setBindings(t, d, []bindings.Binding{binding(1, "/code/collie", 1), binding(2, "/code/agent", 5)})
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
	if len(sess.started) != 2 || sess.started[0].Agent != "claude" || sess.started[0].ID != 100 || !strings.HasSuffix(sess.started[0].Binding.Folder, "/collie") {
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
			setBindings(t, d, []bindings.Binding{binding(1, "/code/collie", 2), refused})
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
	setBindings(t, d, []bindings.Binding{binding(1, "/code/collie", 1)})
	_ = d.Handle(context.Background(), offer(1, 1))
	if len(api.acks) != 1 || api.acks[0].Accept || !strings.Contains(api.acks[0].Reason, "cannot run sessions yet") {
		t.Errorf("acks = %+v", api.acks)
	}
}

func TestTheSameOfferIsAckedOnce(t *testing.T) {
	// The cable replays pending offers on subscribe and the long-poll may serve them too.
	api := &fakeAPI{}
	d := &Dispatcher{API: api, Sessions: &fakeSessions{}, MaxConcurrency: 2}
	setBindings(t, d, []bindings.Binding{binding(1, "/code/collie", 2)})
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
	setBindings(t, d, []bindings.Binding{binding(1, "/code/collie", 10)})
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
	setBindings(t, d, []bindings.Binding{binding(1, "/code/collie", 2)})
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

func TestAFailedDeclineIsRetriedOnRedelivery(t *testing.T) {
	api := &fakeAPI{err: map[int64]error{1: errors.New("connection reset")}}
	d := &Dispatcher{API: api, Sessions: &fakeSessions{}, MaxConcurrency: 2}
	setBindings(t, d, []bindings.Binding{binding(1, "/code/collie", 1)})
	o := offer(1, 9) // runner 9 is not bound: declined
	_ = d.Handle(context.Background(), o)
	delete(api.err, 1)
	_ = d.Handle(context.Background(), o) // collie redelivers the same offer
	if len(api.acks) != 2 || api.acks[1].Accept || api.acks[1].Reason != "runner 9 is not bound on this machine" {
		t.Errorf("acks = %+v, want the decline sent again", api.acks)
	}
	// A decline collie did get (or one that is gone) is not sent again.
	_ = d.Handle(context.Background(), o)
	if len(api.acks) != 2 {
		t.Errorf("acks = %+v after a successful decline", api.acks)
	}
}

func TestFolderIsCheckedAgainOnEveryOffer(t *testing.T) {
	api := &fakeAPI{}
	sess := &fakeSessions{}
	d := &Dispatcher{API: api, Sessions: sess, MaxConcurrency: 4}
	bs := []bindings.Binding{binding(1, "/code/collie", 4)}
	root := setBindings(t, d, bs)
	folder := filepath.Join(root, "collie")
	outside, _ := filepath.EvalSymlinks(t.TempDir())

	_ = d.Handle(context.Background(), offer(1, 1))
	if len(sess.started) != 1 || sess.started[0].Binding.Folder != folder {
		t.Fatalf("first offer: started = %+v", sess.started)
	}

	// After the sync, the folder is replaced with a symlink that leads outside the root folders.
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, folder); err != nil {
		t.Fatal(err)
	}
	_ = d.Handle(context.Background(), offer(2, 1))
	last := api.acks[len(api.acks)-1]
	if last.Accept || !strings.Contains(last.Reason, "outside the root folders") {
		t.Errorf("offer after the swap: %+v", last)
	}
	if len(sess.started) != 1 {
		t.Errorf("a session started in a folder outside the roots: %+v", sess.started[1:])
	}
	if got := d.Running(); len(got) != 1 {
		t.Errorf("the refused offer kept a slot: running = %v", got)
	}

	// An ancestor swapped for a symlink inside the roots still resolves: the session gets the real path.
	_ = os.Remove(folder)
	real := filepath.Join(root, "real-collie")
	_ = os.MkdirAll(real, 0o755)
	_ = os.Symlink(real, folder)
	_ = d.Handle(context.Background(), offer(3, 1))
	if len(sess.started) != 2 || sess.started[1].Binding.Folder != real {
		t.Errorf("started = %+v, want the resolved %s", sess.started, real)
	}
}

func TestARedirectedRootNoLongerCounts(t *testing.T) {
	for _, swap := range []string{"root", "ancestor of the root"} {
		t.Run(swap, func(t *testing.T) {
			base, _ := filepath.EvalSymlinks(t.TempDir())
			parent := filepath.Join(base, "home")
			root := filepath.Join(parent, "code")
			folder := filepath.Join(root, "collie")
			_ = os.MkdirAll(folder, 0o755)
			// An outside tree with the same layout, so the cached binding path still exists after the swap.
			outside := filepath.Join(base, "evil")
			_ = os.MkdirAll(filepath.Join(outside, "home", "code", "collie"), 0o755)

			api := &fakeAPI{}
			sess := &fakeSessions{}
			d := &Dispatcher{API: api, Sessions: sess, MaxConcurrency: 4}
			b := binding(1, folder, 4)
			d.SetBindings([]bindings.Binding{b}, bindings.Allowed{Roots: []string{root}})

			_ = d.Handle(context.Background(), offer(1, 1))
			if len(sess.started) != 1 {
				t.Fatalf("before the swap: started = %+v", sess.started)
			}

			// Swap the root (or its parent) for a symlink into the outside tree.
			switch swap {
			case "root":
				_ = os.RemoveAll(root)
				_ = os.Symlink(filepath.Join(outside, "home", "code"), root)
			default:
				_ = os.RemoveAll(parent)
				_ = os.Symlink(filepath.Join(outside, "home"), parent)
			}
			_ = d.Handle(context.Background(), offer(2, 1))
			last := api.acks[len(api.acks)-1]
			if last.Accept || !strings.Contains(last.Reason, "root folders changed") {
				t.Errorf("after swapping the %s: %+v", swap, last)
			}
			if len(sess.started) != 1 {
				t.Errorf("a session started in the redirected tree: %+v", sess.started[1:])
			}
		})
	}
}

// collie offers research to a codebase's code bindings when it has no research binding: the code agent takes it.
func TestResearchRunsOnTheCodeAgentWithoutAResearchAgent(t *testing.T) {
	api := &fakeAPI{}
	sess := &fakeSessions{}
	d := &Dispatcher{API: api, Sessions: sess, MaxConcurrency: 4}
	own := binding(2, "/code/other", 1)
	own.Agents["research"] = "codex"
	setBindings(t, d, []bindings.Binding{binding(1, "/code/collie", 1), own})

	research := offer(1, 1)
	research.Purpose = "research"
	_ = d.Handle(context.Background(), research)
	theirs := offer(2, 2)
	theirs.Purpose = "research"
	_ = d.Handle(context.Background(), theirs)

	if len(api.acks) != 2 || !api.acks[0].Accept || !api.acks[1].Accept {
		t.Fatalf("acks = %+v", api.acks)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.started) != 2 || sess.started[0].Agent != "claude" || sess.started[1].Agent != "codex" {
		t.Errorf("research ran on %+v, want the code agent, then the binding's own research agent", sess.started)
	}
}

// Builds filling the machine-wide limit (one of them waiting for its review) leave room for reviews, which only their
// binding limits; another build is declined. collie counts the same way.
func TestReviewsAreNotHeldByTheMachineWideLimit(t *testing.T) {
	api := &fakeAPI{}
	sess := &fakeSessions{}
	d := &Dispatcher{API: api, Sessions: sess, MaxConcurrency: 2}
	// One binding takes both purposes here: its limit (3) counts the builds and the reviews alike.
	setBindings(t, d, []bindings.Binding{binding(1, "/code/collie", 3)})
	ctx := context.Background()
	review := func(id int64) machine.Dispatch { o := offer(id, 1); o.Purpose = "reviewer"; return o }

	_ = d.Handle(ctx, offer(1, 1))
	_ = d.Handle(ctx, offer(2, 1))
	_ = d.Handle(ctx, review(3))
	_ = d.Handle(ctx, offer(4, 1))
	_ = d.Handle(ctx, review(5))
	_ = d.Handle(ctx, review(6))

	want := []bool{true, true, true, false, false, false}
	if len(api.acks) != len(want) {
		t.Fatalf("acks = %+v", api.acks)
	}
	for i, a := range api.acks {
		if a.Accept != want[i] {
			t.Errorf("offer %d: accepted %v (%s), want %v", a.DispatchID, a.Accept, a.Reason, want[i])
		}
	}
	if r := api.acks[3].Reason; !strings.Contains(r, "machine at capacity (2 of 2") {
		t.Errorf("the third build was declined with %q", r)
	}
	if r := api.acks[4].Reason; !strings.Contains(r, "binding at capacity (3 of 3") {
		t.Errorf("the review over its binding's limit was declined with %q", r)
	}
}

func TestExactFoldersAreAcceptedButNotTheirChildren(t *testing.T) {
	api := &fakeAPI{}
	d := &Dispatcher{API: api, Sessions: &fakeSessions{}, MaxConcurrency: 5}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "collie")
	child := filepath.Join(repo, "sub")
	other := filepath.Join(base, "other")
	for _, dir := range []string{child, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d.SetBindings([]bindings.Binding{binding(1, repo, 1), binding(2, child, 1), binding(3, other, 1)},
		bindings.Allowed{Exact: []string{repo}})
	ctx := context.Background()
	for i := int64(1); i <= 3; i++ {
		_ = d.Handle(ctx, offer(i, i))
	}
	if !api.acks[0].Accept {
		t.Errorf("exact folder declined: %+v", api.acks[0])
	}
	for _, a := range api.acks[1:] {
		if a.Accept || !strings.Contains(a.Reason, "not a discovered repo") {
			t.Errorf("offer %d: %+v", a.DispatchID, a)
		}
	}

	// Without exact folders (discovery off) the same repo is outside the roots.
	api2 := &fakeAPI{}
	d2 := &Dispatcher{API: api2, Sessions: &fakeSessions{}, MaxConcurrency: 5}
	d2.SetBindings([]bindings.Binding{binding(1, repo, 1)}, bindings.Allowed{Roots: []string{other}})
	_ = d2.Handle(ctx, offer(1, 1))
	if api2.acks[0].Accept || !strings.Contains(api2.acks[0].Reason, "outside the root folders") {
		t.Errorf("without discovery: %+v", api2.acks[0])
	}
}

func TestASwappedExactFolderIsDeclined(t *testing.T) {
	api := &fakeAPI{}
	d := &Dispatcher{API: api, Sessions: &fakeSessions{}, MaxConcurrency: 5}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real1, evil := filepath.Join(base, "real"), filepath.Join(base, "evil")
	_ = os.MkdirAll(real1, 0o755)
	_ = os.MkdirAll(evil, 0o755)
	link := filepath.Join(base, "collie")
	if err := os.Symlink(real1, link); err != nil {
		t.Fatal(err)
	}
	d.SetBindings([]bindings.Binding{binding(1, link, 1)}, bindings.Allowed{Exact: []string{link}})
	// After the sync, the discovered folder is pointed somewhere else.
	_ = os.Remove(link)
	if err := os.Symlink(evil, link); err != nil {
		t.Fatal(err)
	}
	_ = d.Handle(context.Background(), offer(1, 1))
	if api.acks[0].Accept {
		t.Errorf("accepted an offer into a swapped folder: %+v", api.acks[0])
	}
}
