package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/discovery"
	"github.com/collies-ai/collie-agent/internal/dispatch"
	"github.com/collies-ai/collie-agent/internal/machine"
)

// flakyRemote answers bindings with err while it is set, and records acks.
type flakyRemote struct {
	mu       sync.Mutex
	bindings []machine.Binding
	err      error
	acks     []bool
}

func (f *flakyRemote) Codebases(context.Context) ([]machine.Codebase, error) { return nil, nil }
func (f *flakyRemote) PutInventory(context.Context, []machine.InventoryRepo) error {
	return nil
}
func (f *flakyRemote) Bindings(context.Context) ([]machine.Binding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bindings, f.err
}
func (f *flakyRemote) PollDispatches(context.Context, time.Duration) ([]machine.Dispatch, error) {
	return nil, nil
}
func (f *flakyRemote) Ack(_ context.Context, id int64, accept bool, _ string) (*machine.Accepted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, accept)
	if !accept {
		return nil, nil
	}
	return &machine.Accepted{SessionID: id, DispatchID: id}, nil
}

func (f *flakyRemote) lastAck(t *testing.T) bool {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.acks) == 0 {
		t.Fatal("no ack")
	}
	return f.acks[len(f.acks)-1]
}

func TestALostRepoIsRefusedEvenWhenTheBindingsSyncFails(t *testing.T) {
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	remote := &flakyRemote{bindings: []machine.Binding{{RunnerID: 11, Project: "collies", Codebase: "collie", Purpose: "code",
		Folder: repo, Agents: map[string]string{"code": "claude"}, MaxConcurrency: 5}}}
	found := []discovery.Repo{{Path: repo, Source: discovery.SourceClaude}}
	r := &Runner{
		Profile:  testProfile(t, ""),
		Remote:   remote,
		Settings: &config.Settings{Discover: &[]string{"claude"}},
		Discover: func(_, _ []string) []discovery.Repo { return found },
		state:    &config.State{},
	}
	r.dispatcher = &dispatch.Dispatcher{API: remote, Sessions: &recSessions{}, MaxConcurrency: 5}
	ctx := context.Background()
	offer := func(id int64) machine.Dispatch {
		return machine.Dispatch{Type: "dispatch", DispatchID: id, SessionID: id, RunnerID: 11, Kind: "work_goal", Purpose: "code"}
	}

	if err := r.reportInventory(ctx); err != nil {
		t.Fatal(err)
	}
	_ = r.dispatcher.Handle(ctx, offer(1))
	if !remote.lastAck(t) {
		t.Fatal("the discovered repo was not accepted")
	}

	// The repo is no longer trusted, and collie cannot be reached for the bindings.
	found = nil
	remote.mu.Lock()
	remote.err = errors.New("503")
	remote.mu.Unlock()
	if err := r.reportInventory(ctx); err != nil {
		t.Fatal(err)
	}
	_ = r.dispatcher.Handle(ctx, offer(2))
	if remote.lastAck(t) {
		t.Error("an offer into a repo discovery no longer finds was accepted after a failed sync")
	}
	if !r.resync.Load() {
		t.Error("a failed sync did not ask for a retry")
	}

	// The next scan finds the same thing, and collie answers again: the sync is retried.
	remote.mu.Lock()
	remote.err = nil
	remote.mu.Unlock()
	if err := r.reportInventory(ctx); err != nil {
		t.Fatal(err)
	}
	if r.resync.Load() {
		t.Error("the retried sync did not clear the flag")
	}
	_ = r.dispatcher.Handle(ctx, offer(3))
	if remote.lastAck(t) {
		t.Error("accepted after the retried sync")
	}
}
