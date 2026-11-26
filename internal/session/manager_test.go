package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	startErr  error
	startHold chan struct{} // when set, started events wait for it to close
	finishErr []error       // returned by successive finished calls, then nil
	usage     []*machine.Usage
	reviews   []json.RawMessage
	// resuming
	reissued     []int64
	reissueErr   error
	suspended    []int64
	resumed      []string // session_url of each resumed event
	resumeFailed []string // reason of each resume_failed event
}

func (f *fakeAPI) ReissueSessionToken(_ context.Context, id int64) (*machine.Reissued, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reissued = append(f.reissued, id)
	if f.reissueErr != nil {
		return nil, f.reissueErr
	}
	return &machine.Reissued{SessionID: id, Token: "cms_new", ExpiresAt: time.Now().Add(time.Hour), MCPURL: "https://collie.test/p/collies/mcp2"}, nil
}

func (f *fakeAPI) SessionSuspended(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspended = append(f.suspended, id)
	return nil
}

func (f *fakeAPI) SessionResumed(_ context.Context, _ int64, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, url)
	return nil
}

func (f *fakeAPI) SessionResumeFailed(_ context.Context, _ int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeFailed = append(f.resumeFailed, reason)
	return nil
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
	if f.startHold != nil {
		<-f.startHold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, url)
	return nil
}

func (f *fakeAPI) SessionFinished(_ context.Context, _ int64, code int, usage *machine.Usage, review json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.finishErr) > 0 {
		err := f.finishErr[0]
		f.finishErr = f.finishErr[1:]
		return err
	}
	f.finished = append(f.finished, code)
	f.usage = append(f.usage, usage)
	f.reviews = append(f.reviews, review)
	return nil
}

func (f *fakeAPI) snapshot() ([]string, []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...), append([]int(nil), f.finished...)
}

// fakeCLI is a shell script standing in for an agent CLI. It records what it got into $FAKE_OUT/<name>/. As claude it
// insists on a terminal, asks the trust question (as Claude Code 2.1 does: "No, exit" selected, spaces drawn as cursor
// moves, and Enter alone exits; FAKE_TRUST=old asks it as older versions did, "Yes, proceed" selected), touches its hooks' files and shows the Remote Control link as a
// terminal hyperlink; with FAKE_IDLE it then goes idle and waits to be told to /exit. As codex it prints --json events
// with usage.
const fakeCLI = `#!/bin/sh
name=$(basename "$0")
out="$FAKE_OUT/$name"
mkdir -p "$out"
printf '%s\n' "$@" > "$out/args"
pwd -P > "$out/pwd"
printf '%s' "$COLLIE_SESSION_TOKEN" > "$out/token"
printf '%s' "$COLLIE_MCP_URL" > "$out/mcp_url"
git rev-parse --abbrev-ref HEAD > "$out/branch" 2>/dev/null
git log --format=%s > "$out/log" 2>/dev/null
if [ "$name" = claude ]; then
  if [ ! -t 0 ] || [ ! -t 1 ]; then echo "claude: Remote Control needs a terminal" >&2; exit 9; fi
  sdir=""
  prev=""
  for a in "$@"; do
    if [ "$prev" = "--settings" ]; then sdir=$(dirname "$a"); fi
    if [ "$prev" = "--session-id" ] && [ -n "$CLAUDE_CONFIG_DIR" ]; then
      mkdir -p "$CLAUDE_CONFIG_DIR/projects/p"; echo '{}' > "$CLAUDE_CONFIG_DIR/projects/p/$a.jsonl"
    fi
    prev="$a"
  done
  if [ "$FAKE_TRUST" = old ]; then
    printf '│ Do you trust the files in this folder? │\n│ ❯ 1. Yes, proceed │\n│   2. No, exit │\n'
    printf '│ Enter to confirm · Esc to exit │\n'
    read -r answer
    if [ -n "$answer" ]; then echo "refused: $answer" > "$out/trusted"; exit 7; fi
  else
    printf ' Quick\033[1Csafety\033[1Ccheck:\033[1CIs\033[1Cthis\033[1Ca\033[1Cproject\033[1Cyou\033[1Ccreated\033[1Cor\033[1Cone\033[1Cyou\033[1Ctrust?\033[1C(Like\033[1Cyour\r\n'
    printf ' own code, a well-known open source project, or work from your team).\r\n\r\n'
    printf ' \033[1m❯\033[1CNo,\033[1Cexit\033[0m\r\n'
    printf '   Yes,\033[1CI\033[1Ctrust\033[1Cthis\033[1Cfolder\r\n\r\n'
    printf ' Enter\033[1Cto\033[1Cconfirm\033[1C·\033[1CEsc\033[1Cto\033[1Ccancel\r\n'
    read -r answer
    if [ "$answer" != "$(printf '\033[B')" ]; then echo "refused: $answer" > "$out/trusted"; exit 7; fi
  fi
  echo trusted > "$out/trusted"
  printf '%s\n%s' "$COLLIE_REVIEW_FILE" "$COLLIE_REVIEW_SCHEMA" > "$out/review_env"
  if [ -n "$FAKE_REVIEW" ] && [ -n "$COLLIE_REVIEW_FILE" ]; then printf '%s' "$FAKE_REVIEW" > "$COLLIE_REVIEW_FILE"; fi
  if [ -n "$FAKE_ORPHAN" ]; then nohup sleep 60 >/dev/null 2>&1 & echo $! > "$out/orphan"; fi
  touch "$sdir/activity"
  printf 'Remote Control \033]8;;https://claude.ai/code/session_01TESTabc-_9\033\\/rc active\033]8;;\033\\\n'
  if [ -n "$FAKE_IDLE" ]; then
    if [ -n "$FAKE_LATE_TRUST" ]; then
      echo "The model wrote: Do you trust the files in this folder?"
      echo "1. Yes, proceed"
    fi
    touch "$sdir/idle"
    read -r cmd
    echo "$cmd" > "$out/typed"
    if [ "$cmd" = "/exit" ]; then exit 0; fi
    exit 4
  fi
else
  outfile=""
  prev=""
  for a in "$@"; do
    if [ "$prev" = "-o" ]; then outfile="$a"; fi
    prev="$a"
  done
  if [ -n "$FAKE_REVIEW" ] && [ -n "$outfile" ]; then printf '%s' "$FAKE_REVIEW" > "$outfile"; fi
  if [ -n "$CODEX_HOME" ] && [ "$2" != resume ]; then
    mkdir -p "$CODEX_HOME/sessions/2026/11/26"; echo '{}' > "$CODEX_HOME/sessions/2026/11/26/rollout-2026-11-26T10-00-00-t1.jsonl"
  fi
  echo '{"type":"thread.started","thread_id":"t1"}'
  echo '{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":200,"output_tokens":300}}'
  echo 'not json'
  if [ -n "$FAKE_HUGE" ]; then
    printf '{"type":"item.completed","text":"'; head -c 1200000 /dev/zero | tr '\0' x; printf '"}\n'
    echo '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}'
  fi
  echo '{"type":"turn.completed","usage":{"input_tokens":800,"cached_input_tokens":100,"output_tokens":50}}'
fi
if [ -n "$FAKE_BIG" ]; then head -c 300000 /dev/zero | tr '\0' x; echo; touch "$out/wrote"; fi
if [ -n "$FAKE_SLEEP" ]; then sleep "$FAKE_SLEEP"; fi
exit ${FAKE_EXIT:-0}
`

// gitRun runs git in dir and fails the test on an error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// env builds a repo cloned from an origin whose default branch is main (one commit, "first"), fake claude and codex
// in PATH, and a manager.
func env(t *testing.T) (*Manager, *fakeAPI, string, string) {
	t.Helper()
	base, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(base, "repo")
	origin := filepath.Join(base, "origin.git")
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
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(base, "claude-config"))
	t.Setenv("CODEX_HOME", filepath.Join(base, "codex-home"))
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	gitRun(t, base, "init", "-q", "--bare", "-b", "main", origin)
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	gitRun(t, repo, "remote", "add", "origin", origin)
	gitRun(t, repo, "push", "-q", "origin", "main")
	gitRun(t, repo, "remote", "set-head", "origin", "main")
	api := &fakeAPI{}
	m := &Manager{API: api, Dir: filepath.Join(base, "sessions"), MaxConcurrency: 2, KillGrace: 200 * time.Millisecond, RetryWait: time.Millisecond,
		// The fake reads whole lines: no need to wait for it to draw, and the idle tests' timeouts are short.
		AnswerSettle: 10 * time.Millisecond, AnswerKeyGap: 10 * time.Millisecond}
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

	wt := WorktreePath(repo, 42)
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
	for _, want := range []string{"--remote-control collie collies #42", "--permission-mode acceptEdits", "--strict-mcp-config", "--settings " + filepath.Join(m.Dir, "42", "settings.json")} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude args lack %q: %q", want, args)
		}
	}
	for _, a := range args {
		if a == "-p" || a == "--print" {
			t.Errorf("claude runs in print mode, where Remote Control does not work: %q", args)
		}
	}
	// The args file has one line per argument line: the prompt (several lines) comes last, right after --settings.
	if i := strings.Index(joined, filepath.Join(m.Dir, "42", "settings.json")+" You are collie's worker"); i < 0 || !strings.HasSuffix(joined, "</routine-fire-payload>") {
		t.Errorf("the prompt is not the session's opening message, last: %q", joined)
	}
	if read(t, filepath.Join(out, "claude", "trusted")) != "trusted" {
		t.Error("the trust dialog was not answered")
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
	for _, want := range []string{"exec", "--sandbox danger-full-access", `mcp_servers.collie.url="https://collie.test/p/collies/mcp"`, `mcp_servers.collie.bearer_token_env_var="COLLIE_SESSION_TOKEN"`, "--cd " + WorktreePath(repo, 7)} {
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
	// A session that cannot be resumed (the agent kept no transcript) ends on a stop. One that can is suspended
	// (TestAGracefulShutdownSuspendsTheSession).
	m, api, repo, _ := env(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
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

func TestReviewsAreNotCappedAtMaxConcurrency(t *testing.T) {
	// The machine's slots full of builds (often the ones waiting for their review) must not keep a review from
	// starting: the dispatcher acks it outside the machine-wide limit, and the manager must start it the same way.
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
	review := session(3, "claude", repo, "auto")
	review.Dispatch.Kind, review.Dispatch.Purpose = "review", "reviewer"
	review.Binding.Agents = map[string]string{"reviewer": "claude"}
	if err := m.Start(ctx, review, nil); err != nil {
		t.Fatalf("review with the machine full of builds: %v", err)
	}
	if err := m.Start(ctx, session(4, "claude", repo, "auto"), nil); err == nil || !strings.Contains(err.Error(), "at capacity") {
		t.Errorf("third build: %v, want at capacity (the review does not free a build slot either)", err)
	}
	waitUntil(t, "the three agents to start", func() bool { s, _ := api.snapshot(); return len(s) >= 3 })
}

func TestASessionThatCannotStartIsReportedFinished(t *testing.T) {
	// The dispatch was accepted before Start: collie counts the session as running until it hears it finished.
	m, api, repo, _ := env(t)
	m.MaxConcurrency = 1
	t.Setenv("FAKE_SLEEP", "30")
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); m.Wait() }()
	if err := m.Start(ctx, session(1, "claude", repo, "auto"), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx, session(2, "claude", repo, "auto"), nil); err == nil {
		t.Fatal("second session started past max_concurrency 1")
	}
	if err := m.Start(ctx, session(1, "claude", repo, "auto"), nil); err == nil {
		t.Fatal("session 1 started twice")
	}
	waitUntil(t, "session 2 to be reported finished", func() bool { _, f := api.snapshot(); return len(f) == 1 })
	cancel()
	m.Wait()
	_, finished := api.snapshot()
	if len(finished) != 2 || finished[0] != -1 {
		t.Errorf("finished = %v; want session 2 (-1) when it could not start, then session 1 when cancelled, and no extra finish for the duplicate Start", finished)
	}
}

func TestTheDispatcherCapsSessionsAtMaxConcurrency(t *testing.T) {
	// End to end with the dispatcher: offers beyond the machine's max_concurrency are declined, not started.
	m, _, repo, _ := env(t)
	t.Setenv("FAKE_SLEEP", "30")
	acks := &ackRecorder{}
	d := &dispatch.Dispatcher{API: acks, Sessions: m, MaxConcurrency: 2}
	b := session(0, "claude", repo, "auto").Binding
	b.MaxConcurrency = 5
	d.SetBindings([]bindings.Binding{b}, bindings.Allowed{Roots: []string{filepath.Dir(repo)}})
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
	if err := addWorktree(context.Background(), repo, stale, WorktreePath(repo, 77), "collie/session-77"); err != nil {
		t.Fatal(err)
	}
	// And a directory with no metadata at all.
	_ = os.MkdirAll(filepath.Join(m.Dir, "78"), 0o700)
	if !strings.Contains(worktrees(t, repo), WorktreePath(repo, 77)) {
		t.Fatal("setup: no worktree")
	}
	m.Sweep(context.Background())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale session directory is still there")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "78")); !os.IsNotExist(err) {
		t.Error("the directory without metadata is still there")
	}
	if strings.Contains(worktrees(t, repo), WorktreePath(repo, 77)) {
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

// R1-1: a session never deletes a branch it did not create.
func TestABranchThatAlreadyExistsIsNeverDeleted(t *testing.T) {
	m, api, repo, out := env(t)
	gitRun(t, repo, "branch", "collie/session-21")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "unmerged work") // on main: the branch keeps its own tip
	tip := gitRun(t, repo, "rev-parse", "collie/session-21")
	run(t, m, session(21, "claude", repo, "auto"))
	if got := gitRun(t, repo, "rev-parse", "collie/session-21"); got != tip {
		t.Errorf("the existing branch moved or was deleted: %s, want %s", got, tip)
	}
	if _, err := os.Stat(filepath.Join(out, "claude")); !os.IsNotExist(err) {
		t.Error("the agent ran on a branch the session did not create")
	}
	started, finished := api.snapshot()
	if len(started) != 0 || len(finished) != 1 || finished[0] != -1 {
		t.Errorf("started %q finished %v, want only finished -1", started, finished)
	}
}

// R1-4: the session starts from the remote's default branch, never from the checkout the repo is on.
func TestASessionStartsFromTheDefaultBranchNotTheCheckout(t *testing.T) {
	for _, unsetHead := range []bool{false, true} {
		m, _, repo, out := env(t)
		gitRun(t, repo, "checkout", "-q", "-b", "feature")
		gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "feature only")
		if unsetHead { // origin/HEAD unknown locally: ask origin
			gitRun(t, repo, "remote", "set-head", "origin", "--delete")
		}
		run(t, m, session(31, "claude", repo, "auto"))
		if got := read(t, filepath.Join(out, "claude", "log")); got != "first" {
			t.Errorf("unset origin/HEAD %v: the session's history is %q, want only main's", unsetHead, got)
		}
	}
}

func TestNoDefaultBranchFailsClearly(t *testing.T) {
	m, api, repo, out := env(t)
	gitRun(t, repo, "remote", "remove", "origin")
	run(t, m, session(32, "claude", repo, "auto"))
	if _, err := os.Stat(filepath.Join(out, "claude")); !os.IsNotExist(err) {
		t.Error("the agent ran without a default branch to start from")
	}
	if _, finished := api.snapshot(); len(finished) != 1 || finished[0] != -1 {
		t.Errorf("finished = %v", finished)
	}
	if err := addWorktree(context.Background(), repo, t.TempDir(), WorktreePath(repo, 1), "collie/x"); err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Errorf("addWorktree = %v, want a clear default-branch error", err)
	}
}

// R1-3: a 401 on a session endpoint stops the runner, not only the session; a 409 stays local.
func TestA401OnASessionEndpointStopsTheRunner(t *testing.T) {
	cases := map[string]func(*fakeAPI){
		"renewal":         func(a *fakeAPI) { a.renewErr = fmt.Errorf("%w: 401", machine.ErrRevoked) },
		"lifecycle event": func(a *fakeAPI) { a.startErr = fmt.Errorf("%w: 401", machine.ErrRevoked) },
	}
	for name, setup := range cases {
		m, api, repo, _ := env(t)
		setup(api)
		t.Setenv("FAKE_SLEEP", "30")
		ctx, cancel := context.WithCancel(context.Background())
		var got atomic.Value
		m.OnRevoked(func(err error) { got.Store(err); cancel() }) // what the runner does: stop everything
		s := session(41, "claude", repo, "auto")
		s.Accepted.ExpiresAt = time.Now()
		m.RenewBefore = time.Hour
		done := make(chan struct{})
		if err := m.Start(ctx, s, func() { close(done) }); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("%s: the revocation did not stop the session", name)
		}
		if err, _ := got.Load().(error); !errors.Is(err, machine.ErrRevoked) {
			t.Errorf("%s: OnRevoked got %v", name, err)
		}
		cancel()
	}

	m, api, repo, _ := env(t)
	api.renewErr = machine.ErrGone
	revoked := false
	m.OnRevoked(func(error) { revoked = true })
	s := session(42, "claude", repo, "auto")
	s.Accepted.ExpiresAt = time.Now()
	m.RenewBefore = time.Hour
	t.Setenv("FAKE_SLEEP", "30")
	run(t, m, s)
	if revoked {
		t.Error("a session that is gone (409) stopped the runner")
	}
}

// R1-5: the usage codex reports reaches the finished event.
func TestCodexUsageIsReported(t *testing.T) {
	m, api, repo, _ := env(t)
	run(t, m, session(51, "codex", repo, "auto"))
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.usage) != 1 || api.usage[0] == nil {
		t.Fatalf("usage = %v", api.usage)
	}
	u := *api.usage[0]
	if u.InputTokens != 1700 || u.CacheReadInputTokens != 300 || u.OutputTokens != 350 {
		t.Errorf("usage = %+v, want input 1700, cache read 300, output 350", u)
	}
}

func TestClaudeReportsNoUsageOfItsOwn(t *testing.T) {
	m, api, repo, _ := env(t)
	run(t, m, session(52, "claude", repo, "auto"))
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.usage) != 1 || api.usage[0] != nil {
		t.Errorf("usage = %v: Claude reports its usage to collie itself", api.usage)
	}
}

// R1-6: a slow collie does not hold up the agent's output, its supervision or its shutdown.
func TestASlowCollieDoesNotHoldUpTheAgent(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		m, api, repo, out := env(t)
		api.startHold = make(chan struct{})
		t.Setenv("FAKE_BIG", "1") // more output than any pipe or terminal buffer holds
		done := make(chan struct{})
		if err := m.Start(context.Background(), session(61, agent, repo, "auto"), func() { close(done) }); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, agent+" to write all its output while collie is slow", func() bool {
			_, err := os.Stat(filepath.Join(out, agent, "wrote"))
			return err == nil
		})
		close(api.startHold)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("%s: the session did not finish", agent)
		}
		started, finished := api.snapshot()
		if len(started) == 0 || len(finished) != 1 || finished[0] != 0 {
			t.Errorf("%s: started %q finished %v: the events must still arrive, in order", agent, started, finished)
		}
	}
}

func TestAnIdleClaudeSessionIsEnded(t *testing.T) {
	m, api, repo, out := env(t)
	t.Setenv("FAKE_IDLE", "1")
	m.IdleTimeout = time.Second // longer than the fake takes to draw its trust dialog
	m.IdlePoll = 20 * time.Millisecond
	run(t, m, session(71, "claude", repo, "auto"))
	if got := read(t, filepath.Join(out, "claude", "typed")); got != "/exit" {
		t.Errorf("typed %q, want /exit", got)
	}
	if _, finished := api.snapshot(); len(finished) != 1 || finished[0] != 0 {
		t.Errorf("finished = %v, want 0 for an agent that exited when told", finished)
	}
}

func TestAWorkingClaudeSessionIsNotEnded(t *testing.T) {
	m, api, repo, _ := env(t)
	t.Setenv("FAKE_SLEEP", "1") // active (the activity file is newer than any idle one) the whole time
	m.IdleTimeout = time.Second // longer than the fake takes to draw its trust dialog
	m.IdlePoll = 20 * time.Millisecond
	run(t, m, session(72, "claude", repo, "auto"))
	if _, finished := api.snapshot(); len(finished) != 1 || finished[0] != 0 {
		t.Errorf("finished = %v, want the agent's own exit", finished)
	}
}

func TestClaudeHooksTouchTheIdleFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := (Claude{}).Command(Launch{Name: "n", Worktree: dir, Prompt: "p", MCPURL: "https://x/mcp", TokenEnv: TokenEnv, SessionDir: dir}); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PreToolUse": ActivityFile, "PostToolUse": ActivityFile, "UserPromptSubmit": ActivityFile, "Stop": IdleFile, "Notification": IdleFile}
	if len(cfg.Hooks) != len(want) {
		t.Errorf("hooks = %v", cfg.Hooks)
	}
	for event, file := range want {
		h := cfg.Hooks[event]
		if len(h) != 1 || len(h[0].Hooks) != 1 || h[0].Hooks[0].Command != "touch "+shellQuote(filepath.Join(dir, file)) {
			t.Errorf("%s hook = %+v", event, h)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "settings.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("settings.json mode %o", fi.Mode().Perm())
	}
}

// R2: dialogs are answered only while the agent starts up; the same words later get no input.
func TestATrustLineAfterStartupGetsNoInput(t *testing.T) {
	m, _, repo, out := env(t)
	t.Setenv("FAKE_IDLE", "1")
	t.Setenv("FAKE_LATE_TRUST", "1")
	m.IdleTimeout = time.Second // longer than the fake takes to draw its trust dialog
	m.IdlePoll = 20 * time.Millisecond
	run(t, m, session(81, "claude", repo, "auto"))
	if got := read(t, filepath.Join(out, "claude", "typed")); got != "/exit" {
		t.Errorf("the agent read %q first: something was typed for the late trust line", got)
	}
}

func TestOnlyTheTrustQuestionIsAnswered(t *testing.T) {
	for _, line := range []string{"1. Yes, proceed", "Do you want to proceed?", "trust this folder", "Yes, I trust this folder", "Bash command: rm -rf x — Yes, proceed"} {
		if claudeTrust.MatchString(line) {
			t.Errorf("%q is taken for the trust dialog", line)
		}
	}
	for _, line := range []string{
		"│ Do you trust the files in this folder? │",
		" Quick safety check: Is this a project you created or one you trust? (Like your",
		"Quicksafetycheck:Isthisaprojectyoucreatedoroneyoutrust?(Likeyour",
	} {
		if !claudeTrust.MatchString(line) {
			t.Errorf("the trust question %q is not recognized", line)
		}
	}
}

func TestTheTrustDialogIsAnsweredYes(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		keys  []string
		done  bool
	}{
		{"2.1: No selected above Yes", []string{" Quick safety check: Is this a project you created or one you trust? (Like your", " own code...).", "", " ❯ No, exit", "   Yes, I trust this folder", "", " Enter to confirm · Esc to cancel"}, []string{keyDown, keyEnter}, true},
		{"older: Yes selected", []string{"│ Do you trust the files in this folder? │", "│ ❯ 1. Yes, proceed │", "│   2. No, exit │", "│ Enter to confirm · Esc to exit │"}, []string{keyEnter}, true},
		{"No selected below Yes", []string{"Is this a project you created or one you trust?", "  1. Yes, I trust this folder", "  2. Something else, maybe", "❯ 3. No, exit", "Enter to confirm"}, []string{keyUp, keyEnter}, true},
		{"compacted by a terminal without spaces", []string{"Isthisaprojectyoucreatedoroneyoutrust?", "❯No,exit", "Yes,Itrustthisfolder", "Entertoconfirm·Esctocancel"}, []string{keyDown, keyEnter}, true},
		{"footer not drawn yet", []string{"Is this a project you created or one you trust?", "❯ No, exit"}, nil, false},
		{"no selection marker: left to a person", []string{"Is this a project you created or one you trust?", "No, exit", "Yes, I trust this folder", "Enter to confirm"}, nil, true},
		{"no Yes option: left to a person", []string{"Is this a project you created or one you trust?", "❯ No, exit", "Enter to confirm"}, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keys, done := claudeTrustKeys(c.lines)
			if done != c.done || strings.Join(keys, "|") != strings.Join(c.keys, "|") {
				t.Errorf("got %q, %v; want %q, %v", keys, done, c.keys, c.done)
			}
		})
	}
	long := []string{"Is this a project you created or one you trust?"}
	for range 45 {
		long = append(long, "something else")
	}
	if keys, done := claudeTrustKeys(long); !done || keys != nil {
		t.Errorf("a dialog with no footer in 40 lines got %q, %v", keys, done)
	}
}

func TestTheOlderTrustDialogIsAnsweredToo(t *testing.T) {
	m, _, repo, out := env(t)
	t.Setenv("FAKE_TRUST", "old")
	run(t, m, session(84, "claude", repo, "auto"))
	if got := read(t, filepath.Join(out, "claude", "trusted")); got != "trusted" {
		t.Errorf("the older trust dialog got %q", got)
	}
}

func TestStripEscapesKeepsTheSpacesCursorMovesDraw(t *testing.T) {
	if got := stripEscapes("\x1b[1mYes,\x1b[1CI\x1b[3Ctrust\x1b[Cit\x1b[0m"); got != "Yes, I   trust it" {
		t.Errorf("stripEscapes = %q", got)
	}
}

func TestSessionWorktreesLiveInTheRepoOutOfItsStatus(t *testing.T) {
	m, _, repo, out := env(t)
	t.Setenv("FAKE_IDLE", "1")
	m.IdleTimeout = time.Second
	m.IdlePoll = 20 * time.Millisecond
	done := make(chan struct{})
	if err := m.Start(context.Background(), session(85, "claude", repo, "auto"), func() { close(done) }); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the agent to start", func() bool { _, err := os.Stat(filepath.Join(out, "claude", "trusted")); return err == nil })
	wt := WorktreePath(repo, 85)
	if want := filepath.Join(repo, ".claude", "worktrees", "collie-session-85"); wt != want {
		t.Fatalf("WorktreePath = %s, want %s", wt, want)
	}
	if got := read(t, filepath.Join(out, "claude", "pwd")); got != wt {
		t.Errorf("ran in %s, want %s", got, wt)
	}
	if st := gitRun(t, repo, "status", "--porcelain"); st != "" {
		t.Errorf("the session worktree shows in the repo's status: %q", st)
	}
	<-done
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("the worktree is still there")
	}
	if strings.Count(read(t, filepath.Join(repo, ".git", "info", "exclude")), "/.claude/worktrees/") != 1 {
		t.Error("the worktrees folder is not excluded exactly once")
	}
	// A second session does not add it again.
	run(t, m, session(86, "claude", repo, "auto"))
	if strings.Count(read(t, filepath.Join(repo, ".git", "info", "exclude")), "/.claude/worktrees/") != 1 {
		t.Error("the worktrees folder was excluded twice")
	}
}

// Sessions started by a version that kept the worktree in the session directory are still swept.
func TestALegacySessionWorktreeIsSwept(t *testing.T) {
	m, _, repo, _ := env(t)
	stale := filepath.Join(m.Dir, "79")
	_ = os.MkdirAll(stale, 0o700)
	gitRun(t, repo, "worktree", "add", "-q", "-b", "collie/session-79", filepath.Join(stale, "worktree"), "main")
	if err := os.WriteFile(filepath.Join(stale, metaFile), []byte(`{"repo":"`+repo+`","branch":"collie/session-79"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Sweep(context.Background())
	if strings.Contains(worktrees(t, repo), "session-79") || gitRun(t, repo, "branch", "--list", "collie/session-79") != "" {
		t.Errorf("the legacy worktree was not cleaned up:\n%s", worktrees(t, repo))
	}
}

// Cleanup removes only a session worktree, whatever the metadata says.
func TestCleanupLeavesAForeignWorktreeAlone(t *testing.T) {
	m, _, repo, _ := env(t)
	other := filepath.Join(filepath.Dir(repo), "other")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "mine", other, "main")
	dir := filepath.Join(m.Dir, "80")
	_ = os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, metaFile), []byte(`{"repo":"`+repo+`","worktree":"`+other+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := removeWorktree(context.Background(), dir); err == nil {
		t.Error("no error for a worktree that is not a session's")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a foreign worktree was removed: %v", err)
	}
}

// R2: a record over the line limit is skipped and the scan goes on.
func TestAnOversizedRecordDoesNotLoseTheUsageAfterIt(t *testing.T) {
	m, api, repo, _ := env(t)
	t.Setenv("FAKE_HUGE", "1")
	run(t, m, session(82, "codex", repo, "auto"))
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.usage) != 1 || api.usage[0] == nil || api.usage[0].InputTokens != 1710 || api.usage[0].OutputTokens != 355 {
		t.Errorf("usage = %+v, want the turn after the oversized record counted", api.usage)
	}
}

func TestLineSplitter(t *testing.T) {
	in := "a\r\nb\n" + strings.Repeat("x", 100) + "\nc\n" + strings.Repeat("y", 100)
	sc := bufio.NewScanner(strings.NewReader(in))
	sc.Buffer(make([]byte, 16), 33)
	sc.Split(lineSplitter(32))
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a,,b,c" {
		t.Errorf("lines = %q", got)
	}
}

// R2: when git fails after it created the branch (a failing post-checkout hook), the session still owns and deletes
// it, and the same session can run again.
func TestABranchCreatedByAFailedWorktreeAddIsCleanedUp(t *testing.T) {
	m, api, repo, out := env(t)
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, m, session(83, "claude", repo, "auto"))
	if _, finished := api.snapshot(); len(finished) != 1 || finished[0] != -1 {
		t.Fatalf("finished = %v, want -1 for a failed worktree", finished)
	}
	if b := gitRun(t, repo, "branch", "--list", "collie/session-83"); b != "" {
		t.Errorf("the branch the failed add created is still there: %q", b)
	}
	_ = os.Remove(hook)
	run(t, m, session(83, "claude", repo, "auto"))
	if got := read(t, filepath.Join(out, "claude", "branch")); got != "collie/session-83" {
		t.Errorf("the retry ran on %q", got)
	}
}

// R3-1/R4-1: the session owns a branch from the moment git created it for the session, and only then: when the
// checkout fails after that, cleanup still deletes it, and the same session can run again.
func TestABranchIsOwnedFromItsCreation(t *testing.T) {
	m, _, repo, _ := env(t)
	sessionDir := filepath.Join(m.Dir, "91")
	wt := WorktreePath(repo, 91)
	// The checkout fails after the branch exists: the worktree path is taken by a file.
	_ = os.MkdirAll(filepath.Dir(wt), 0o755)
	if err := os.WriteFile(wt, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := addWorktree(context.Background(), repo, sessionDir, wt, "collie/session-91"); err == nil {
		t.Fatal("addWorktree succeeded on a taken path")
	}
	if _, err := removeWorktree(context.Background(), sessionDir); err != nil {
		t.Logf("removeWorktree: %v", err)
	}
	if b := gitRun(t, repo, "branch", "--list", "collie/session-91"); b != "" {
		t.Errorf("the branch the session created is orphaned: %q", b)
	}
	if read(t, wt) != "x" {
		t.Error("cleanup touched a file that is not a worktree")
	}
	_ = os.Remove(wt)
	if err := addWorktree(context.Background(), repo, sessionDir, wt, "collie/session-91"); err != nil {
		t.Errorf("the same session cannot run again: %v", err)
	}
}

// R3-2: a stop while git runs, after it created the branch, still leaves the branch owned and cleaned up.
func TestAStopAfterTheBranchIsCreatedDoesNotOrphanIt(t *testing.T) {
	m, _, repo, _ := env(t)
	marker := filepath.Join(t.TempDir(), "in-hook")
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sessionDir := filepath.Join(m.Dir, "92")
	errc := make(chan error, 1)
	go func() { errc <- addWorktree(ctx, repo, sessionDir, WorktreePath(repo, 92), "collie/session-92") }()
	waitUntil(t, "git to run the hook", func() bool { _, err := os.Stat(marker); return err == nil })
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("addWorktree succeeded although it was stopped")
	}
	if _, err := removeWorktree(context.Background(), sessionDir); err != nil {
		t.Logf("removeWorktree: %v", err)
	}
	if b := gitRun(t, repo, "branch", "--list", "collie/session-92"); b != "" {
		t.Errorf("the branch created before the stop is orphaned: %q", b)
	}
}

const claudeHelp = `Options:
  --permission-mode <mode>              Permission mode to use for the session
                                        (choices: "acceptEdits", "auto",
                                        "bypassPermissions", "manual",
                                        "dontAsk", "plan")
  --plugin-dir <path>                   Load plugins from a directory`

func TestParseClaudeModes(t *testing.T) {
	if got := strings.Join(parseClaudeModes(claudeHelp), ","); got != "acceptEdits,auto,bypassPermissions,manual,dontAsk,plan" {
		t.Errorf("modes = %q", got)
	}
	if got := parseClaudeModes("Usage: claude [options]"); got != nil {
		t.Errorf("modes without the flag = %q", got)
	}
}

func TestClaudeRunsInItsAutoModeWhenItHasOne(t *testing.T) {
	orig := claudeModes
	t.Cleanup(func() { claudeModes = orig })
	for _, c := range []struct {
		modes      []string
		mode, want string
	}{
		{[]string{"acceptEdits", "auto", "plan"}, "auto", "auto"},
		{[]string{"acceptEdits", "auto", "plan"}, "", "auto"},
		{[]string{"acceptEdits", "bypassPermissions", "plan"}, "auto", "acceptEdits"},
		{nil, "auto", "acceptEdits"},
		{nil, "full", "bypassPermissions"},
		{nil, "plan", "plan"},
	} {
		claudeModes = func(string) []string { return c.modes }
		if got := claudeMode(c.mode, "/bin/claude"); got != c.want {
			t.Errorf("mode %q with %q: %q, want %q", c.mode, c.modes, got, c.want)
		}
	}
}

// started runs cmd in dir, in a session of its own (out of any process group collie-agent signals), and returns it.
func started(t *testing.T, dir, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd
}

func exited(cmd *exec.Cmd, within time.Duration) bool {
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

// Processes the agent left in its worktree outside its process group are stopped before the worktree is removed:
// SIGTERM, then SIGKILL for one that ignores it. A process outside the worktree is left alone.
func TestRemovingAWorktreeStopsTheProcessesLeftInIt(t *testing.T) {
	m, _, repo, _ := env(t)
	grace := ProcessGrace
	ProcessGrace = 300 * time.Millisecond
	t.Cleanup(func() { ProcessGrace = grace })
	dir := filepath.Join(m.Dir, "87")
	wt := WorktreePath(repo, 87)
	if err := addWorktree(context.Background(), repo, dir, wt, "collie/session-87"); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(wt, "web")
	_ = os.MkdirAll(sub, 0o755)
	server := started(t, sub, "exec sleep 60")
	stubborn := started(t, wt, `trap "" TERM; sleep 60 & wait`)
	outside := started(t, repo, "exec sleep 60")
	waitUntil(t, "the processes to show up", func() bool { return len(processesIn(context.Background(), wt)) >= 2 })

	stopped, err := removeWorktree(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if stopped < 2 {
		t.Errorf("stopped %d processes, want at least the 2 in the worktree", stopped)
	}
	if !exited(server, 5*time.Second) {
		t.Error("a process in the worktree is still running")
	}
	if !exited(stubborn, 5*time.Second) {
		t.Error("a process that ignores SIGTERM is still running")
	}
	if exited(outside, 300*time.Millisecond) {
		t.Error("a process outside the worktree was stopped")
	}
}

// What a session leaves behind when the agent exits on its own: a `nohup … &` that survives the end of its group.
func TestASessionLeavesNoProcessBehind(t *testing.T) {
	m, _, repo, out := env(t)
	t.Setenv("FAKE_ORPHAN", "1")
	run(t, m, session(88, "claude", repo, "auto"))
	pid, err := strconv.Atoi(read(t, filepath.Join(out, "claude", "orphan")))
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the orphan to be stopped", func() bool { return syscall.Kill(pid, 0) != nil })
}

// workspace-write cuts the network unless told otherwise: a review could neither run tests that listen on localhost
// nor reach GitHub with gh. Only auto needs it: full has no sandbox, plan stays read-only.
func TestCodexGetsTheNetworkInsideItsWorkspaceSandbox(t *testing.T) {
	const network = "-c sandbox_workspace_write.network_access=true"
	for mode, want := range map[string]bool{"auto": true, "": true, "full": false, "plan": false} {
		c, err := (Codex{}).Command(Launch{PermissionMode: mode, TokenEnv: TokenEnv, Prompt: "p"})
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Join(c.Args, " ")
		if strings.Contains(args, network) != want {
			t.Errorf("mode %q: network access %v, want %v: %q", mode, !want, want, args)
		}
		if c.Args[len(c.Args)-1] != "p" {
			t.Errorf("mode %q: the prompt is not the last argument: %q", mode, c.Args)
		}
	}
}

func reviewSession(id int64, agent, repo string) dispatch.Session {
	s := session(id, agent, repo, "auto")
	s.Dispatch.Purpose, s.Dispatch.Kind = "reviewer", "review"
	s.Binding.Agents = map[string]string{"reviewer": agent}
	return s
}

// The local config's session_setup runs in the worktree before the agent, with the session's details.
func TestSessionSetupRunsInTheWorktreeBeforeTheAgent(t *testing.T) {
	m, api, repo, out := env(t)
	marker := filepath.Join(out, "setup")
	m.Setup = map[string]string{"reviewer": `pwd -P > "` + marker + `"; echo "$COLLIE_PURPOSE $COLLIE_SESSION_ID $COLLIE_PROJECT $COLLIE_CODEBASE" >> "` + marker + `"`}
	run(t, m, reviewSession(89, "codex", repo))
	got := strings.Split(read(t, marker), "\n")
	if got[0] != WorktreePath(repo, 89) || got[1] != "reviewer 89 collies collie" {
		t.Errorf("setup saw %q", got)
	}
	if _, finished := api.snapshot(); len(finished) != 1 || finished[0] != 0 {
		t.Errorf("finished = %v", finished)
	}

	// Another purpose has no setup: nothing runs.
	_ = os.Remove(marker)
	run(t, m, session(90, "codex", repo, "auto"))
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("the reviewer's setup ran for a code session")
	}
}

func TestAFailingSessionSetupStopsTheSession(t *testing.T) {
	m, api, repo, out := env(t)
	m.Setup = map[string]string{"code": "echo no identity >&2; exit 3"}
	run(t, m, session(91, "claude", repo, "auto"))
	if _, err := os.Stat(filepath.Join(out, "claude")); !os.IsNotExist(err) {
		t.Error("the agent ran although its setup failed")
	}
	if _, finished := api.snapshot(); len(finished) != 1 || finished[0] != -1 {
		t.Errorf("finished = %v, want -1", finished)
	}
}

// A codex review answers in the review schema; collie-agent sends that answer with the finished event.
func TestACodexReviewSendsItsStructuredAnswer(t *testing.T) {
	m, api, repo, out := env(t)
	review := `{"verdict":"changes_requested","summary":"Read x.go, ran go test.","reason":"","findings":[{"pr_number":9,"path":"x.go","line":3,"blocking":true,"severity":"major","origin":"new","body":"off by one"}]}`
	t.Setenv("FAKE_REVIEW", review)
	run(t, m, reviewSession(92, "codex", repo))
	args := read(t, filepath.Join(out, "codex", "args"))
	for _, want := range []string{"--output-schema\n" + filepath.Join(m.Dir, "92", "review.schema.json"), "-o\n" + filepath.Join(m.Dir, "92", ReviewFile)} {
		if !strings.Contains(args, want) {
			t.Errorf("codex args lack %q:\n%s", want, args)
		}
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.reviews) != 1 || !json.Valid(api.reviews[0]) || !strings.Contains(string(api.reviews[0]), `"verdict":"changes_requested"`) {
		t.Errorf("reviews sent = %q", api.reviews)
	}
}

func TestAnInvalidReviewAnswerIsNotSent(t *testing.T) {
	m, api, repo, _ := env(t)
	t.Setenv("FAKE_REVIEW", `{"verdict":"looks fine"}`)
	run(t, m, reviewSession(93, "codex", repo))
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.reviews) != 1 || api.reviews[0] != nil {
		t.Errorf("reviews sent = %q, want none", api.reviews)
	}
}

func TestOnlyReviewsGetTheReviewSchema(t *testing.T) {
	for purpose, want := range map[string]bool{"reviewer": true, "code": false, "research": false} {
		c, err := (Codex{}).Command(Launch{Purpose: purpose, TokenEnv: TokenEnv, SessionDir: t.TempDir(), Prompt: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(strings.Join(c.Args, " "), "--output-schema"); got != want {
			t.Errorf("purpose %s: output schema %v, want %v", purpose, got, want)
		}
		if c.Args[len(c.Args)-1] != "p" {
			t.Errorf("purpose %s: the prompt is not last: %q", purpose, c.Args)
		}
	}
	if !json.Valid(reviewSchema) {
		t.Error("the embedded review schema is not valid JSON")
	}
}

// A Claude review gets the same review file and schema through the environment, and its answer is sent too.
func TestAClaudeReviewWritesItsAnswerToTheReviewFile(t *testing.T) {
	m, api, repo, out := env(t)
	t.Setenv("FAKE_REVIEW", `{"verdict":"approve","summary":"Read it all.","reason":"","findings":[]}`)
	run(t, m, reviewSession(94, "claude", repo))
	got := strings.Split(read(t, filepath.Join(out, "claude", "review_env")), "\n")
	if want := []string{filepath.Join(m.Dir, "94", ReviewFile), filepath.Join(m.Dir, "94", "review.schema.json")}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("review env = %q, want %q", got, want)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.reviews) != 1 || !strings.Contains(string(api.reviews[0]), `"verdict":"approve"`) {
		t.Errorf("reviews sent = %q", api.reviews)
	}
}

// A code session gets no review file.
func TestOnlyReviewSessionsGetTheReviewFile(t *testing.T) {
	m, _, repo, out := env(t)
	run(t, m, session(95, "claude", repo, "auto"))
	if got := read(t, filepath.Join(out, "claude", "review_env")); got != "" {
		t.Errorf("a code session got the review env %q", got)
	}
}

func TestClaudeSessionsDoNotPushToThePhoneUnlessAllowed(t *testing.T) {
	dir := t.TempDir()
	l := Launch{Name: "n", Worktree: dir, Prompt: "p", MCPURL: "https://collie.test/p/x/mcp", TokenEnv: TokenEnv, SessionDir: dir}
	cmd, err := Claude{}.Command(l)
	if err != nil {
		t.Fatal(err)
	}
	presence := filepath.Join(dir, PresenceFile)
	if !slices.Contains(cmd.Env, "CLAUDE_CLIENT_PRESENCE_FILE="+presence) {
		t.Errorf("env %q has no presence file", cmd.Env)
	}
	if _, err := os.Stat(presence); err != nil {
		t.Errorf("the presence file must exist while the session runs: %v", err)
	}

	other := t.TempDir()
	l.SessionDir, l.PhoneNotifications = other, true
	cmd, err = Claude{}.Command(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "CLAUDE_CLIENT_PRESENCE_FILE=") {
			t.Errorf("phone_notifications on: %q", e)
		}
	}
	if _, err := os.Stat(filepath.Join(other, PresenceFile)); !os.IsNotExist(err) {
		t.Errorf("phone_notifications on wrote a presence file: %v", err)
	}
}

// adopter records the sessions Resume adopts.
type adopter struct {
	mu      sync.Mutex
	adopted []int64
	freed   []int64
}

func (a *adopter) Adopt(id, _ int64, _ string) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.adopted = append(a.adopted, id)
	return func() { a.mu.Lock(); a.freed = append(a.freed, id); a.mu.Unlock() }, nil
}

// suspend starts a session whose agent keeps working and shuts collie-agent down gracefully once ready says so.
func suspend(t *testing.T, m *Manager, s dispatch.Session, ready func() bool) {
	t.Helper()
	t.Setenv("FAKE_SLEEP", "30")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	if err := m.Start(ctx, s, func() { close(done) }); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the agent to be ready", ready)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent was not stopped")
	}
	t.Setenv("FAKE_SLEEP", "")
}

// restart is a new collie-agent run on the same sessions directory: it resumes what the last one left and waits for it.
func restart(t *testing.T, old *Manager, api *fakeAPI) (*Manager, *adopter, map[int64][]string) {
	t.Helper()
	m := &Manager{API: api, Dir: old.Dir, MaxConcurrency: 2, KillGrace: 200 * time.Millisecond, RetryWait: time.Millisecond,
		AnswerSettle: 10 * time.Millisecond, AnswerKeyGap: 10 * time.Millisecond, ResumeURLWait: 5 * time.Second}
	ad := &adopter{}
	var mu sync.Mutex
	notes := map[int64][]string{}
	m.Resume(context.Background(), ad, func(id int64, status, reason string) {
		mu.Lock()
		notes[id] = append(notes[id], strings.TrimSpace(status+" "+reason))
		mu.Unlock()
	})
	done := make(chan struct{})
	go func() { m.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the resumed sessions did not finish")
	}
	return m, ad, notes
}

// noTokenOnDisk fails when a session token shows in any file of the sessions directory.
func noTokenOnDisk(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(path)
		for _, tok := range []string{"cms_secret", "cms_new"} {
			if strings.Contains(string(b), tok) {
				t.Errorf("%s holds the session token %s", path, tok)
			}
		}
		return nil
	})
}

func TestAGracefulShutdownSuspendsTheSession(t *testing.T) {
	m, api, repo, out := env(t)
	suspend(t, m, session(51, "claude", repo, "auto"), func() bool { s, _ := api.snapshot(); return len(s) >= 2 })

	_, finished := api.snapshot()
	if len(finished) != 0 || !slices.Equal(api.suspended, []int64{51}) {
		t.Fatalf("finished %v, suspended %v; want only suspended", finished, api.suspended)
	}
	rec, err := readMeta(filepath.Join(m.Dir, "51"))
	if err != nil || !rec.resumable() {
		t.Fatalf("record %+v (%v) cannot resume the session", rec, err)
	}
	if rec.Dispatch.DispatchID != 51 || rec.Dispatch.Purpose != "code" || rec.Agent != "claude" || rec.PermissionMode != "auto" ||
		rec.Dispatch.Codebase != "collie" || rec.Dispatch.Project != "collies" {
		t.Errorf("record = %+v", rec)
	}
	args := strings.Split(read(t, filepath.Join(out, "claude", "args")), "\n")
	if argAfter(args, "--session-id") != rec.AgentSession || len(rec.AgentSession) != 36 {
		t.Errorf("claude got --session-id %q, the record has %q", argAfter(args, "--session-id"), rec.AgentSession)
	}
	if !strings.Contains(worktrees(t, repo), WorktreePath(repo, 51)) {
		t.Error("the suspended session's worktree was removed")
	}
	if b, _ := exec.Command("git", "-C", repo, "branch", "--list", "collie/session-51").Output(); len(strings.TrimSpace(string(b))) == 0 {
		t.Error("the suspended session's branch was deleted")
	}
	fi, err := os.Stat(filepath.Join(m.Dir, "51", metaFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("record: %v, mode %v; want 0600", err, fi.Mode().Perm())
	}
	noTokenOnDisk(t, m.Dir)

	// A sweep never removes it.
	m.Sweep(context.Background())
	if _, err := os.Stat(filepath.Join(m.Dir, "51")); err != nil {
		t.Error("the sweep removed a session that can resume")
	}
}

func TestARestartResumesAClaudeSession(t *testing.T) {
	m, api, repo, out := env(t)
	suspend(t, m, session(52, "claude", repo, "full"), func() bool { s, _ := api.snapshot(); return len(s) >= 2 })
	uuid := func() string { r, _ := readMeta(filepath.Join(m.Dir, "52")); return r.AgentSession }()

	m2, ad, notes := restart(t, m, api)
	if !slices.Equal(api.reissued, []int64{52}) || !slices.Equal(ad.adopted, []int64{52}) || !slices.Equal(ad.freed, []int64{52}) {
		t.Errorf("reissued %v, adopted %v, freed %v", api.reissued, ad.adopted, ad.freed)
	}
	args := strings.Split(read(t, filepath.Join(out, "claude", "args")), "\n")
	if argAfter(args, "--resume") != uuid || slices.Contains(args, "--session-id") || slices.Contains(args, "--fork-session") {
		t.Errorf("claude args = %q; want --resume %s", args, uuid)
	}
	if argAfter(args, "--permission-mode") != "bypassPermissions" || argAfter(args, "--remote-control") != "collie collies #52" {
		t.Errorf("the resume lost the session's flags: %q", args)
	}
	if !strings.HasSuffix(strings.Join(args, "\n"), "--settings\n"+filepath.Join(m.Dir, "52", "settings.json")+"\n"+strings.TrimRight(resumePrompt, "\n")) {
		t.Errorf("args = %q, want the resume prompt last", args)
	}
	if got := read(t, filepath.Join(out, "claude", "token")); got != "cms_new" {
		t.Errorf("token = %q, want the reissued one", got)
	}
	if got := read(t, filepath.Join(out, "claude", "mcp_url")); got != "https://collie.test/p/collies/mcp2" {
		t.Errorf("mcp url = %q", got)
	}
	if got := read(t, filepath.Join(out, "claude", "pwd")); got != WorktreePath(repo, 52) {
		t.Errorf("resumed in %s", got)
	}
	if !slices.Equal(api.resumed, []string{"https://claude.ai/code/session_01TESTabc-_9"}) {
		t.Errorf("resumed events = %q, want one with the link", api.resumed)
	}
	if _, finished := api.snapshot(); !slices.Equal(finished, []int{0}) {
		t.Errorf("finished = %v, want the resumed agent's exit 0", finished)
	}
	if got := notes[52]; !slices.Equal(got, []string{"resuming", "resumed"}) {
		t.Errorf("notes = %q", got)
	}
	if _, err := os.Stat(filepath.Join(m2.Dir, "52")); !os.IsNotExist(err) {
		t.Error("the finished session was not cleaned up")
	}
}

func TestARestartResumesACodexSession(t *testing.T) {
	m, api, repo, out := env(t)
	s := session(53, "codex", repo, "auto")
	suspend(t, m, s, func() bool {
		b, _ := os.ReadFile(filepath.Join(m.Dir, "53", "agent.log"))
		return strings.Contains(string(b), `"output_tokens":50`)
	})
	if rec, _ := readMeta(filepath.Join(m.Dir, "53")); rec.AgentSession != "t1" || !slices.Equal(api.suspended, []int64{53}) {
		t.Fatalf("record thread %q, suspended %v", rec.AgentSession, api.suspended)
	}

	restart(t, m, api)
	args := strings.Split(read(t, filepath.Join(out, "codex", "args")), "\n")
	if len(args) < 4 || args[0] != "exec" || args[1] != "resume" || args[2] != "--json" ||
		!strings.HasSuffix(strings.Join(args, "\n"), "\nt1\n"+strings.TrimRight(resumePrompt, "\n")) {
		t.Errorf("codex args = %q; want exec resume --json ... t1 <resume prompt>", args)
	}
	if !slices.Contains(args, `sandbox_mode="workspace-write"`) || !slices.Contains(args, "sandbox_workspace_write.network_access=true") ||
		!slices.Contains(args, `mcp_servers.collie.bearer_token_env_var="COLLIE_SESSION_TOKEN"`) || slices.Contains(args, "--cd") {
		t.Errorf("codex args = %q; want the same overrides as the first run", args)
	}
	if !slices.Equal(api.resumed, []string{""}) {
		t.Errorf("resumed = %q", api.resumed)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.usage) != 1 || api.usage[0] == nil || api.usage[0].OutputTokens != 700 || api.usage[0].InputTokens != 3400 {
		t.Errorf("usage = %+v; want both runs added up", api.usage)
	}
}

func TestACodexReviewResumesWithItsOutputSchema(t *testing.T) {
	m, api, repo, out := env(t)
	suspend(t, m, reviewSession(54, "codex", repo), func() bool {
		r, _ := readMeta(filepath.Join(m.Dir, "54"))
		return r.AgentSession == "t1"
	})
	restart(t, m, api)
	args := strings.Split(read(t, filepath.Join(out, "codex", "args")), "\n")
	dir := filepath.Join(m.Dir, "54")
	if argAfter(args, "--output-schema") != filepath.Join(dir, "review.schema.json") || argAfter(args, "-o") != filepath.Join(dir, ReviewFile) {
		t.Errorf("codex args = %q", args)
	}
}

func TestAResumeCollieNoLongerHoldsIsCleanedUpWithoutAnEvent(t *testing.T) {
	m, api, repo, _ := env(t)
	suspend(t, m, session(55, "claude", repo, "auto"), func() bool { s, _ := api.snapshot(); return len(s) >= 2 })
	api.reissueErr = fmt.Errorf("%w: gone", machine.ErrGone)

	_, ad, notes := restart(t, m, api)
	if len(api.resumeFailed) != 0 || len(api.resumed) != 0 {
		t.Errorf("resume_failed %q, resumed %q; collie said the session is gone", api.resumeFailed, api.resumed)
	}
	if !slices.Equal(ad.freed, []int64{55}) {
		t.Errorf("freed %v", ad.freed)
	}
	if got := notes[55]; len(got) != 2 || !strings.HasPrefix(got[1], "failed collie no longer holds") {
		t.Errorf("notes = %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "55")); !os.IsNotExist(err) {
		t.Error("the session directory is still there")
	}
	if strings.Contains(worktrees(t, repo), WorktreePath(repo, 55)) {
		t.Error("the worktree is still there")
	}
}

func TestAResumeWithoutItsWorktreeFails(t *testing.T) {
	m, api, repo, _ := env(t)
	suspend(t, m, session(56, "claude", repo, "auto"), func() bool { s, _ := api.snapshot(); return len(s) >= 2 })
	gitRun(t, repo, "worktree", "remove", "--force", WorktreePath(repo, 56))

	_, _, notes := restart(t, m, api)
	if len(api.resumeFailed) != 1 || !strings.Contains(api.resumeFailed[0], "worktree") {
		t.Errorf("resume_failed = %q, want the missing worktree", api.resumeFailed)
	}
	if _, finished := api.snapshot(); len(finished) != 0 || len(api.resumed) != 0 {
		t.Errorf("finished %v, resumed %v", finished, api.resumed)
	}
	if got := notes[56]; len(got) != 2 || !strings.Contains(got[1], "worktree") {
		t.Errorf("notes = %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "56")); !os.IsNotExist(err) {
		t.Error("the session directory is still there")
	}
	if b, _ := exec.Command("git", "-C", repo, "branch", "--list", "collie/session-56").Output(); len(strings.TrimSpace(string(b))) > 0 {
		t.Error("the branch is still there")
	}
}

func TestAResumeWithoutTheTranscriptFails(t *testing.T) {
	m, api, repo, _ := env(t)
	suspend(t, m, session(57, "claude", repo, "auto"), func() bool { s, _ := api.snapshot(); return len(s) >= 2 })
	_ = os.RemoveAll(os.Getenv("CLAUDE_CONFIG_DIR"))
	restart(t, m, api)
	if len(api.resumeFailed) != 1 || !strings.Contains(api.resumeFailed[0], "transcript") {
		t.Errorf("resume_failed = %q", api.resumeFailed)
	}
}

func TestRecordsThatCannotResumeAreReported(t *testing.T) {
	m, api, _, _ := env(t)
	for name, content := range map[string]string{"60": "{not json", "61": `{"repo":"/nowhere"}`} {
		_ = os.MkdirAll(filepath.Join(m.Dir, name), 0o700)
		_ = os.WriteFile(filepath.Join(m.Dir, name, metaFile), []byte(content), 0o600)
	}
	_ = os.MkdirAll(filepath.Join(m.Dir, "62"), 0o700)    // nothing recorded
	_ = os.MkdirAll(filepath.Join(m.Dir, "stray"), 0o700) // no session id at all
	_, ad, notes := restart(t, m, api)
	if len(api.resumeFailed) != 3 || len(ad.adopted) != 0 {
		t.Errorf("resume_failed %q, adopted %v; want 3 reports and nothing adopted", api.resumeFailed, ad.adopted)
	}
	if len(notes) != 3 || !strings.Contains(notes[60][0], "cannot be read") {
		t.Errorf("notes = %q", notes)
	}
	entries, _ := os.ReadDir(m.Dir)
	if len(entries) != 0 {
		t.Errorf("left %d entries in the sessions dir", len(entries))
	}
}

func TestResumedSessionsShowInTheHeartbeatAndHoldASlot(t *testing.T) {
	m, api, repo, _ := env(t)
	suspend(t, m, session(58, "claude", repo, "auto"), func() bool { s, _ := api.snapshot(); return len(s) >= 2 })
	t.Setenv("FAKE_SLEEP", "30")
	d := &dispatch.Dispatcher{API: &ackRecorder{}, MaxConcurrency: 1}
	m2 := &Manager{API: api, Dir: m.Dir, MaxConcurrency: 1, KillGrace: 200 * time.Millisecond, RetryWait: time.Millisecond,
		AnswerSettle: 10 * time.Millisecond, AnswerKeyGap: 10 * time.Millisecond}
	d.Sessions = m2
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); m2.Wait() }()
	m2.Resume(ctx, d, nil)
	if got := d.Running(); !slices.Equal(got, []string{"58"}) {
		t.Errorf("running = %v right after Resume, want the resumed session", got)
	}
	if _, err := d.Adopt(59, 1, "code"); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Errorf("a second session: %v, want at capacity", err)
	}
	if err := m2.Start(ctx, session(60, "claude", repo, "auto"), nil); err == nil || !strings.Contains(err.Error(), "at capacity") {
		t.Errorf("a new session: %v, want at capacity", err)
	}
}
