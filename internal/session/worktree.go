package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// worktreeMeta is written next to each session's worktree, so a sweep after a crash knows which repo it belongs to.
type worktreeMeta struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

const metaFile = "session.json"

// git runs git in dir and returns its trimmed output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// defaultBase is the commit a session's branch starts from: origin's default branch when the repo knows it
// (refreshed with a fetch that may fail offline), else the repo's HEAD.
func defaultBase(ctx context.Context, repo string) string {
	ref, err := git(ctx, repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if err != nil || ref == "" {
		return "HEAD"
	}
	branch := strings.TrimPrefix(ref, "refs/remotes/origin/")
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, _ = git(fctx, repo, "fetch", "--quiet", "origin", branch) // best effort: offline still works from the last fetch
	return "origin/" + branch
}

// addWorktree creates dir as a worktree of repo on a fresh branch from the default branch, and records it so a
// sweep can remove it if collie-agent dies.
func addWorktree(ctx context.Context, repo, dir, branch string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	meta, _ := json.Marshal(worktreeMeta{Repo: repo, Branch: branch})
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), metaFile), meta, 0o600); err != nil {
		return err
	}
	_, err := git(ctx, repo, "worktree", "add", "-b", branch, dir, defaultBase(ctx, repo))
	return err
}

// removeWorktree removes the worktree and its local branch (whatever the agent pushed stays on the remote), and the
// session's directory with it. It keeps going on errors and returns the first one.
func removeWorktree(ctx context.Context, sessionDir string) error {
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	b, err := os.ReadFile(filepath.Join(sessionDir, metaFile))
	if err == nil {
		var m worktreeMeta
		if json.Unmarshal(b, &m) == nil && m.Repo != "" {
			wt := filepath.Join(sessionDir, "worktree")
			if _, statErr := os.Stat(wt); statErr == nil {
				_, err := git(ctx, m.Repo, "worktree", "remove", "--force", wt)
				keep(err)
			}
			_, _ = git(ctx, m.Repo, "worktree", "prune")
			if m.Branch != "" {
				_, _ = git(ctx, m.Repo, "branch", "-D", m.Branch)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		keep(err)
	}
	keep(os.RemoveAll(sessionDir))
	return first
}

// Sweep removes the worktrees of sessions that are not running: what a crash or a kill left behind. It returns how
// many it removed.
func Sweep(ctx context.Context, sessionsDir string, running func(name string) bool) (int, error) {
	entries, err := os.ReadDir(sessionsDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	var first error
	for _, e := range entries {
		if !e.IsDir() || (running != nil && running(e.Name())) {
			continue
		}
		if err := removeWorktree(ctx, filepath.Join(sessionsDir, e.Name())); err != nil && first == nil {
			first = err
		}
		n++
	}
	return n, first
}
