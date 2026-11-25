// Package service registers `collie-agent run` as a per-user service: a launchd user agent on macOS and a systemd
// user unit on Linux. Each profile gets its own service name, so a second profile runs as a second service.
package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/collies-ai/collie-agent/internal/config"
)

// Spec is what a service runs.
type Spec struct {
	Profile config.Profile
	// Executable is the absolute path of the collie-agent binary.
	Executable string
	// Env is passed to the service: PATH (so it finds the agent CLIs later) and the config overrides in use.
	Env map[string]string
}

// Args are the arguments the service passes to collie-agent.
func (s Spec) Args() []string {
	args := []string{"run", "--service"}
	if !s.Profile.IsDefault() {
		args = append(args, "--profile", s.Profile.Name)
	}
	return args
}

// Status is what the OS says about the service.
type Status struct {
	Installed bool   // the plist or unit file exists
	Running   bool   // the OS reports it running
	Detail    string // the OS's own words (launchd state, systemd ActiveState)
	Path      string // the plist or unit file
}

// Commander runs an OS command and returns its combined output. Tests replace it.
type Commander func(name string, args ...string) (string, error)

// ExecCommander runs the command for real.
func ExecCommander(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Manager installs, removes and inspects the service of one profile.
type Manager interface {
	// Name is the launchd label or the systemd unit name.
	Name() string
	// Path is the plist or unit file.
	Path() string
	// Render returns the plist or unit file contents.
	Render(Spec) (string, error)
	Install(Spec) error
	Uninstall() error
	Status() (Status, error)
}

// ErrUnsupported means the OS has no service manager collie-agent knows.
var ErrUnsupported = errors.New("service install supports macOS (launchd) and Linux (systemd) only")

// Options say where the service manager keeps its files.
type Options struct {
	GOOS    string
	Home    string // the user's home directory
	UID     int
	Run     Commander
	Profile config.Profile
}

// New returns the Manager for opts.GOOS.
func New(opts Options) (Manager, error) {
	if opts.Run == nil {
		opts.Run = ExecCommander
	}
	switch opts.GOOS {
	case "darwin":
		return &Launchd{Home: opts.Home, UID: opts.UID, Run: opts.Run, Profile: opts.Profile}, nil
	case "linux":
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" || !filepath.IsAbs(cfg) {
			cfg = filepath.Join(opts.Home, ".config")
		}
		return &Systemd{ConfigHome: cfg, Run: opts.Run, Profile: opts.Profile}, nil
	default:
		return nil, ErrUnsupported
	}
}

// LaunchdLabel is the launchd label of a profile: sh.collies.collie-agent, or sh.collies.collie-agent-<profile>.
func LaunchdLabel(p config.Profile) string { return "sh.collies." + config.AppName + p.Suffix() }

// SystemdUnit is the systemd unit of a profile: collie-agent.service, or collie-agent-<profile>.service.
func SystemdUnit(p config.Profile) string { return config.AppName + p.Suffix() + ".service" }

func writeFile(path, contents string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(contents), 0o644)
}

func validateSpec(s Spec) error {
	if !filepath.IsAbs(s.Executable) {
		return fmt.Errorf("executable %q must be an absolute path", s.Executable)
	}
	return nil
}
