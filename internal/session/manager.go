package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/collies-ai/collie-agent/internal/dispatch"
	"github.com/collies-ai/collie-agent/internal/machine"
)

// TokenEnv is the environment variable that carries the session token to the agent.
const TokenEnv = "COLLIE_SESSION_TOKEN"

// API is the part of the machine API a session needs.
type API interface {
	RenewSessionToken(ctx context.Context, sessionID int64) (time.Time, error)
	SessionStarted(ctx context.Context, sessionID int64, sessionURL string) error
	SessionFinished(ctx context.Context, sessionID int64, exitCode int, usage *machine.Usage) error
}

// Manager runs sessions. It implements dispatch.Sessions.
type Manager struct {
	API API
	// Dir holds one directory per session (0700): its worktree, the agent's log and private files such as the MCP
	// config. It is the profile's "sessions" directory.
	Dir string
	Log *log.Logger
	// MaxConcurrency caps the sessions running at once (the dispatcher checks it too, before acking).
	MaxConcurrency int
	// Adapters and LookPath are replaced by tests.
	Adapters map[string]Adapter
	LookPath func(string) (string, error)
	// KillGrace is how long a stopped agent gets between SIGTERM and SIGKILL.
	KillGrace time.Duration
	// RenewBefore is how long before the token expires it is renewed.
	RenewBefore time.Duration
	// RetryWait is the pause between attempts to report an event; tests shorten it.
	RetryWait time.Duration

	mu      sync.Mutex
	running map[int64]bool
	wg      sync.WaitGroup
}

var _ dispatch.Sessions = (*Manager)(nil)

func (m *Manager) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log.Printf(format, args...)
	}
}

func (m *Manager) adapter(agent string) (Adapter, string, error) {
	adapters := m.Adapters
	if adapters == nil {
		adapters = Adapters
	}
	a, ok := adapters[agent]
	if !ok {
		return nil, "", fmt.Errorf("collie-agent has no adapter for agent %q", agent)
	}
	look := m.LookPath
	if look == nil {
		look = exec.LookPath
	}
	path, err := look(a.Binary())
	if err != nil {
		return nil, "", fmt.Errorf("%s is not installed on this machine", a.Binary())
	}
	return a, path, nil
}

// CanRun says whether this machine can run the agent: an adapter exists and its CLI is installed.
func (m *Manager) CanRun(agent string) error {
	_, _, err := m.adapter(agent)
	return err
}

// Running reports whether the session is running (by its directory name), for the startup sweep.
func (m *Manager) isRunning(name string) bool {
	id, err := strconv.ParseInt(name, 10, 64)
	if err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[id]
}

// Sweep removes the worktrees that sessions which are no longer running left behind (a crash, a kill -9).
func (m *Manager) Sweep(ctx context.Context) {
	n, err := Sweep(ctx, m.Dir, m.isRunning)
	if n > 0 {
		m.logf("removed %d stale session worktree(s)", n)
	}
	if err != nil {
		m.logf("sweeping stale worktrees: %v", err)
	}
}

// Wait waits for every session to end and be cleaned up (after ctx was cancelled, on shutdown).
func (m *Manager) Wait() { m.wg.Wait() }

// Start starts the session in the background and returns. done is called once the session ended and was cleaned up.
func (m *Manager) Start(ctx context.Context, s dispatch.Session, done func()) error {
	a, path, err := m.adapter(s.Agent)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.running == nil {
		m.running = map[int64]bool{}
	}
	if m.MaxConcurrency > 0 && len(m.running) >= m.MaxConcurrency {
		m.mu.Unlock()
		return fmt.Errorf("at capacity (%d sessions)", m.MaxConcurrency)
	}
	if m.running[s.ID] {
		m.mu.Unlock()
		return fmt.Errorf("session %d is already running", s.ID)
	}
	m.running[s.ID] = true
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.running, s.ID)
			m.mu.Unlock()
			if done != nil {
				done()
			}
		}()
		m.run(ctx, s, a, path)
	}()
	return nil
}

// run is one session from worktree to cleanup.
func (m *Manager) run(ctx context.Context, s dispatch.Session, a Adapter, binary string) {
	dir := filepath.Join(m.Dir, strconv.FormatInt(s.ID, 10))
	worktree := filepath.Join(dir, "worktree")
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := removeWorktree(cctx, dir); err != nil {
			m.logf("session %d: removing the worktree: %v", s.ID, err)
		}
	}()

	exitCode, err := m.execute(ctx, s, a, binary, dir, worktree)
	if err != nil {
		m.logf("session %d: %v", s.ID, err)
	}
	m.report(ctx, s.ID, "finished", func(c context.Context) error {
		return m.API.SessionFinished(c, s.ID, exitCode, nil)
	})
	m.logf("session %d finished with exit code %d", s.ID, exitCode)
}

// execute prepares the worktree, runs the agent until it exits (or ctx ends) and returns its exit code; -1 when it
// never ran or was killed.
func (m *Manager) execute(ctx context.Context, s dispatch.Session, a Adapter, binary, dir, worktree string) (int, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return -1, err
	}
	// The folder was resolved and checked against the root folders when the offer was accepted: use that path as is,
	// without resolving it again.
	branch := fmt.Sprintf("collie/session-%d", s.ID)
	if err := addWorktree(ctx, s.Binding.Folder, worktree, branch); err != nil {
		return -1, fmt.Errorf("creating the worktree: %w", err)
	}
	name := fmt.Sprintf("collie %s #%d", s.Dispatch.Project, s.ID)
	payload := payloadText(s.Dispatch)
	cmdSpec, err := a.Command(Launch{
		Name: name, Worktree: worktree, Prompt: PromptFor(s.Dispatch.Purpose, payload), MCPURL: mcpURL(s),
		PermissionMode: s.Binding.PermissionMode, TokenEnv: TokenEnv, SessionDir: dir,
	})
	if err != nil {
		return -1, err
	}

	logFile, err := os.OpenFile(filepath.Join(dir, "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return -1, err
	}
	defer logFile.Close()

	cmd := exec.Command(binary, cmdSpec.Args...)
	cmd.Dir = worktree
	cmd.Env = append(os.Environ(), cmdSpec.Env...)
	cmd.Env = append(cmd.Env, TokenEnv+"="+s.Accepted.SessionToken, "COLLIE_MCP_URL="+mcpURL(s))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its own process group, so a stop reaches its children
	out, w := io.Pipe()
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		w.Close()
		return -1, fmt.Errorf("starting %s: %w", a.Binary(), err)
	}
	m.logf("session %d: started %s in %s (%s)", s.ID, a.Binary(), worktree, payload)
	m.report(ctx, s.ID, "started", func(c context.Context) error { return m.API.SessionStarted(c, s.ID, "") })

	// Scan the output: keep it in the session's log and report the session link once it shows up.
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		reported := false
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(logFile, line)
			if !reported {
				if url := a.SessionURL(line); url != "" {
					reported = true
					m.logf("session %d: %s", s.ID, url)
					m.report(ctx, s.ID, "started", func(c context.Context) error { return m.API.SessionStarted(c, s.ID, url) })
				}
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait(); w.Close() }()

	renewCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	tokenDead := make(chan error, 1)
	go m.renew(renewCtx, s, tokenDead)

	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		m.logf("session %d: stopping the agent (%v)", s.ID, ctx.Err())
		m.stop(cmd, waited)
		waitErr = errors.New("stopped")
	case err := <-tokenDead:
		m.logf("session %d: stopping the agent, its token is no longer valid: %v", s.ID, err)
		m.stop(cmd, waited)
		waitErr = errors.New("stopped")
	}
	<-scanned
	return exitCode(cmd, waitErr), nil
}

// stop sends the agent's process group SIGTERM, then SIGKILL after KillGrace, and waits for it.
func (m *Manager) stop(cmd *exec.Cmd, waited <-chan error) {
	grace := m.KillGrace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-waited:
	case <-time.After(grace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waited
	}
}

func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return cmd.ProcessState.ExitCode()
	}
	if err == nil {
		return 0
	}
	return -1 // killed by a signal, or stopped
}

// renew keeps the session token alive: it renews it RenewBefore its expiry, and reports on dead when collie says the
// session is gone or the machine was revoked.
func (m *Manager) renew(ctx context.Context, s dispatch.Session, dead chan<- error) {
	before := m.RenewBefore
	if before <= 0 {
		before = 10 * time.Minute
	}
	expires := s.Accepted.ExpiresAt
	for {
		wait := time.Until(expires) - before
		if expires.IsZero() || wait < time.Second {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		next, err := m.API.RenewSessionToken(ctx, s.ID)
		switch {
		case err == nil:
			expires = next
		case ctx.Err() != nil:
			return
		case errors.Is(err, machine.ErrGone), errors.Is(err, machine.ErrRevoked):
			dead <- err
			return
		default:
			m.logf("session %d: renewing the token: %v (retrying)", s.ID, err)
			expires = time.Now().Add(before + time.Minute) // try again in a minute
		}
	}
}

// report sends a lifecycle event, retrying network errors and collie's "send it again" a few times. It still
// reports while collie-agent shuts down (ctx cancelled), so the session's slot is freed.
func (m *Manager) report(ctx context.Context, sessionID int64, event string, send func(context.Context) error) {
	wait := m.RetryWait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	for attempt := 1; attempt <= 5; attempt++ {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := send(c)
		cancel()
		switch {
		case err == nil:
			return
		case errors.Is(err, machine.ErrGone), errors.Is(err, machine.ErrRevoked):
			m.logf("session %d: %s event not recorded: %v", sessionID, event, err)
			return
		}
		m.logf("session %d: reporting %s (attempt %d): %v", sessionID, event, attempt, err)
		time.Sleep(wait * time.Duration(attempt))
	}
}

func mcpURL(s dispatch.Session) string {
	if s.Accepted.MCPURL != "" {
		return s.Accepted.MCPURL
	}
	return s.Dispatch.MCPURL
}

// payloadText is the job reference as collie writes it (RunnerDispatch#payload), from the offer's payload field or,
// when missing, rebuilt from its kind and reference.
func payloadText(d machine.Dispatch) string {
	var p string
	if len(d.Payload) > 0 && d.Payload[0] == '"' {
		_ = jsonUnmarshalString(d.Payload, &p)
	}
	if p != "" {
		return p
	}
	if d.Kind == "work_goal" && d.Codebase != "" {
		return fmt.Sprintf("collie work_goal #%s in %s", d.Reference, d.Codebase)
	}
	return fmt.Sprintf("collie %s #%s", d.Kind, d.Reference)
}
