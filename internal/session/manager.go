package session

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

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
	// IdleTimeout is how long an interactive agent (Claude Code) may sit idle, waiting for input with nothing to do,
	// before it is ended. Default 15 minutes. IdlePoll is how often that is checked, and ExitGrace how long the agent
	// gets to exit after its exit command is typed (default 30 seconds) before it is stopped.
	IdleTimeout time.Duration
	IdlePoll    time.Duration
	// AnswerSettle is how long the answer to a startup dialog waits once the dialog showed (default 500ms), and
	// AnswerKeyGap the pause between its keys (default 150ms).
	AnswerSettle time.Duration
	AnswerKeyGap time.Duration
	ExitGrace    time.Duration

	mu        sync.Mutex
	running   map[int64]bool
	onRevoked func(error)
	wg        sync.WaitGroup
}

// OnRevoked sets what to call when a session endpoint answers 401: the machine was revoked, so the whole runner stops,
// not only the session.
func (m *Manager) OnRevoked(f func(error)) {
	m.mu.Lock()
	m.onRevoked = f
	m.mu.Unlock()
}

func (m *Manager) revoked(err error) {
	m.mu.Lock()
	f := m.onRevoked
	m.mu.Unlock()
	if f != nil {
		f(err)
	}
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

// run is one session from worktree to cleanup. Its lifecycle events go out through one queue, in order, so a slow
// collie never holds up the agent, its output or its supervision.
func (m *Manager) run(ctx context.Context, s dispatch.Session, a Adapter, binary string) {
	dir := filepath.Join(m.Dir, strconv.FormatInt(s.ID, 10))
	// The folder was resolved and checked against the root folders when the offer was accepted: use that path as is,
	// without resolving it again. The worktree goes inside it (WorktreePath).
	worktree := WorktreePath(s.Binding.Folder, s.ID)
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := removeWorktree(cctx, dir); err != nil {
			m.logf("session %d: removing the worktree: %v", s.ID, err)
		}
	}()

	events := make(chan func(), 4) // at most started, started with the link, finished: emit never blocks
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for send := range events {
			send()
		}
	}()
	emit := func(event string, send func(context.Context) error) {
		events <- func() { m.report(ctx, s.ID, event, send) }
	}

	exitCode, usage, err := m.execute(ctx, s, a, binary, dir, worktree, emit)
	if err != nil {
		m.logf("session %d: %v", s.ID, err)
	}
	emit("finished", func(c context.Context) error { return m.API.SessionFinished(c, s.ID, exitCode, usage) })
	close(events)
	<-sent
	m.logf("session %d finished with exit code %d", s.ID, exitCode)
}

// execute prepares the worktree, runs the agent until it exits (or ctx ends, its token dies or it sits idle) and
// returns its exit code (-1 when it never ran or was killed) and the usage its output reported.
func (m *Manager) execute(ctx context.Context, s dispatch.Session, a Adapter, binary, dir, worktree string, emit func(string, func(context.Context) error)) (int, *machine.Usage, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return -1, nil, err
	}
	branch := fmt.Sprintf("collie/session-%d", s.ID)
	if err := addWorktree(ctx, s.Binding.Folder, dir, worktree, branch); err != nil {
		return -1, nil, fmt.Errorf("creating the worktree: %w", err)
	}
	name := fmt.Sprintf("collie %s #%d", s.Dispatch.Project, s.ID)
	payload := payloadText(s.Dispatch)
	spec, err := a.Command(Launch{
		Name: name, Worktree: worktree, Prompt: PromptFor(s.Dispatch.Purpose, payload), MCPURL: mcpURL(s),
		PermissionMode: s.Binding.PermissionMode, TokenEnv: TokenEnv, SessionDir: dir,
	})
	if err != nil {
		return -1, nil, err
	}

	logFile, err := os.OpenFile(filepath.Join(dir, "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return -1, nil, err
	}
	defer logFile.Close()

	cmd := exec.Command(binary, spec.Args...)
	cmd.Dir = worktree
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Env = append(cmd.Env, TokenEnv+"="+s.Accepted.SessionToken, "COLLIE_MCP_URL="+mcpURL(s))
	var (
		out   io.Reader
		tty   *os.File       // the terminal of an interactive agent
		pipeW *io.PipeWriter // the output pipe of a headless one
	)
	if spec.TTY {
		// A new session with the terminal as its controlling one: the agent leads its own process group.
		tty, err = pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 220})
		if err != nil {
			return -1, nil, fmt.Errorf("starting %s: %w", a.Binary(), err)
		}
		defer tty.Close()
		out = tty
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its own process group, so a stop reaches its children
		var r *io.PipeReader
		r, pipeW = io.Pipe()
		cmd.Stdout, cmd.Stderr = pipeW, pipeW
		if err := cmd.Start(); err != nil {
			pipeW.Close()
			return -1, nil, fmt.Errorf("starting %s: %w", a.Binary(), err)
		}
		out = r
	}
	started := time.Now()
	m.logf("session %d: started %s in %s (%s)", s.ID, a.Binary(), worktree, payload)
	emit("started", func(c context.Context) error { return m.API.SessionStarted(c, s.ID, "") })

	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait() // an interactive agent's leftovers get SIGHUP from the kernel when it exits
		if pipeW != nil {
			pipeW.Close()
		}
		waited <- err
	}()

	// Drain the output from the start: keep it in the session's log (without terminal escape codes), report the
	// session link once it shows up, answer the dialogs the adapter knows, and note when the agent last wrote.
	var (
		obs        Observed
		lastOutput atomic.Int64
	)
	lastOutput.Store(time.Now().UnixNano())
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		answered := make([]bool, len(spec.Answers))
		dialogs := make([][]string, len(spec.Answers)) // the lines of each dialog since its question showed
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), maxLine+1)
		sc.Split(lineSplitter(maxLine))
		for sc.Scan() {
			lastOutput.Store(time.Now().UnixNano())
			raw := sc.Text()
			text := stripEscapes(raw)
			if strings.TrimSpace(text) != "" {
				fmt.Fprintln(logFile, text)
			}
			hadURL := obs.URL != ""
			a.Observe(raw, &obs)
			if !hadURL && obs.URL != "" {
				url := obs.URL
				m.logf("session %d: %s", s.ID, url)
				emit("started", func(c context.Context) error { return m.API.SessionStarted(c, s.ID, url) })
			}
			for i, ans := range spec.Answers {
				if answered[i] || tty == nil || !m.startingUp(dir, started) {
					continue
				}
				if dialogs[i] == nil && ans.Match.MatchString(text) {
					dialogs[i] = []string{}
				}
				if dialogs[i] == nil {
					continue
				}
				dialogs[i] = append(dialogs[i], text)
				keys, done := ans.respond(dialogs[i])
				if !done {
					continue
				}
				answered[i] = true
				if len(keys) == 0 {
					m.logf("session %d: %s shows a dialog collie-agent cannot answer; answer it in the session", s.ID, a.Binary())
					continue
				}
				go typeKeys(tty, keys, m.answerSettle(), m.answerKeyGap())
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()

	renewCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	tokenDead := make(chan error, 1)
	go m.renew(renewCtx, s, tokenDead)

	var tick <-chan time.Time
	if spec.IdleFiles {
		poll := m.IdlePoll
		if poll <= 0 {
			poll = 15 * time.Second
		}
		t := time.NewTicker(poll)
		defer t.Stop()
		tick = t.C
	}

	var waitErr error
loop:
	for {
		select {
		case waitErr = <-waited:
			break loop
		case <-ctx.Done():
			m.logf("session %d: stopping the agent (%v)", s.ID, ctx.Err())
			m.stop(cmd, waited)
			waitErr = errors.New("stopped")
			break loop
		case err := <-tokenDead:
			m.logf("session %d: stopping the agent, its token is no longer valid: %v", s.ID, err)
			m.stop(cmd, waited)
			waitErr = errors.New("stopped")
			break loop
		case <-tick:
			if !m.idle(dir, started, time.Unix(0, lastOutput.Load())) {
				continue
			}
			m.logf("session %d: the agent has been idle for %v, ending it", s.ID, m.idleTimeout())
			waitErr = m.end(cmd, tty, spec.ExitInput, waited)
			break loop
		}
	}
	select {
	case <-scanned:
	case <-time.After(5 * time.Second): // a terminal some leftover still holds: stop reading it
		if tty != nil {
			tty.Close()
		}
		<-scanned
	}
	return exitCode(cmd, waitErr), obs.Usage, nil
}

// startingUp reports whether the agent is still starting: it did no work yet (its hooks never marked activity) and
// it started less than StartupWindow ago. Only then are startup dialogs answered.
func (m *Manager) startingUp(dir string, started time.Time) bool {
	return time.Since(started) < StartupWindow && mtime(filepath.Join(dir, ActivityFile)).IsZero()
}

func (m *Manager) answerSettle() time.Duration {
	if m.AnswerSettle > 0 {
		return m.AnswerSettle
	}
	return 500 * time.Millisecond
}

func (m *Manager) answerKeyGap() time.Duration {
	if m.AnswerKeyGap > 0 {
		return m.AnswerKeyGap
	}
	return 150 * time.Millisecond
}

func (m *Manager) idleTimeout() time.Duration {
	if m.IdleTimeout > 0 {
		return m.IdleTimeout
	}
	return 15 * time.Minute
}

// idle reports whether an interactive agent has been waiting for input, with nothing to do, for the idle timeout.
// Its hooks touch the activity file when it works and the idle file when it stops: it is idle when the last touch was
// the idle file, that long ago. An agent whose hooks never ran (hooks disabled in its settings) is judged by its
// output instead: idle once it wrote nothing for that long.
func (m *Manager) idle(dir string, started, lastOutput time.Time) bool {
	timeout := m.idleTimeout()
	act, idle := mtime(filepath.Join(dir, ActivityFile)), mtime(filepath.Join(dir, IdleFile))
	if act.IsZero() && idle.IsZero() {
		return time.Since(started) > timeout && time.Since(lastOutput) > timeout
	}
	return !idle.IsZero() && !idle.Before(act) && time.Since(idle) > timeout
}

func mtime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// end ends an idle interactive agent the way a person would (typing its exit command), and stops it if it does not
// exit within a while.
func (m *Manager) end(cmd *exec.Cmd, tty *os.File, input string, waited <-chan error) error {
	if tty != nil && input != "" {
		grace := m.ExitGrace
		if grace <= 0 {
			grace = 30 * time.Second
		}
		_, _ = tty.WriteString(input)
		select {
		case err := <-waited:
			return err
		case <-time.After(grace):
		}
	}
	m.stop(cmd, waited)
	return errors.New("stopped")
}

// maxLine is the longest line of output read whole; a longer one is skipped up to its end.
const maxLine = 1 << 20

// lineSplitter splits output on \n or \r (a terminal program redraws lines with \r). A line longer than max is
// dropped and reading resumes after its end, so one huge record never stops the scan.
func lineSplitter(max int) bufio.SplitFunc {
	skipping := false
	return func(data []byte, atEOF bool) (int, []byte, error) {
		i := bytes.IndexAny(data, "\r\n")
		switch {
		case i >= 0 && skipping:
			skipping = false
			return i + 1, nil, nil // the tail of a dropped line
		case i >= 0:
			return i + 1, data[:i], nil
		case len(data) >= max:
			skipping = true
			return len(data), nil, nil
		case atEOF && len(data) > 0 && !skipping:
			return len(data), data, nil
		case atEOF:
			return len(data), nil, nil
		}
		return 0, nil, nil
	}
}

// escapes are terminal control sequences: CSI (colors, cursor moves), OSC (titles, hyperlinks) and single-character
// escapes.
var escapes = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// cursorForward is CSI n C: an interactive CLI such as Claude Code draws the spaces between words with it.
var cursorForward = regexp.MustCompile(`\x1b\[([0-9]*)C`)

// stripEscapes removes the control sequences, keeping the spaces that cursor moves forward stand for.
func stripEscapes(s string) string {
	s = cursorForward.ReplaceAllStringFunc(s, func(seq string) string {
		n, _ := strconv.Atoi(seq[2 : len(seq)-1])
		return strings.Repeat(" ", min(max(n, 1), 256))
	})
	return escapes.ReplaceAllString(s, "")
}

// typeKeys types an answer into the agent's terminal, one key at a time: Claude Code drops keys that arrive while it
// is still drawing the dialog, or together in one read.
func typeKeys(tty *os.File, keys []string, settle, gap time.Duration) {
	time.Sleep(settle)
	for i, k := range keys {
		if i > 0 {
			time.Sleep(gap)
		}
		if _, err := tty.WriteString(k); err != nil {
			return
		}
	}
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
	if err == nil && cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return cmd.ProcessState.ExitCode()
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
		case errors.Is(err, machine.ErrRevoked):
			m.revoked(err) // the machine is cut off: the runner stops, and this session with it
			dead <- err
			return
		case errors.Is(err, machine.ErrGone):
			dead <- err // only this session ended
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
		case errors.Is(err, machine.ErrRevoked):
			m.logf("session %d: %s event not recorded: %v", sessionID, event, err)
			m.revoked(err)
			return
		case errors.Is(err, machine.ErrGone):
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
