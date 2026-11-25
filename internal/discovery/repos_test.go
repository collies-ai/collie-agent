package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/collies-ai/collie-agent/internal/machine"
)

func TestParseRemote(t *testing.T) {
	type want struct {
		host, name string
		ssh, ok    bool
	}
	cases := map[string]want{
		"https://github.com/collies-ai/collie-agent.git":          {"github.com", "collies-ai/collie-agent", false, true},
		"https://github.com/collies-ai/collie-agent":              {"github.com", "collies-ai/collie-agent", false, true},
		"https://x-access-token:abc@github.com/Collies-AI/Collie": {"github.com", "collies-ai/collie", false, true},
		"git@github.com:collies-ai/collie-agent.git":              {"github.com", "collies-ai/collie-agent", true, true},
		"git@github-work:jfromaniello/collie.git":                 {"github-work", "jfromaniello/collie", true, true},
		"ssh://git@github.com/jfromaniello/collie.git":            {"github.com", "jfromaniello/collie", true, true},
		"ssh://git@GitHub.com:22/jfromaniello/collie":             {"github.com", "jfromaniello/collie", true, true},
		"https://github.com/collies-ai/collie-agent/":             {"github.com", "collies-ai/collie-agent", false, true},
		"https://gitlab.com/jfromaniello/collie.git":              {"gitlab.com", "jfromaniello/collie", false, true},
		"/srv/git/collie.git":                                     {},
		"file:///srv/git/owner/collie.git":                        {},
		"collie":                                                  {},
		"https://github.com/onlyowner":                            {},
	}
	for in, w := range cases {
		host, name, ssh, ok := ParseRemote(in)
		if got := (want{host, name, ssh, ok}); got != w {
			t.Errorf("ParseRemote(%q) = %+v, want %+v", in, got, w)
		}
	}
}

func TestGitHubRepoChecksTheHost(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "ssh_config")
	writeFile(t, cfg, `# work account
Host github-work
    HostName github.com
    IdentityFile ~/.ssh/work
Host gh-* !gh-blocked
  HostName %h.github.com.example
Host gh-ok
  HostName github.com
Host=gitlab-alias
  HostName=gitlab.com
Host ghe
  HostName github.com
Match host ghe
  HostName git.internal
Host *
  User git
`)
	setSSHConfig(t, cfg)
	h := newHostResolver()
	cases := map[string]string{
		"git@github.com:jfromaniello/collie.git":        "jfromaniello/collie",
		"https://github.com/jfromaniello/collie":        "jfromaniello/collie",
		"git@github-work:jfromaniello/collie.git":       "jfromaniello/collie", // alias resolving to github.com
		"ssh://git@github-work/jfromaniello/collie.git": "jfromaniello/collie",
		"git@ghe:jfromaniello/collie.git":               "jfromaniello/collie", // Match blocks are ignored
		"git@gh-ok:jfromaniello/collie.git":             "",                    // the earlier gh-* block wins
		"git@gh-blocked:jfromaniello/collie.git":        "",                    // negated: falls through to no HostName
		"git@gitlab-alias:jfromaniello/collie.git":      "",
		"https://gitlab.com/jfromaniello/collie.git":    "",
		"git@gitlab.com:jfromaniello/collie.git":        "",
		"https://github-work/jfromaniello/collie.git":   "", // aliases are ssh only
		"git@unknown-alias:jfromaniello/collie.git":     "",
		"/srv/git/jfromaniello/collie.git":              "",
	}
	for in, want := range cases {
		if got := h.GitHubRepo(in); got != want {
			t.Errorf("GitHubRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSSHHostNameOverridesALiteralGitHubHost(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "ssh_config")
	writeFile(t, cfg, "Host github.com\n  HostName git.internal.example\n")
	setSSHConfig(t, cfg)
	h := newHostResolver()
	if got := h.GitHubRepo("git@github.com:jfromaniello/collie.git"); got != "" {
		t.Errorf("ssh to github.com redirected elsewhere matched: %q", got)
	}
	// https does not go through ssh.
	if got := h.GitHubRepo("https://github.com/jfromaniello/collie.git"); got != "jfromaniello/collie" {
		t.Errorf("https = %q", got)
	}
}

func TestRepoFullNameIgnoresHost(t *testing.T) {
	if got := RepoFullName("git@github-work:jfromaniello/collie.git"); got != "jfromaniello/collie" {
		t.Errorf("RepoFullName = %q", got)
	}
}

// setSSHConfig points SSHConfigPath at path for the test (never the person's real ~/.ssh/config).
func setSSHConfig(t *testing.T, path string) {
	t.Helper()
	old := SSHConfigPath
	SSHConfigPath = func() string { return path }
	t.Cleanup(func() { SSHConfigPath = old })
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// realDir returns dir with symlinks resolved (t.TempDir lives under a symlink on macOS).
func realDir(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// makeRepo creates dir/.git/config with the given remotes.
func makeRepo(t *testing.T, dir string, remotes map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\trepositoryformatversion = 0\n"
	names := make([]string, 0, len(remotes))
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cfg += "[remote \"" + n + "\"]\n\turl = " + remotes[n] + "\n\tfetch = +refs/heads/*:refs/remotes/" + n + "/*\n"
	}
	cfg += "[branch \"main\"]\n\tremote = origin\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanAndMatchReportsOnlyMatchingRepos(t *testing.T) {
	setSSHConfig(t, "")
	root := realDir(t, t.TempDir())
	makeRepo(t, filepath.Join(root, "collie"), map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	makeRepo(t, filepath.Join(root, "work", "agent"), map[string]string{"origin": "https://github.com/collies-ai/collie-agent.git"})
	makeRepo(t, filepath.Join(root, "fork"), map[string]string{"origin": "git@github.com:me/collie.git", "upstream": "https://github.com/jfromaniello/collie"})
	makeRepo(t, filepath.Join(root, "other"), map[string]string{"origin": "git@github.com:someone/else.git"})
	makeRepo(t, filepath.Join(root, "gitlab"), map[string]string{"origin": "git@gitlab.com:jfromaniello/collie.git"})
	makeRepo(t, filepath.Join(root, "node_modules", "dep"), map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	makeRepo(t, filepath.Join(root, ".hidden", "repo"), map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	makeRepo(t, filepath.Join(root, "a", "b", "c", "d", "e", "deep"), map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	// A repo nested inside another repo is not scanned (the walk stops at the outer one).
	makeRepo(t, filepath.Join(root, "collie", "nested"), map[string]string{"origin": "git@github.com:collies-ai/collie-agent.git"})
	// A .git file pointing nowhere is not a repo.
	writeFile(t, filepath.Join(root, "wt", ".git"), "gitdir: /elsewhere\n")

	repos := ScanRepos([]string{root, filepath.Join(root, "missing")})
	codebases := []machine.Codebase{
		{CodebaseID: 1, Codebase: "collie", Project: "collies", RepoFullName: "jfromaniello/collie"},
		{CodebaseID: 10, Codebase: "collie-agent", Project: "collies", RepoFullName: "Collies-AI/Collie-Agent"},
		{CodebaseID: 99, Codebase: "nowhere", Project: "collies", RepoFullName: "collies-ai/nowhere"},
		{CodebaseID: 100, Codebase: "unset", Project: "collies"},
	}
	got := Inventory(Match(repos, codebases))
	want := []machine.InventoryRepo{
		{CodebaseID: 1, Path: filepath.Join(root, "collie")},
		{CodebaseID: 1, Path: filepath.Join(root, "fork")}, // matched by its upstream remote, ranked after origin
		{CodebaseID: 10, Path: filepath.Join(root, "work", "agent")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("inventory = %+v\nwant %+v", got, want)
	}
}

func TestCloneRankingIsNotOverriddenByTheRemote(t *testing.T) {
	setSSHConfig(t, "")
	now := time.Now()
	origin := []Remote{{"origin", "git@github.com:jfromaniello/collie.git"}}
	fork := []Remote{{"origin", "git@github.com:me/collie.git"}, {"upstream", "https://github.com/jfromaniello/collie"}}
	repos := []Repo{
		{Path: "/a/origin-worktree", Remotes: origin, Worktree: true, LastUsed: now},
		{Path: "/a/origin-old", Remotes: origin, LastUsed: now.Add(-time.Hour)},
		{Path: "/a/fork-main", Remotes: fork, LastUsed: now},
		{Path: "/a/origin-same", Remotes: origin, LastUsed: now},
	}
	got := Match(repos, []machine.Codebase{{CodebaseID: 1, RepoFullName: "jfromaniello/collie"}})
	var order []string
	for _, c := range got[0].Candidates {
		order = append(order, filepath.Base(c.Repo.Path))
	}
	// Main checkouts before the worktree, newer before older; at a tie, origin before upstream.
	if want := []string{"origin-same", "fork-main", "origin-old", "origin-worktree"}; !reflect.DeepEqual(order, want) {
		t.Errorf("ranking = %v, want %v", order, want)
	}
}

func TestMatchFlagsForkAndPrefersOrigin(t *testing.T) {
	setSSHConfig(t, "")
	repos := []Repo{
		{Path: "/a/fork", Remotes: []Remote{{"upstream", "https://github.com/jfromaniello/collie"}, {"origin", "git@github.com:me/collie.git"}}},
		{Path: "/a/both", Remotes: []Remote{{"upstream", "https://github.com/jfromaniello/collie"}, {"origin", "git@github.com:jfromaniello/collie.git"}}},
	}
	got := Match(repos, []machine.Codebase{{CodebaseID: 1, RepoFullName: "jfromaniello/collie"}, {CodebaseID: 2, RepoFullName: "me/collie"}})
	if len(got) != 2 || len(got[0].Candidates) != 2 || len(got[1].Candidates) != 1 {
		t.Fatalf("matches = %+v", got)
	}
	c := got[0].Candidates
	if c[0].Repo.Path != "/a/both" || c[0].Remote != "origin" || c[0].ViaOtherRemote() {
		t.Errorf("chosen = %+v, want /a/both via origin", c[0])
	}
	if c[1].Repo.Path != "/a/fork" || c[1].Remote != "upstream" || !c[1].ViaOtherRemote() {
		t.Errorf("fork = %+v, want /a/fork via upstream (flagged)", c[1])
	}
	if f := got[1].Chosen(); f.Repo.Path != "/a/fork" || f.Remote != "origin" {
		t.Errorf("me/collie chosen = %+v", f)
	}
}

func TestMatchThroughSSHAlias(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	writeFile(t, cfg, "Host github-work\n  HostName github.com\n")
	setSSHConfig(t, cfg)
	repos := []Repo{{Path: "/w/collie", Remotes: []Remote{{"origin", "git@github-work:jfromaniello/collie.git"}}}}
	got := Inventory(Match(repos, []machine.Codebase{{CodebaseID: 1, RepoFullName: "jfromaniello/collie"}}))
	if want := []machine.InventoryRepo{{CodebaseID: 1, Path: "/w/collie"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("inventory = %+v", got)
	}
	setSSHConfig(t, filepath.Join(t.TempDir(), "missing"))
	if got := Match(repos, []machine.Codebase{{CodebaseID: 1, RepoFullName: "jfromaniello/collie"}}); len(got) != 0 {
		t.Errorf("without the alias the remote must not match: %+v", got)
	}
}

func TestMatchRanksSeveralClones(t *testing.T) {
	setSSHConfig(t, "")
	root := realDir(t, t.TempDir())
	origin := map[string]string{"origin": "git@github.com:jfromaniello/collie.git"}
	makeRepo(t, filepath.Join(root, "old"), origin)
	makeRepo(t, filepath.Join(root, "recent"), origin)
	makeRepo(t, filepath.Join(root, "feature"), origin)
	makeRepo(t, filepath.Join(root, "trunk"), origin)
	writeFile(t, filepath.Join(root, "feature", ".git", "HEAD"), "ref: refs/heads/feature-x\n")
	writeFile(t, filepath.Join(root, "trunk", ".git", "HEAD"), "ref: refs/heads/develop\n")
	writeFile(t, filepath.Join(root, "trunk", ".git", "refs", "remotes", "origin", "HEAD"), "ref: refs/remotes/origin/develop\n")
	bareWT := makeBareWorktree(t, root, origin)

	var repos []Repo
	for _, d := range []string{bareWT, "old", "recent", "feature", "trunk"} {
		if !filepath.IsAbs(d) {
			d = filepath.Join(root, d)
		}
		r, ok := ResolveRepo(d)
		if !ok {
			t.Fatalf("ResolveRepo(%s) failed", d)
		}
		repos = append(repos, r)
	}
	now := time.Now()
	repos[0].LastUsed = now.Add(time.Hour) // the worktree, most recent of all, still ranks last
	repos[1].LastUsed = now.Add(-48 * time.Hour)
	repos[2].LastUsed = now
	// feature and trunk: same LastUsed (unknown), trunk is on its default branch.
	got := Match(repos, []machine.Codebase{{CodebaseID: 1, RepoFullName: "jfromaniello/collie"}})
	if len(got) != 1 {
		t.Fatalf("matches = %+v", got)
	}
	var order []string
	for _, c := range got[0].Candidates {
		order = append(order, filepath.Base(c.Repo.Path))
	}
	want := []string{"recent", "old", "trunk", "feature", "wt"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("ranking = %v, want %v", order, want)
	}
	if !got[0].Candidates[4].Repo.Worktree {
		t.Errorf("bare repo worktree not flagged: %+v", got[0].Candidates[4].Repo)
	}
}

func TestResolveRepoFollowsWorktreeToMainCheckout(t *testing.T) {
	root := realDir(t, t.TempDir())
	main := filepath.Join(root, "collie")
	makeRepo(t, main, map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	writeFile(t, filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	// A linked worktree elsewhere, as `git worktree add` lays it out (relative commondir).
	admin := filepath.Join(main, ".git", "worktrees", "wt1")
	writeFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/feature\n")
	writeFile(t, filepath.Join(admin, "commondir"), "../..\n")
	wt := filepath.Join(root, "elsewhere", "collie-wt")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+admin+"\n")
	// Claude Code's worktrees live inside the repo, with a relative gitdir.
	admin2 := filepath.Join(main, ".git", "worktrees", "s1")
	writeFile(t, filepath.Join(admin2, "commondir"), "../..\n")
	inner := filepath.Join(main, ".claude", "worktrees", "s1")
	writeFile(t, filepath.Join(inner, ".git"), "gitdir: ../../../.git/worktrees/s1\n")

	for _, folder := range []string{main, filepath.Join(main, "sub", "dir"), wt, filepath.Join(wt, "pkg"), inner} {
		_ = os.MkdirAll(folder, 0o755)
		r, ok := ResolveRepo(folder)
		if !ok {
			t.Fatalf("ResolveRepo(%s) failed", folder)
		}
		if r.Path != main || r.Worktree || r.Branch != "main" {
			t.Errorf("ResolveRepo(%s) = %+v, want main checkout %s on main", folder, r, main)
		}
		if want := []Remote{{"origin", "git@github.com:jfromaniello/collie.git"}}; !reflect.DeepEqual(r.Remotes, want) {
			t.Errorf("remotes = %+v", r.Remotes)
		}
	}
	// The scan reaches the worktree folder too and reports the main checkout once.
	repos := ScanRepos([]string{root})
	if len(repos) != 1 || repos[0].Path != main {
		t.Errorf("ScanRepos = %+v", repos)
	}
	// Scanning only the worktree's folder: its main checkout is outside that root, so nothing is reported.
	if repos := ScanRepos([]string{filepath.Join(root, "elsewhere")}); len(repos) != 0 {
		t.Errorf("ScanRepos(elsewhere) = %+v", repos)
	}
	if _, ok := ResolveRepo(filepath.Join(root, "elsewhere")); ok {
		t.Error("a folder outside any repo resolved")
	}
}

func TestSymlinkedClonesAreDeduped(t *testing.T) {
	setSSHConfig(t, "")
	root := realDir(t, t.TempDir())
	main := filepath.Join(root, "code", "collie")
	makeRepo(t, main, map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "code"), link); err != nil {
		t.Fatal(err)
	}
	a, _ := ResolveRepo(filepath.Join(link, "collie"))
	b, _ := ResolveRepo(main)
	if a.Path != main || b.Path != main {
		t.Fatalf("paths = %q, %q, want %q", a.Path, b.Path, main)
	}
	// Both roots reach the clone: it is reported once.
	repos := ScanRepos([]string{filepath.Join(root, "code"), link})
	if len(repos) != 1 {
		t.Errorf("ScanRepos = %+v", repos)
	}
	a.LastUsed = time.Now()
	got := Match([]Repo{b, a}, []machine.Codebase{{CodebaseID: 1, RepoFullName: "jfromaniello/collie"}})
	if len(got) != 1 || len(got[0].Candidates) != 1 || got[0].Chosen().Repo.LastUsed.IsZero() {
		t.Errorf("matches = %+v", got)
	}
}

// makeBareWorktree creates root/bare.git with the remotes and a worktree of it at root/wt; it returns the worktree.
func makeBareWorktree(t *testing.T, root string, remotes map[string]string) string {
	t.Helper()
	bare := filepath.Join(root, "bare.git")
	makeRepo(t, filepath.Join(root, "tmp-bare"), remotes)
	if err := os.Rename(filepath.Join(root, "tmp-bare", ".git"), bare); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(root, "tmp-bare"))
	admin := filepath.Join(bare, "worktrees", "wt")
	writeFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(admin, "commondir"), "../..\n")
	wt := filepath.Join(root, "wt")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+admin+"\n")
	return wt
}

func TestScanWithNoRoots(t *testing.T) {
	if got := ScanRepos(nil); len(got) != 0 {
		t.Errorf("ScanRepos(nil) = %v", got)
	}
}

func TestFindAgents(t *testing.T) {
	oldLook, oldVer := LookPath, Version
	defer func() { LookPath, Version = oldLook, oldVer }()
	LookPath = func(name string) (string, error) {
		if name == "claude" {
			return "/usr/local/bin/claude", nil
		}
		return "", errors.New("not found")
	}
	Version = func(_ context.Context, path string) string { return "2.1.0 (Claude Code)" }
	got := FindAgents(context.Background())
	want := []machine.Agent{{Name: "claude", Version: "2.1.0 (Claude Code)"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindAgents = %+v", got)
	}
}
