package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const settingsFile = "config.json"

// Settings is the profile's local config file (config.json). The machine has the last word: these override what
// collie sends.
type Settings struct {
	// RootFolders are the only places collie-agent looks for repos and the only places a binding's folder may be.
	RootFolders []string `json:"root_folders"`
	// Discover lists where else repos are found, by the person's consent at pairing: "claude" (the folders Claude Code
	// trusts) and "codex" (the projects Codex trusts). Those exact repo folders are then allowed like root folders,
	// never their children. Absent (nil) means the person was never asked; an empty list means they said no.
	Discover *[]string `json:"discover,omitempty"`
	// MaxConcurrency is the machine-wide limit of sessions at once (0: the default).
	MaxConcurrency int `json:"max_concurrency,omitempty"`
	// PermissionMode, when set, is the permission mode of every binding on this machine, whatever collie says: auto,
	// full or plan. A binding's pin can still set its own.
	PermissionMode string `json:"permission_mode,omitempty"`
	// SessionSetup runs a command in a session's worktree before its agent starts, by the dispatch's purpose (code,
	// research, reviewer, qa-browser): e.g. give the reviewer its own git and GitHub identity. A command that fails
	// stops the session before the agent runs.
	SessionSetup map[string]string `json:"session_setup,omitempty"`
	// Bindings pins or overrides collie's bindings, matched by runner_id or by project + codebase (+ purpose).
	Bindings []PinnedBinding `json:"bindings,omitempty"`
}

// PinnedBinding overrides the fields it sets on the matching binding from collie. Disabled turns that binding off on
// this machine whatever collie says.
type PinnedBinding struct {
	RunnerID       int64             `json:"runner_id,omitempty"`
	Project        string            `json:"project,omitempty"`
	Codebase       string            `json:"codebase,omitempty"`
	Purpose        string            `json:"purpose,omitempty"`
	Folder         string            `json:"folder,omitempty"`
	Agents         map[string]string `json:"agents,omitempty"`
	PermissionMode string            `json:"permission_mode,omitempty"`
	MaxConcurrency int               `json:"max_concurrency,omitempty"`
	Disabled       bool              `json:"disabled,omitempty"`
}

// DiscoverSources are the sources Discover accepts.
var DiscoverSources = []string{"claude", "codex"}

// DiscoverFrom returns the discovery sources the person allowed (none when never asked or answered no).
func (s *Settings) DiscoverFrom() []string {
	if s == nil || s.Discover == nil {
		return nil
	}
	return *s.Discover
}

// ParseDiscover reads a --discover value: a comma-separated list of DiscoverSources, or "none".
func ParseDiscover(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if v == "none" {
		return []string{}, nil
	}
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if !slices.Contains(DiscoverSources, part) {
			return nil, fmt.Errorf("discover %q is not one of %s, or none", part, strings.Join(DiscoverSources, ", "))
		}
		if !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	return out, nil
}

// PermissionModes are the modes a binding can run in: auto (the agent works on its own inside the worktree and asks
// before anything risky), full (no prompts and no sandbox) and plan (read-only).
var PermissionModes = []string{"auto", "full", "plan"}

// validPermissionMode reports whether mode is empty (not set) or one of PermissionModes. A typo is an error rather
// than a silent default: the local config is where the person narrows what agents may do.
func validPermissionMode(mode string) bool {
	return mode == "" || slices.Contains(PermissionModes, mode)
}

// SettingsPath is the profile's config.json.
func (p Profile) SettingsPath() string { return filepath.Join(p.Dir, settingsFile) }

// LoadSettings reads config.json; a missing file is empty settings. Root folders are expanded (~) and made absolute.
func (p Profile) LoadSettings() (*Settings, error) {
	b, err := os.ReadFile(p.SettingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return &Settings{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("reading %s: %w", p.SettingsPath(), err)
	}
	if s.MaxConcurrency < 0 {
		return nil, fmt.Errorf("%s: max_concurrency must not be negative", p.SettingsPath())
	}
	if !validPermissionMode(s.PermissionMode) {
		return nil, fmt.Errorf("%s: permission_mode %q is not one of %s", p.SettingsPath(), s.PermissionMode, strings.Join(PermissionModes, ", "))
	}
	for _, b := range s.Bindings {
		if !validPermissionMode(b.PermissionMode) {
			return nil, fmt.Errorf("%s: binding permission_mode %q is not one of %s", p.SettingsPath(), b.PermissionMode, strings.Join(PermissionModes, ", "))
		}
	}
	for _, d := range s.DiscoverFrom() {
		if !slices.Contains(DiscoverSources, d) {
			return nil, fmt.Errorf("%s: discover %q is not one of %s", p.SettingsPath(), d, strings.Join(DiscoverSources, ", "))
		}
	}
	roots := make([]string, 0, len(s.RootFolders))
	for _, r := range s.RootFolders {
		abs, err := ExpandPath(r)
		if err != nil {
			return nil, fmt.Errorf("%s: root folder %q: %w", p.SettingsPath(), r, err)
		}
		roots = append(roots, abs)
	}
	s.RootFolders = roots
	return &s, nil
}

// SaveSettings writes config.json (0600).
func (p Profile) SaveSettings(s *Settings) error {
	if err := p.EnsureDir(); err != nil {
		return err
	}
	return writePrivate(p.SettingsPath(), s)
}

// ExpandPath turns ~ and relative paths into a clean absolute path.
func ExpandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("empty path")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	return filepath.Abs(path)
}
