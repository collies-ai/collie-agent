package service

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collies-ai/collie-agent/internal/config"
)

func profile(t *testing.T, name string) config.Profile {
	t.Helper()
	t.Setenv("COLLIE_AGENT_CONFIG_DIR", t.TempDir())
	p, err := config.Resolve(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func spec(p config.Profile) Spec {
	return Spec{Profile: p, Executable: "/Users/ana/.local/bin/collie-agent", Env: map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin:/bin", "COLLIE_AGENT_CONFIG_DIR": "/tmp/a & b"}}
}

// fakeRun records commands and answers from a script keyed by the command line.
type fakeRun struct {
	calls   []string
	answers map[string]string
	fail    map[string]bool
	// exits gives a failing command a real exit status (an *exec.ExitError), as launchctl and systemctl return.
	exits map[string]int
	// seq answers successive calls of a command line in order; the last step repeats. It wins over the maps above.
	seq map[string][]step
}

// step is one scripted answer: code 0 succeeds, any other code fails with that exit status.
type step struct {
	out  string
	code int
}

func (f *fakeRun) run(name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if steps := f.seq[line]; len(steps) > 0 {
		s := steps[0]
		if len(steps) > 1 {
			f.seq[line] = steps[1:]
		}
		if s.code != 0 {
			return s.out, exitWith(s.code)
		}
		return s.out, nil
	}
	if code, ok := f.exits[line]; ok {
		return f.answers[line], exitWith(code)
	}
	if f.fail[line] {
		return f.answers[line], fmt.Errorf("exit status 3")
	}
	return f.answers[line], nil
}

// exitWith returns the *exec.ExitError of a real process that exits with code.
func exitWith(code int) error {
	return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
}

// plistValue is a decoded property list value: string, bool, int (as string), []any or map[string]any.
func decodePlist(t *testing.T, doc string) map[string]any {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(doc))
	d.Strict = true
	// Skip to <plist> and decode its single <dict>.
	for {
		tok, err := d.Token()
		if err != nil {
			t.Fatalf("plist is not valid XML: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "dict" {
			v := decodeDict(t, d)
			// Nothing but the closing tags may follow.
			for {
				tok, err := d.Token()
				if err == io.EOF {
					return v
				}
				if err != nil {
					t.Fatalf("plist is not valid XML: %v", err)
				}
				if _, ok := tok.(xml.StartElement); ok {
					t.Fatal("plist has more than one top-level value")
				}
			}
		}
	}
}

func decodeValue(t *testing.T, d *xml.Decoder, se xml.StartElement) any {
	t.Helper()
	switch se.Name.Local {
	case "dict":
		return decodeDict(t, d)
	case "array":
		var out []any
		for {
			tok, err := d.Token()
			if err != nil {
				t.Fatal(err)
			}
			switch tt := tok.(type) {
			case xml.StartElement:
				out = append(out, decodeValue(t, d, tt))
			case xml.EndElement:
				return out
			}
		}
	case "true", "false":
		if err := d.Skip(); err != nil {
			t.Fatal(err)
		}
		return se.Name.Local == "true"
	case "string", "integer":
		var s string
		if err := d.DecodeElement(&s, &se); err != nil {
			t.Fatal(err)
		}
		return s
	default:
		t.Fatalf("unexpected plist element <%s>", se.Name.Local)
		return nil
	}
}

func decodeDict(t *testing.T, d *xml.Decoder) map[string]any {
	t.Helper()
	out := map[string]any{}
	var key string
	for {
		tok, err := d.Token()
		if err != nil {
			t.Fatal(err)
		}
		switch tt := tok.(type) {
		case xml.StartElement:
			if tt.Name.Local == "key" {
				if key != "" {
					t.Fatalf("key %q has no value", key)
				}
				if err := d.DecodeElement(&key, &tt); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if key == "" {
				t.Fatalf("value <%s> without a key", tt.Name.Local)
			}
			out[key] = decodeValue(t, d, tt)
			key = ""
		case xml.EndElement:
			if key != "" {
				t.Fatalf("key %q has no value", key)
			}
			return out
		}
	}
}

func TestLaunchdPlist(t *testing.T) {
	p := profile(t, "")
	l := &Launchd{Home: "/Users/ana", UID: 501, Profile: p}
	doc, err := l.Render(spec(p))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc, `<?xml version="1.0" encoding="UTF-8"?>`) || !strings.Contains(doc, `<!DOCTYPE plist`) {
		t.Error("missing XML declaration or plist doctype")
	}
	v := decodePlist(t, doc)
	if v["Label"] != "sh.collies.collie-agent" {
		t.Errorf("Label = %v", v["Label"])
	}
	args := fmt.Sprint(v["ProgramArguments"])
	if args != "[/Users/ana/.local/bin/collie-agent run --service]" {
		t.Errorf("ProgramArguments = %s", args)
	}
	if v["RunAtLoad"] != true {
		t.Error("RunAtLoad must be true")
	}
	ka, _ := v["KeepAlive"].(map[string]any)
	if ka["SuccessfulExit"] != false {
		t.Errorf("KeepAlive = %v, want SuccessfulExit false (restart only on failure)", v["KeepAlive"])
	}
	env, _ := v["EnvironmentVariables"].(map[string]any)
	if env["PATH"] != "/opt/homebrew/bin:/usr/bin:/bin" || env["COLLIE_AGENT_CONFIG_DIR"] != "/tmp/a & b" {
		t.Errorf("EnvironmentVariables = %v", env)
	}
	if v["StandardOutPath"] != "/Users/ana/Library/Logs/collie-agent.log" {
		t.Errorf("StandardOutPath = %v", v["StandardOutPath"])
	}
	if l.Path() != "/Users/ana/Library/LaunchAgents/sh.collies.collie-agent.plist" {
		t.Errorf("Path = %s", l.Path())
	}

	// On a Mac, plutil has the last word.
	if plutil, err := exec.LookPath("plutil"); err == nil {
		f := filepath.Join(t.TempDir(), "agent.plist")
		_ = os.WriteFile(f, []byte(doc), 0o644)
		if out, err := exec.Command(plutil, "-lint", f).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v: %s", err, out)
		}
	}
}

func TestSystemdUnit(t *testing.T) {
	p := profile(t, "")
	d := &Systemd{ConfigHome: "/home/ana/.config", Profile: p}
	unit, err := d.Render(Spec{Profile: p, Executable: "/home/ana/.local/bin/collie-agent", Env: map[string]string{"PATH": "/usr/bin:/bin", "X": `50% "$HOME"`}})
	if err != nil {
		t.Fatal(err)
	}
	sections := parseUnit(t, unit)
	if got := sections["Service"]["ExecStart"]; len(got) != 1 || got[0] != `"/home/ana/.local/bin/collie-agent" "run" "--service"` {
		t.Errorf("ExecStart = %q", got)
	}
	if got := sections["Service"]["Restart"]; len(got) != 1 || got[0] != "on-failure" {
		t.Errorf("Restart = %q", got)
	}
	if got := sections["Install"]["WantedBy"]; len(got) != 1 || got[0] != "default.target" {
		t.Errorf("WantedBy = %q", got)
	}
	envs := sections["Service"]["Environment"]
	if len(envs) != 2 || envs[0] != `"PATH=/usr/bin:/bin"` || envs[1] != `"X=50%% \"$$HOME\""` {
		t.Errorf("Environment = %q", envs)
	}
	if d.Path() != "/home/ana/.config/systemd/user/collie-agent.service" {
		t.Errorf("Path = %s", d.Path())
	}

	// Where systemd is installed, let it check the unit too.
	if analyze, err := exec.LookPath("systemd-analyze"); err == nil {
		dir := t.TempDir()
		exe := filepath.Join(dir, "collie-agent")
		_ = os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
		u, _ := d.Render(Spec{Profile: p, Executable: exe})
		f := filepath.Join(dir, "collie-agent.service")
		_ = os.WriteFile(f, []byte(u), 0o644)
		// System mode: --user needs a user runtime directory that CI containers lack. The syntax is the same, and
		// systemd-analyze exits 0 on bad settings it ignores, so any line about our file is a failure. Lines about
		// the host's own units (e.g. deprecated options in /usr/lib/systemd) are not ours.
		out, err := exec.Command(analyze, "verify", f).CombinedOutput()
		if err != nil {
			t.Errorf("systemd-analyze verify: %v: %s", err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, f) {
				t.Errorf("systemd-analyze verify: %s", line)
			}
		}
	}
}

// parseUnit reads a unit file into section → key → values, failing on anything that is not a section, a comment, a
// blank line or key=value.
func parseUnit(t *testing.T, unit string) map[string]map[string][]string {
	t.Helper()
	out := map[string]map[string][]string{}
	section := ""
	for i, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			out[section] = map[string][]string{}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || section == "" {
				t.Fatalf("line %d is not key=value inside a section: %q", i+1, line)
			}
			out[section][k] = append(out[section][k], v)
		}
	}
	for _, s := range []string{"Unit", "Service", "Install"} {
		if _, ok := out[s]; !ok {
			t.Errorf("missing [%s]", s)
		}
	}
	return out
}

func TestProfilesGetSeparateServices(t *testing.T) {
	def := profile(t, "")
	work := profile(t, "work")
	if LaunchdLabel(def) == LaunchdLabel(work) || SystemdUnit(def) == SystemdUnit(work) {
		t.Fatal("profiles share a service name")
	}
	if LaunchdLabel(work) != "sh.collies.collie-agent-work" || SystemdUnit(work) != "collie-agent-work.service" {
		t.Errorf("work names = %s, %s", LaunchdLabel(work), SystemdUnit(work))
	}

	l := &Launchd{Home: "/Users/ana", UID: 501, Profile: work}
	doc, _ := l.Render(spec(work))
	v := decodePlist(t, doc)
	if fmt.Sprint(v["ProgramArguments"]) != "[/Users/ana/.local/bin/collie-agent run --service --profile work]" {
		t.Errorf("ProgramArguments = %v", v["ProgramArguments"])
	}
	if v["StandardOutPath"] != "/Users/ana/Library/Logs/collie-agent-work.log" {
		t.Errorf("log = %v", v["StandardOutPath"])
	}
	if l.Path() != "/Users/ana/Library/LaunchAgents/sh.collies.collie-agent-work.plist" {
		t.Errorf("plist path = %s", l.Path())
	}

	d := &Systemd{ConfigHome: "/home/ana/.config", Profile: work}
	unit, _ := d.Render(spec(work))
	if !strings.Contains(unit, `ExecStart="/Users/ana/.local/bin/collie-agent" "run" "--service" "--profile" "work"`) {
		t.Errorf("unit:\n%s", unit)
	}
	if !strings.Contains(unit, "(profile work)") {
		t.Error("description does not name the profile")
	}
	if d.Path() != "/home/ana/.config/systemd/user/collie-agent-work.service" {
		t.Errorf("unit path = %s", d.Path())
	}
}

func TestRenderNeedsAnAbsoluteExecutable(t *testing.T) {
	p := profile(t, "")
	if _, err := (&Systemd{Profile: p}).Render(Spec{Profile: p, Executable: "collie-agent"}); err == nil {
		t.Error("systemd accepted a relative executable")
	}
	if _, err := (&Launchd{Profile: p}).Render(Spec{Profile: p, Executable: "collie-agent"}); err == nil {
		t.Error("launchd accepted a relative executable")
	}
}

func TestLaunchdInstallUninstallStatus(t *testing.T) {
	p := profile(t, "work")
	home := t.TempDir()
	running := "sh.collies.collie-agent-work = {\n\tactive count = 1\n\tstate = running\n}"
	f := &fakeRun{seq: map[string][]step{
		// Gone once the bootout finished, loaded after the bootstrap.
		"launchctl print gui/501/sh.collies.collie-agent-work": {{"Could not find service", 113}, {running, 0}},
	}}
	l := &Launchd{Home: home, UID: 501, Run: f.run, Profile: p, Sleep: func(time.Duration) {}}
	if err := l.Install(spec(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Path()); err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	want := []string{
		"launchctl bootout gui/501/sh.collies.collie-agent-work",
		"launchctl print gui/501/sh.collies.collie-agent-work",
		"launchctl bootstrap gui/501 " + l.Path(),
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
	st, _ := l.Status()
	if !st.Installed || !st.Running || st.Detail != "running" {
		t.Errorf("status = %+v", st)
	}
	if err := l.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Path()); !os.IsNotExist(err) {
		t.Error("plist still there after uninstall")
	}
}

func TestSystemdInstallUninstallStatus(t *testing.T) {
	p := profile(t, "")
	cfg := t.TempDir()
	f := &fakeRun{answers: map[string]string{"systemctl --user is-active collie-agent.service": "inactive"},
		fail: map[string]bool{"systemctl --user is-active collie-agent.service": true}}
	d := &Systemd{ConfigHome: cfg, Run: f.run, Profile: p}
	if err := d.Install(spec(p)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable collie-agent.service",
		"systemctl --user restart collie-agent.service",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
	st, _ := d.Status()
	if !st.Installed || st.Running || st.Detail != "inactive" {
		t.Errorf("status = %+v", st)
	}
	if err := d.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.Path()); !os.IsNotExist(err) {
		t.Error("unit still there after uninstall")
	}
}

func TestSystemdInstallReportsFailures(t *testing.T) {
	p := profile(t, "")
	f := &fakeRun{answers: map[string]string{"systemctl --user daemon-reload": "Failed to connect to bus"},
		fail: map[string]bool{"systemctl --user daemon-reload": true}}
	d := &Systemd{ConfigHome: t.TempDir(), Run: f.run, Profile: p}
	err := d.Install(spec(p))
	if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewPicksTheOSManager(t *testing.T) {
	p := profile(t, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	m, _ := New(Options{GOOS: "darwin", Home: "/Users/ana", Profile: p})
	if _, ok := m.(*Launchd); !ok {
		t.Errorf("darwin: %T", m)
	}
	m, _ = New(Options{GOOS: "linux", Home: "/home/ana", Profile: p})
	if s, ok := m.(*Systemd); !ok || s.ConfigHome != "/home/ana/.config" {
		t.Errorf("linux: %#v", m)
	}
	if _, err := New(Options{GOOS: "windows", Profile: p}); err != ErrUnsupported {
		t.Errorf("windows: %v", err)
	}
}

func TestSystemdUninstallKeepsTheUnitWhenStopFails(t *testing.T) {
	p := profile(t, "")
	f := &fakeRun{answers: map[string]string{"systemctl --user disable --now collie-agent.service": "Failed to connect to bus"},
		fail: map[string]bool{"systemctl --user disable --now collie-agent.service": true}}
	d := &Systemd{ConfigHome: t.TempDir(), Run: f.run, Profile: p}
	_ = writeFile(d.Path(), "[Unit]\n")
	err := d.Uninstall()
	if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(d.Path()); err != nil {
		t.Error("removed the unit although the service could not be stopped")
	}
}

func TestSystemdUninstallReportsReloadFailure(t *testing.T) {
	p := profile(t, "")
	f := &fakeRun{fail: map[string]bool{"systemctl --user daemon-reload": true}}
	d := &Systemd{ConfigHome: t.TempDir(), Run: f.run, Profile: p}
	_ = writeFile(d.Path(), "[Unit]\n")
	if err := d.Uninstall(); err == nil || !strings.Contains(err.Error(), "daemon-reload") {
		t.Fatalf("err = %v", err)
	}
}

func TestSystemdUninstallWithoutUnitFile(t *testing.T) {
	stop := "systemctl --user stop collie-agent.service"
	cases := []struct {
		name    string
		out     string
		exit    int // 0: the stop succeeds
		wantErr bool
	}{
		{"still loaded and running: stopped", "", 0, false},
		{"not loaded (exit 5): already stopped", "Failed to stop collie-agent.service: Unit collie-agent.service not loaded.", 5, false},
		{"manager error: reported", "Failed to connect to bus: No medium found", 1, true},
		{"'not loaded' text with another exit status: reported", "Job failed: unit not loaded", 1, true},
		{"'not found' text with another exit status: reported", "Failed to stop: Unit not found", 4, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := profile(t, "")
			f := &fakeRun{answers: map[string]string{stop: c.out}, exits: map[string]int{}}
			if c.exit != 0 {
				f.exits[stop] = c.exit
			}
			d := &Systemd{ConfigHome: t.TempDir(), Run: f.run, Profile: p}
			err := d.Uninstall()
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error: %v", err, c.wantErr)
			}
			if strings.Join(f.calls, "\n") != stop {
				t.Errorf("calls = %v, want only %q", f.calls, stop)
			}
		})
	}
}

func TestLaunchdUninstallKeepsThePlistWhenBootoutFails(t *testing.T) {
	p := profile(t, "")
	target := "gui/501/sh.collies.collie-agent"
	f := &fakeRun{answers: map[string]string{"launchctl bootout " + target: "Boot-out failed: 5: Input/output error"},
		fail: map[string]bool{"launchctl bootout " + target: true}}
	l := &Launchd{Home: t.TempDir(), UID: 501, Run: f.run, Profile: p}
	_ = writeFile(l.Path(), "<plist/>")
	err := l.Uninstall()
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(l.Path()); err != nil {
		t.Error("removed the plist although the agent could not be stopped")
	}
}

func TestLaunchdUninstallOfAnUnloadedAgent(t *testing.T) {
	p := profile(t, "")
	target := "gui/501/sh.collies.collie-agent"
	f := &fakeRun{answers: map[string]string{"launchctl print " + target: "Bad request.\nCould not find service \"sh.collies.collie-agent\" in domain for user gui: 501"},
		fail: map[string]bool{"launchctl print " + target: true}}
	l := &Launchd{Home: t.TempDir(), UID: 501, Run: f.run, Profile: p}
	_ = writeFile(l.Path(), "<plist/>")
	if err := l.Uninstall(); err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "bootout") {
		t.Errorf("booted out an agent launchd does not know: %v", f.calls)
	}
	if _, err := os.Stat(l.Path()); !os.IsNotExist(err) {
		t.Error("plist still there")
	}
}

func TestLaunchdUninstallKeepsThePlistWhenTheLookupFails(t *testing.T) {
	p := profile(t, "")
	target := "gui/501/sh.collies.collie-agent"
	f := &fakeRun{answers: map[string]string{"launchctl print " + target: "Operation not permitted"},
		fail: map[string]bool{"launchctl print " + target: true}}
	l := &Launchd{Home: t.TempDir(), UID: 501, Run: f.run, Profile: p}
	_ = writeFile(l.Path(), "<plist/>")
	if err := l.Uninstall(); err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(l.Path()); err != nil {
		t.Error("removed the plist although launchd's state is unknown")
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "bootout") {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestNotFoundDetectionUsesTheExitStatus(t *testing.T) {
	run := func(code string) error { return exec.Command("sh", "-c", "exit "+code).Run() }
	if !launchdNotFound("", run("113")) || launchdNotFound("No such process", run("1")) || launchdNotFound("Could not find service", nil) {
		t.Error("launchdNotFound does not follow launchctl's exit status 113")
	}
	if !systemdNotLoaded("", run("5")) || systemdNotLoaded("Unit x.service not loaded.", run("1")) || systemdNotLoaded("not loaded", nil) {
		t.Error("systemdNotLoaded does not follow systemctl's exit status 5")
	}
}

func TestLaunchdUninstallKeepsThePlistOnNoSuchProcessWithAnotherExit(t *testing.T) {
	for _, cmd := range []string{"print", "bootout"} {
		t.Run(cmd, func(t *testing.T) {
			p := profile(t, "")
			target := "gui/501/sh.collies.collie-agent"
			line := "launchctl " + cmd + " " + target
			f := &fakeRun{answers: map[string]string{line: "Boot-out failed: 3: No such process"}, exits: map[string]int{line: 5}}
			l := &Launchd{Home: t.TempDir(), UID: 501, Run: f.run, Profile: p}
			_ = writeFile(l.Path(), "<plist/>")
			if err := l.Uninstall(); err == nil {
				t.Fatal("uninstall succeeded without launchd confirming the job is gone")
			}
			if _, err := os.Stat(l.Path()); err != nil {
				t.Error("removed the plist")
			}
		})
	}
}

func TestLaunchdUninstallAcceptsExit113(t *testing.T) {
	p := profile(t, "")
	target := "gui/501/sh.collies.collie-agent"
	f := &fakeRun{exits: map[string]int{"launchctl print " + target: 113}}
	l := &Launchd{Home: t.TempDir(), UID: 501, Run: f.run, Profile: p}
	_ = writeFile(l.Path(), "<plist/>")
	if err := l.Uninstall(); err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(l.Path()); !os.IsNotExist(err) {
		t.Error("plist still there")
	}
}

const (
	launchdRunning  = "sh.collies.collie-agent = {\n\tactive count = 1\n\tstate = running\n}"
	launchdNotKnown = "Bad request.\nCould not find service \"sh.collies.collie-agent\" in domain for user gui: 501"
	launchdEIO      = "Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors."
)

// count returns how many recorded calls equal line.
func count(calls []string, line string) int {
	n := 0
	for _, c := range calls {
		if c == line {
			n++
		}
	}
	return n
}

// Re-installing over a running agent: launchd keeps the old job for a few seconds after bootout returns, and a
// bootstrap in that window fails with EIO (exit 5). Install must wait for the job to go and retry the bootstrap, and
// end with the agent loaded.
func TestLaunchdInstallRetriesBootstrapAfterBootout(t *testing.T) {
	p := profile(t, "")
	target := "gui/501/sh.collies.collie-agent"
	home := t.TempDir()
	l := &Launchd{Home: home, UID: 501, Profile: p}
	bootstrap := "launchctl bootstrap gui/501 " + l.Path()
	f := &fakeRun{seq: map[string][]step{
		// Still there right after bootout, then gone, then loaded again by the bootstrap.
		"launchctl print " + target: {{launchdRunning, 0}, {launchdNotKnown, 113}, {launchdRunning, 0}},
		bootstrap:                   {{launchdEIO, 5}, {"", 0}},
	}}
	var slept []time.Duration
	l.Run, l.Sleep = f.run, func(d time.Duration) { slept = append(slept, d) }
	if err := l.Install(spec(p)); err != nil {
		t.Fatalf("install: %v\ncalls:\n%s", err, strings.Join(f.calls, "\n"))
	}
	if n := count(f.calls, bootstrap); n != 2 {
		t.Errorf("bootstrap ran %d times, want 2\ncalls:\n%s", n, strings.Join(f.calls, "\n"))
	}
	if f.calls[0] != "launchctl bootout "+target {
		t.Errorf("first call = %q, want the bootout", f.calls[0])
	}
	if f.calls[1] != "launchctl print "+target {
		t.Errorf("second call = %q, want a print waiting for the bootout", f.calls[1])
	}
	if len(slept) == 0 {
		t.Error("install never waited")
	}
	st, _ := l.Status()
	if !st.Running || st.Detail != "running" {
		t.Errorf("status = %+v", st)
	}
}

// A bootstrap that keeps failing is retried a bounded number of times, and the error tells the user how to load the
// agent by hand.
func TestLaunchdInstallGivesUpAndSaysHowToLoad(t *testing.T) {
	p := profile(t, "")
	target := "gui/501/sh.collies.collie-agent"
	l := &Launchd{Home: t.TempDir(), UID: 501, Profile: p}
	bootstrap := "launchctl bootstrap gui/501 " + l.Path()
	f := &fakeRun{seq: map[string][]step{
		"launchctl print " + target: {{launchdNotKnown, 113}},
		bootstrap:                   {{launchdEIO, 5}},
	}}
	var total time.Duration
	l.Run, l.Sleep = f.run, func(d time.Duration) { total += d }
	err := l.Install(spec(p))
	if err == nil {
		t.Fatal("install succeeded with a bootstrap that always fails")
	}
	for _, want := range []string{"Input/output error", bootstrap[len("launchctl "):]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if n := count(f.calls, bootstrap); n < 2 || n > 10 {
		t.Errorf("bootstrap ran %d times", n)
	}
	if total > 30*time.Second {
		t.Errorf("waited %v", total)
	}
}

// Other bootstrap failures (a broken plist, permissions) are not retried.
func TestLaunchdInstallDoesNotRetryOtherFailures(t *testing.T) {
	p := profile(t, "")
	l := &Launchd{Home: t.TempDir(), UID: 501, Profile: p}
	bootstrap := "launchctl bootstrap gui/501 " + l.Path()
	f := &fakeRun{seq: map[string][]step{
		"launchctl print gui/501/sh.collies.collie-agent": {{launchdNotKnown, 113}},
		bootstrap: {{"Bootstrap failed: 1: Operation not permitted", 1}},
	}}
	l.Run, l.Sleep = f.run, func(time.Duration) {}
	if err := l.Install(spec(p)); err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("err = %v", err)
	}
	if n := count(f.calls, bootstrap); n != 1 {
		t.Errorf("bootstrap ran %d times, want 1", n)
	}
}

// A job that never goes away does not hang install: it stops waiting and tries the bootstrap anyway.
func TestLaunchdInstallStopsWaitingForAStuckBootout(t *testing.T) {
	p := profile(t, "")
	l := &Launchd{Home: t.TempDir(), UID: 501, Profile: p}
	bootstrap := "launchctl bootstrap gui/501 " + l.Path()
	f := &fakeRun{seq: map[string][]step{
		"launchctl print gui/501/sh.collies.collie-agent": {{launchdRunning, 0}},
	}}
	var total time.Duration
	l.Run, l.Sleep = f.run, func(d time.Duration) { total += d }
	if err := l.Install(spec(p)); err != nil {
		t.Fatal(err)
	}
	if count(f.calls, bootstrap) != 1 {
		t.Errorf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
	if total == 0 || total > 30*time.Second {
		t.Errorf("waited %v", total)
	}
}

// When the retries run out while launchd is still busy, install waits for the old job once more and makes a last
// attempt before giving up.
func TestLaunchdInstallMakesALastAttemptAfterTheRetries(t *testing.T) {
	p := profile(t, "")
	l := &Launchd{Home: t.TempDir(), UID: 501, Profile: p}
	bootstrap := "launchctl bootstrap gui/501 " + l.Path()
	busy := step{launchdEIO, 5}
	f := &fakeRun{seq: map[string][]step{
		"launchctl print gui/501/sh.collies.collie-agent": {{launchdNotKnown, 113}},
		bootstrap: {busy, busy, busy, busy, busy, {"", 0}},
	}}
	l.Run, l.Sleep = f.run, func(time.Duration) {}
	if err := l.Install(spec(p)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if n := count(f.calls, bootstrap); n != len(launchdBootstrapBackoff)+2 {
		t.Errorf("bootstrap ran %d times, want %d", n, len(launchdBootstrapBackoff)+2)
	}
	last := f.calls[len(f.calls)-2]
	if last != "launchctl print gui/501/sh.collies.collie-agent" {
		t.Errorf("the last attempt did not wait for the old job first; call before it = %q", last)
	}
}

// The command the error suggests can be pasted into a shell even when the home directory has spaces or quotes.
func TestLaunchdInstallErrorQuotesThePlistPath(t *testing.T) {
	p := profile(t, "")
	home := filepath.Join(t.TempDir(), "Ana's Mac")
	l := &Launchd{Home: home, UID: 501, Profile: p}
	f := &fakeRun{seq: map[string][]step{
		"launchctl print gui/501/sh.collies.collie-agent": {{launchdNotKnown, 113}},
		"launchctl bootstrap gui/501 " + l.Path():         {{"Bootstrap failed: 1: Operation not permitted", 1}},
	}}
	l.Run, l.Sleep = f.run, func(time.Duration) {}
	err := l.Install(spec(p))
	if err == nil {
		t.Fatal("install succeeded")
	}
	want := "launchctl bootstrap gui/501 '" + strings.ReplaceAll(l.Path(), "'", `'\''`) + "'"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
	if got := shellQuote("/Users/ana/Library/LaunchAgents/sh.collies.collie-agent.plist"); got != "/Users/ana/Library/LaunchAgents/sh.collies.collie-agent.plist" {
		t.Errorf("a safe path was quoted: %s", got)
	}
}
