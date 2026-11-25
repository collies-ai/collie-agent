package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
}

// Adapter knows how to run one agent CLI headless on a collie job.
type Adapter interface {
	// Binary is the CLI to run (looked up in PATH).
	Binary() string
	Command(l Launch) (Command, error)
	// SessionURL extracts the link to watch the session from a line of the agent's output, or "".
	SessionURL(line string) string
}

// Adapters are the agents collie-agent can run, by the name collie uses in bindings.
var Adapters = map[string]Adapter{
	"claude": Claude{},
	"codex":  Codex{},
}

// Claude runs Claude Code headless with --remote-control, so the session shows in claude.ai/code and the mobile app
// and a person can watch or step in. Only collie's MCP is loaded (--strict-mcp-config), with the session token.
type Claude struct{}

// Binary is claude.
func (Claude) Binary() string { return "claude" }

// claudePermission maps the binding's permission mode to Claude Code's: auto edits inside the worktree, full skips every
// prompt, plan only plans. Anything else is auto.
var claudePermission = map[string]string{"auto": "acceptEdits", "full": "bypassPermissions", "plan": "plan"}

// Command writes the MCP config (0600, in the session dir) and builds the claude command line.
func (Claude) Command(l Launch) (Command, error) {
	mode, ok := claudePermission[l.PermissionMode]
	if !ok {
		mode = claudePermission["auto"]
	}
	cfg := map[string]any{"mcpServers": map[string]any{"collie": map[string]any{
		"type":    "http",
		"url":     l.MCPURL,
		"headers": map[string]string{"Authorization": "Bearer ${" + l.TokenEnv + "}"},
	}}}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	path := filepath.Join(l.SessionDir, "mcp.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return Command{}, fmt.Errorf("writing the MCP config: %w", err)
	}
	return Command{
		Binary: "claude",
		Args: []string{
			"--remote-control", l.Name,
			"--permission-mode", mode,
			"--mcp-config", path, "--strict-mcp-config",
			"-p", l.Prompt,
		},
	}, nil
}

var claudeURL = regexp.MustCompile(`https://claude\.ai/code/session_[A-Za-z0-9_-]+`)

// SessionURL finds the claude.ai/code link --remote-control prints.
func (Claude) SessionURL(line string) string { return claudeURL.FindString(line) }

// Codex runs `codex exec` headless with collie's MCP (streamable HTTP, bearer token from the environment). It never
// goes through Claude, and it has no session view, so it reports lifecycle only.
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
			"exec",
			"--cd", l.Worktree,
			"--sandbox", sandbox,
			"-c", "approval_policy=\"never\"",
			"-c", "mcp_servers.collie.url=" + string(url),
			"-c", "mcp_servers.collie.bearer_token_env_var=" + string(env),
			l.Prompt,
		},
	}, nil
}

// SessionURL is always "": codex has no session view.
func (Codex) SessionURL(string) string { return "" }
