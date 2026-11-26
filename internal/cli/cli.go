// Package cli is the collie-agent command line.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/collies-ai/collie-agent/internal/agent"
	"github.com/collies-ai/collie-agent/internal/browser"
	"github.com/collies-ai/collie-agent/internal/cable"
	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/discovery"
	"github.com/collies-ai/collie-agent/internal/machine"
	"github.com/collies-ai/collie-agent/internal/pairing"
	"github.com/collies-ai/collie-agent/internal/service"
	"github.com/collies-ai/collie-agent/internal/session"
	"github.com/collies-ai/collie-agent/internal/version"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
	// exitStopped is used by `run` outside a service when it stops on purpose: the machine was revoked or is not
	// paired. Inside a service (--service) it exits 0 instead, so launchd and systemd do not restart it.
	exitStopped = 78
)

const usage = `collie-agent connects this machine to collie, so collie can wake coding agents here.

Usage:
  collie-agent pair [--server URL] [--name NAME] [--no-browser] [--force] [--discover claude,codex|none]
  collie-agent unpair
  collie-agent service install|uninstall|status
  collie-agent roots add|remove <folder> | roots list
  collie-agent status
  collie-agent run
  collie-agent version

Every command takes --profile NAME (or $COLLIE_AGENT_PROFILE) to work on a separate
instance with its own pairing, config and service. The default is one instance per OS user.
`

// App holds what the commands need from the outside world, so tests can replace it.
type App struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// IsTerminal reports whether Stdin is a terminal a person can answer questions on.
	IsTerminal  func() bool
	Getenv      func(string) string
	GOOS        string
	Home        string
	UID         int
	Commander   service.Commander
	Executable  func() (string, error)
	OpenBrowser func(string) error
	Hostname    func() (string, error)
	// Context is the base context of long-running commands; Main cancels it on SIGINT and SIGTERM.
	Context context.Context
	// PairingClient, when set, replaces the default HTTP client of the device flow (tests).
	PairingClient func(server string) *pairing.Client
	// MachineAPI, when set, replaces the default machine API client (tests).
	MachineAPI func(server, token string) agent.Heartbeater
	// RunSleep, when set, replaces the heartbeat loop's sleep (tests).
	RunSleep func(ctx context.Context, d time.Duration) error
}

// Main runs collie-agent with args and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	home, _ := os.UserHomeDir()
	app := &App{
		Stdin: os.Stdin, IsTerminal: stdinIsTerminal,
		Stdout: stdout, Stderr: stderr, Getenv: os.Getenv, GOOS: runtime.GOOS, Home: home, UID: os.Getuid(),
		Commander: service.ExecCommander, Executable: os.Executable, OpenBrowser: browser.Open, Hostname: os.Hostname,
		Context: ctx,
	}
	return app.Run(args)
}

// Run dispatches a command line.
func (a *App) Run(args []string) int {
	profileName, rest, err := extractProfile(args)
	if err != nil {
		fmt.Fprintln(a.Stderr, "collie-agent:", err)
		return exitUsage
	}
	if profileName == "" {
		profileName = a.Getenv("COLLIE_AGENT_PROFILE")
	}
	if len(rest) == 0 {
		fmt.Fprint(a.Stderr, usage)
		return exitUsage
	}
	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usage)
		return exitOK
	case "version", "--version":
		fmt.Fprintf(a.Stdout, "collie-agent %s (%s/%s)\n", version.Version, a.GOOS, runtime.GOARCH)
		return exitOK
	}
	profile, err := config.Resolve(profileName)
	if err != nil {
		fmt.Fprintln(a.Stderr, "collie-agent:", err)
		return exitUsage
	}
	switch cmd {
	case "pair":
		return a.pair(profile, cmdArgs)
	case "unpair":
		return a.unpair(profile, cmdArgs)
	case "run":
		return a.run(profile, cmdArgs)
	case "status":
		return a.status(profile, cmdArgs)
	case "service":
		return a.service(profile, cmdArgs)
	case "roots":
		return a.roots(profile, cmdArgs)
	default:
		fmt.Fprintf(a.Stderr, "collie-agent: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
}

// extractProfile pulls --profile NAME / --profile=NAME (or -profile) out of args, wherever it is.
func extractProfile(args []string) (string, []string, error) {
	var profile string
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		switch {
		case a == "--profile" || a == "-profile":
			if i+1 >= len(args) {
				return "", nil, errors.New("--profile needs a name")
			}
			profile = args[i+1]
			i++
		case strings.HasPrefix(a, "--profile="):
			profile = strings.TrimPrefix(a, "--profile=")
		case strings.HasPrefix(a, "-profile="):
			profile = strings.TrimPrefix(a, "-profile=")
		default:
			rest = append(rest, a)
		}
	}
	return profile, rest, nil
}

func (a *App) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("collie-agent "+name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	return fs
}

func (a *App) ctx() context.Context {
	if a.Context != nil {
		return a.Context
	}
	return context.Background()
}

func (a *App) fail(err error) int {
	fmt.Fprintln(a.Stderr, "collie-agent:", err)
	return exitError
}

func (a *App) pair(p config.Profile, args []string) int {
	fs := a.newFlagSet("pair")
	server := fs.String("server", "", "collie URL (default "+config.DefaultServer+")")
	name := fs.String("name", "", "name of this machine in collie (default: the hostname)")
	noBrowser := fs.Bool("no-browser", false, "do not open the browser, only print the URL")
	force := fs.Bool("force", false, "pair again even if this profile is already paired")
	discoverFlag := fs.String("discover", "", "where else to find repos: claude,codex (the folders they trust) or none; asked when not given")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	var discover []string
	if *discoverFlag != "" {
		d, err := config.ParseDiscover(*discoverFlag)
		if err != nil {
			fmt.Fprintln(a.Stderr, "collie-agent:", err)
			return exitUsage
		}
		discover = d
	}
	existing, err := p.LoadCredentials()
	if err != nil && !errors.Is(err, config.ErrNotPaired) {
		return a.fail(err)
	}
	if existing != nil && !*force {
		return a.fail(fmt.Errorf("profile %q is already paired as machine #%s with %s; use --force to pair it again", p.Name, existing.MachineID, existing.Server))
	}
	rawServer := *server
	if rawServer == "" {
		rawServer = config.DefaultServer
	}
	srv, err := config.NormalizeServer(rawServer)
	if err != nil {
		return a.fail(err)
	}
	host, _ := a.Hostname()
	if *name == "" {
		*name = host
		if !p.IsDefault() {
			*name = host + " (" + p.Name + ")"
		}
	}
	if strings.TrimSpace(*name) == "" {
		return a.fail(errors.New("cannot tell this machine's hostname: pass --name"))
	}

	client := pairing.NewClient(srv)
	if a.PairingClient != nil {
		client = a.PairingClient(srv)
	}
	ctx := a.ctx()
	da, err := client.Authorize(ctx, pairing.Machine{Name: *name, OS: a.GOOS, Hostname: host})
	if err != nil {
		return a.fail(err)
	}
	link := da.VerificationURIComplete
	if link == "" {
		link = da.VerificationURI
	}
	fmt.Fprintf(a.Stdout, "To pair %q with collie, open\n\n    %s\n\nand confirm this code:\n\n    %s\n\n", *name, da.VerificationURI, da.UserCode)
	opened := false
	if !*noBrowser && a.OpenBrowser != nil {
		opened = a.OpenBrowser(link) == nil
	}
	if opened {
		fmt.Fprintln(a.Stdout, "Opened your browser. Waiting for approval...")
	} else {
		fmt.Fprintf(a.Stdout, "Or open %s\nWaiting for approval...\n", link)
	}

	res, err := client.Poll(ctx, da)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(a.Stderr, "collie-agent: pairing cancelled")
			return exitError
		}
		return a.fail(err)
	}
	creds := &config.Credentials{Server: srv, MachineID: res.MachineID, MachineToken: res.MachineToken, Name: *name, PairedAt: time.Now().UTC()}
	if err := p.SaveCredentials(creds); err != nil {
		return a.fail(fmt.Errorf("paired, but could not save the credentials: %w", err))
	}
	fmt.Fprintf(a.Stdout, "Paired as machine #%s. Credentials saved to %s.\n", res.MachineID, p.CredentialsPath())
	if err := a.askDiscover(p, discover); err != nil {
		return a.fail(fmt.Errorf("paired, but could not save the discovery answer: %w", err))
	}
	next := "collie-agent service install"
	if !p.IsDefault() {
		next += " --profile " + p.Name
	}
	fmt.Fprintf(a.Stdout, "Next: run `%s` to keep it running in the background.\n", next)
	if existing != nil {
		fmt.Fprintln(a.Stdout, "If the service was already installed, run that command again so it picks up the new pairing.")
	}
	return exitOK
}

// askDiscover records in config.json whether repos are also found where Claude Code and Codex were trusted. The
// --discover flag (flag non-nil) always sets it. Otherwise the person is asked once, on a terminal, and only when
// config.json has no answer yet, so pair --force keeps an earlier one. Without a terminal nothing is written: only root
// folders, as before.
func (a *App) askDiscover(p config.Profile, flag []string) error {
	s, err := p.LoadSettings()
	if err != nil {
		return err
	}
	answer := flag
	if answer == nil {
		if s.Discover != nil || a.IsTerminal == nil || !a.IsTerminal() || a.Stdin == nil {
			return nil
		}
		fmt.Fprint(a.Stdout, "Use the repos you already opened with Claude Code or Codex? [Y/n] ")
		line, err := bufio.NewReader(a.Stdin).ReadString('\n')
		if err != nil && (err != io.EOF || strings.TrimSpace(line) == "") {
			// No answer (end of input or a read error) is not consent: leave it unset.
			fmt.Fprintln(a.Stdout, "\nNo answer: only repos under root folders will be found (pair --force --discover claude,codex turns discovery on).")
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			answer = slices.Clone(config.DiscoverSources)
		default:
			answer = []string{}
		}
	}
	s.Discover = &answer
	if err := p.SaveSettings(s); err != nil {
		return err
	}
	if len(answer) > 0 {
		fmt.Fprintf(a.Stdout, "Repos trusted in %s will be found and allowed (\"discover\" in %s).\n", strings.Join(answer, " and "), p.SettingsPath())
	} else {
		fmt.Fprintf(a.Stdout, "Only repos under root folders will be found (collie-agent roots add <folder>).\n")
	}
	return nil
}

func (a *App) unpair(p config.Profile, args []string) int {
	fs := a.newFlagSet("unpair")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := p.DeleteCredentials(); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.Stdout, "Forgot the pairing of profile %q. Revoke the machine in collie (Settings → Runners) too.\n", p.Name)
	return exitOK
}

func (a *App) run(p config.Profile, args []string) int {
	fs := a.newFlagSet("run")
	asService := fs.Bool("service", false, "running under launchd or systemd: exit 0 when stopped on purpose so it is not restarted")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	logger := log.New(a.Stderr, "collie-agent: ", log.LstdFlags)
	stopped := exitStopped
	if *asService {
		stopped = exitOK
	}
	unlock, err := agent.Lock(p)
	if err != nil {
		logger.Print(err)
		return exitError
	}
	defer unlock()
	creds, err := p.LoadCredentials()
	if errors.Is(err, config.ErrNotPaired) {
		logger.Printf("profile %q is not paired: run `collie-agent pair` and then start the service again", p.Name)
		return stopped
	}
	if err != nil {
		logger.Print(err)
		return exitError
	}
	settings, err := p.LoadSettings()
	if err != nil {
		logger.Print(err)
		return exitError
	}
	ua := "collie-agent/" + version.Version
	var api agent.Heartbeater = machine.NewClient(creds.Server, creds.MachineToken, ua)
	if a.MachineAPI != nil {
		api = a.MachineAPI(creds.Server, creds.MachineToken)
	}
	r := &agent.Runner{Profile: p, MachineID: creds.MachineID, API: api, Settings: settings, Log: logger, Sleep: a.RunSleep}
	r.Discover = func(roots, sources []string) []discovery.Repo { return discovery.Discover(a.Home, sources, roots) }
	// The full machine API turns on discovery, bindings and dispatches; the cable is the push channel for them.
	if remote, ok := api.(agent.Remote); ok {
		r.Remote = remote
		r.Cable = &cable.Client{Server: creds.Server, Token: creds.MachineToken, UserAgent: ua}
	}
	if sapi, ok := api.(session.API); ok {
		max := settings.MaxConcurrency
		if max <= 0 {
			max = agent.DefaultMaxConcurrency
		}
		r.Sessions = &session.Manager{API: sapi, Dir: filepath.Join(p.Dir, "sessions"), Log: logger, MaxConcurrency: max, Setup: settings.SessionSetup, PhoneNotifications: settings.PhoneNotifications}
	}
	logger.Printf("collie-agent %s running as machine #%s with %s (profile %q, %d root folders, discovery %v)", version.Version, creds.MachineID, creds.Server, p.Name, len(settings.RootFolders), settings.DiscoverFrom())
	if err := r.Loop(a.ctx()); err != nil {
		if errors.Is(err, machine.ErrRevoked) {
			return stopped
		}
		logger.Print(err)
		return exitError
	}
	logger.Print("stopped")
	return exitOK
}

func (a *App) serviceManager(p config.Profile) (service.Manager, error) {
	return service.New(service.Options{GOOS: a.GOOS, Home: a.Home, UID: a.UID, Run: a.Commander, Profile: p})
}

func (a *App) status(p config.Profile, args []string) int {
	fs := a.newFlagSet("status")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	w := a.Stdout
	fmt.Fprintf(w, "Profile:     %s (%s)\n", p.Name, p.Dir)
	creds, err := p.LoadCredentials()
	switch {
	case errors.Is(err, config.ErrNotPaired):
		fmt.Fprintln(w, "Pairing:     not paired (run `collie-agent pair`)")
	case err != nil:
		fmt.Fprintf(w, "Pairing:     unreadable: %v\n", err)
	default:
		fmt.Fprintf(w, "Pairing:     machine #%s %q with %s, since %s\n", creds.MachineID, creds.Name, creds.Server, creds.PairedAt.Local().Format(time.RFC3339))
	}

	st, _ := p.LoadState()
	running := agent.IsLocked(p)
	switch {
	case creds != nil && st != nil && st.Revoked && st.MachineID == creds.MachineID:
		fmt.Fprintf(w, "Connection:  revoked: collie rejected the machine token at %s; pair again with --force\n", st.LastErrorAt.Local().Format(time.RFC3339))
	case !running:
		fmt.Fprintln(w, "Connection:  not running")
	case st == nil || st.LastHeartbeatAt.IsZero():
		fmt.Fprintln(w, "Connection:  running, no heartbeat yet")
	default:
		fmt.Fprintf(w, "Connection:  running, last heartbeat %s ago\n", time.Since(st.LastHeartbeatAt).Round(time.Second))
	}
	if running && st != nil && st.Connection != "" {
		fmt.Fprintf(w, "Dispatches:  over the %s, %d running\n", st.Connection, len(st.Running))
	}
	if settings, err := p.LoadSettings(); err != nil {
		fmt.Fprintf(w, "Config:      unreadable: %v\n", err)
	} else {
		if len(settings.RootFolders) == 0 {
			fmt.Fprintln(w, "Roots:       none (collie-agent roots add <folder>)")
		} else {
			fmt.Fprintf(w, "Roots:       %s\n", strings.Join(settings.RootFolders, ", "))
		}
		switch {
		case settings.Discover == nil:
			fmt.Fprintln(w, "Discovery:   off (not asked; pair --force --discover claude,codex turns it on)")
		case len(*settings.Discover) == 0:
			fmt.Fprintln(w, "Discovery:   off")
		default:
			fmt.Fprintf(w, "Discovery:   repos trusted in %s\n", strings.Join(*settings.Discover, ", "))
		}
	}
	if st != nil && st.Repos > 0 {
		fmt.Fprintf(w, "Repos:       %d matching a codebase\n", st.Repos)
	}
	if st != nil {
		for _, b := range st.Bindings {
			line := fmt.Sprintf("Binding:     #%d %s/%s", b.RunnerID, b.Project, b.Codebase)
			if b.Purpose != "" {
				line += " " + b.Purpose
			}
			line += " in " + b.Folder
			if b.Pinned {
				line += " (pinned)"
			}
			if b.PermissionMode != "" {
				line += fmt.Sprintf(", permissions %s", b.PermissionMode)
				if b.PermissionFrom != "" && b.PermissionFrom != "collie" {
					line += " from the " + b.PermissionFrom
				}
			}
			if b.Refused != "" {
				line += " — refused: " + b.Refused
			}
			fmt.Fprintln(w, line)
		}
	}
	if st != nil && st.LastError != "" && !st.Revoked && st.LastErrorAt.After(st.LastHeartbeatAt) {
		fmt.Fprintf(w, "Last error:  %s (%s)\n", st.LastError, st.LastErrorAt.Local().Format(time.RFC3339))
	}

	if m, err := a.serviceManager(p); err == nil {
		s, _ := m.Status()
		if s.Installed {
			fmt.Fprintf(w, "Service:     %s installed, %s\n", m.Name(), s.Detail)
		} else {
			fmt.Fprintf(w, "Service:     not installed (run `collie-agent service install`)\n")
		}
	}
	return exitOK
}

func (a *App) service(p config.Profile, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Stderr, "usage: collie-agent service install|uninstall|status")
		return exitUsage
	}
	m, err := a.serviceManager(p)
	if err != nil {
		return a.fail(err)
	}
	switch args[0] {
	case "install":
		if _, err := p.LoadCredentials(); err != nil {
			return a.fail(err)
		}
		exe, err := a.Executable()
		if err != nil {
			return a.fail(fmt.Errorf("cannot find the collie-agent binary: %w", err))
		}
		env, err := envFrom(a.Getenv)
		if err != nil {
			return a.fail(err)
		}
		spec := service.Spec{Profile: p, Executable: exe, Env: env}
		if err := m.Install(spec); err != nil {
			return a.fail(err)
		}
		fmt.Fprintf(a.Stdout, "Installed and started %s (%s).\n", m.Name(), m.Path())
		switch lm := m.(type) {
		case *service.Launchd:
			fmt.Fprintf(a.Stdout, "Logs: %s\n", lm.LogPath())
		case *service.Systemd:
			fmt.Fprintf(a.Stdout, "Logs: journalctl --user -u %s -f\n", lm.Name())
			fmt.Fprintln(a.Stdout, "To keep it running after you log out: loginctl enable-linger \"$USER\"")
		}
		return exitOK
	case "uninstall":
		if err := m.Uninstall(); err != nil {
			return a.fail(err)
		}
		fmt.Fprintf(a.Stdout, "Stopped and removed %s.\n", m.Name())
		return exitOK
	case "status":
		s, err := m.Status()
		if err != nil {
			return a.fail(err)
		}
		if !s.Installed {
			fmt.Fprintf(a.Stdout, "%s: not installed\n", m.Name())
			return exitError
		}
		fmt.Fprintf(a.Stdout, "%s: %s (%s)\n", m.Name(), s.Detail, s.Path)
		if !s.Running {
			return exitError
		}
		return exitOK
	default:
		fmt.Fprintf(a.Stderr, "collie-agent: unknown service command %q (install, uninstall or status)\n", args[0])
		return exitUsage
	}
}

// envFrom is the environment the service gets: the installing shell's PATH, so it finds the same agent CLIs, and the
// config base as an absolute path, since launchd and systemd do not start it in this working directory.
func envFrom(getenv func(string) string) (map[string]string, error) {
	env := map[string]string{}
	if v := getenv("PATH"); v != "" {
		env["PATH"] = v
	}
	if v := getenv("COLLIE_AGENT_CONFIG_DIR"); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return nil, err
		}
		env["COLLIE_AGENT_CONFIG_DIR"] = abs
	}
	// config.Resolve ignores a relative XDG_CONFIG_HOME, as the XDG spec says; pass it on only when it counts.
	if v := getenv("XDG_CONFIG_HOME"); v != "" && filepath.IsAbs(v) {
		env["XDG_CONFIG_HOME"] = v
	}
	return env, nil
}

// roots manages the root folders: the only places collie-agent looks for repos and the only places a binding may
// point to. The running service reads them at start: restart it after a change.
func (a *App) roots(p config.Profile, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	s, err := p.LoadSettings()
	if err != nil {
		return a.fail(err)
	}
	switch args[0] {
	case "list":
		if len(s.RootFolders) == 0 {
			fmt.Fprintln(a.Stdout, "No root folders. Add one with `collie-agent roots add <folder>`.")
			return exitOK
		}
		for _, r := range s.RootFolders {
			fmt.Fprintln(a.Stdout, r)
		}
		return exitOK
	case "add", "remove":
		if len(args) != 2 {
			fmt.Fprintf(a.Stderr, "usage: collie-agent roots %s <folder>\n", args[0])
			return exitUsage
		}
		folder, err := config.ExpandPath(args[1])
		if err != nil {
			return a.fail(err)
		}
		idx := -1
		for i, r := range s.RootFolders {
			if r == folder {
				idx = i
			}
		}
		if args[0] == "add" {
			if info, err := os.Stat(folder); err != nil || !info.IsDir() {
				return a.fail(fmt.Errorf("%s is not a directory", folder))
			}
			if idx >= 0 {
				fmt.Fprintf(a.Stdout, "%s is already a root folder.\n", folder)
				return exitOK
			}
			s.RootFolders = append(s.RootFolders, folder)
		} else {
			if idx < 0 {
				return a.fail(fmt.Errorf("%s is not a root folder", folder))
			}
			s.RootFolders = append(s.RootFolders[:idx], s.RootFolders[idx+1:]...)
		}
		if err := p.SaveSettings(s); err != nil {
			return a.fail(err)
		}
		fmt.Fprintf(a.Stdout, "Root folders: %s\nRestart the service to apply it (collie-agent service install).\n", strings.Join(s.RootFolders, ", "))
		return exitOK
	default:
		fmt.Fprintf(a.Stderr, "collie-agent: unknown roots command %q (add, remove or list)\n", args[0])
		return exitUsage
	}
}
