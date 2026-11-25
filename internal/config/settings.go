package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const settingsFile = "config.json"

// Settings is the profile's local config file (config.json). The machine has the last word: these override what
// collie sends.
type Settings struct {
	// RootFolders are the only places collie-agent looks for repos and the only places a binding's folder may be.
	RootFolders []string `json:"root_folders"`
	// MaxConcurrency is the machine-wide limit of sessions at once (0: the default).
	MaxConcurrency int `json:"max_concurrency,omitempty"`
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
