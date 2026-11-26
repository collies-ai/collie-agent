package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/collies-ai/collie-agent/internal/bindings"
	"github.com/collies-ai/collie-agent/internal/dispatch"
	"github.com/collies-ai/collie-agent/internal/machine"
)

// ReviewFile is where a review agent that takes an output schema writes its answer, in the session dir.
const ReviewFile = "review.json"

// TokenEnv is the environment variable that carries the session token to the agent.
const TokenEnv = "COLLIE_SESSION_TOKEN"

// API is the part of the machine API a session needs.
type API interface {
	RenewSessionToken(ctx context.Context, sessionID int64) (time.Time, error)
	SessionStarted(ctx context.Context, sessionID int64, sessionURL string) error
	SessionFinished(ctx context.Context, sessionID int64, exitCode int, usage *machine.Usage, review json.RawMessage) error
	// Resuming after a restart: a new token for a session collie still holds, and the suspend/resume events.
	ReissueSessionToken(ctx context.Context, sessionID int64) (*machine.Reissued, error)
	SessionSuspended(ctx context.Context, sessionID int64) error
	SessionResumed(ctx context.Context, sessionID int64, sessionURL string) error
	SessionResumeFailed(ctx context.Context, sessionID int64, reason string) error
}

// Adopter takes a resumed session into the dispatcher's count: it shows in the heartbeat's running sessions and holds
// a slot like any other. done frees it.
type Adopter interface {
	Adopt(sessionID, runnerID int64, purpose string) (done func(), err error)
}

// ResumeNote is told what happened to each session found at startup: status "resuming", "resumed" or "failed" (with
// the reason).
type ResumeNote func(sessionID int64, status, reason string)

// Manager runs sessions. It implements dispatch.Sessions.
type Manager struct {
	API API
	// Dir holds one directory per session (0700): its worktree, the agent's log and private files such as the MCP
	// config. It is the profile's "sessions" directory.
	Dir string
	Log *log.Logger
	// MaxConcurrency caps the sessions running at once (the dispatcher checks it too, before acking). Sessions of an
	// uncapped purpose (dispatch.Uncapped: reviews) do not count against it, as in the dispatcher and in collie.
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
	// Setup is the local config's session_setup: a command per purpose run in the worktree before the agent starts.
	Setup map[string]string
	// PhoneNotifications lets Claude Code sessions push to the person's phone (settings phone_notifications).
	PhoneNotifications bool
	// AnswerSettle is how long the answer to a startup dialog waits once the dialog showed (default 500ms), and
	// AnswerKeyGap the pause between its keys (default 150ms).
	AnswerSettle time.Duration
	AnswerKeyGap time.Duration
	ExitGrace    time.Duration
	// ResumeURLWait is how long a resumed interactive agent gets to show its session link before the resumed event goes
	// out without one (default 30 seconds).
	ResumeURLWait time.Duration

	mu         sync.Mutex
	running    map[int64]bool // session id → whether it counts against MaxConcurrency
	onRevoked  func(error)
	isRevoked  bool
	resumeNote ResumeNote
	wg         sync.WaitGroup
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
	m.isRevoked = true
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

var errAlreadyRunning = errors.New("already running")

// cappedRunning counts the running sessions that count against MaxConcurrency. The caller holds m.mu.
func (m *Manager) cappedRunning() int {
	n := 0
	for _, capped := range m.running {
		if capped {
			n++
		}
	}
	return n
}

// Running reports whether the session is running (by its directory name), for the startup sweep.
func (m *Manager) isRunning(name string) bool {
	id, err := strconv.ParseInt(name, 10, 64)
	if err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.running[id]
	return ok
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

// Resume picks up the sessions a previous run left on disk, before new work is taken: each session whose record can
// resume it is adopted (it shows in the heartbeat's running sessions and holds a slot), gets a new token from collie
// and runs again in its worktree with `claude --resume` or `codex exec resume`. A session that cannot be resumed is
// reported as resume_failed with the reason and cleaned up; one collie no longer holds is only cleaned up. Folders
// that are no session at all are swept. The work runs in the background; Resume returns once every session is adopted.
func (m *Manager) Resume(ctx context.Context, adopt Adopter, note ResumeNote) {
	m.mu.Lock()
	m.resumeNote = note
	m.mu.Unlock()
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.logf("reading the sessions left by the last run: %v", err)
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() || m.isRunning(e.Name()) {
			continue
		}
		dir := filepath.Join(m.Dir, e.Name())
		id, idErr := strconv.ParseInt(e.Name(), 10, 64)
		if idErr != nil || id <= 0 {
			m.logf("removing %s: not a session", dir)
			m.cleanup(0, dir)
			continue
		}
		rec, err := readMeta(dir)
		var reason string
		switch {
		case errors.Is(err, os.ErrNotExist):
			reason = "collie-agent stopped before the session was set up"
		case err != nil:
			reason = "its session record cannot be read: " + err.Error()
		case rec.SessionID == 0:
			reason = "it was recorded by a collie-agent that could not resume sessions"
		case rec.SessionID != id:
			reason = fmt.Sprintf("its session record is about session %d", rec.SessionID)
		case !rec.resumable():
			reason = "the agent had not started yet"
		}
		if reason == "" {
			reason = m.adopt(ctx, id, dir, rec, adopt)
		}
		if reason != "" {
			m.resumeFailed(ctx, id, dir, "could not resume: "+reason)
		}
	}
}

// adopt takes a resumable session in and resumes it in the background. It returns why it cannot, or "".
func (m *Manager) adopt(ctx context.Context, id int64, dir string, rec worktreeMeta, adopt Adopter) string {
	a, path, err := m.adapter(rec.Agent)
	if err != nil {
		return err.Error()
	}
	var done func()
	if adopt != nil {
		if done, err = adopt.Adopt(id, rec.Dispatch.RunnerID, rec.Dispatch.Purpose); err != nil {
			return err.Error()
		}
	}
	m.mu.Lock()
	if m.running == nil {
		m.running = map[int64]bool{}
	}
	m.running[id] = !dispatch.Uncapped(rec.Dispatch.Purpose)
	m.mu.Unlock()
	m.noteResume(id, "resuming", "")
	m.logf("session %d: resuming it (%s, %s)", id, rec.Agent, rec.AgentSession)
	m.launch(id, done, func() { m.resume(ctx, id, dir, rec, a, path) })
	return ""
}

// resumeFailed reports in the background that a session could not be resumed, and cleans it up.
func (m *Manager) resumeFailed(ctx context.Context, id int64, dir, reason string) {
	m.logf("session %d: %s", id, reason)
	m.noteResume(id, "failed", reason)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.report(ctx, id, "resume_failed", func(c context.Context) error { return m.API.SessionResumeFailed(c, id, reason) })
		m.cleanup(id, dir)
	}()
}

// resume stops what the last run left in the worktree, gets a new token and runs the session again.
func (m *Manager) resume(ctx context.Context, id int64, dir string, rec worktreeMeta, a Adapter, binary string) {
	if fi, err := os.Stat(rec.Worktree); err == nil && fi.IsDir() && ownedWorktree(rec.Repo, dir, rec.Worktree) {
		if n := stopProcessesIn(ctx, rec.Worktree); n > 0 {
			m.logf("session %d: stopped %d process(es) the last run left in its worktree", id, n)
		}
	}
	tok, err := m.reissue(ctx, id)
	switch {
	case err == nil:
	case errors.Is(err, machine.ErrGone):
		reason := "collie no longer holds this session"
		m.logf("session %d: not resumed, %s: %v", id, reason, err)
		m.noteResume(id, "failed", reason)
		m.cleanup(id, dir)
		return
	case errors.Is(err, machine.ErrRevoked):
		m.revoked(err)
		m.noteResume(id, "failed", "the machine was revoked")
		m.cleanup(id, dir)
		return
	case ctx.Err() != nil:
		return // shut down again: it stays suspended for the next start
	default:
		reason := "could not resume: no new session token: " + err.Error()
		m.logf("session %d: %s", id, reason)
		m.noteResume(id, "failed", reason)
		m.report(ctx, id, "resume_failed", func(c context.Context) error { return m.API.SessionResumeFailed(c, id, reason) })
		m.cleanup(id, dir)
		return
	}
	s := dispatch.Session{
		ID:       id,
		Dispatch: *rec.Dispatch,
		Agent:    rec.Agent,
		Binding: bindings.Binding{Binding: machine.Binding{RunnerID: rec.Dispatch.RunnerID, Project: rec.Dispatch.Project,
			Codebase: rec.Dispatch.Codebase, Folder: rec.Repo, PermissionMode: rec.PermissionMode}},
		Accepted: machine.Accepted{SessionID: id, DispatchID: rec.Dispatch.DispatchID, MCPURL: tok.MCPURL, SessionToken: tok.Token, ExpiresAt: tok.ExpiresAt},
	}
	m.run(ctx, s, a, binary, &rec)
}

// reissue asks collie for a new session token, retrying network errors for a while: right after a reboot the network
// may not be up yet.
func (m *Manager) reissue(ctx context.Context, id int64) (*machine.Reissued, error) {
	wait := m.RetryWait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	var err error
	for attempt := 1; attempt <= 8; attempt++ {
		var tok *machine.Reissued
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		tok, err = m.API.ReissueSessionToken(c, id)
		cancel()
		if err == nil || errors.Is(err, machine.ErrGone) || errors.Is(err, machine.ErrRevoked) || ctx.Err() != nil {
			return tok, err
		}
		m.logf("session %d: getting a new token (attempt %d): %v", id, attempt, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait * time.Duration(attempt)):
		}
	}
	return nil, err
}

// Wait waits for every session to end and be cleaned up (after ctx was cancelled, on shutdown).
func (m *Manager) Wait() { m.wg.Wait() }

// Start starts the session in the background and returns. done is called once the session ended and was cleaned up.
// The dispatch was already accepted, so collie counts the session as running: when it cannot start, collie is told it
// finished (exit code -1), which frees its slot there and looks at the waiting work again, instead of waiting for a
// heartbeat to notice it is gone.
func (m *Manager) Start(ctx context.Context, s dispatch.Session, done func()) error {
	err := m.start(ctx, s, done)
	// A second Start of a running session must not end the one that runs.
	if err != nil && !errors.Is(err, errAlreadyRunning) {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.report(ctx, s.ID, "finished", func(c context.Context) error { return m.API.SessionFinished(c, s.ID, -1, nil, nil) })
		}()
	}
	return err
}

func (m *Manager) start(ctx context.Context, s dispatch.Session, done func()) error {
	a, path, err := m.adapter(s.Agent)
	if err != nil {
		return err
	}
	capped := !dispatch.Uncapped(s.Dispatch.Purpose)
	m.mu.Lock()
	if m.running == nil {
		m.running = map[int64]bool{}
	}
	if _, ok := m.running[s.ID]; ok {
		m.mu.Unlock()
		return fmt.Errorf("session %d: %w", s.ID, errAlreadyRunning)
	}
	if capped && m.MaxConcurrency > 0 && m.cappedRunning() >= m.MaxConcurrency {
		m.mu.Unlock()
		return fmt.Errorf("at capacity (%d sessions)", m.MaxConcurrency)
	}
	m.running[s.ID] = capped
	m.mu.Unlock()
	m.launch(s.ID, done, func() { m.run(ctx, s, a, path, nil) })
	return nil
}

// launch runs f in the background for a session that holds a slot in m.running, then frees the slot and calls done.
func (m *Manager) launch(id int64, done func(), f func()) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.running, id)
			m.mu.Unlock()
			if done != nil {
				done()
			}
		}()
		f()
	}()
}

// notStarted is an error from before the agent process started.
type notStarted struct{ error }

func (e notStarted) Unwrap() error { return e.error }

// run is one session from worktree to cleanup. Its lifecycle events go out through one queue, in order, so a slow
// collie never holds up the agent, its output or its supervision. resume is the record of a session resumed after a
// restart (nil for a new one): it runs again in its worktree, and a failure before the agent starts is reported as
// resume_failed.
//
// A graceful shutdown (ctx cancelled) of a session that can be resumed suspends it instead: collie is told so and its
// worktree, branch and session dir stay for the next start.
func (m *Manager) run(ctx context.Context, s dispatch.Session, a Adapter, binary string, resume *worktreeMeta) {
	dir := filepath.Join(m.Dir, strconv.FormatInt(s.ID, 10))
	// The folder was resolved and checked against the root folders when the offer was accepted: use that path as is,
	// without resolving it again. The worktree goes inside it (WorktreePath).
	worktree := WorktreePath(s.Binding.Folder, s.ID)
	if resume != nil {
		worktree = resume.Worktree
	}
	keep := false
	defer func() {
		if keep {
			return
		}
		m.cleanup(s.ID, dir)
	}()

	events := make(chan func(), 4) // at most started (resumed), started with the link, finished: emit never blocks
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
	defer func() {
		close(events)
		<-sent
	}()

	exitCode, usage, suspend, err := m.execute(ctx, s, a, binary, dir, worktree, resume, emit)
	var ns notStarted
	switch {
	case resume != nil && errors.As(err, &ns) && ctx.Err() != nil:
		// Shut down again before the agent could be resumed: it stays suspended for the next start.
		m.logf("session %d: not resumed before the shutdown; it stays suspended", s.ID)
		keep = true
	case resume != nil && errors.As(err, &ns):
		m.logf("session %d: could not resume: %v", s.ID, err)
		reason := "could not resume: " + err.Error()
		m.noteResume(s.ID, "failed", reason)
		emit("resume_failed", func(c context.Context) error { return m.API.SessionResumeFailed(c, s.ID, reason) })
	case suspend:
		if err := updateMeta(dir, func(r *worktreeMeta) { r.Usage = usage }); err != nil {
			m.logf("session %d: saving its usage: %v", s.ID, err)
		}
		emit("suspended", func(c context.Context) error { return m.API.SessionSuspended(c, s.ID) })
		keep = true
		m.logf("session %d suspended; it resumes when collie-agent starts again", s.ID)
	default:
		if err != nil {
			m.logf("session %d: %v", s.ID, err)
		}
		review := m.readReview(s.ID, dir)
		emit("finished", func(c context.Context) error { return m.API.SessionFinished(c, s.ID, exitCode, usage, review) })
		m.logf("session %d finished with exit code %d", s.ID, exitCode)
	}
}

// cleanup removes a session's worktree, branch and directory.
func (m *Manager) cleanup(id int64, dir string) {
	cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stopped, err := removeWorktree(cctx, dir)
	if stopped > 0 {
		m.logf("session %d: stopped %d process(es) still running in its worktree", id, stopped)
	}
	if err != nil {
		m.logf("session %d: removing the worktree: %v", id, err)
	}
}

func (m *Manager) noteResume(id int64, status, reason string) {
	m.mu.Lock()
	f := m.resumeNote
	m.mu.Unlock()
	if f != nil {
		f(id, status, reason)
	}
}

func (m *Manager) revokedNow() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isRevoked
}

// execute prepares the worktree, runs the agent until it exits (or ctx ends, its token dies or it sits idle) and
// returns its exit code (-1 when it never ran or was killed), the usage its output reported and whether it was stopped
// by a graceful shutdown with what it needs to be resumed (suspend). An error from before the agent started is a
// notStarted. A resumed session (resume) runs again in its worktree, on the resume prompt.
func (m *Manager) execute(ctx context.Context, s dispatch.Session, a Adapter, binary, dir, worktree string, resume *worktreeMeta, emit func(string, func(context.Context) error)) (int, *machine.Usage, bool, error) {
	fail := func(err error) (int, *machine.Usage, bool, error) { return -1, nil, false, notStarted{err} }
	var agentSession, prompt string
	payload := payloadText(s.Dispatch)
	if resume == nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fail(err)
		}
		agentSession = a.NewSessionID()
		d := s.Dispatch
		if err := updateMeta(dir, func(r *worktreeMeta) {
			r.SessionID, r.Dispatch, r.Agent, r.PermissionMode, r.AgentSession = s.ID, &d, s.Agent, s.Binding.PermissionMode, agentSession
			if d.Purpose == "reviewer" {
				r.ReviewSchema, r.ReviewFile = filepath.Join(dir, "review.schema.json"), filepath.Join(dir, ReviewFile)
			}
		}); err != nil {
			return fail(fmt.Errorf("writing the session record: %w", err))
		}
		branch := fmt.Sprintf("collie/session-%d", s.ID)
		if err := addWorktree(ctx, s.Binding.Folder, dir, worktree, branch); err != nil {
			return fail(fmt.Errorf("creating the worktree: %w", err))
		}
		if err := m.setup(ctx, s, worktree); err != nil {
			return fail(err)
		}
		prompt = PromptFor(s.Dispatch.Purpose, payload)
	} else {
		if err := checkResumable(ctx, *resume, dir, a); err != nil {
			return fail(err)
		}
		// The idle and startup clocks start again: the markers are the previous run's.
		_ = os.Remove(filepath.Join(dir, ActivityFile))
		_ = os.Remove(filepath.Join(dir, IdleFile))
		if err := updateMeta(dir, func(r *worktreeMeta) { r.Resumes++ }); err != nil {
			return fail(fmt.Errorf("writing the session record: %w", err))
		}
		agentSession, prompt = resume.AgentSession, resumePrompt
	}
	name := fmt.Sprintf("collie %s #%d", s.Dispatch.Project, s.ID)
	spec, err := a.Command(Launch{
		Name: name, Worktree: worktree, Prompt: prompt, MCPURL: mcpURL(s),
		PermissionMode: s.Binding.PermissionMode, Purpose: s.Dispatch.Purpose, Binary: binary, TokenEnv: TokenEnv, SessionDir: dir,
		PhoneNotifications: m.PhoneNotifications, AgentSession: agentSession, Resume: resume != nil,
	})
	if err != nil {
		return fail(err)
	}

	logFile, err := os.OpenFile(filepath.Join(dir, "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	defer logFile.Close()

	cmd := exec.Command(binary, spec.Args...)
	cmd.Dir = worktree
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Env = append(cmd.Env, TokenEnv+"="+s.Accepted.SessionToken, "COLLIE_MCP_URL="+mcpURL(s))
	if s.Dispatch.Purpose == "reviewer" {
		// Whatever the agent, a review can answer in collie's review shape: collie's review steps tell it to write the
		// verdict to COLLIE_REVIEW_FILE (codex is also held to it by --output-schema), and readReview sends it.
		schema, err := writeReviewSchema(dir)
		if err != nil {
			return fail(err)
		}
		cmd.Env = append(cmd.Env, "COLLIE_REVIEW_FILE="+filepath.Join(dir, ReviewFile), "COLLIE_REVIEW_SCHEMA="+schema)
	}
	var (
		out   io.Reader
		tty   *os.File       // the terminal of an interactive agent
		pipeW *io.PipeWriter // the output pipe of a headless one
	)
	if spec.TTY {
		// A new session with the terminal as its controlling one: the agent leads its own process group.
		tty, err = pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 220})
		if err != nil {
			return fail(fmt.Errorf("starting %s: %w", a.Binary(), err))
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
			return fail(fmt.Errorf("starting %s: %w", a.Binary(), err))
		}
		out = r
	}
	started := time.Now()
	// A resumed session sends one resumed event (collie counts each one): with the session link when the agent shows
	// one soon enough, else without it.
	var resumedOnce sync.Once
	resumed := func(url string) {
		resumedOnce.Do(func() {
			m.noteResume(s.ID, "resumed", "")
			emit("resumed", func(c context.Context) error { return m.API.SessionResumed(c, s.ID, url) })
		})
	}
	var resumeWait <-chan time.Time
	if resume != nil {
		m.logf("session %d: resumed %s in %s (%s)", s.ID, a.Binary(), worktree, payload)
		if spec.TTY {
			wait := m.ResumeURLWait
			if wait <= 0 {
				wait = 30 * time.Second
			}
			t := time.NewTimer(wait)
			defer t.Stop()
			resumeWait = t.C
		} else {
			resumed("")
		}
	} else {
		m.logf("session %d: started %s in %s (%s)", s.ID, a.Binary(), worktree, payload)
		emit("started", func(c context.Context) error { return m.API.SessionStarted(c, s.ID, "") })
	}

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
	if resume != nil && resume.Usage != nil {
		u := *resume.Usage // what the agent reported before it was suspended
		obs.Usage = &u
	}
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
			hadURL, hadThread := obs.URL != "", obs.Thread != ""
			a.Observe(raw, &obs)
			if !hadURL && obs.URL != "" {
				url := obs.URL
				m.logf("session %d: %s", s.ID, url)
				if resume != nil {
					resumed(url)
				} else {
					emit("started", func(c context.Context) error { return m.API.SessionStarted(c, s.ID, url) })
				}
			}
			if !hadThread && obs.Thread != "" && resume == nil {
				thread := obs.Thread
				if err := updateMeta(dir, func(r *worktreeMeta) { r.AgentSession = thread }); err != nil {
					m.logf("session %d: saving its thread id: %v", s.ID, err)
				}
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

	var (
		waitErr  error
		shutdown bool
	)
loop:
	for {
		select {
		case waitErr = <-waited:
			break loop
		case <-resumeWait:
			resumed("")
		case <-ctx.Done():
			m.logf("session %d: stopping the agent (%v)", s.ID, ctx.Err())
			m.stop(cmd, waited)
			waitErr = errors.New("stopped")
			shutdown = true
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
	if resume != nil {
		resumed("") // it ended before showing its link: collie hears of the resume before the end
	}
	suspend := false
	if shutdown && !m.revokedNow() {
		r, err := readMeta(dir)
		suspend = err == nil && r.resumable() && a.HasTranscript(r.AgentSession)
	}
	return exitCode(cmd, waitErr), obs.Usage, suspend, nil
}

// checkResumable checks that what a resume needs is still there: the worktree (a session worktree, still on the
// session's branch) and the agent's transcript of the session.
func checkResumable(ctx context.Context, r worktreeMeta, dir string, a Adapter) error {
	if !ownedWorktree(r.Repo, dir, r.Worktree) {
		return fmt.Errorf("%s is not a session worktree", r.Worktree)
	}
	if fi, err := os.Stat(r.Worktree); err != nil || !fi.IsDir() {
		return fmt.Errorf("its worktree %s is gone", r.Worktree)
	}
	if _, err := git(ctx, r.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+r.Branch); err != nil {
		return fmt.Errorf("its branch %s is gone", r.Branch)
	}
	if head, err := git(ctx, r.Worktree, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != r.Branch {
		return fmt.Errorf("its worktree is not on branch %s", r.Branch)
	}
	if !a.HasTranscript(r.AgentSession) {
		return fmt.Errorf("%s kept no transcript of session %s", a.Binary(), r.AgentSession)
	}
	return nil
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

// setup runs the local config's session_setup command for the dispatch's purpose in the worktree, before the agent
// starts: whatever the person needs there (an identity for the agent, tooling). Its output goes to the session's
// log. A command that fails, or runs past two minutes, stops the session: its agent would run without it.
func (m *Manager) setup(ctx context.Context, s dispatch.Session, worktree string) error {
	command := strings.TrimSpace(m.Setup[s.Dispatch.Purpose])
	if command == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = worktree
	cmd.Env = append(os.Environ(),
		"COLLIE_SESSION_ID="+strconv.FormatInt(s.ID, 10), "COLLIE_PURPOSE="+s.Dispatch.Purpose,
		"COLLIE_PROJECT="+s.Dispatch.Project, "COLLIE_CODEBASE="+s.Dispatch.Codebase)
	out, err := cmd.CombinedOutput()
	m.logf("session %d: session_setup (%s): %s", s.ID, s.Dispatch.Purpose, strings.TrimSpace(string(out)))
	if err != nil {
		tail := strings.TrimSpace(string(out))
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return fmt.Errorf("session_setup for %s failed: %v: %s", s.Dispatch.Purpose, err, tail)
	}
	return nil
}

// writeReviewSchema writes the review shape to the session dir and returns its path.
func writeReviewSchema(dir string) (string, error) {
	schema := filepath.Join(dir, "review.schema.json")
	if err := os.WriteFile(schema, reviewSchema, 0o600); err != nil {
		return "", fmt.Errorf("writing the review schema: %w", err)
	}
	return schema, nil
}

// readReview returns the review the agent answered with (ReviewFile), when it wrote one with a known verdict.
func (m *Manager) readReview(id int64, dir string) json.RawMessage {
	b, err := os.ReadFile(filepath.Join(dir, ReviewFile))
	if err != nil {
		return nil
	}
	var r struct {
		Verdict string `json:"verdict"`
	}
	if json.Unmarshal(b, &r) != nil || (r.Verdict != "approve" && r.Verdict != "changes_requested" && r.Verdict != "escalate") {
		m.logf("session %d: the review the agent wrote is not valid; not sent", id)
		return nil
	}
	var compact bytes.Buffer
	if json.Compact(&compact, b) != nil {
		return nil
	}
	return compact.Bytes()
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
