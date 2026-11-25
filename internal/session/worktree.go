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
// Branch is set only once `git worktree add -b` created it: cleanup deletes only a branch this session created.
type worktreeMeta struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
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

// defaultBase is where a session's branch starts: the remote's default branch, never the checkout the repo happens to
// be on (a feature branch would carry its commits into the session's PR). It is origin/HEAD when the repo knows it,
// else what origin says its HEAD is. The branch is fetched first, best effort: offline it starts from the last fetch.
func defaultBase(ctx context.Context, repo string) (string, error) {
	branch := ""
	if ref, err := git(ctx, repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		branch = strings.TrimPrefix(ref, "refs/remotes/origin/")
	} else {
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		out, lerr := git(lctx, repo, "ls-remote", "--symref", "origin", "HEAD")
		cancel()
		if lerr != nil {
			return "", fmt.Errorf("cannot tell the default branch of %s: origin/HEAD is not set and origin did not answer (%v); run `git remote set-head origin --auto` there", repo, lerr)
		}
		branch = remoteHead(out)
		if branch == "" {
			return "", fmt.Errorf("cannot tell the default branch of %s: origin/HEAD is not set and origin did not say; run `git remote set-head origin <branch>` there", repo)
		}
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, _ = git(fctx, repo, "fetch", "--quiet", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	if _, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch+"^{commit}"); err != nil {
		return "", fmt.Errorf("the default branch origin/%s of %s is not fetched and origin did not answer", branch, repo)
	}
	return "origin/" + branch, nil
}

// remoteHead reads the branch from `git ls-remote --symref origin HEAD` ("ref: refs/heads/main\tHEAD").
func remoteHead(out string) string {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(line, "ref: refs/heads/")
		if !ok {
			continue
		}
		if branch, target, ok := strings.Cut(rest, "\t"); ok && strings.TrimSpace(target) == "HEAD" {
			return branch
		}
	}
	return ""
}

// addWorktree creates dir as a worktree of repo on a new branch from the default branch, and records it so a sweep
// can remove it if collie-agent dies. A branch that already exists is an error, never reused or deleted.
func addWorktree(ctx context.Context, repo, dir, branch string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	metaPath := filepath.Join(filepath.Dir(dir), metaFile)
	write := func(m worktreeMeta) error {
		b, _ := json.Marshal(m)
		return os.WriteFile(metaPath, b, 0o600)
	}
	// The repo first, so a sweep can prune a half-made worktree; the branch only once this session created it.
	if err := write(worktreeMeta{Repo: repo}); err != nil {
		return err
	}
	if _, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return fmt.Errorf("branch %s already exists in %s: collie-agent does not reuse or delete it", branch, repo)
	}
	base, err := defaultBase(ctx, repo)
	if err != nil {
		return err
	}
	if _, err := git(ctx, repo, "worktree", "add", "-b", branch, dir, base); err != nil {
		return err
	}
	return write(worktreeMeta{Repo: repo, Branch: branch})
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
