// Package config resolves where a profile keeps its files and reads and writes them: the credentials from pairing
// (0600) and the state the running service leaves for `collie-agent status`.
//
// Every profile is isolated: its own directory, credential and service name. The default profile is the one
// instance per OS user; `--profile <name>` is the opt-in second instance.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DefaultServer is the collie instance collie-agent pairs with unless --server says otherwise.
const DefaultServer = "https://collies.sh"

// DefaultProfile is the profile used when --profile is not given.
const DefaultProfile = "default"

// AppName is the base name of directories and services.
const AppName = "collie-agent"

const (
	credentialsFile = "credentials.json"
	stateFile       = "state.json"
	lockFile        = "run.lock"
)

var profileFormat = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidateProfile accepts lowercase letters, digits and dashes (at most 32), so the name is safe in paths and
// service names.
func ValidateProfile(name string) error {
	if !profileFormat.MatchString(name) {
		return fmt.Errorf("invalid profile %q: use lowercase letters, digits and dashes (at most 32)", name)
	}
	return nil
}

// Profile is a named, isolated instance of collie-agent.
type Profile struct {
	Name string
	// Dir holds the credentials, the state and the run lock. Created 0700.
	Dir string
}

// IsDefault reports whether this is the default profile.
func (p Profile) IsDefault() bool { return p.Name == DefaultProfile }

// Suffix is "" for the default profile and "-<name>" otherwise, appended to directory and service names.
func (p Profile) Suffix() string {
	if p.IsDefault() {
		return ""
	}
	return "-" + p.Name
}

// CredentialsPath is the 0600 file with the machine id and token.
func (p Profile) CredentialsPath() string { return filepath.Join(p.Dir, credentialsFile) }

// StatePath is where `run` records its last heartbeat and error.
func (p Profile) StatePath() string { return filepath.Join(p.Dir, stateFile) }

// LockPath is the file `run` locks so only one instance of a profile runs.
func (p Profile) LockPath() string { return filepath.Join(p.Dir, lockFile) }

// Resolve returns the profile's paths. The base directory is $COLLIE_AGENT_CONFIG_DIR when set, otherwise
// $XDG_CONFIG_HOME or ~/.config. The default profile lives in <base>/collie-agent, another in
// <base>/collie-agent-<name>.
func Resolve(name string) (Profile, error) {
	if name == "" {
		name = DefaultProfile
	}
	if err := ValidateProfile(name); err != nil {
		return Profile{}, err
	}
	base, err := baseDir()
	if err != nil {
		return Profile{}, err
	}
	p := Profile{Name: name}
	p.Dir = filepath.Join(base, AppName+p.Suffix())
	return p, nil
}

func baseDir() (string, error) {
	if d := os.Getenv("COLLIE_AGENT_CONFIG_DIR"); d != "" {
		return filepath.Abs(d)
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" && filepath.IsAbs(d) {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find the home directory: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}

// EnsureDir creates the profile directory with 0700 and tightens it if it already existed.
func (p Profile) EnsureDir() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(p.Dir, 0o700)
}

// NormalizeServer checks a collie URL and strips its trailing slash. It must be https, except for a local server
// (localhost, 127.0.0.1, ::1) used in development.
func NormalizeServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid server URL %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return "", fmt.Errorf("server URL %q must use https", raw)
		}
	default:
		return "", fmt.Errorf("server URL %q must use https", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("server URL %q must not have a query or fragment", raw)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// Credentials is what pairing returns: the machine and its long-lived token, plus the server they belong to.
type Credentials struct {
	Server       string    `json:"server"`
	MachineID    string    `json:"machine_id"`
	MachineToken string    `json:"machine_token"`
	Name         string    `json:"name,omitempty"`
	PairedAt     time.Time `json:"paired_at"`
}

// ErrNotPaired means the profile has no credentials yet.
var ErrNotPaired = errors.New("not paired: run `collie-agent pair` first")

// LoadCredentials reads the profile's credentials, or returns ErrNotPaired.
func (p Profile) LoadCredentials() (*Credentials, error) {
	b, err := os.ReadFile(p.CredentialsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotPaired
	}
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("reading %s: %w", p.CredentialsPath(), err)
	}
	if c.MachineToken == "" || c.Server == "" {
		return nil, ErrNotPaired
	}
	return &c, nil
}

// SaveCredentials writes the credentials with 0600, atomically, so the token is never readable by other users,
// not even for a moment.
func (p Profile) SaveCredentials(c *Credentials) error {
	if err := p.EnsureDir(); err != nil {
		return err
	}
	return writePrivate(p.CredentialsPath(), c)
}

// DeleteCredentials forgets the pairing. A missing file is not an error.
func (p Profile) DeleteCredentials() error {
	err := os.Remove(p.CredentialsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// State is what `run` leaves behind for `status`: when it last reached collie and what went wrong.
type State struct {
	PID             int       `json:"pid,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at,omitempty"`
	BindingsVersion string    `json:"bindings_version,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	LastErrorAt     time.Time `json:"last_error_at,omitempty"`
	// Revoked is set when collie answered 401: the machine was revoked or its token is invalid.
	Revoked bool `json:"revoked,omitempty"`
	// MachineID is the machine the state is about, so a new pairing does not inherit an old "revoked".
	MachineID string `json:"machine_id,omitempty"`
	// Connection is how dispatches arrive now: "cable", "long-poll" or "" (not connected).
	Connection string `json:"connection,omitempty"`
	// Bindings are the bindings as this machine serves them, with why one is refused.
	Bindings []BindingState `json:"bindings,omitempty"`
	// Repos is how many repos matching a codebase the last inventory reported.
	Repos int `json:"repos,omitempty"`
	// Running are the sessions running now.
	Running []string `json:"running,omitempty"`
}

// BindingState is one binding in the state file.
type BindingState struct {
	RunnerID int64  `json:"runner_id"`
	Project  string `json:"project"`
	Codebase string `json:"codebase"`
	Folder   string `json:"folder"`
	Pinned   bool   `json:"pinned,omitempty"`
	Refused  string `json:"refused,omitempty"`
}

// LoadState reads the state; a missing file is an empty state.
func (p Profile) LoadState() (*State, error) {
	b, err := os.ReadFile(p.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return &State{}, nil //nolint:nilerr // a corrupt state file is rewritten by the next heartbeat
	}
	return &s, nil
}

// SaveState writes the state (0600, atomically).
func (p Profile) SaveState(s *State) error {
	if err := p.EnsureDir(); err != nil {
		return err
	}
	return writePrivate(p.StatePath(), s)
}

func writePrivate(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
