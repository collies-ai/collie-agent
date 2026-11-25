package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/collies-ai/collie-agent/internal/machine"
)

// Launch is everything an adapter needs to build the agent's command.
type Launch struct {
	Name           string // a short name for the session, shown where the agent shows sessions
	Worktree       string // the working directory
	Prompt         string
	MCPURL         string
	PermissionMode string // the binding's mode: auto, full or plan
	// TokenEnv is the environment variable that holds the session token: adapters reference it and never write the
	// token itself to a file or a command line, where other users could read it.
	TokenEnv   string
	SessionDir string // private to this session (0700), outside the worktree
}

// Command is the agent process to start.
type Command struct {
	Binary string
	Args   []string
	Env    []string // added to the environment
	// TTY runs the agent in a pseudo-terminal: for an interactive CLI.
	TTY bool
	// Answers are typed into the terminal the first time a line of output matches (TTY only).
	Answers []Answer
	// IdleFiles says the agent touches ActivityFile and IdleFile in the session dir (through its hooks): the manager
	// ends it once it has been idle for the idle timeout, typing ExitInput first (TTY only).
	IdleFiles bool
	ExitInput string
}

// Answer is input typed into an interactive agent when a line of its output (without escape codes) matches.
type Answer struct {
	Match *regexp.Regexp
	Input string
}

// The marker files an interactive agent's hooks touch in its session dir: activity when it works (a prompt, a tool),
// idle when it stops and waits for input.
const (
	ActivityFile = "activity"
	IdleFile     = "idle"
)

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

// claudePermission maps the binding's permission mode to Claude Code's: auto edits inside the worktree, full skips every
// prompt, plan only plans. Anything else is auto.
var claudePermission = map[string]string{"auto": "acceptEdits", "full": "bypassPermissions", "plan": "plan"}

// claudeTrust matches the workspace trust dialog a new folder shows. The worktree is a checkout of a repo the person
// bound inside their root folders, so collie-agent accepts it (Enter on "Yes, proceed").
var claudeTrust = regexp.MustCompile(`(?i)do you trust|trust (the files in )?this folder|yes, proceed`)

// Command writes the MCP config and the hooks settings (0600, in the session dir) and builds the claude command line.
func (Claude) Command(l Launch) (Command, error) {
	mode, ok := claudePermission[l.PermissionMode]
	if !ok {
		mode = claudePermission["auto"]
	}
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
	return Command{
		Binary: "claude",
		Args: []string{
			"--remote-control", l.Name,
			"--permission-mode", mode,
			"--mcp-config", mcpPath, "--strict-mcp-config",
			"--settings", settingsPath,
			l.Prompt,
		},
		TTY:       true,
		Answers:   []Answer{{Match: claudeTrust, Input: "\r"}},
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

// codexSandbox maps collie's permission modes to codex's sandbox: auto writes inside the workspace, full has no
// sandbox, plan is read-only. Anything else is auto.
var codexSandbox = map[string]string{"auto": "workspace-write", "full": "danger-full-access", "plan": "read-only"}

// Command builds the codex exec command line.
func (Codex) Command(l Launch) (Command, error) {
	sandbox, ok := codexSandbox[l.PermissionMode]
	if !ok {
		sandbox = codexSandbox["auto"]
	}
	url, _ := json.Marshal(l.MCPURL)
	env, _ := json.Marshal(l.TokenEnv)
	return Command{
		Binary: "codex",
		Args: []string{
			"exec", "--json",
			"--cd", l.Worktree,
			"--sandbox", sandbox,
			"-c", "approval_policy=\"never\"",
			"-c", "mcp_servers.collie.url=" + string(url),
			"-c", "mcp_servers.collie.bearer_token_env_var=" + string(env),
			l.Prompt,
		},
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
