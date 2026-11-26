// Package agent is what `collie-agent run` does: hold the profile's single-instance lock, keep the machine online
// with a heartbeat, report the repos it finds, keep its bindings in sync and receive dispatches over the cable (or
// the long-poll while the cable is down), acking each one.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/collies-ai/collie-agent/internal/bindings"
	"github.com/collies-ai/collie-agent/internal/cable"
	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/discovery"
	"github.com/collies-ai/collie-agent/internal/dispatch"
	"github.com/collies-ai/collie-agent/internal/machine"
	"github.com/collies-ai/collie-agent/internal/session"
	"github.com/collies-ai/collie-agent/internal/version"
)

// HeartbeatInterval is how often a healthy agent reports to collie.
const HeartbeatInterval = 30 * time.Second

// MaxBackoff caps the wait between failed heartbeats.
const MaxBackoff = 5 * time.Minute

// InventoryInterval is how often the repos are scanned and reported again.
const InventoryInterval = 10 * time.Minute

// Inventory retries after a failed report: from InventoryRetryMin, doubling up to InventoryRetryMax, so a rescan collie
// asked for is not lost to one transient error until the next InventoryInterval.
const (
	InventoryRetryMin = 5 * time.Second
	InventoryRetryMax = 2 * time.Minute
)

// MaxCableBackoff caps the wait between cable reconnects; the long-poll runs meanwhile.
const MaxCableBackoff = time.Minute

// DefaultMaxConcurrency is the machine-wide limit of sessions when the local config sets none.
const DefaultMaxConcurrency = 2

// ErrAlreadyRunning means another `collie-agent run` holds this profile's lock.
var ErrAlreadyRunning = errors.New("collie-agent is already running for this profile (one instance per profile; use --profile for a second one)")

// Heartbeater is the part of the machine API the heartbeat needs.
type Heartbeater interface {
	Heartbeat(ctx context.Context, hb machine.Heartbeat) (*machine.HeartbeatResponse, error)
}

// Remote is the rest of the machine API: discovery, bindings and dispatches.
type Remote interface {
	Codebases(ctx context.Context) ([]machine.Codebase, error)
	PutInventory(ctx context.Context, repos []machine.InventoryRepo) error
	Bindings(ctx context.Context) ([]machine.Binding, error)
	PollDispatches(ctx context.Context, wait time.Duration) ([]machine.Dispatch, error)
	Ack(ctx context.Context, dispatchID int64, accept bool, reason string) (*machine.Accepted, error)
}

// Cable is the push channel (internal/cable.Client).
type Cable interface {
	Run(ctx context.Context, onSubscribed func(), onDispatch func(machine.Dispatch), onCodebasesChanged func(version string)) error
}

// Runner is `collie-agent run` for one profile.
type Runner struct {
	Profile   config.Profile
	MachineID string
	API       Heartbeater
	// Remote enables discovery, bindings and dispatches. Without it the runner only sends heartbeats.
	Remote Remote
	// Cable is the push channel; without it dispatches only arrive over the long-poll.
	Cable Cable
	// Sessions runs accepted offers (step 9). Nil declines every offer.
	Sessions dispatch.Sessions
	Settings *config.Settings
	Log      *log.Logger
	// Sleep waits d or until ctx is done; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
	// FindAgents and Discover are replaced by tests. Discover finds the repos under the root folders and, for each of
	// the sources the person allowed (config.json discover), the ones Claude Code or Codex trust.
	FindAgents func(ctx context.Context) []machine.Agent
	Discover   func(roots, sources []string) []discovery.Repo

	dispatcher *dispatch.Dispatcher
	// stop ends the whole Loop with err (a revocation found anywhere, including a late ack after a reconnect).
	stop    func(err error)
	stateMu sync.Mutex
	state   *config.State
	agents  []machine.Agent

	// syncMu serializes bindings syncs: the heartbeat and a discovery that changed the exact folders both sync.
	syncMu sync.Mutex
	// exact are the repo folders discovery found through Claude Code, Codex or the fallback scan: allowed as binding
	// folders (exactly, never their children) only while config.json has discover.
	exactMu sync.Mutex
	exact   []string
	// resync says the last bindings sync failed after the exact folders changed: the next heartbeat or scan retries it.
	resync atomic.Bool

	// rescan asks the inventory loop to look for repos now (buffer 1: a burst of signals is one rescan).
	rescan chan struct{}
	// codebasesVersion is the last codebases_version seen from collie, over the cable or the heartbeat.
	cbMu             sync.Mutex
	codebasesVersion string
	// inventoried says a report reached collie: a first codebases_version seen after it may hide a change made in
	// between, so it asks for a rescan.
	inventoried atomic.Bool
}

// signalRescan asks the inventory loop for a rescan without blocking; one already pending covers this one too.
func (r *Runner) signalRescan() {
	if r.rescan == nil {
		return
	}
	select {
	case r.rescan <- struct{}{}:
	default:
	}
}

// codebasesChanged records a codebases_version from collie and asks for a rescan when it is news. Over the cable the
// event itself is news unless its version is not newer than one already seen; in a heartbeat only a newer version is
// (the first one is covered by the scan at start). A heartbeat answered before a cable event can carry the older
// version: it is not a change.
func (r *Runner) codebasesChanged(version string, fromCable bool) {
	r.cbMu.Lock()
	prev := r.codebasesVersion
	news := version != "" && newerVersion(version, prev)
	if news {
		r.codebasesVersion = version
	}
	r.cbMu.Unlock()
	switch {
	case fromCable && (version == "" || news):
		r.logger().Print("collie says the codebases changed: looking for repos again")
		r.signalRescan()
	case !fromCable && news && prev != "":
		r.logger().Print("the codebases version changed: looking for repos again")
		r.signalRescan()
	case !fromCable && news && r.inventoried.Load():
		// The first version seen came after a report: a change made between the two would be absorbed silently.
		r.signalRescan()
	}
}

// newerVersion tells whether v comes after prev: by number when both are integers (collie's counter), otherwise
// when they differ.
func newerVersion(v, prev string) bool {
	if prev == "" {
		return true
	}
	a, errA := strconv.ParseInt(v, 10, 64)
	b, errB := strconv.ParseInt(prev, 10, 64)
	if errA == nil && errB == nil {
		return a > b
	}
	return v != prev
}

func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
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

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logger() *log.Logger {
	if r.Log != nil {
		return r.Log
	}
	return log.New(io.Discard, "", 0)
}

func (r *Runner) settings() *config.Settings {
	if r.Settings != nil {
		return r.Settings
	}
	return &config.Settings{}
}

func (r *Runner) maxConcurrency() int {
	if n := r.settings().MaxConcurrency; n > 0 {
		return n
	}
	return DefaultMaxConcurrency
}

// Dispatcher is the runner's dispatcher (tests inspect it).
func (r *Runner) Dispatcher() *dispatch.Dispatcher { return r.dispatcher }

// updateState changes the state under its lock and writes it.
func (r *Runner) updateState(f func(st *config.State)) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	f(r.state)
	if err := r.Profile.SaveState(r.state); err != nil {
		r.logger().Printf("cannot write %s: %v", r.Profile.StatePath(), err)
	}
}

// noteResume records in the state what happened to a session resumed at startup, for `status`.
func (r *Runner) noteResume(id int64, status, reason string) {
	r.updateState(func(st *config.State) {
		for i := range st.Resumed {
			if st.Resumed[i].SessionID == id {
				st.Resumed[i].Status, st.Resumed[i].Reason, st.Resumed[i].At = status, reason, r.now()
				return
			}
		}
		st.Resumed = append(st.Resumed, config.ResumedSession{SessionID: id, Status: status, Reason: reason, At: r.now()})
	})
}

func (r *Runner) noteError(err error) {
	r.updateState(func(st *config.State) {
		st.LastError = err.Error()
		st.LastErrorAt = r.now()
	})
}

// Loop runs until ctx is done (nil) or collie revokes the machine (machine.ErrRevoked): every part of it stops on a
// 401. Other errors are logged and retried with backoff.
func (r *Runner) Loop(ctx context.Context) error {
	st, _ := r.Profile.LoadState()
	if st == nil || st.MachineID != r.MachineID {
		st = &config.State{MachineID: r.MachineID}
	}
	st.PID = os.Getpid()
	st.StartedAt = r.now()
	st.Revoked = false
	st.Connection = ""
	st.Running = nil
	st.Resumed = nil
	r.state = st
	r.updateState(func(*config.State) {})

	sessions := r.Sessions
	if sessions == nil {
		sessions = dispatch.Unsupported{}
	}
	r.rescan = make(chan struct{}, 1)
	r.dispatcher = &dispatch.Dispatcher{API: r.Remote, Sessions: sessions, Log: r.logger(), MaxConcurrency: r.maxConcurrency()}

	// The runner waits for running sessions to be stopped (suspended) and cleaned up before it returns.
	if w, ok := sessions.(interface{ Wait() }); ok {
		defer w.Wait()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fatal := make(chan error, 1)
	r.stop = func(err error) {
		select {
		case fatal <- err:
		default: // the first error wins
		}
		cancel()
	}
	// A 401 on a session endpoint (renewing a token, reporting an event) is a revocation like any other: stop it all.
	if rv, ok := sessions.(interface{ OnRevoked(func(error)) }); ok {
		rv.OnRevoked(r.stop)
	}
	// Before taking new work, sessions that keep files on disk resume what the last run left (they are in the first
	// heartbeat's running sessions) or, without resume, sweep it.
	if rs, ok := sessions.(interface {
		Resume(context.Context, session.Adopter, session.ResumeNote)
	}); ok {
		rs.Resume(ctx, r.dispatcher, r.noteResume)
	} else if sw, ok := sessions.(interface{ Sweep(context.Context) }); ok {
		sw.Sweep(ctx)
	}
	var wg sync.WaitGroup
	start := func(f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(ctx); err != nil {
				r.stop(err)
			}
		}()
	}

	if r.Remote != nil {
		if len(r.settings().RootFolders) == 0 && len(r.settings().DiscoverFrom()) == 0 {
			r.logger().Print("no root folders configured and discovery is off: no repos are reported and every binding is refused (collie-agent roots add <folder>)")
		}
		r.agents = r.findAgents(ctx)
	}
	bound := make(chan struct{})
	start(func(ctx context.Context) error { return r.heartbeatLoop(ctx, bound) })
	if r.Remote != nil {
		start(r.inventoryLoop)
		start(func(ctx context.Context) error {
			// Offers before the first bindings sync would all be declined: wait for it.
			select {
			case <-bound:
			case <-ctx.Done():
				return nil
			}
			return r.connectionLoop(ctx)
		})
	}
	wg.Wait()
	select {
	case err := <-fatal:
		if errors.Is(err, machine.ErrRevoked) {
			r.logger().Printf("stopping: %v", err)
			r.updateState(func(st *config.State) {
				st.Revoked = true
				st.Connection = ""
				st.LastError = err.Error()
				st.LastErrorAt = r.now()
			})
		}
		return err
	default:
		r.updateState(func(st *config.State) { st.Connection = "" })
		return nil
	}
}

func (r *Runner) findAgents(ctx context.Context) []machine.Agent {
	if r.FindAgents != nil {
		return r.FindAgents(ctx)
	}
	return discovery.FindAgents(ctx)
}

// heartbeatLoop sends heartbeats and syncs the bindings whenever collie's bindings_version changes. bound is closed
// after the first successful sync (or the first heartbeat when there is no Remote).
func (r *Runner) heartbeatLoop(ctx context.Context, bound chan struct{}) error {
	var once sync.Once
	markBound := func() { once.Do(func() { close(bound) }) }
	backoff := time.Duration(0)
	failures := 0
	synced := ""
	for {
		hb := machine.Heartbeat{
			Version:        version.Version,
			OS:             runtime.GOOS + "/" + runtime.GOARCH,
			MaxConcurrency: r.maxConcurrency(),
			Agents:         r.agents,
			RootsCount:     len(r.settings().RootFolders),
		}
		if r.dispatcher != nil {
			hb.Running = r.dispatcher.Running()
		}
		resp, err := r.API.Heartbeat(ctx, hb)
		switch {
		case err == nil:
			if failures > 0 {
				r.logger().Printf("connected to collie again after %d failed heartbeats", failures)
			}
			failures, backoff = 0, 0
			r.updateState(func(st *config.State) {
				st.LastHeartbeatAt = r.now()
				st.BindingsVersion = resp.BindingsVersion
				st.LastError = ""
				st.Running = hb.Running
			})
			r.codebasesChanged(resp.CodebasesVersion, false)
			if r.Remote == nil {
				markBound()
			} else if resp.BindingsVersion != synced || synced == "" || r.resync.Load() {
				if err := r.syncBindings(ctx); err != nil {
					if errors.Is(err, machine.ErrRevoked) || ctx.Err() != nil {
						return ignoreCanceled(ctx, err)
					}
					r.logger().Printf("syncing bindings: %v", err)
					r.noteError(err)
				} else {
					synced = resp.BindingsVersion
					markBound()
				}
			}
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, machine.ErrRevoked):
			return err
		default:
			failures++
			backoff = nextBackoff(backoff)
			r.logger().Printf("heartbeat failed (retrying in %s): %v", backoff, err)
			r.noteError(err)
		}
		wait := HeartbeatInterval
		if backoff > 0 {
			wait = backoff
		}
		if err := r.sleep(ctx, wait); err != nil {
			return nil
		}
	}
}

func ignoreCanceled(ctx context.Context, err error) error {
	if ctx.Err() != nil && !errors.Is(err, machine.ErrRevoked) {
		return nil
	}
	return err
}

// allowed is where binding folders may be: inside the root folders, or exactly a discovered repo when discovery is on.
func (r *Runner) allowed() bindings.Allowed {
	a := bindings.Allowed{Roots: r.settings().RootFolders}
	if len(r.settings().DiscoverFrom()) > 0 {
		r.exactMu.Lock()
		a.Exact = slices.Clone(r.exact)
		r.exactMu.Unlock()
	}
	return a
}

// setExact records the exact folders of the latest discovery and reports whether they changed.
func (r *Runner) setExact(repos []discovery.Repo) bool {
	var exact []string
	for _, repo := range repos {
		if repo.Source != discovery.SourceRoot {
			exact = append(exact, repo.Path)
		}
	}
	slices.Sort(exact)
	r.exactMu.Lock()
	defer r.exactMu.Unlock()
	if slices.Equal(exact, r.exact) {
		return false
	}
	r.exact = exact
	return true
}

// applyAllowed gives the dispatcher the current allowed folders, keeping its bindings. It holds syncMu so a sync in
// flight cannot put back the folders it read before.
func (r *Runner) applyAllowed() {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	r.dispatcher.SetAllowed(r.allowed())
}

// syncBindings fetches collie's bindings, applies the local config and hands them to the dispatcher.
func (r *Runner) syncBindings(ctx context.Context) error {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	server, err := r.Remote.Bindings(ctx)
	if err != nil {
		return err
	}
	allowed := r.allowed() // read under syncMu: a discovery that changes it after this point syncs again
	resolved := bindings.Resolve(server, r.settings(), allowed)
	r.dispatcher.SetBindings(resolved, allowed)
	r.resync.Store(false)
	states := make([]config.BindingState, 0, len(resolved))
	for _, b := range resolved {
		if !b.Usable() {
			r.logger().Printf("refusing binding %d (%s/%s): %s", b.RunnerID, b.Project, b.Codebase, b.Refused)
		}
		states = append(states, config.BindingState{RunnerID: b.RunnerID, Project: b.Project, Codebase: b.Codebase, Folder: b.Folder,
			Purpose: b.Purpose, Pinned: b.Pinned, Refused: b.Refused, PermissionMode: b.PermissionMode, PermissionFrom: b.PermissionFrom})
	}
	r.updateState(func(st *config.State) { st.Bindings = states })
	return nil
}

// inventoryLoop looks for repos and reports the ones that match a codebase: now, every InventoryInterval, and right
// away when collie says the codebases changed (a rescan signal).
func (r *Runner) inventoryLoop(ctx context.Context) error {
	retry := time.Duration(0)
	for {
		wait := InventoryInterval
		if err := r.reportInventory(ctx); err != nil {
			if errors.Is(err, machine.ErrRevoked) {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			// The refresh stays pending: retry soon, backing off, instead of waiting for the next interval.
			retry = min(max(retry*2, InventoryRetryMin), InventoryRetryMax)
			wait = retry
			r.logger().Printf("reporting repos (retrying in %s): %v", wait, err)
			r.noteError(err)
		} else {
			retry = 0
			r.inventoried.Store(true)
		}
		if err := r.waitInventory(ctx, wait); err != nil {
			return nil
		}
	}
}

// waitInventory waits d (through the injectable sleep) or a rescan signal, whichever comes first. It returns an error
// only when ctx is done.
func (r *Runner) waitInventory(ctx context.Context, d time.Duration) error {
	sleepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.sleep(sleepCtx, d) }()
	select {
	case <-r.rescan:
		cancel()
		<-done
		return ctx.Err()
	case <-done:
		return ctx.Err()
	}
}

func (r *Runner) reportInventory(ctx context.Context) error {
	discover := r.Discover
	if discover == nil {
		discover = func(roots, sources []string) []discovery.Repo {
			home, _ := os.UserHomeDir()
			return discovery.Discover(home, sources, roots)
		}
	}
	repos := discover(r.settings().RootFolders, r.settings().DiscoverFrom())
	// Discovered folders are allowed binding folders. When they change, the dispatcher's boundary changes right away
	// (a repo no longer trusted is refused from the next offer, whatever collie answers), and the bindings are synced
	// again so the ones refused for lack of a folder are checked again. A failed sync is retried by the next heartbeat
	// or scan.
	if changed := r.setExact(repos); len(r.settings().DiscoverFrom()) > 0 && r.dispatcher != nil && (changed || r.resync.Load()) {
		if changed {
			r.applyAllowed()
		}
		if err := r.syncBindings(ctx); err != nil {
			r.resync.Store(true)
			if errors.Is(err, machine.ErrRevoked) {
				return err
			}
			r.logger().Printf("syncing bindings after discovery: %v", err)
		}
	}
	codebases, err := r.Remote.Codebases(ctx)
	if err != nil {
		return err
	}
	inv := discovery.Inventory(discovery.Match(repos, codebases))
	if err := r.Remote.PutInventory(ctx, inv); err != nil {
		return err
	}
	r.updateState(func(st *config.State) { st.Repos = len(inv) })
	return nil
}

// connectionLoop keeps the cable up, reconnecting with backoff, and long-polls while it is down.
func (r *Runner) connectionLoop(ctx context.Context) error {
	backoff := time.Second
	for {
		if r.Cable != nil {
			subscribedAt := time.Time{}
			cableCtx, stopCable := context.WithCancel(ctx)
			err := r.Cable.Run(cableCtx, func() {
				subscribedAt = r.now()
				r.updateState(func(st *config.State) { st.Connection = "cable" })
				r.logger().Print("connected to collie over the cable")
			}, func(d machine.Dispatch) {
				// Ack outside the read loop: an ack is an HTTP call and the cable must keep reading pings.
				// The ack runs on the runner's context, so it can outlive this socket: a revocation it finds stops the
				// whole runner, not just this connection.
				go func() {
					if err := r.dispatcher.Handle(ctx, d); err != nil {
						r.stop(err)
					}
				}()
			}, func(version string) { r.codebasesChanged(version, true) })
			stopCable()
			if ctx.Err() != nil {
				return nil
			}
			// collie refused the machine token on the cable (a 401 on the upgrade, or disconnect: unauthorized): the
			// machine was revoked, like any other 401. Stop instead of falling back to the long-poll.
			if errors.Is(err, cable.ErrUnauthorized) {
				return fmt.Errorf("%w (%v)", machine.ErrRevoked, err)
			}
			if !subscribedAt.IsZero() && r.now().Sub(subscribedAt) > time.Minute {
				backoff = time.Second // it was healthy for a while: reconnect quickly
			}
			r.logger().Printf("cable down (%v): long-polling, reconnecting in %s", err, backoff)
		}
		r.updateState(func(st *config.State) { st.Connection = "long-poll" })
		if err := r.pollFor(ctx, backoff); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if r.Cable == nil {
			continue
		}
		backoff *= 2
		if backoff > MaxCableBackoff {
			backoff = MaxCableBackoff
		}
	}
}

// pollFor long-polls GET /machine/v1/dispatches for about d (at least one poll), handling each offer.
func (r *Runner) pollFor(ctx context.Context, d time.Duration) error {
	deadline := r.now().Add(d)
	for {
		wait := deadline.Sub(r.now())
		if wait < time.Second {
			wait = time.Second
		}
		if wait > 25*time.Second {
			wait = 25 * time.Second
		}
		offers, err := r.Remote.PollDispatches(ctx, wait)
		switch {
		case errors.Is(err, machine.ErrRevoked):
			return err
		case ctx.Err() != nil:
			return nil
		case err != nil:
			r.logger().Printf("long-poll: %v", err)
			if err := r.sleep(ctx, 5*time.Second); err != nil {
				return nil
			}
		}
		for _, o := range offers {
			if err := r.dispatcher.Handle(ctx, o); err != nil {
				return err
			}
		}
		if !r.now().Before(deadline) {
			return nil
		}
	}
}

func nextBackoff(d time.Duration) time.Duration {
	if d == 0 {
		return 5 * time.Second
	}
	d *= 2
	if d > MaxBackoff {
		return MaxBackoff
	}
	return d
}

// Lock takes the profile's single-instance lock. The returned function releases it. The OS releases it too if the
// process dies, so a crash never leaves a stale lock.
func Lock(p config.Profile) (func(), error) {
	if err := p.EnsureDir(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("locking %s: %w", p.LockPath(), err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// IsLocked reports whether a `run` of the profile holds the lock right now.
func IsLocked(p config.Profile) bool {
	f, err := os.OpenFile(p.LockPath(), os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
