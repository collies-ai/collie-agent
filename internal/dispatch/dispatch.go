// Package dispatch decides what to do with an offer from collie: it checks the binding and the local capacity,
// acks the offer (accept or decline with a reason), and hands accepted ones to the session runner.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"

	"github.com/collies-ai/collie-agent/internal/bindings"
	"github.com/collies-ai/collie-agent/internal/machine"
)

// Session is an accepted offer, ready to run.
type Session struct {
	ID       int64
	Dispatch machine.Dispatch
	Binding  bindings.Binding
	// Agent is the agent CLI the binding names for this purpose (e.g. "claude", "codex").
	Agent    string
	Accepted machine.Accepted
}

// Sessions runs accepted sessions (step 9: a worktree and an agent process per session). Start must not block for
// the whole session: it starts it and calls done when it ends.
type Sessions interface {
	// CanRun says whether this runner can run the agent at all; when not, the offer is declined with the reason.
	CanRun(agent string) error
	Start(ctx context.Context, s Session, done func()) error
}

// Unsupported is the Sessions of a collie-agent without a session runner: it declines every offer, so collie hands
// the work to another runner instead of losing it.
type Unsupported struct{}

// CanRun always refuses.
func (Unsupported) CanRun(string) error {
	return errors.New("this collie-agent version cannot run sessions yet")
}

// Start is never reached.
func (Unsupported) Start(context.Context, Session, func()) error {
	return errors.New("this collie-agent version cannot run sessions yet")
}

// Acker is the part of the machine API the dispatcher needs.
type Acker interface {
	Ack(ctx context.Context, dispatchID int64, accept bool, reason string) (*machine.Accepted, error)
}

// Dispatcher handles offers. It is safe for concurrent use: offers arrive from the cable and the long-poll.
type Dispatcher struct {
	API      Acker
	Sessions Sessions
	Log      *log.Logger
	// MaxConcurrency is the machine-wide limit of sessions at once.
	MaxConcurrency int

	mu          sync.Mutex
	bindings    map[int64]bindings.Binding // by runner id
	running     map[int64]int64            // session id → runner id
	reserved    map[int64]int64            // offers being acked: session id → runner id
	handled     map[int64]bool             // offer (session) ids already handled, so the cable and the long-poll don't ack twice
	rootFolders []string
}

// SetBindings replaces the bindings the dispatcher accepts offers for, and the root folders their folders must stay
// inside (checked again on every offer).
func (d *Dispatcher) SetBindings(bs []bindings.Binding, roots []string) {
	m := make(map[int64]bindings.Binding, len(bs))
	for _, b := range bs {
		m[b.RunnerID] = b
	}
	d.mu.Lock()
	d.bindings = m
	d.rootFolders = append([]string(nil), roots...)
	d.mu.Unlock()
}

// Running returns the ids of the sessions running now, for the heartbeat.
func (d *Dispatcher) Running() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]int64, 0, len(d.running))
	for id := range d.running {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = fmt.Sprint(id)
	}
	return out
}

func (d *Dispatcher) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log.Printf(format, args...)
	}
}

// offerKey identifies an offer: collie sends a new session id each time it offers a dispatch again.
func offerKey(m machine.Dispatch) int64 {
	if m.SessionID != 0 {
		return m.SessionID
	}
	return -m.DispatchID
}

// Handle acks one offer. It returns machine.ErrRevoked when collie rejected the token; other ack errors are logged
// (collie redelivers an offer nobody acked).
func (d *Dispatcher) Handle(ctx context.Context, m machine.Dispatch) error {
	key := offerKey(m)
	reason, b, agent := d.reserve(key, m)
	if reason == "duplicate" {
		return nil
	}
	if reason != "" {
		return d.decline(ctx, key, m, reason)
	}

	// The folder was checked when the bindings synced; check it again now, so a folder (or an ancestor) swapped for
	// a symlink since then cannot send the session outside the root folders. The session gets the fresh path.
	folder, err := bindings.Within(b.Folder, d.roots())
	if err != nil {
		d.release(key)
		return d.decline(ctx, key, m, "binding refused by the machine: "+err.Error())
	}
	b.Folder = folder

	acc, err := d.API.Ack(ctx, m.DispatchID, true, "")
	if err != nil {
		d.release(key)
		if errors.Is(err, machine.ErrRevoked) {
			return err
		}
		if errors.Is(err, machine.ErrGone) {
			d.logf("dispatch %d is no longer ours: %v", m.DispatchID, err)
		} else {
			d.logf("accepting dispatch %d: %v", m.DispatchID, err)
			d.forget(key) // let a redelivery of the same offer try again
		}
		return nil
	}
	sid := acc.SessionID
	if sid == 0 {
		sid = m.SessionID
	}
	d.promote(key, sid, b.RunnerID)
	s := Session{ID: sid, Dispatch: m, Binding: b, Agent: agent, Accepted: *acc}
	d.logf("accepted dispatch %d (%s %s) as session %d with %s in %s", m.DispatchID, m.Kind, m.Reference, sid, agent, b.Folder)
	if err := d.Sessions.Start(ctx, s, func() { d.finish(sid) }); err != nil {
		d.logf("session %d did not start: %v", sid, err)
		d.finish(sid)
	}
	return nil
}

// decline sends a decline. A failure collie may never have seen (network, 5xx) forgets the offer, so a redelivery
// is declined again instead of dropped as a duplicate.
func (d *Dispatcher) decline(ctx context.Context, key int64, m machine.Dispatch, reason string) error {
	d.logf("declining dispatch %d (%s %s): %s", m.DispatchID, m.Kind, m.Reference, reason)
	_, err := d.API.Ack(ctx, m.DispatchID, false, reason)
	switch {
	case err == nil, errors.Is(err, machine.ErrGone):
		return nil
	case errors.Is(err, machine.ErrRevoked):
		return err
	default:
		d.logf("declining dispatch %d: %v", m.DispatchID, err)
		d.forget(key)
		return nil
	}
}

func (d *Dispatcher) roots() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rootFolders
}

// reserve checks an offer and, when it can be accepted, reserves a slot for it while it is acked. It returns a
// decline reason ("" to accept, "duplicate" to ignore).
func (d *Dispatcher) reserve(key int64, m machine.Dispatch) (string, bindings.Binding, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handled == nil {
		d.handled, d.running, d.reserved = map[int64]bool{}, map[int64]int64{}, map[int64]int64{}
	}
	if d.handled[key] {
		return "duplicate", bindings.Binding{}, ""
	}
	d.handled[key] = true

	b, ok := d.bindings[m.RunnerID]
	if !ok {
		return fmt.Sprintf("runner %d is not bound on this machine", m.RunnerID), b, ""
	}
	if !b.Usable() {
		return "binding refused by the machine: " + b.Refused, b, ""
	}
	agent := agentFor(b, m.Purpose)
	if agent == "" {
		return fmt.Sprintf("no agent configured for purpose %q", m.Purpose), b, ""
	}
	if err := d.Sessions.CanRun(agent); err != nil {
		return err.Error(), b, ""
	}
	busy, busyRunner := 0, 0
	for _, r := range d.running {
		busy++
		if r == b.RunnerID {
			busyRunner++
		}
	}
	for _, r := range d.reserved {
		busy++
		if r == b.RunnerID {
			busyRunner++
		}
	}
	if max := d.MaxConcurrency; max > 0 && busy >= max {
		return fmt.Sprintf("machine at capacity (%d of %d sessions)", busy, max), b, ""
	}
	if busyRunner >= b.MaxConcurrency {
		return fmt.Sprintf("binding at capacity (%d of %d sessions)", busyRunner, b.MaxConcurrency), b, ""
	}
	d.reserved[key] = b.RunnerID
	return "", b, agent
}

// agentFor picks the binding's agent for a purpose; "code" is collie's default purpose.
func agentFor(b bindings.Binding, purpose string) string {
	if purpose == "" {
		purpose = "code"
	}
	return b.Agents[purpose]
}

func (d *Dispatcher) release(key int64) {
	d.mu.Lock()
	delete(d.reserved, key)
	d.mu.Unlock()
}

func (d *Dispatcher) forget(key int64) {
	d.mu.Lock()
	delete(d.handled, key)
	d.mu.Unlock()
}

func (d *Dispatcher) promote(key, sessionID, runnerID int64) {
	d.mu.Lock()
	delete(d.reserved, key)
	d.running[sessionID] = runnerID
	d.mu.Unlock()
}

func (d *Dispatcher) finish(sessionID int64) {
	d.mu.Lock()
	delete(d.running, sessionID)
	d.mu.Unlock()
}
