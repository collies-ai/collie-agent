package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/collies-ai/collie-agent/internal/machine"
)

// Launch is everything an adapter needs to build the agent's command.
type Launch struct {
	Name           string // a short name for the session, shown where the agent shows sessions
	Worktree       string // the working directory
	Prompt         string
	MCPURL         string
	PermissionMode string // the binding's mode: auto, full or plan
	Purpose        string // the dispatch's purpose: code, research, reviewer or qa-browser
	Binary         string // the agent CLI's resolved path
	// TokenEnv is the environment variable that holds the session token: adapters reference it and never write the
	// token itself to a file or a command line, where other users could read it.
	TokenEnv   string
	SessionDir string // private to this session (0700), outside the worktree
	// PhoneNotifications lets the agent push to the person's phone; off, Claude Code is told someone is at the
	// terminal (CLAUDE_CLIENT_PRESENCE_FILE), which keeps its pushes quiet.
	PhoneNotifications bool
}

// Command is the agent process to start.
type Command struct {
	Binary string
	Args   []string
	Env    []string // added to the environment
	// TTY runs the agent in a pseudo-terminal: for an interactive CLI.
	TTY bool
	// Answers are typed into the terminal when a startup dialog shows (TTY only): once each, and only while the agent
	// is starting, before it did any work (no activity file) and within StartupWindow.
	Answers []Answer
	// IdleFiles says the agent touches ActivityFile and IdleFile in the session dir (through its hooks): the manager
	// ends it once it has been idle for the idle timeout, typing ExitInput first (TTY only).
	IdleFiles bool
	ExitInput string
}

// StartupWindow is how long after the start a startup dialog is still answered.
const StartupWindow = 2 * time.Minute

// Answer is typed into an interactive agent when a dialog shows while it starts up. Match finds the dialog's first
// line in the output (without escape codes). Then, with Respond, the dialog's lines so far are passed on every new
// line until it is done, and the keys it returns are typed one by one (none: the dialog was not understood and is
// left to a person); without Respond, Input is typed right away.
type Answer struct {
	Match   *regexp.Regexp
	Input   string
	Respond func(lines []string) (keys []string, done bool)
}

func (a Answer) respond(lines []string) ([]string, bool) {
	if a.Respond != nil {
		return a.Respond(lines)
	}
	return []string{a.Input}, true
}

// The marker files an interactive agent's hooks touch in its session dir: activity when it works (a prompt, a tool),
// idle when it stops and waits for input.
const (
	ActivityFile = "activity"
	IdleFile     = "idle"
)

// PresenceFile, in the session dir, tells Claude Code someone is at the terminal so it does not push to the phone.
const PresenceFile = "presence"

// Adapter knows how to run one agent CLI on a collie job.
type Adapter interface {
	// Binary is the CLI to run (looked up in PATH).
	Binary() string
	Command(l Launch) (Command, error)
	// Observe reads one line of the agent's output (raw, escape codes included) for the session link and token usage.
	Observe(line string, o *Observed)
}

// Observed is what the output told about the session so far.
type Observed struct {
	URL   string
	Usage *machine.Usage
}

// Adapters are the agents collie-agent can run, by the name collie uses in bindings.
var Adapters = map[string]Adapter{
	"claude": Claude{},
	"codex":  Codex{},
}

// Claude runs an interactive Claude Code session with Remote Control (`claude --remote-control <name> <prompt>`), so
// it shows in claude.ai/code and the mobile app and a person can watch or step in. Remote Control needs an
// interactive session, so it runs in a pseudo-terminal, starting on the prompt. Only collie's MCP is loaded
// (--strict-mcp-config), with the session token.
//
// An interactive session does not exit when its work is done: hooks touch the activity and idle files, and the
// manager types /exit once it has been idle long enough. Claude reports its usage to collie itself (runner_rules).
type Claude struct{}

// Binary is claude.
func (Claude) Binary() string { return "claude" }

// claudePermission maps the binding's permission mode to Claude Code's: auto is Claude's own auto mode (it decides
// what needs a person and works on its own otherwise), full skips every prompt, plan only plans. Anything else is
// auto. A Claude Code without auto mode gets acceptEdits for auto (claudeMode).
var claudePermission = map[string]string{"auto": "auto", "full": "bypassPermissions", "plan": "plan"}

// claudeModes reads the permission modes a claude CLI accepts from its --help ("choices: ..."); tests replace it.
var claudeModes = func(binary string) []string {
	if binary == "" {
		return nil
	}
	if real, err := filepath.EvalSymlinks(binary); err == nil {
		binary = real // the native installer links claude to the current version: an upgrade is a new path
	}
	if v, ok := claudeModesCache.Load(binary); ok {
		return v.([]string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, binary, "--help").Output()
	modes := parseClaudeModes(string(out))
	claudeModesCache.Store(binary, modes)
	return modes
}

var claudeModesCache sync.Map

var (
	claudeModesFlag = regexp.MustCompile(`--permission-mode[^(]*\(choices:([^)]*)\)`)
	quoted          = regexp.MustCompile(`"([A-Za-z]+)"`)
)

// parseClaudeModes finds the choices of --permission-mode in claude's help text, nil when it lists none.
func parseClaudeModes(help string) []string {
	m := claudeModesFlag.FindStringSubmatch(help)
	if m == nil {
		return nil
	}
	var modes []string
	for _, q := range quoted.FindAllStringSubmatch(m[1], -1) {
		modes = append(modes, q[1])
	}
	return modes
}

// claudeMode is the --permission-mode for the binding's mode with this claude: auto falls back to acceptEdits when
// the CLI does not list auto (or its help could not be read).
func claudeMode(permissionMode, binary string) string {
	mode, ok := claudePermission[permissionMode]
	if !ok {
		mode = claudePermission["auto"]
	}
	if mode == "auto" && !slices.Contains(claudeModes(binary), "auto") {
		return "acceptEdits"
	}
	return mode
}

// claudeTrust matches the question of the workspace trust dialog a folder Claude Code was never trusted in shows,
// and nothing else: an option label such as "Yes, proceed" also appears in other dialogs. Older versions ask "Do you
// trust the files in this folder?" with "Yes, proceed" selected; 2.1 asks "Is this a project you created or one you
// trust?" with "No, exit" selected. The worktree is a checkout of a repo the person bound inside their root folders,
// so collie-agent accepts it (claudeTrustKeys). A worktree inside a repo the person already trusted asks nothing.
var claudeTrust = regexp.MustCompile(`(?i)do\s*you\s*trust\s*the\s*files\s*in\s*this\s*folder\s*\?|is\s*this\s*a\s*project\s*you\s*created\s*or\s*one\s*you\s*trust\s*\?`)

var (
	// trustOption is an option of the trust dialog once compacted (no spaces, lower case, frame and marker removed).
	trustOption = regexp.MustCompile(`^(\d+\.)?(yes|no),`)
	trustYes    = regexp.MustCompile(`^(\d+\.)?yes,(itrustthisfolder|proceed)`)
	// trustEnd is the dialog's footer: every option has been drawn by then.
	trustEnd = regexp.MustCompile(`(?i)enter\s*to\s*confirm`)
)

// Keys of a terminal.
const (
	keyUp    = "\x1b[A"
	keyDown  = "\x1b[B"
	keyEnter = "\r"
)

// claudeTrustKeys answers the trust dialog with "Yes": once its footer shows, it moves the selection from the selected
// option (❯) to the "Yes" one, then confirms. A dialog whose options it cannot tell apart is left to a person, never
// answered blind: Enter on a default "No, exit" ends the session.
func claudeTrustKeys(lines []string) ([]string, bool) {
	if len(lines) > 40 {
		return nil, true // no footer: not the dialog we know
	}
	if !trustEnd.MatchString(lines[len(lines)-1]) {
		return nil, false
	}
	selected, yes, n := -1, -1, 0
	for _, line := range lines {
		t := strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(line, "\u00a0", " ")), ""))
		t = strings.Trim(t, "│|")
		marked := strings.HasPrefix(t, "❯") || strings.HasPrefix(t, ">")
		t = strings.TrimLeft(t, "❯>")
		if !trustOption.MatchString(t) {
			continue
		}
		if marked {
			selected = n
		}
		if trustYes.MatchString(t) {
			yes = n
		}
		n++
	}
	if selected < 0 || yes < 0 {
		return nil, true
	}
	var keys []string
	for i := selected; i < yes; i++ {
		keys = append(keys, keyDown)
	}
	for i := selected; i > yes; i-- {
		keys = append(keys, keyUp)
	}
	return append(keys, keyEnter), true
}

// Command writes the MCP config and the hooks settings (0600, in the session dir) and builds the claude command line.
func (Claude) Command(l Launch) (Command, error) {
	mode := claudeMode(l.PermissionMode, l.Binary)
	mcp := map[string]any{"mcpServers": map[string]any{"collie": map[string]any{
		"type":    "http",
		"url":     l.MCPURL,
		"headers": map[string]string{"Authorization": "Bearer ${" + l.TokenEnv + "}"},
	}}}
	mcpPath := filepath.Join(l.SessionDir, "mcp.json")
	if err := writeJSON(mcpPath, mcp); err != nil {
		return Command{}, fmt.Errorf("writing the MCP config: %w", err)
	}
	settingsPath := filepath.Join(l.SessionDir, "settings.json")
	if err := writeJSON(settingsPath, claudeSettings(l.SessionDir)); err != nil {
		return Command{}, fmt.Errorf("writing the settings: %w", err)
	}
	var env []string
	if !l.PhoneNotifications {
		// Claude Code skips its phone pushes while this file exists, as if the person were at the terminal.
		presence := filepath.Join(l.SessionDir, PresenceFile)
		if err := os.WriteFile(presence, nil, 0o600); err != nil {
			return Command{}, fmt.Errorf("writing the presence file: %w", err)
		}
		env = append(env, "CLAUDE_CLIENT_PRESENCE_FILE="+presence)
	}
	return Command{
		Binary: "claude",
		Env:    env,
		Args: []string{
			"--remote-control", l.Name,
			"--permission-mode", mode,
			"--mcp-config", mcpPath, "--strict-mcp-config",
			"--settings", settingsPath,
			l.Prompt,
		},
		TTY:       true,
		Answers:   []Answer{{Match: claudeTrust, Respond: claudeTrustKeys}},
		IdleFiles: true,
		ExitInput: "/exit\r",
	}, nil
}

// claudeSettings are hooks that touch the activity file whenever the session works (a prompt, a tool) and the idle
// file when it stops or waits for a person. Not SessionStart: a session stuck on a dialog before its first prompt
// has done no work, and is judged by its output.
func claudeSettings(dir string) map[string]any {
	touch := func(file string) []any {
		return []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": "touch " + shellQuote(filepath.Join(dir, file)),
		}}}}
	}
	return map[string]any{"hooks": map[string]any{
		"UserPromptSubmit": touch(ActivityFile),
		"PreToolUse":       touch(ActivityFile),
		"PostToolUse":      touch(ActivityFile),
		"Stop":             touch(IdleFile),
		"Notification":     touch(IdleFile),
	}}
}

var claudeURL = regexp.MustCompile(`https://claude\.ai/code/session_[A-Za-z0-9_-]+`)

// Observe finds the claude.ai/code link of the Remote Control session (shown as text or as a terminal hyperlink).
func (Claude) Observe(line string, o *Observed) {
	if o.URL == "" {
		o.URL = claudeURL.FindString(line)
	}
}

// Codex runs `codex exec --json` headless with collie's MCP (streamable HTTP, bearer token from the environment). It
// never goes through Claude, and it has no session view, so it reports lifecycle and usage only.
type Codex struct{}

// Binary is codex.
func (Codex) Binary() string { return "codex" }

// codexSandbox maps collie's permission modes to codex's sandbox: auto writes inside the workspace (with the network
// on), full has no sandbox, plan is read-only. Anything else is auto.
var codexSandbox = map[string]string{"auto": "workspace-write", "full": "danger-full-access", "plan": "read-only"}

// Command builds the codex exec command line.
func (Codex) Command(l Launch) (Command, error) {
	sandbox, ok := codexSandbox[l.PermissionMode]
	if !ok {
		sandbox = codexSandbox["auto"]
	}
	var network []string
	if sandbox == "workspace-write" {
		// workspace-write cuts the network by default, and with approval_policy never nothing can ask for it: tests that
		// listen on localhost and gh (a review's comment on the PR) fail. Writes stay inside the worktree.
		network = []string{"-c", "sandbox_workspace_write.network_access=true"}
	}
	// A review answers with the review itself (ReviewFile), in the shape collie takes: codex enforces it. Any agent is
	// also told where to write it (COLLIE_REVIEW_FILE, set by the manager).
	var review []string
	if l.Purpose == "reviewer" {
		schema, err := writeReviewSchema(l.SessionDir)
		if err != nil {
			return Command{}, err
		}
		review = []string{"--output-schema", schema, "-o", filepath.Join(l.SessionDir, ReviewFile)}
	}
	url, _ := json.Marshal(l.MCPURL)
	env, _ := json.Marshal(l.TokenEnv)
	return Command{
		Binary: "codex",
		Args: append(append([]string{
			"exec", "--json",
			"--cd", l.Worktree,
			"--sandbox", sandbox,
			"-c", "approval_policy=\"never\"",
		}, append(network, review...)...), []string{
			"-c", "mcp_servers.collie.url=" + string(url),
			"-c", "mcp_servers.collie.bearer_token_env_var=" + string(env),
			l.Prompt,
		}...),
	}, nil
}

// codexEvent is the part of a `codex exec --json` event that carries usage: every turn.completed has the turn's
// tokens.
type codexEvent struct {
	Type  string `json:"type"`
	Usage *struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
	} `json:"usage"`
}

// Observe adds up the usage of every completed turn. Codex has no session link.
func (Codex) Observe(line string, o *Observed) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return
	}
	var e codexEvent
	if json.Unmarshal([]byte(line), &e) != nil || e.Type != "turn.completed" || e.Usage == nil {
		return
	}
	if o.Usage == nil {
		o.Usage = &machine.Usage{}
	}
	// codex counts cached input inside input_tokens; collie wants them apart.
	o.Usage.InputTokens += e.Usage.InputTokens - e.Usage.CachedInputTokens
	o.Usage.CacheReadInputTokens += e.Usage.CachedInputTokens
	o.Usage.OutputTokens += e.Usage.OutputTokens
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// shellQuote quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
