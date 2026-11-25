package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collies-ai/collie-agent/internal/bindings"
	"github.com/collies-ai/collie-agent/internal/dispatch"
	"github.com/collies-ai/collie-agent/internal/machine"
)

// fakeAPI records the lifecycle calls.
type fakeAPI struct {
	mu        sync.Mutex
	started   []string // session_url of each started event ("" for the first one)
	finished  []int
	renewals  int
	renewErr  error
	finishErr []error // returned by successive finished calls, then nil
}

func (f *fakeAPI) RenewSessionToken(context.Context, int64) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewals++
	if f.renewErr != nil {
		return time.Time{}, f.renewErr
	}
	return time.Now().Add(time.Hour), nil
}

func (f *fakeAPI) SessionStarted(_ context.Context, _ int64, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, url)
	return nil
}

func (f *fakeAPI) SessionFinished(_ context.Context, _ int64, code int, _ *machine.Usage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.finishErr) > 0 {
		err := f.finishErr[0]
		f.finishErr = f.finishErr[1:]
		return err
	}
	f.finished = append(f.finished, code)
	return nil
}

func (f *fakeAPI) snapshot() ([]string, []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...), append([]int(nil), f.finished...)
}

// fakeCLI is a shell script standing in for an agent CLI. It records what it got into $FAKE_OUT/<name>/.
const fakeCLI = `#!/bin/sh
out="$FAKE_OUT/$(basename "$0")"
mkdir -p "$out"
printf '%s\n' "$@" > "$out/args"
pwd -P > "$out/pwd"
printf '%s' "$COLLIE_SESSION_TOKEN" > "$out/token"
printf '%s' "$COLLIE_MCP_URL" > "$out/mcp_url"
git rev-parse --abbrev-ref HEAD > "$out/branch" 2>/dev/null
echo "starting"
echo "Remote Control: https://claude.ai/code/session_01TESTabc-_9 (open it in the app)"
if [ -n "$FAKE_SLEEP" ]; then sleep "$FAKE_SLEEP"; fi
exit ${FAKE_EXIT:-0}
`

// env builds a repo with one commit, fake claude and codex in PATH, and a manager.
func env(t *testing.T) (*Manager, *fakeAPI, string, string) {
	t.Helper()
	base, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(base, "repo")
	bin := filepath.Join(base, "bin")
	out := filepath.Join(base, "out")
	for _, d := range []string{repo, bin, out} {
		_ = os.MkdirAll(d, 0o755)
	}
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(fakeCLI), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_OUT", out)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "first"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	api := &fakeAPI{}
	m := &Manager{API: api, Dir: filepath.Join(base, "sessions"), MaxConcurrency: 2, KillGrace: 200 * time.Millisecond, RetryWait: time.Millisecond}
	return m, api, repo, out
}

func session(id int64, agent, repo, mode string) dispatch.Session {
	return dispatch.Session{
		ID:    id,
		Agent: agent,
		Dispatch: machine.Dispatch{Type: "dispatch", DispatchID: id, SessionID: id, Kind: "work_goal", Purpose: "code",
			Reference: "148", Payload: json.RawMessage(`"collie work_goal #148 in collie"`), Project: "collies", Codebase: "collie"},
		Binding: bindings.Binding{Binding: machine.Binding{RunnerID: 1, Folder: repo, PermissionMode: mode, MaxConcurrency: 2,
			Agents: map[string]string{"code": agent}}},
		Accepted: machine.Accepted{SessionID: id, SessionToken: "cms_secret", MCPURL: "https://collie.test/p/collies/mcp", ExpiresAt: time.Now().Add(time.Hour)},
	}
}

// run starts a session and waits for done.
func run(t *testing.T, m *Manager, s dispatch.Session) {
	t.Helper()
	done := make(chan struct{})
	if err := m.Start(context.Background(), s, func() { close(done) }); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the session did not finish")
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

func worktrees(t *testing.T, repo string) string {
	out, _ := exec.Command("git", "-C", repo, "worktree", "list").CombinedOutput()
	return string(out)
}

func TestClaudeSessionRunsInItsWorktreeAndCleansUp(t *testing.T) {
	m, api, repo, out := env(t)
	run(t, m, session(42, "claude", repo, "auto"))

	wt := filepath.Join(m.Dir, "42", "worktree")
	if got := read(t, filepath.Join(out, "claude", "pwd")); got != wt {
		t.Errorf("ran in %s, want the session worktree %s", got, wt)
	}
	if got := read(t, filepath.Join(out, "claude", "branch")); got != "collie/session-42" {
		t.Errorf("branch = %s", got)
	}
	if got := read(t, filepath.Join(out, "claude", "token")); got != "cms_secret" {
		t.Errorf("token env = %q", got)
	}
	if got := read(t, filepath.Join(out, "claude", "mcp_url")); got != "https://collie.test/p/collies/mcp" {
		t.Errorf("mcp url env = %q", got)
	}
	args := strings.Split(read(t, filepath.Join(out, "claude", "args")), "\n")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--remote-control collie collies #42", "--permission-mode acceptEdits", "--strict-mcp-config", "-p "} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude args lack %q: %q", want, args)
		}
	}
	if !strings.Contains(joined, "<routine-fire-payload>") || !strings.Contains(joined, "collie work_goal #148 in collie") {
		t.Errorf("the prompt does not carry the job reference: %q", joined)
	}
	if strings.Contains(joined, "cms_secret") {
		t.Error("the session token is on the command line")
	}

	started, finished := api.snapshot()
	if len(started) != 2 || started[0] != "" || started[1] != "https://claude.ai/code/session_01TESTabc-_9" {
		t.Errorf("started events = %q, want an immediate one and one with the claude.ai link", started)
	}
	if len(finished) != 1 || finished[0] != 0 {
		t.Errorf("finished = %v", finished)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "42")); !os.IsNotExist(err) {
		t.Error("the session directory was not removed")
	}
	if strings.Contains(worktrees(t, repo), "session-42") || strings.Contains(worktrees(t, repo), wt) {
		t.Errorf("worktree still registered:\n%s", worktrees(t, repo))
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", "collie/session-42").Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Error("the session branch was not deleted")
	}
}

func TestTheMCPConfigReferencesTheTokenWithoutHoldingIt(t *testing.T) {
	dir := t.TempDir()
	cmd, err := Claude{}.Command(Launch{Name: "n", Worktree: dir, Prompt: "p", MCPURL: "https://collie.test/p/x/mcp", PermissionMode: "full", TokenEnv: TokenEnv, SessionDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--permission-mode bypassPermissions") {
		t.Errorf("full mode: %q", cmd.Args)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	c := cfg.MCPServers["collie"]
	if len(cfg.MCPServers) != 1 || c.Type != "http" || c.URL != "https://collie.test/p/x/mcp" || c.Headers["Authorization"] != "Bearer ${COLLIE_SESSION_TOKEN}" {
		t.Errorf("mcp config = %s", b)
	}
	info, _ := os.Stat(filepath.Join(dir, "mcp.json"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mcp.json mode = %o", info.Mode().Perm())
	}
}

func TestCodexSessionNeverGoesThroughClaude(t *testing.T) {
	m, api, repo, out := env(t)
	run(t, m, session(7, "codex", repo, "full"))
	if _, err := os.Stat(filepath.Join(out, "claude")); !os.IsNotExist(err) {
		t.Error("claude was invoked for a codex session")
	}
	args := strings.Join(strings.Split(read(t, filepath.Join(out, "codex", "args")), "\n"), " ")
	for _, want := range []string{"exec", "--sandbox danger-full-access", `mcp_servers.collie.url="https://collie.test/p/collies/mcp"`, `mcp_servers.collie.bearer_token_env_var="COLLIE_SESSION_TOKEN"`, "--cd " + filepath.Join(m.Dir, "7", "worktree")} {
		if !strings.Contains(args, want) {
			t.Errorf("codex args lack %q: %s", want, args)
		}
	}
	if strings.Contains(args, "cms_secret") {
		t.Error("the session token is on the command line")
	}
	started, finished := api.snapshot()
	if len(started) != 1 || started[0] != "" {
		t.Errorf("codex has no session view: started = %q", started)
	}
	if len(finished) != 1 || finished[0] != 0 {
		t.Errorf("finished = %v", finished)
	}
	if c, _ := (Codex{}).Command(Launch{PermissionMode: "auto", TokenEnv: TokenEnv}); !strings.Contains(strings.Join(c.Args, " "), "--sandbox workspace-write") {
		t.Errorf("auto mode: %q", c.Args)
	}
}

func TestACrashIsReportedAndCleanedUp(t *testing.T) {
	m, api, repo, _ := env(t)
	t.Setenv("FAKE_EXIT", "3")
	api.finishErr = []error{machine.ErrRetry, errors.New("connection reset")} // collie asks to send it again, then a blip
	run(t, m, session(9, "claude", repo, "auto"))
	_, finished := api.snapshot()
	if len(finished) != 1 || finished[0] != 3 {
		t.Errorf("finished = %v, want exit code 3 after retries", finished)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "9")); !os.IsNotExist(err) {
		t.Error("the crashed session's directory is still there")
	}
	if strings.Contains(worktrees(t, repo), "session") {
		t.Errorf("worktree still registered:\n%s", worktrees(t, repo))
	}
}

func TestAWorktreeFailureStillReportsFinished(t *testing.T) {
	m, api, _, _ := env(t)
	run(t, m, session(5, "claude", filepath.Join(t.TempDir(), "not-a-repo"), "auto"))
	started, finished := api.snapshot()
	if len(started) != 0 || len(finished) != 1 || finished[0] != -1 {
		t.Errorf("started %q finished %v, want only finished -1", started, finished)
	}
}

func TestStoppingStopsTheAgentAndCleansUp(t *testing.T) {
	m, api, repo, _ := env(t)
	t.Setenv("FAKE_SLEEP", "30")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	if err := m.Start(ctx, session(11, "claude", repo, "auto"), func() { close(done) }); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the agent to start", func() bool { s, _ := api.snapshot(); return len(s) > 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent was not stopped")
	}
	_, finished := api.snapshot()
	if len(finished) != 1 || finished[0] != -1 {
		t.Errorf("finished = %v, want -1 for a stopped agent", finished)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "11")); !os.IsNotExist(err) {
		t.Error("the stopped session's directory is still there")
	}
}

func TestADeadTokenStopsTheAgent(t *testing.T) {
	m, api, repo, _ := env(t)
	t.Setenv("FAKE_SLEEP", "30")
	api.renewErr = machine.ErrGone
	s := session(12, "claude", repo, "auto")
	s.Accepted.ExpiresAt = time.Now() // due for renewal right away
	m.RenewBefore = time.Hour
	run(t, m, s)
	_, finished := api.snapshot()
	if len(finished) != 1 || finished[0] != -1 || api.renewals == 0 {
		t.Errorf("finished = %v after %d renewals", finished, api.renewals)
	}
}

func TestConcurrencyIsCappedAtMaxConcurrency(t *testing.T) {
	m, api, repo, _ := env(t)
	m.MaxConcurrency = 2
	t.Setenv("FAKE_SLEEP", "30")
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); m.Wait() }()
	for _, id := range []int64{1, 2} {
		if err := m.Start(ctx, session(id, "claude", repo, "auto"), nil); err != nil {
			t.Fatalf("session %d: %v", id, err)
		}
	}
	if err := m.Start(ctx, session(3, "claude", repo, "auto"), nil); err == nil || !strings.Contains(err.Error(), "at capacity") {
		t.Errorf("third session: %v, want at capacity", err)
	}
	waitUntil(t, "both agents to start", func() bool { s, _ := api.snapshot(); return len(s) >= 2 })
}

func TestTheDispatcherCapsSessionsAtMaxConcurrency(t *testing.T) {
	// End to end with the dispatcher: offers beyond the machine's max_concurrency are declined, not started.
	m, _, repo, _ := env(t)
	t.Setenv("FAKE_SLEEP", "30")
	acks := &ackRecorder{}
	d := &dispatch.Dispatcher{API: acks, Sessions: m, MaxConcurrency: 2}
	b := session(0, "claude", repo, "auto").Binding
	b.MaxConcurrency = 5
	d.SetBindings([]bindings.Binding{b}, []string{filepath.Dir(repo)})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); m.Wait() }()
	for id := int64(1); id <= 4; id++ {
		o := session(id, "claude", repo, "auto").Dispatch
		o.RunnerID = 1
		if err := d.Handle(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	accepted := 0
	for _, a := range acks.acks {
		if a {
			accepted++
		}
	}
	if accepted != 2 || len(d.Running()) != 2 {
		t.Errorf("accepted %d, running %v; want 2 with max_concurrency 2", accepted, d.Running())
	}
}

type ackRecorder struct {
	mu   sync.Mutex
	acks []bool
}

func (a *ackRecorder) Ack(_ context.Context, id int64, accept bool, _ string) (*machine.Accepted, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acks = append(a.acks, accept)
	if !accept {
		return nil, nil
	}
	return &machine.Accepted{SessionID: id, SessionToken: "cms_x", MCPURL: "https://collie.test/p/collies/mcp", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestStaleWorktreesAreCleanedAtStartup(t *testing.T) {
	m, _, repo, _ := env(t)
	// What a kill -9 leaves: a session directory with a registered worktree and its branch.
	stale := filepath.Join(m.Dir, "77")
	if err := addWorktree(context.Background(), repo, filepath.Join(stale, "worktree"), "collie/session-77"); err != nil {
		t.Fatal(err)
	}
	// And a directory with no metadata at all.
	_ = os.MkdirAll(filepath.Join(m.Dir, "78"), 0o700)
	if !strings.Contains(worktrees(t, repo), "77") {
		t.Fatal("setup: no worktree")
	}
	m.Sweep(context.Background())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale session directory is still there")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "78")); !os.IsNotExist(err) {
		t.Error("the directory without metadata is still there")
	}
	if strings.Contains(worktrees(t, repo), "77") {
		t.Errorf("worktree still registered:\n%s", worktrees(t, repo))
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", "collie/session-77").Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Error("the stale branch was not deleted")
	}
}

func TestCanRun(t *testing.T) {
	m, _, _, _ := env(t)
	if err := m.CanRun("claude"); err != nil {
		t.Errorf("claude: %v", err)
	}
	if err := m.CanRun("gemini"); err == nil {
		t.Error("an agent without an adapter is runnable")
	}
	m.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if err := m.CanRun("codex"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("missing CLI: %v", err)
	}
}

func TestPromptsPerPurpose(t *testing.T) {
	cases := map[string]string{
		"code":       "You are collie's worker for this repo.",
		"research":   "You are collie's worker for this repo.",
		"qa-browser": "You are collie's QA agent",
		"reviewer":   "You are collie's code reviewer",
	}
	for purpose, start := range cases {
		p := PromptFor(purpose, "collie review #230")
		if !strings.HasPrefix(p, start) || !strings.HasSuffix(p, "<routine-fire-payload>\ncollie review #230\n</routine-fire-payload>\n") {
			t.Errorf("%s prompt:\n%s", purpose, p)
		}
	}
	if got := payloadText(machine.Dispatch{Kind: "verify", Reference: "212"}); got != "collie verify #212" {
		t.Errorf("rebuilt payload = %q", got)
	}
	if got := payloadText(machine.Dispatch{Kind: "work_goal", Reference: "31", Codebase: "api"}); got != "collie work_goal #31 in api" {
		t.Errorf("rebuilt payload = %q", got)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPermissionModes(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ mode, claude, codex string }{
		{"auto", "acceptEdits", "workspace-write"},
		{"full", "bypassPermissions", "danger-full-access"},
		{"plan", "plan", "read-only"},
		{"", "acceptEdits", "workspace-write"},
		{"weird", "acceptEdits", "workspace-write"},
	}
	for _, c := range cases {
		l := Launch{Name: "s", Worktree: dir, Prompt: "p", MCPURL: "https://x/mcp", PermissionMode: c.mode, TokenEnv: TokenEnv, SessionDir: dir}
		cl, err := Claude{}.Command(l)
		if err != nil {
			t.Fatal(err)
		}
		if got := argAfter(cl.Args, "--permission-mode"); got != c.claude {
			t.Errorf("claude %q: --permission-mode %q, want %q", c.mode, got, c.claude)
		}
		cx, err := Codex{}.Command(l)
		if err != nil {
			t.Fatal(err)
		}
		if got := argAfter(cx.Args, "--sandbox"); got != c.codex {
			t.Errorf("codex %q: --sandbox %q, want %q", c.mode, got, c.codex)
		}
	}
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
