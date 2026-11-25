// Package agent is what `collie-agent run` does: hold the profile's single-instance lock and keep the machine online
// with a heartbeat loop. The connection that receives dispatches comes in a later version.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/machine"
	"github.com/collies-ai/collie-agent/internal/version"
)

// HeartbeatInterval is how often a healthy agent reports to collie.
const HeartbeatInterval = 30 * time.Second

// MaxBackoff caps the wait between failed heartbeats.
const MaxBackoff = 5 * time.Minute

// DefaultMaxConcurrency is the machine-wide limit of sessions until bindings configure one.
const DefaultMaxConcurrency = 2

// ErrAlreadyRunning means another `collie-agent run` holds this profile's lock.
var ErrAlreadyRunning = errors.New("collie-agent is already running for this profile (one instance per profile; use --profile for a second one)")

// Heartbeater is the part of the machine API the loop needs.
type Heartbeater interface {
	Heartbeat(ctx context.Context, hb machine.Heartbeat) (*machine.HeartbeatResponse, error)
}

// Runner is the heartbeat loop of one profile.
type Runner struct {
	Profile   config.Profile
	MachineID string
	API       Heartbeater
	Log       *log.Logger
	// Sleep waits d or until ctx is done; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
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

// Loop sends heartbeats until ctx is done (nil) or collie revokes the machine (machine.ErrRevoked). Other errors
// are logged and retried with exponential backoff.
func (r *Runner) Loop(ctx context.Context) error {
	st, _ := r.Profile.LoadState()
	if st == nil || st.MachineID != r.MachineID {
		st = &config.State{MachineID: r.MachineID}
	}
	st.PID = os.Getpid()
	st.StartedAt = r.now()
	st.Revoked = false
	r.save(st)

	hb := machine.Heartbeat{
		Version:        version.Version,
		OS:             runtime.GOOS + "/" + runtime.GOARCH,
		MaxConcurrency: DefaultMaxConcurrency,
	}
	backoff := time.Duration(0)
	failures := 0
	for {
		resp, err := r.API.Heartbeat(ctx, hb)
		switch {
		case err == nil:
			if failures > 0 {
				r.logger().Printf("connected to collie again after %d failed heartbeats", failures)
			}
			failures, backoff = 0, 0
			st.LastHeartbeatAt = r.now()
			st.BindingsVersion = resp.BindingsVersion
			st.LastError = ""
			r.save(st)
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, machine.ErrRevoked):
			r.logger().Printf("stopping: %v", err)
			st.Revoked = true
			st.LastError = err.Error()
			st.LastErrorAt = r.now()
			r.save(st)
			return err
		default:
			failures++
			backoff = nextBackoff(backoff)
			r.logger().Printf("heartbeat failed (retrying in %s): %v", backoff, err)
			st.LastError = err.Error()
			st.LastErrorAt = r.now()
			r.save(st)
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

func (r *Runner) save(st *config.State) {
	if err := r.Profile.SaveState(st); err != nil {
		r.logger().Printf("cannot write %s: %v", r.Profile.StatePath(), err)
	}
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
