package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/collies-ai/collie-agent/internal/config"
)

// Launchd manages a launchd user agent in ~/Library/LaunchAgents.
type Launchd struct {
	Home    string
	UID     int
	Run     Commander
	Profile config.Profile
	// Sleep waits between launchctl retries; nil means time.Sleep. Tests replace it.
	Sleep func(time.Duration)
}

// Name is the launchd label.
func (l *Launchd) Name() string { return LaunchdLabel(l.Profile) }

// Path is ~/Library/LaunchAgents/<label>.plist.
func (l *Launchd) Path() string {
	return filepath.Join(l.Home, "Library", "LaunchAgents", l.Name()+".plist")
}

// LogPath is where the agent's output goes: ~/Library/Logs/collie-agent[-<profile>].log.
func (l *Launchd) LogPath() string {
	return filepath.Join(l.Home, "Library", "Logs", config.AppName+l.Profile.Suffix()+".log")
}

func (l *Launchd) domain() string { return "gui/" + strconv.Itoa(l.UID) }

// Render returns the plist. It starts at login, and launchd restarts it when it exits with an error; a clean exit
// (the machine was revoked or is not paired) leaves it stopped.
func (l *Launchd) Render(s Spec) (string, error) {
	if err := validateSpec(s); err != nil {
		return "", err
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	key(&b, "Label")
	str(&b, l.Name())
	key(&b, "ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range append([]string{s.Executable}, s.Args()...) {
		b.WriteString("\t")
		str(&b, a)
	}
	b.WriteString("\t</array>\n")
	if len(s.Env) > 0 {
		key(&b, "EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("\t")
			key(&b, k)
			b.WriteString("\t")
			str(&b, s.Env[k])
		}
		b.WriteString("\t</dict>\n")
	}
	key(&b, "RunAtLoad")
	b.WriteString("\t<true/>\n")
	key(&b, "KeepAlive")
	b.WriteString("\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	key(&b, "ThrottleInterval")
	b.WriteString("\t<integer>10</integer>\n")
	key(&b, "ProcessType")
	str(&b, "Background")
	key(&b, "StandardOutPath")
	str(&b, l.LogPath())
	key(&b, "StandardErrorPath")
	str(&b, l.LogPath())
	b.WriteString("</dict>\n</plist>\n")
	return b.String(), nil
}

func key(b *bytes.Buffer, k string) {
	b.WriteString("\t<key>")
	_ = xml.EscapeText(b, []byte(k))
	b.WriteString("</key>\n")
}

func str(b *bytes.Buffer, v string) {
	b.WriteString("\t<string>")
	_ = xml.EscapeText(b, []byte(v))
	b.WriteString("</string>\n")
}

// How long Install waits for launchd to let go of a booted-out job, and how often it looks.
const (
	launchdBootoutTimeout = 15 * time.Second
	launchdBootoutPoll    = 250 * time.Millisecond
)

// launchdBootstrapBackoff are the waits between bootstrap attempts that fail with EIO.
var launchdBootstrapBackoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

// Install writes the plist and (re)loads it with launchctl bootstrap.
//
// bootout returns before launchd has finished tearing the old job down, and a bootstrap in that window fails with
// "Bootstrap failed: 5: Input/output error". So Install waits until launchctl print no longer knows the label, then
// bootstraps, retrying on EIO. If it still fails the error says how to load the agent by hand.
func (l *Launchd) Install(s Spec) error {
	plist, err := l.Render(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.LogPath()), 0o755); err != nil {
		return err
	}
	if err := writeFile(l.Path(), plist); err != nil {
		return err
	}
	// A previous version may be loaded: unload it first, ignoring "not loaded".
	_, _ = l.Run("launchctl", "bootout", l.target())
	l.waitUntilGone()
	return l.bootstrap()
}

// target is the service target of launchctl: gui/<uid>/<label>.
func (l *Launchd) target() string { return l.domain() + "/" + l.Name() }

// bootstrap loads the plist, retrying while launchd is still tearing down a job that was just booted out. If it still
// fails the error says how to load the agent by hand.
func (l *Launchd) bootstrap() error {
	var out string
	var err error
	for attempt := 0; ; attempt++ {
		out, err = l.Run("launchctl", "bootstrap", l.domain(), l.Path())
		if err == nil {
			return nil
		}
		if attempt >= len(launchdBootstrapBackoff) || !launchdBusy(out, err) {
			break
		}
		l.sleep(launchdBootstrapBackoff[attempt])
	}
	if launchdBusy(out, err) {
		// Out of retries while launchd was still busy: wait for the old job once more and make a last attempt, so a
		// teardown that just finished does not leave the agent unloaded.
		l.waitUntilGone()
		if out, err = l.Run("launchctl", "bootstrap", l.domain(), l.Path()); err == nil {
			return nil
		}
	}
	return fmt.Errorf("launchctl bootstrap: %v: %s\nThe agent is not loaded. Load it with: launchctl bootstrap %s %s",
		err, out, l.domain(), shellQuote(l.Path()))
}

// loaded asks launchctl print whether launchd knows the label. Only launchd's own "not found" means no: any other
// failure leaves the state unknown and is returned.
func (l *Launchd) loaded() (bool, error) {
	out, err := l.Run("launchctl", "print", l.target())
	if err == nil {
		return true, nil
	}
	if launchdNotFound(out, err) {
		return false, nil
	}
	return false, fmt.Errorf("launchctl print %s: %v: %s", l.target(), err, out)
}

// installed reports whether the plist exists.
func (l *Launchd) installed() bool {
	_, err := os.Stat(l.Path())
	return err == nil
}

// Start loads the plist when launchd does not know the agent (stopped, or never loaded since login), and otherwise
// asks launchd to run it; an agent already running keeps running.
func (l *Launchd) Start() error {
	if !l.installed() {
		return ErrNotInstalled
	}
	loaded, err := l.loaded()
	if err != nil {
		return err
	}
	if !loaded {
		return l.bootstrap()
	}
	if out, err := l.Run("launchctl", "kickstart", l.target()); err != nil {
		return fmt.Errorf("launchctl kickstart %s: %v: %s", l.target(), err, out)
	}
	return nil
}

// Stop unloads the agent (bootout), so KeepAlive does not bring it back. The plist stays: launchd loads it again at
// the next login, or Start does.
func (l *Launchd) Stop() error {
	loaded, err := l.loaded()
	if err != nil {
		return err
	}
	if !loaded {
		if !l.installed() {
			return ErrNotInstalled
		}
		return nil // already stopped
	}
	if out, err := l.Run("launchctl", "bootout", l.target()); err != nil && !launchdNotFound(out, err) {
		return fmt.Errorf("launchctl bootout %s: %v: %s", l.target(), err, out)
	}
	l.waitUntilGone()
	return nil
}

// Restart kills the running agent and starts it again (kickstart -k), or loads it when launchd does not know it.
// The agent gets SIGTERM, so it stops its sessions as on any shutdown.
func (l *Launchd) Restart() error {
	if !l.installed() {
		return ErrNotInstalled
	}
	loaded, err := l.loaded()
	if err != nil {
		return err
	}
	if !loaded {
		return l.bootstrap()
	}
	if out, err := l.Run("launchctl", "kickstart", "-k", l.target()); err != nil {
		return fmt.Errorf("launchctl kickstart -k %s: %v: %s", l.target(), err, out)
	}
	return nil
}

// shellQuote single-quotes s for a POSIX shell when it holds anything but safe characters.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// waitUntilGone polls launchctl print until launchd no longer knows the label, for at most launchdBootoutTimeout.
// Any other answer than "loaded" or "not found" ends the wait: the bootstrap that follows reports the real problem.
func (l *Launchd) waitUntilGone() {
	target := l.domain() + "/" + l.Name()
	for waited := time.Duration(0); ; waited += launchdBootoutPoll {
		if _, err := l.Run("launchctl", "print", target); err != nil || waited >= launchdBootoutTimeout {
			return
		}
		l.sleep(launchdBootoutPoll)
	}
}

// launchdBusy reports whether bootstrap failed with EIO, which launchd returns while the old job is still going away.
func launchdBusy(out string, err error) bool {
	return exitCode(err) == 5 || strings.Contains(out, "Input/output error")
}

func (l *Launchd) sleep(d time.Duration) {
	if l.Sleep != nil {
		l.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Uninstall unloads the agent and deletes the plist. A label launchd does not know is already stopped; any other
// bootout failure is returned before the plist is touched, so the CLI never says it removed an agent that still runs.
func (l *Launchd) Uninstall() error {
	target := l.domain() + "/" + l.Name()
	if out, err := l.Run("launchctl", "print", target); err != nil {
		// Only a job launchd confirms it does not know is already stopped. Any other failure (permissions, the
		// service manager) leaves its state unknown: keep the plist and say so.
		if !launchdNotFound(out, err) {
			return fmt.Errorf("launchctl print %s: %v: %s", target, err, out)
		}
	} else if out, err := l.Run("launchctl", "bootout", target); err != nil && !launchdNotFound(out, err) {
		return fmt.Errorf("launchctl bootout %s: %v: %s", target, err, out)
	}
	if err := os.Remove(l.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// launchdNotFoundExit is launchctl's exit status for an unknown service (ESRCH as reported by launchctl: 113).
const launchdNotFoundExit = 113

// launchdNotFound reports whether launchctl failed because the job is not loaded, and nothing else: exit 113, or
// launchctl's own "Could not find service" message.
func launchdNotFound(out string, err error) bool {
	if err == nil {
		return false
	}
	return exitCode(err) == launchdNotFoundExit || strings.Contains(out, "Could not find service")
}

// Status asks launchctl print about the label.
func (l *Launchd) Status() (Status, error) {
	st := Status{Path: l.Path()}
	if _, err := os.Stat(l.Path()); err == nil {
		st.Installed = true
	}
	out, err := l.Run("launchctl", "print", l.domain()+"/"+l.Name())
	if err != nil {
		st.Detail = "not loaded"
		return st, nil
	}
	st.Detail = "loaded"
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "state = "); ok {
			st.Detail = v
			st.Running = v == "running"
			break
		}
	}
	return st, nil
}
