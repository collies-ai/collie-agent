package discovery

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/collies-ai/collie-agent/internal/machine"
)

// setTempDirs replaces TempDirs for the test (t.TempDir itself lives in a temp dir).
func setTempDirs(t *testing.T, dirs ...string) {
	t.Helper()
	old := TempDirs
	TempDirs = dirs
	t.Cleanup(func() { TempDirs = old })
}

// claudeJSON is ~/.claude.json in the shape Claude Code writes it (trimmed to what matters plus a few neighbours).
func claudeJSON(projects map[string]string) string {
	out := `{"numStartups": 12, "theme": "dark", "projects": {`
	first := true
	for path, extra := range projects {
		if !first {
			out += ","
		}
		first = false
		out += strconv.Quote(path) + `: {"allowedTools": [], "mcpServers": {}, "lastCost": 0.1` + extra + `}`
	}
	return out + `}, "oauthAccount": {"emailAddress": "x"}}`
}

func TestCandidatesFromClaudeAndCodex(t *testing.T) {
	setSSHConfig(t, "")
	base := realDir(t, t.TempDir())
	home := filepath.Join(base, "home")
	fakeTmp := filepath.Join(base, "tmp")
	setTempDirs(t, fakeTmp)

	collie := filepath.Join(home, "Projects", "collie")
	agent := filepath.Join(home, "Projects", "collie-agent")
	plugin := filepath.Join(home, "src", "plugin")
	untrusted := filepath.Join(home, "Projects", "untrusted")
	scratch := filepath.Join(fakeTmp, "scratch")
	makeRepo(t, collie, map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	makeRepo(t, agent, map[string]string{"origin": "https://github.com/collies-ai/collie-agent.git"})
	makeRepo(t, plugin, map[string]string{"origin": "git@github.com:jfromaniello/collie-plugin.git"})
	makeRepo(t, untrusted, map[string]string{"origin": "git@github.com:someone/untrusted.git"})
	makeRepo(t, scratch, map[string]string{"origin": "git@github.com:someone/scratch.git"})
	noRemote := filepath.Join(home, "notes")
	makeRepo(t, noRemote, nil)
	// A Claude Code worktree of collie (trusted in ~/.claude.json like any folder) and a subfolder of agent.
	admin := filepath.Join(collie, ".git", "worktrees", "s1")
	writeFile(t, filepath.Join(admin, "commondir"), "../..\n")
	wt := filepath.Join(collie, ".claude", "worktrees", "s1")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+admin+"\n")
	agentSub := filepath.Join(agent, "internal")
	_ = os.MkdirAll(agentSub, 0o755)

	started := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(home, ".claude.json"), claudeJSON(map[string]string{
		collie:                         `, "hasTrustDialogAccepted": true, "lastStartTime": ` + strconv.FormatInt(started.UnixMilli(), 10),
		agentSub:                       `, "hasTrustDialogAccepted": true`,
		untrusted:                      `, "hasTrustDialogAccepted": false`,
		wt:                             `, "hasTrustDialogAccepted": true`,
		scratch:                        `, "hasTrustDialogAccepted": true`,
		noRemote:                       `, "hasTrustDialogAccepted": true`,
		filepath.Join(home, "deleted"): `, "hasTrustDialogAccepted": true`,
		"relative/path":                `, "hasTrustDialogAccepted": true`,
	}))
	// agent's last use comes from its transcripts folder.
	transcripts := filepath.Join(home, ".claude", "projects", claudeProjectDir(agentSub))
	_ = os.MkdirAll(transcripts, 0o755)
	agentUsed := time.Date(2026, 11, 22, 8, 0, 0, 0, time.Local)
	_ = os.Chtimes(transcripts, agentUsed, agentUsed)

	writeFile(t, filepath.Join(home, ".codex", "config.toml"), `model = "gpt-5"

[projects."`+home+`"]
trust_level = "untrusted"

[projects."`+plugin+`"]
trust_level = "trusted"   # added by codex

[projects.'`+collie+`']
trust_level = 'trusted'

[projects."`+scratch+`"]
trust_level = "trusted"

[mcp_servers.collie]
trust_level = "trusted"
`)

	folders := Candidates(home, []string{"claude", "codex"}, nil)
	got := map[string]Folder{}
	for _, f := range folders {
		got[f.Source+" "+f.Path] = f
	}
	wantKeys := []string{"claude " + collie, "claude " + agentSub, "claude " + noRemote, "codex " + plugin, "codex " + collie}
	if len(got) != len(wantKeys) {
		t.Errorf("candidates = %+v", folders)
	}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("missing candidate %s in %+v", k, folders)
		}
	}
	if !got["claude "+collie].LastUsed.Equal(started) {
		t.Errorf("collie LastUsed = %v, want lastStartTime %v", got["claude "+collie].LastUsed, started)
	}
	if !got["claude "+agentSub].LastUsed.Equal(agentUsed) {
		t.Errorf("agent LastUsed = %v, want the transcripts mtime %v", got["claude "+agentSub].LastUsed, agentUsed)
	}

	repos := Discover(home, []string{"claude", "codex"}, nil)
	byPath := map[string]Repo{}
	for _, r := range repos {
		byPath[r.Path] = r
	}
	if len(repos) != 3 {
		t.Errorf("repos = %+v", repos)
	}
	if r := byPath[collie]; r.Source != SourceClaude || !r.LastUsed.Equal(started) {
		t.Errorf("collie = %+v", r)
	}
	if r := byPath[agent]; r.Source != SourceClaude {
		t.Errorf("agent (from its subfolder) = %+v", r)
	}
	if r := byPath[plugin]; r.Source != SourceCodex {
		t.Errorf("plugin = %+v", r)
	}

	// Only codex: claude.json is not read.
	if repos := Discover(home, []string{"codex"}, nil); len(repos) != 2 {
		t.Errorf("codex only = %+v", repos)
	}
	// No sources: nothing but the roots.
	if repos := Discover(home, nil, nil); len(repos) != 0 {
		t.Errorf("no sources = %+v", repos)
	}
	// The repos then match like any other.
	m := Match(repos, []machine.Codebase{{CodebaseID: 1, RepoFullName: "jfromaniello/collie"}, {CodebaseID: 2, RepoFullName: "collies-ai/collie-agent"}})
	if len(m) != 2 || m[0].Chosen().Repo.Path != collie || m[1].Chosen().Repo.Path != agent {
		t.Errorf("matches = %+v", m)
	}
}

func TestMissingClaudeJSONFallsBackToTheBoundedScan(t *testing.T) {
	setSSHConfig(t, "")
	setTempDirs(t)
	home := realDir(t, t.TempDir())
	inProjects := filepath.Join(home, "Projects", "team", "collie")
	inWork := filepath.Join(home, "work", "agent")
	elsewhere := filepath.Join(home, "Desktop", "other")
	tooDeep := filepath.Join(home, "code", "a", "b", "c", "d", "deep")
	for _, d := range []string{inProjects, inWork, elsewhere, tooDeep} {
		makeRepo(t, d, map[string]string{"origin": "git@github.com:o/" + filepath.Base(d) + ".git"})
	}
	var got []string
	for _, r := range Discover(home, []string{"claude"}, nil) {
		if r.Source != SourceScan {
			t.Errorf("%s source = %q", r.Path, r.Source)
		}
		got = append(got, r.Path)
	}
	if want := []string{inProjects, inWork}; !reflect.DeepEqual(got, want) {
		t.Errorf("scan = %v, want %v", got, want)
	}

	// An unreadable claude.json falls back too; a readable one does not.
	writeFile(t, filepath.Join(home, ".claude.json"), "{not json")
	if n := len(Discover(home, []string{"claude"}, nil)); n != 2 {
		t.Errorf("unreadable claude.json: %d repos", n)
	}
	writeFile(t, filepath.Join(home, ".claude.json"), `{"projects": {}}`)
	if n := len(Discover(home, []string{"claude"}, nil)); n != 0 {
		t.Errorf("readable claude.json scanned anyway: %d repos", n)
	}
}

func TestRootsAndDiscoveryDedupe(t *testing.T) {
	setSSHConfig(t, "")
	setTempDirs(t)
	home := realDir(t, t.TempDir())
	repo := filepath.Join(home, "Projects", "collie")
	makeRepo(t, repo, map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	writeFile(t, filepath.Join(home, ".claude.json"), claudeJSON(map[string]string{repo: `, "hasTrustDialogAccepted": true, "lastStartTime": 1795539594565`}))
	repos := Discover(home, []string{"claude"}, []string{filepath.Join(home, "Projects")})
	if len(repos) != 1 || repos[0].Source != SourceRoot || repos[0].LastUsed.IsZero() {
		t.Errorf("repos = %+v (want one, from the root, with Claude's last use)", repos)
	}
}

func TestSkipped(t *testing.T) {
	setTempDirs(t, "/tmp", "/var/folders")
	for path, want := range map[string]bool{
		"/tmp":                                true,
		"/tmp/x":                              true,
		"/tmpfoo/x":                           false,
		"/var/folders/ab/T/x":                 true,
		"/Users/a/collie/.claude/worktrees/x": true,
		"/Users/a/collie/.claude/worktrees":   true,
		"/Users/a/collie/.claude":             false,
		"/Users/a/collie":                     false,
	} {
		if got := skipped(path); got != want {
			t.Errorf("skipped(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCodexProjectHeader(t *testing.T) {
	for line, want := range map[string]string{
		`[projects."/Users/a/x"]`:          "/Users/a/x",
		`[projects.'/Users/a/x y']`:        "/Users/a/x y",
		`[ projects . "/Users/a/\"q\""  ]`: `/Users/a/"q"`,
		`[projects]`:                       "",
		`[mcp_servers."/x"]`:               "",
		`[[projects."/x"]]`:                "",
	} {
		if got := codexProjectHeader(line); got != want {
			t.Errorf("codexProjectHeader(%s) = %q, want %q", line, got, want)
		}
	}
}

func TestCodexTrustInsideStringsIsNotConfig(t *testing.T) {
	setTempDirs(t)
	home := realDir(t, t.TempDir())
	good, evil1, evil2, evil3 := filepath.Join(home, "good"), filepath.Join(home, "evil1"), filepath.Join(home, "evil2"), filepath.Join(home, "evil3")
	for _, d := range []string{good, evil1, evil2, evil3} {
		_ = os.MkdirAll(d, 0o755)
	}
	write := func(content string) {
		writeFile(t, filepath.Join(home, ".codex", "config.toml"), content)
	}
	paths := func() []string {
		var out []string
		for _, f := range codexFolders(home) {
			out = append(out, f.Path)
		}
		return out
	}

	write(`instructions = """
[projects."` + evil1 + `"]
trust_level = "trusted"
"""
notes = '''
[projects."` + evil2 + `"]
trust_level = "trusted" '''
list = [
  "a",
  '[projects."` + evil3 + `"]',
  [ "nested", "]" ],
]
[projects."` + good + `"]
trust_level = "trusted"
`)
	if got := paths(); !reflect.DeepEqual(got, []string{good}) {
		t.Errorf("trusted = %v, want only %s", got, good)
	}

	// A file that cannot be read unambiguously trusts nothing.
	for _, broken := range []string{
		`x = """` + "\n" + `[projects."` + good + `"]` + "\n" + `trust_level = "trusted"` + "\n",
		`x = [` + "\n" + `[projects."` + good + `"]` + "\n" + `trust_level = "trusted"` + "\n",
		`x = "open` + "\n" + `[projects."` + good + `"]` + "\n" + `trust_level = "trusted"` + "\n",
		`[projects."` + good + `"]` + "\n" + `trust_level = "trusted"` + "\n" + "garbage line\n",
	} {
		write(broken)
		if got := paths(); len(got) != 0 {
			t.Errorf("broken config %q trusted %v", broken, got)
		}
	}
	// A trust value that is not a plain one-line string is not trusted.
	write(`[projects."` + good + `"]` + "\n" + `trust_level = """trusted"""` + "\n")
	if got := paths(); len(got) != 0 {
		t.Errorf("multiline trust value counted: %v", got)
	}
}
