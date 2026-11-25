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

	"github.com/collies-ai/collie-agent/internal/config"
)

// Launchd manages a launchd user agent in ~/Library/LaunchAgents.
type Launchd struct {
	Home    string
	UID     int
	Run     Commander
	Profile config.Profile
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

// Install writes the plist and (re)loads it with launchctl bootstrap.
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
	_, _ = l.Run("launchctl", "bootout", l.domain()+"/"+l.Name())
	if out, err := l.Run("launchctl", "bootstrap", l.domain(), l.Path()); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	return nil
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

// launchdNotFound reports whether launchctl failed because the job is not loaded, and nothing else.
func launchdNotFound(out string, err error) bool {
	if exitCode(err) == launchdNotFoundExit {
		return true
	}
	return strings.Contains(out, "Could not find service") || strings.Contains(out, "No such process")
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
