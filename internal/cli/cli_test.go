package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/pairing"
)

type harness struct {
	app      *App
	out, err *bytes.Buffer
	base     string
	home     string
	commands []string
	opened   []string
}

func newHarness(t *testing.T, goos string) *harness {
	t.Helper()
	h := &harness{out: &bytes.Buffer{}, err: &bytes.Buffer{}, base: t.TempDir(), home: t.TempDir()}
	t.Setenv("COLLIE_AGENT_CONFIG_DIR", h.base)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("COLLIE_AGENT_PROFILE", "")
	h.app = &App{
		Stdout: h.out, Stderr: h.err, Getenv: os.Getenv, GOOS: goos, Home: h.home, UID: 501,
		Commander: func(name string, args ...string) (string, error) {
			h.commands = append(h.commands, strings.Join(append([]string{name}, args...), " "))
			if name == "launchctl" && len(args) > 0 && args[0] == "print" {
				// No agent is loaded: service install does not wait for one to go away.
				return "Could not find service", errors.New("exit status 113")
			}
			return "", nil
		},
		Executable:  func() (string, error) { return "/usr/local/bin/collie-agent", nil },
		OpenBrowser: func(u string) error { h.opened = append(h.opened, u); return nil },
		Hostname:    func() (string, error) { return "ana-mbp", nil },
		Context:     context.Background(),
		PairingClient: func(server string) *pairing.Client {
			c := pairing.NewClient(server)
			c.Sleep = func(context.Context, time.Duration) error { return nil }
			return c
		},
		RunSleep: func(context.Context, time.Duration) error { return nil },
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.out.Reset()
	h.err.Reset()
	return h.app.Run(args)
}

// collieServer approves the device on the second poll and answers heartbeats with heartbeatStatus.
func collieServer(t *testing.T, heartbeatStatus *atomic.Int32) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/device_authorization":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dev", "user_code": "BCDF-GHJK", "verification_uri": "https://collie.test/device",
				"verification_uri_complete": "https://collie.test/device?user_code=BCDF-GHJK", "expires_in": 600, "interval": 5,
				"name": r.PostForm.Get("name"),
			})
		case "/oauth/token":
			if polls.Add(1) == 1 {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"machine_id": 42, "machine_token": "machine-secret", "token_type": "Bearer"}`))
		case "/machine/v1/heartbeat":
			if heartbeatStatus != nil && heartbeatStatus.Load() != 200 {
				w.WriteHeader(int(heartbeatStatus.Load()))
				return
			}
			_, _ = w.Write([]byte(`{"bindings_version": 1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPairSavesCredentialsWith0600(t *testing.T) {
	h := newHarness(t, "linux")
	srv := collieServer(t, nil)
	if code := h.run("pair", "--server", srv.URL); code != 0 {
		t.Fatalf("pair = %d: %s", code, h.err)
	}
	if !strings.Contains(h.out.String(), "BCDF-GHJK") || !strings.Contains(h.out.String(), "https://collie.test/device") {
		t.Errorf("pair did not print the code and URL:\n%s", h.out)
	}
	if len(h.opened) != 1 || h.opened[0] != "https://collie.test/device?user_code=BCDF-GHJK" {
		t.Errorf("opened = %v", h.opened)
	}
	path := filepath.Join(h.base, "collie-agent", "credentials.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("credentials mode = %o", info.Mode().Perm())
	}
	var c config.Credentials
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &c)
	if c.MachineID != "42" || c.MachineToken != "machine-secret" || c.Server != srv.URL || c.Name != "ana-mbp" {
		t.Errorf("credentials = %+v", c)
	}

	// Pairing again needs --force.
	if code := h.run("pair", "--server", srv.URL); code == 0 || !strings.Contains(h.err.String(), "--force") {
		t.Errorf("second pair = %d: %s", code, h.err)
	}
}

func TestPairNoBrowser(t *testing.T) {
	h := newHarness(t, "linux")
	srv := collieServer(t, nil)
	if code := h.run("pair", "--no-browser", "--server", srv.URL); code != 0 {
		t.Fatalf("pair = %d: %s", code, h.err)
	}
	if len(h.opened) != 0 {
		t.Errorf("opened a browser: %v", h.opened)
	}
	if !strings.Contains(h.out.String(), "Or open https://collie.test/device?user_code=BCDF-GHJK") {
		t.Errorf("output:\n%s", h.out)
	}
}

func TestPairRejectsPlainHTTP(t *testing.T) {
	h := newHarness(t, "linux")
	if code := h.run("pair", "--server", "http://collies.sh"); code == 0 {
		t.Error("accepted http://collies.sh")
	}
}

func TestProfileIsolatesConfigAndService(t *testing.T) {
	h := newHarness(t, "linux")
	srv := collieServer(t, nil)
	if code := h.run("pair", "--server", srv.URL); code != 0 {
		t.Fatalf("pair default = %d: %s", code, h.err)
	}
	if code := h.run("--profile", "work", "pair", "--server", srv.URL); code != 0 {
		t.Fatalf("pair work = %d: %s", code, h.err)
	}
	for _, dir := range []string{"collie-agent", "collie-agent-work"} {
		if _, err := os.Stat(filepath.Join(h.base, dir, "credentials.json")); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
	work, _ := os.ReadFile(filepath.Join(h.base, "collie-agent-work", "credentials.json"))
	if !strings.Contains(string(work), `"name": "ana-mbp (work)"`) {
		t.Errorf("work credentials: %s", work)
	}

	if code := h.run("service", "install"); code != 0 {
		t.Fatalf("service install = %d: %s", code, h.err)
	}
	if code := h.run("service", "install", "--profile=work"); code != 0 {
		t.Fatalf("service install work = %d: %s", code, h.err)
	}
	unitDir := filepath.Join(h.home, ".config", "systemd", "user")
	def, err := os.ReadFile(filepath.Join(unitDir, "collie-agent.service"))
	if err != nil {
		t.Fatal(err)
	}
	wk, err := os.ReadFile(filepath.Join(unitDir, "collie-agent-work.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(def), `ExecStart="/usr/local/bin/collie-agent" "run" "--service"`+"\n") {
		t.Errorf("default unit:\n%s", def)
	}
	if !strings.Contains(string(wk), `ExecStart="/usr/local/bin/collie-agent" "run" "--service" "--profile" "work"`) {
		t.Errorf("work unit:\n%s", wk)
	}
	joined := strings.Join(h.commands, "\n")
	if !strings.Contains(joined, "systemctl --user enable collie-agent.service") || !strings.Contains(joined, "systemctl --user enable collie-agent-work.service") {
		t.Errorf("commands:\n%s", joined)
	}

	// The env var picks the profile too.
	t.Setenv("COLLIE_AGENT_PROFILE", "work")
	h.run("status")
	if !strings.Contains(h.out.String(), "Profile:     work") {
		t.Errorf("status with COLLIE_AGENT_PROFILE:\n%s", h.out)
	}
}

func TestLaunchdServiceInstall(t *testing.T) {
	h := newHarness(t, "darwin")
	srv := collieServer(t, nil)
	h.run("pair", "--server", srv.URL) // pairs the default profile only
	plist := filepath.Join(h.home, "Library", "LaunchAgents", "sh.collies.collie-agent-work.plist")
	if code := h.run("--profile", "work", "service", "install"); code == 0 {
		t.Error("installed the service of an unpaired profile")
	}
	if _, err := os.Stat(plist); err == nil {
		t.Error("wrote a plist for an unpaired profile")
	}
	h.run("pair", "--profile", "work", "--server", srv.URL)
	if code := h.run("service", "install", "--profile", "work"); code != 0 {
		t.Fatalf("service install = %d: %s", code, h.err)
	}
	if _, err := os.Stat(plist); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(h.commands, "\n"), "launchctl bootstrap gui/501 "+plist) {
		t.Errorf("commands: %v", h.commands)
	}
	if code := h.run("service", "uninstall", "--profile", "work"); code != 0 {
		t.Fatalf("uninstall = %d", code)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Error("plist still there")
	}
}

func TestServiceInstallNeedsPairing(t *testing.T) {
	h := newHarness(t, "linux")
	if code := h.run("service", "install"); code == 0 || !strings.Contains(h.err.String(), "not paired") {
		t.Errorf("service install = %d: %s", code, h.err)
	}
}

func TestRunStopsOnRevocation(t *testing.T) {
	var status atomic.Int32
	status.Store(401)
	h := newHarness(t, "linux")
	srv := collieServer(t, &status)
	h.run("pair", "--server", srv.URL)

	if code := h.run("run"); code != exitStopped {
		t.Errorf("run = %d, want %d", code, exitStopped)
	}
	if !strings.Contains(h.err.String(), "revoked") {
		t.Errorf("log does not say revoked:\n%s", h.err)
	}
	if code := h.run("run", "--service"); code != 0 {
		t.Errorf("run --service = %d, want 0 so the service manager does not restart it", code)
	}
	h.run("status")
	if !strings.Contains(h.out.String(), "Connection:  revoked") {
		t.Errorf("status:\n%s", h.out)
	}
}

func TestRunNotPaired(t *testing.T) {
	h := newHarness(t, "linux")
	if code := h.run("run"); code != exitStopped {
		t.Errorf("run = %d", code)
	}
	if code := h.run("run", "--service"); code != 0 {
		t.Errorf("run --service = %d", code)
	}
}

func TestRunHeartbeatsUntilStopped(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	h := newHarness(t, "linux")
	srv := collieServer(t, &status)
	h.run("pair", "--server", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	h.app.Context = ctx
	h.app.RunSleep = func(sctx context.Context, d time.Duration) error {
		// Several loops sleep concurrently (heartbeat, inventory, connection): stop after a few heartbeats' worth.
		if n.Add(1) >= 3 {
			cancel()
			return context.Canceled
		}
		time.Sleep(5 * time.Millisecond)
		return sctx.Err()
	}
	if code := h.run("run"); code != 0 {
		t.Fatalf("run = %d: %s", code, h.err)
	}
	st, _ := os.ReadFile(filepath.Join(h.base, "collie-agent", "state.json"))
	if !strings.Contains(string(st), `"last_heartbeat_at"`) || !strings.Contains(string(st), `"machine_id": "42"`) {
		t.Errorf("state: %s", st)
	}
}

func TestUsageAndVersion(t *testing.T) {
	h := newHarness(t, "linux")
	if code := h.run(); code != exitUsage {
		t.Errorf("no args = %d", code)
	}
	if code := h.run("version"); code != 0 || !strings.HasPrefix(h.out.String(), "collie-agent ") {
		t.Errorf("version = %d %q", code, h.out)
	}
	if code := h.run("bogus"); code != exitUsage {
		t.Errorf("bogus = %d", code)
	}
	if code := h.run("--profile", "../etc", "status"); code != exitUsage {
		t.Errorf("bad profile = %d", code)
	}
	if code := h.run("status", "--profile"); code != exitUsage {
		t.Errorf("--profile without a name = %d", code)
	}
}

func TestServiceGetsAnAbsoluteConfigDir(t *testing.T) {
	h := newHarness(t, "linux")
	srv := collieServer(t, nil)
	work := t.TempDir()
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COLLIE_AGENT_CONFIG_DIR", "./config")
	t.Setenv("XDG_CONFIG_HOME", "relative/xdg")
	if code := h.run("pair", "--server", srv.URL); code != 0 {
		t.Fatalf("pair = %d: %s", code, h.err)
	}
	if code := h.run("service", "install"); code != 0 {
		t.Fatalf("service install = %d: %s", code, h.err)
	}
	unit, err := os.ReadFile(filepath.Join(h.home, ".config", "systemd", "user", "collie-agent.service"))
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.EvalSymlinks(work)
	got := string(unit)
	if !strings.Contains(got, `Environment="COLLIE_AGENT_CONFIG_DIR=`+filepath.Join(work, "config")+`"`) &&
		!strings.Contains(got, `Environment="COLLIE_AGENT_CONFIG_DIR=`+filepath.Join(abs, "config")+`"`) {
		t.Errorf("unit does not pass the absolute config dir:\n%s", got)
	}
	if strings.Contains(got, "XDG_CONFIG_HOME") {
		t.Errorf("unit passes a relative XDG_CONFIG_HOME:\n%s", got)
	}
}

func TestRootsAddListRemove(t *testing.T) {
	h := newHarness(t, "linux")
	dir := t.TempDir()
	if code := h.run("roots", "list"); code != 0 || !strings.Contains(h.out.String(), "No root folders") {
		t.Errorf("empty list = %d %q", code, h.out)
	}
	if code := h.run("roots", "add", dir); code != 0 {
		t.Fatalf("add = %d: %s", code, h.err)
	}
	if code := h.run("roots", "add", dir); code != 0 || !strings.Contains(h.out.String(), "already") {
		t.Errorf("add twice = %d %q", code, h.out)
	}
	if code := h.run("roots", "add", filepath.Join(dir, "missing")); code == 0 {
		t.Error("added a missing folder")
	}
	h.run("roots")
	if strings.TrimSpace(h.out.String()) != dir {
		t.Errorf("list = %q", h.out)
	}
	cfg, _ := os.ReadFile(filepath.Join(h.base, "collie-agent", "config.json"))
	if !strings.Contains(string(cfg), dir) {
		t.Errorf("config.json = %s", cfg)
	}
	// Profiles keep their own roots.
	h.run("--profile", "work", "roots", "list")
	if !strings.Contains(h.out.String(), "No root folders") {
		t.Errorf("work profile roots = %q", h.out)
	}
	h.run("status")
	if !strings.Contains(h.out.String(), "Roots:       "+dir) {
		t.Errorf("status:\n%s", h.out)
	}
	if code := h.run("roots", "remove", dir); code != 0 {
		t.Fatalf("remove = %d: %s", code, h.err)
	}
	if code := h.run("roots", "remove", dir); code == 0 {
		t.Error("removed a folder that is not a root")
	}
	if code := h.run("roots", "bogus"); code != exitUsage {
		t.Errorf("bogus = %d", code)
	}
}

// discoverIn reads the discover key of the default profile's config.json: (nil, false) when absent.
func discoverIn(t *testing.T, h *harness) ([]string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.base, "collie-agent", "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	v, ok := raw["discover"]
	if !ok {
		return nil, false
	}
	var out []string
	if err := json.Unmarshal(v, &out); err != nil {
		t.Fatal(err)
	}
	return out, true
}

func TestPairAsksAboutDiscovery(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"\n", []string{"claude", "codex"}},
		{"y\n", []string{"claude", "codex"}},
		{"Yes\n", []string{"claude", "codex"}},
		{"", []string{"claude", "codex"}}, // EOF counts as Enter
		{"n\n", []string{}},
		{"no\n", []string{}},
	}
	for _, c := range cases {
		h := newHarness(t, "linux")
		srv := collieServer(t, nil)
		h.app.Stdin = strings.NewReader(c.input)
		h.app.IsTerminal = func() bool { return true }
		if code := h.run("pair", "--server", srv.URL); code != 0 {
			t.Fatalf("pair = %d: %s", code, h.err)
		}
		if !strings.Contains(h.out.String(), "Use the repos you already opened with Claude Code or Codex? [Y/n]") {
			t.Errorf("%q: not asked:\n%s", c.input, h.out)
		}
		got, ok := discoverIn(t, h)
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: discover = %v (set %v), want %v", c.input, got, ok, c.want)
		}
	}
}

func TestPairWithoutATerminalAsksNothing(t *testing.T) {
	h := newHarness(t, "linux")
	srv := collieServer(t, nil)
	h.app.Stdin = strings.NewReader("y\n")
	h.app.IsTerminal = func() bool { return false }
	if code := h.run("pair", "--server", srv.URL); code != 0 {
		t.Fatalf("pair = %d: %s", code, h.err)
	}
	if strings.Contains(h.out.String(), "[Y/n]") {
		t.Errorf("asked without a terminal:\n%s", h.out)
	}
	if got, ok := discoverIn(t, h); ok {
		t.Errorf("discover written without a terminal: %v", got)
	}
}

func TestPairDiscoverFlagAndForceKeepsTheAnswer(t *testing.T) {
	h := newHarness(t, "linux")
	srv := collieServer(t, nil)
	h.app.IsTerminal = func() bool { return true }
	h.app.Stdin = strings.NewReader("y\n")
	if code := h.run("pair", "--server", srv.URL, "--discover", "claude"); code != 0 {
		t.Fatalf("pair = %d: %s", code, h.err)
	}
	if strings.Contains(h.out.String(), "[Y/n]") {
		t.Errorf("asked although --discover was given:\n%s", h.out)
	}
	if got, _ := discoverIn(t, h); !reflect.DeepEqual(got, []string{"claude"}) {
		t.Errorf("discover = %v", got)
	}

	// pair --force does not ask again and keeps the earlier answer.
	h.app.Stdin = strings.NewReader("n\n")
	if code := h.run("pair", "--server", srv.URL, "--force"); code != 0 {
		t.Fatalf("pair --force = %d: %s", code, h.err)
	}
	if strings.Contains(h.out.String(), "[Y/n]") {
		t.Errorf("asked again on --force:\n%s", h.out)
	}
	if got, _ := discoverIn(t, h); !reflect.DeepEqual(got, []string{"claude"}) {
		t.Errorf("discover after --force = %v", got)
	}

	// An explicit --discover changes it.
	if code := h.run("pair", "--server", srv.URL, "--force", "--discover=none"); code != 0 {
		t.Fatalf("pair --discover=none = %d: %s", code, h.err)
	}
	if got, ok := discoverIn(t, h); !ok || len(got) != 0 {
		t.Errorf("discover after none = %v (set %v)", got, ok)
	}
	if code := h.run("status"); code != 0 || !strings.Contains(h.out.String(), "Discovery:   off") {
		t.Errorf("status = %d:\n%s", code, h.out)
	}

	if code := h.run("pair", "--server", srv.URL, "--force", "--discover", "gitlab"); code != exitUsage {
		t.Errorf("--discover gitlab = %d", code)
	}
}
