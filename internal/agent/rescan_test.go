package agent

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/machine"
)

// inventorySleep sleeps briefly, except for the inventory interval, which only ends with ctx: a report after the
// first one can only come from a rescan.
func inventorySleep(ctx context.Context, d time.Duration) error {
	if d == InventoryInterval {
		<-ctx.Done()
		return ctx.Err()
	}
	return shortSleep(ctx, d)
}

// codebasesCable is a cable that subscribes and hands its codebases_changed callback to the test.
type codebasesCable struct{ cb chan func(string) }

func (c *codebasesCable) Run(ctx context.Context, onSubscribed func(), _ func(machine.Dispatch), onCodebasesChanged func(string)) error {
	onSubscribed()
	select {
	case c.cb <- onCodebasesChanged:
	default:
	}
	<-ctx.Done()
	return nil
}

func TestRescansWhenCollieSaysTheCodebasesChanged(t *testing.T) {
	f := newFakeCollie(t)
	f.codebasesV = 1
	r, _, _ := setup(t, f)
	r.Sleep = inventorySleep
	cab := &codebasesCable{cb: make(chan func(string), 1)}
	r.Cable = cab
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Loop(ctx) }()
	defer func() { cancel(); <-done }()

	counts := func() (int, int) { f.mu.Lock(); defer f.mu.Unlock(); return f.codebaseGets, len(f.inventory) }
	waitFor(t, "the first report", func() bool { _, n := counts(); return n == 1 })
	time.Sleep(100 * time.Millisecond) // several heartbeats with the same codebases_version
	if gets, n := counts(); gets != 1 || n != 1 {
		t.Fatalf("rescanned without a change: %d gets, %d reports", gets, n)
	}

	// The heartbeat brings a new codebases_version (the cable-down fallback).
	f.mu.Lock()
	f.codebasesV = 2
	f.mu.Unlock()
	waitFor(t, "a rescan from the heartbeat", func() bool { gets, n := counts(); return gets == 2 && n == 2 })

	// collie pushes codebases_changed over the cable.
	onChanged := <-cab.cb
	onChanged("3")
	waitFor(t, "a rescan from the cable", func() bool { gets, n := counts(); return gets == 3 && n == 3 })
	// The heartbeat then reports version 3 too: no second rescan for the same change.
	f.mu.Lock()
	f.codebasesV = 3
	f.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	if gets, n := counts(); gets != 3 || n != 3 {
		t.Errorf("the same change was rescanned twice: %d gets, %d reports", gets, n)
	}
}

func TestRescanSignalsCoalesce(t *testing.T) {
	r := &Runner{rescan: make(chan struct{}, 1)}
	for i := 0; i < 5; i++ {
		r.codebasesChanged(strconv.Itoa(i+1), true)
	}
	if len(r.rescan) != 1 {
		t.Fatalf("pending rescans = %d, want 1", len(r.rescan))
	}
	r.Sleep = inventorySleep
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The burst is one rescan: the first wait returns at once, the second waits for the interval (or ctx).
	if err := r.waitInventory(ctx, InventoryInterval); err != nil {
		t.Fatalf("waitInventory = %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- r.waitInventory(ctx, InventoryInterval) }()
	select {
	case err := <-waited:
		t.Fatalf("a second rescan came from the same burst (%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := <-waited; err == nil {
		t.Error("waitInventory returned nil after ctx was cancelled")
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		v, prev string
		want    bool
	}{{"2", "", true}, {"3", "2", true}, {"2", "3", false}, {"3", "3", false}, {"10", "9", true}, {"b", "a", true}, {"a", "a", false}} {
		if got := newerVersion(c.v, c.prev); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v", c.v, c.prev, got)
		}
	}
}

func TestAFailedRescanIsRetriedWithBackoff(t *testing.T) {
	f := newFakeCollie(t)
	f.codebasesV = 1
	r, _, _ := setup(t, f)
	var mu sync.Mutex
	var waits []time.Duration
	r.Sleep = func(ctx context.Context, d time.Duration) error {
		if d >= InventoryRetryMin && d <= InventoryRetryMax && d != HeartbeatInterval {
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
			return shortSleep(ctx, time.Millisecond)
		}
		return inventorySleep(ctx, d)
	}
	r.Cable = &codebasesCable{cb: make(chan func(string), 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Loop(ctx) }()
	defer func() { cancel(); <-done }()

	counts := func() (int, int) { f.mu.Lock(); defer f.mu.Unlock(); return f.codebaseGets, len(f.inventory) }
	waitFor(t, "the first report", func() bool { _, n := counts(); return n == 1 })

	// collie fails the next three codebases fetches; the version change must still be reported.
	f.mu.Lock()
	f.failCodebases = 3
	f.codebasesV = 2
	f.mu.Unlock()
	waitFor(t, "the rescan to succeed after retries", func() bool { _, n := counts(); return n == 2 })
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 3 || waits[0] != InventoryRetryMin || waits[1] != 2*InventoryRetryMin || waits[2] != 4*InventoryRetryMin {
		t.Errorf("retry waits = %v", waits)
	}
}

func TestAFirstVersionAfterAReportRescans(t *testing.T) {
	r := &Runner{rescan: make(chan struct{}, 1)}
	r.codebasesChanged("5", false) // before any report: the scan at start covers it
	if len(r.rescan) != 0 {
		t.Fatal("rescan before any report")
	}
	r2 := &Runner{rescan: make(chan struct{}, 1)}
	r2.inventoried.Store(true)
	r2.codebasesChanged("5", false)
	if len(r2.rescan) != 1 {
		t.Error("a first version seen after a report did not rescan")
	}
}

func TestARootAddedWhileRunningIsPickedUp(t *testing.T) {
	f := newFakeCollie(t)
	r, _, repo := setup(t, f)
	r.Sleep = inventorySleep
	root := r.Settings.RootFolders[0]
	r.Settings = &config.Settings{MaxConcurrency: 3}
	var mu sync.Mutex
	var file *config.Settings
	var fileErr error
	r.ReloadSettings = func() (*config.Settings, error) {
		mu.Lock()
		defer mu.Unlock()
		if fileErr != nil {
			return nil, fileErr
		}
		if file == nil {
			return &config.Settings{MaxConcurrency: 3}, nil
		}
		c := *file
		return &c, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Loop(ctx) }()
	defer func() { cancel(); <-done }()

	state := func() (int, int, []machine.InventoryRepo) {
		f.mu.Lock()
		defer f.mu.Unlock()
		roots := -1
		if len(f.heartbeats) > 0 {
			roots = f.heartbeats[len(f.heartbeats)-1].RootsCount
		}
		var last []machine.InventoryRepo
		if len(f.inventory) > 0 {
			last = f.inventory[len(f.inventory)-1]
		}
		return roots, len(f.inventory), last
	}
	waitFor(t, "an empty first report", func() bool { _, n, _ := state(); return n == 1 })
	if roots, _, inv := state(); roots != 0 || len(inv) != 0 {
		t.Fatalf("before the root: roots_count %d, inventory %v", roots, inv)
	}

	// A broken config.json keeps the settings in use.
	mu.Lock()
	fileErr = errors.New("unexpected end of JSON input")
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	if roots, n, _ := state(); roots != 0 || n != 1 {
		t.Fatalf("a broken config.json changed things: roots_count %d, %d reports", roots, n)
	}

	// `roots add` writes config.json: the next heartbeat reports the root and a rescan finds the repo, without the
	// codebases changing. max_concurrency keeps its start value.
	mu.Lock()
	fileErr = nil
	file = &config.Settings{RootFolders: []string{root}, MaxConcurrency: 9}
	mu.Unlock()
	waitFor(t, "the repo under the new root", func() bool {
		roots, _, inv := state()
		return roots == 1 && len(inv) == 1 && inv[0].Path == repo
	})
	f.mu.Lock()
	last := f.heartbeats[len(f.heartbeats)-1]
	f.mu.Unlock()
	if last.MaxConcurrency != 3 {
		t.Errorf("max_concurrency = %d, want the start value 3", last.MaxConcurrency)
	}
}
