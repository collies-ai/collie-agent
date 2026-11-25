package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/collies-ai/collie-agent/internal/machine"
)

func TestRepoFullName(t *testing.T) {
	cases := map[string]string{
		"https://github.com/collies-ai/collie-agent.git":          "collies-ai/collie-agent",
		"https://github.com/collies-ai/collie-agent":              "collies-ai/collie-agent",
		"https://x-access-token:abc@github.com/Collies-AI/Collie": "collies-ai/collie",
		"git@github.com:collies-ai/collie-agent.git":              "collies-ai/collie-agent",
		"git@github-work:jfromaniello/collie.git":                 "jfromaniello/collie",
		"ssh://git@github.com/jfromaniello/collie.git":            "jfromaniello/collie",
		"ssh://git@github.com:22/jfromaniello/collie":             "jfromaniello/collie",
		"https://github.com/collies-ai/collie-agent/":             "collies-ai/collie-agent",
		"/srv/git/collie.git":                                     "",
		"collie":                                                  "",
		"https://github.com/onlyowner":                            "",
	}
	for in, want := range cases {
		if got := RepoFullName(in); got != want {
			t.Errorf("RepoFullName(%q) = %q, want %q", in, got, want)
		}
	}
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
	root := t.TempDir()
	makeRepo(t, filepath.Join(root, "collie"), map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	makeRepo(t, filepath.Join(root, "work", "agent"), map[string]string{"origin": "https://github.com/collies-ai/collie-agent.git"})
	makeRepo(t, filepath.Join(root, "fork"), map[string]string{"origin": "git@github.com:me/collie.git", "upstream": "https://github.com/jfromaniello/collie"})
	makeRepo(t, filepath.Join(root, "other"), map[string]string{"origin": "git@github.com:someone/else.git"})
	makeRepo(t, filepath.Join(root, "node_modules", "dep"), map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	makeRepo(t, filepath.Join(root, ".hidden", "repo"), map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	makeRepo(t, filepath.Join(root, "a", "b", "c", "d", "e", "deep"), map[string]string{"origin": "git@github.com:jfromaniello/collie.git"})
	// A repo nested inside another repo is not scanned (the walk stops at the outer one).
	makeRepo(t, filepath.Join(root, "collie", "nested"), map[string]string{"origin": "git@github.com:collies-ai/collie-agent.git"})
	// A worktree has a .git file, not a directory: collie-agent's own sessions must not be reported.
	_ = os.MkdirAll(filepath.Join(root, "wt"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "wt", ".git"), []byte("gitdir: /elsewhere\n"), 0o644)

	repos := ScanRepos([]string{root, filepath.Join(root, "missing")})
	codebases := []machine.Codebase{
		{CodebaseID: 1, Codebase: "collie", Project: "collies", RepoFullName: "jfromaniello/collie"},
		{CodebaseID: 10, Codebase: "collie-agent", Project: "collies", RepoFullName: "collies-ai/collie-agent"},
		{CodebaseID: 99, Codebase: "nowhere", Project: "collies", RepoFullName: "collies-ai/nowhere"},
	}
	got := Match(repos, codebases)
	sort.Slice(got, func(i, j int) bool { return got[i].Path < got[j].Path })
	want := []machine.InventoryRepo{
		{CodebaseID: 1, Path: filepath.Join(root, "collie")},
		{CodebaseID: 1, Path: filepath.Join(root, "fork")}, // matched by its upstream remote
		{CodebaseID: 10, Path: filepath.Join(root, "work", "agent")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("inventory = %+v\nwant %+v", got, want)
	}
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
