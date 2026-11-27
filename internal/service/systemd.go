package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/collies-ai/collie-agent/internal/config"
)

// Systemd manages a systemd user unit in ~/.config/systemd/user.
type Systemd struct {
	ConfigHome string // $XDG_CONFIG_HOME or ~/.config
	Run        Commander
	Profile    config.Profile
}

// Name is the unit name.
func (d *Systemd) Name() string { return SystemdUnit(d.Profile) }

// Path is ~/.config/systemd/user/<unit>.
func (d *Systemd) Path() string { return filepath.Join(d.ConfigHome, "systemd", "user", d.Name()) }

// Render returns the unit. systemd restarts it when it fails; a clean exit (the machine was revoked or is not
// paired) leaves it stopped.
func (d *Systemd) Render(s Spec) (string, error) {
	if err := validateSpec(s); err != nil {
		return "", err
	}
	var b strings.Builder
	desc := "collie-agent: runs coding agents on collie's tasks"
	if !s.Profile.IsDefault() {
		desc += " (profile " + s.Profile.Name + ")"
	}
	b.WriteString("[Unit]\n")
	b.WriteString("Description=" + desc + "\n")
	b.WriteString("Documentation=https://github.com/collies-ai/collie-agent\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("After=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	args := append([]string{s.Executable}, s.Args()...)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = systemdQuote(a)
	}
	b.WriteString("ExecStart=" + strings.Join(quoted, " ") + "\n")
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("Environment=" + systemdQuote(k+"="+s.Env[k]) + "\n")
	}
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=10\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String(), nil
}

// systemdQuote double-quotes a word for ExecStart= and Environment=, escaping what systemd would otherwise
// interpret: backslashes, quotes, specifiers (%) and variable expansion ($).
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$", "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

// Install writes the unit, reloads systemd and enables and starts the unit.
func (d *Systemd) Install(s Spec) error {
	unit, err := d.Render(s)
	if err != nil {
		return err
	}
	if err := writeFile(d.Path(), unit); err != nil {
		return err
	}
	if out, err := d.Run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %v: %s", err, out)
	}
	if out, err := d.Run("systemctl", "--user", "enable", d.Name()); err != nil {
		return fmt.Errorf("systemctl --user enable: %v: %s", err, out)
	}
	// restart, not start: an upgrade re-runs install and must pick up the new binary.
	if out, err := d.Run("systemctl", "--user", "restart", d.Name()); err != nil {
		return fmt.Errorf("systemctl --user restart: %v: %s", err, out)
	}
	return nil
}

// Uninstall stops and disables the unit, deletes it and reloads systemd. If stopping fails the file is kept, so the
// CLI never says it removed a service that still runs. With the file already gone, systemd may still have the unit
// loaded and running: it is stopped, and only a unit systemd confirms it does not know counts as already stopped.
func (d *Systemd) Uninstall() error {
	if _, err := os.Stat(d.Path()); errors.Is(err, os.ErrNotExist) {
		if out, err := d.Run("systemctl", "--user", "stop", d.Name()); err != nil && !systemdNotLoaded(out, err) {
			return fmt.Errorf("systemctl --user stop %s: %v: %s", d.Name(), err, out)
		}
		return nil
	}
	if out, err := d.Run("systemctl", "--user", "disable", "--now", d.Name()); err != nil {
		return fmt.Errorf("systemctl --user disable --now %s: %v: %s", d.Name(), err, out)
	}
	if err := os.Remove(d.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if out, err := d.Run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %v: %s", err, out)
	}
	return nil
}

// systemdNotLoadedExit is systemctl's exit status for an unknown unit (LSB "program is not installed").
const systemdNotLoadedExit = 5

// systemdNotLoaded reports whether systemctl failed because the unit is not loaded, and nothing else. Only the exit
// status counts: the output is free text that other failures can share.
func systemdNotLoaded(_ string, err error) bool {
	return exitCode(err) == systemdNotLoadedExit
}

// Start starts the unit; a unit already running keeps running.
func (d *Systemd) Start() error {
	if _, err := os.Stat(d.Path()); err != nil {
		return ErrNotInstalled
	}
	if out, err := d.Run("systemctl", "--user", "start", d.Name()); err != nil {
		return fmt.Errorf("systemctl --user start %s: %v: %s", d.Name(), err, out)
	}
	return nil
}

// Stop stops the unit. It stays enabled: systemd starts it again at the next login, or Start does.
func (d *Systemd) Stop() error {
	if out, err := d.Run("systemctl", "--user", "stop", d.Name()); err != nil {
		if systemdNotLoaded(out, err) {
			if _, statErr := os.Stat(d.Path()); statErr != nil {
				return ErrNotInstalled
			}
			return nil
		}
		return fmt.Errorf("systemctl --user stop %s: %v: %s", d.Name(), err, out)
	}
	return nil
}

// Restart restarts the unit, or starts it when it was stopped.
func (d *Systemd) Restart() error {
	if _, err := os.Stat(d.Path()); err != nil {
		return ErrNotInstalled
	}
	if out, err := d.Run("systemctl", "--user", "restart", d.Name()); err != nil {
		return fmt.Errorf("systemctl --user restart %s: %v: %s", d.Name(), err, out)
	}
	return nil
}

// Status asks systemctl is-active about the unit.
func (d *Systemd) Status() (Status, error) {
	st := Status{Path: d.Path()}
	if _, err := os.Stat(d.Path()); err == nil {
		st.Installed = true
	}
	out, _ := d.Run("systemctl", "--user", "is-active", d.Name()) // non-zero exit when not active
	st.Detail = strings.TrimSpace(out)
	if st.Detail == "" {
		st.Detail = "unknown"
	}
	st.Running = st.Detail == "active"
	return st, nil
}
