package agent

import (
	"context"
	"strconv"
	"testing"
	"time"

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
	if err := r.waitInventory(ctx); err != nil {
		t.Fatalf("waitInventory = %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- r.waitInventory(ctx) }()
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
