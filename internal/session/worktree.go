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

// worktreeMeta is written in each session's directory, so a sweep after a crash knows which repo and worktree it
// belongs to. Branch is set only once git created it: cleanup deletes only a branch this session created. Worktree is
// empty in the metadata of versions that kept the worktree in the session directory (<session dir>/worktree).
type worktreeMeta struct {
	Repo     string `json:"repo"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
}

const metaFile = "session.json"

// worktreesDir is where session worktrees live inside the bound repo: the folder Claude Code's own --worktree uses.
// Inside the repo, a session inherits what the agent CLI already knows about the repo: Claude Code's workspace trust
// is per folder and covers the folders below it, so a repo the person trusted asks nothing of a session's worktree.
const worktreesDir = ".claude/worktrees"

// WorktreePath is where the session's worktree goes: <repo>/.claude/worktrees/collie-session-<id>.
func WorktreePath(repo string, id int64) string {
	return filepath.Join(repo, filepath.FromSlash(worktreesDir), fmt.Sprintf("collie-session-%d", id))
}

// ownedWorktree reports whether path is a place collie-agent puts a session worktree: a collie-session-* folder in the
// repo's worktrees folder, or the old <session dir>/worktree. removeWorktree never touches anything else, whatever the
// metadata says.
func ownedWorktree(repo, sessionDir, path string) bool {
	path = filepath.Clean(path)
	if path == filepath.Join(sessionDir, "worktree") {
		return true
	}
	return filepath.Dir(path) == filepath.Join(repo, filepath.FromSlash(worktreesDir)) &&
		strings.HasPrefix(filepath.Base(path), "collie-session-")
}

// excludeWorktrees keeps the worktrees folder out of the repo's `git status` when its .gitignore does not already: it
// adds it to the repo's local exclude file (.git/info/exclude), which is never committed. Best effort.
func excludeWorktrees(ctx context.Context, repo string) {
	if _, err := git(ctx, repo, "check-ignore", "-q", worktreesDir+"/collie-session-0"); err == nil {
		return // already ignored
	}
	path, err := git(ctx, repo, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, path)
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n# collie-agent session worktrees\n/" + worktreesDir + "/\n")
}

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

// addWorktree creates wt as a worktree of repo on a new branch from the default branch, and records it in sessionDir
// so a sweep can remove it if collie-agent dies. A branch that already exists is an error, never reused or deleted;
// so is a taken path.
//
// The branch is created on its own first (`git branch` refuses one that exists, atomically) and recorded as the
// session's right away, so cleanup deletes it whatever happens to the checkout after: a stop, a failing hook. That
// step runs on its own short context, so a stop cannot leave a branch created but not recorded.
func addWorktree(ctx context.Context, repo, sessionDir, wt, branch string) error {
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return err
	}
	metaPath := filepath.Join(sessionDir, metaFile)
	write := func(m worktreeMeta) error {
		b, _ := json.Marshal(m)
		return os.WriteFile(metaPath, b, 0o600)
	}
	// The repo first, so a sweep can prune a half-made worktree.
	if err := write(worktreeMeta{Repo: repo, Worktree: wt}); err != nil {
		return err
	}
	base, err := defaultBase(ctx, repo)
	if err != nil {
		return err
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	_, err = git(bctx, repo, "branch", "--no-track", branch, base)
	cancel()
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("branch %s already exists in %s: collie-agent does not reuse or delete it", branch, repo)
		}
		return err
	}
	if err := write(worktreeMeta{Repo: repo, Worktree: wt, Branch: branch}); err != nil {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _ = git(dctx, repo, "branch", "-D", branch)
		cancel()
		return err
	}
	excludeWorktrees(ctx, repo)
	_, err = git(ctx, repo, "worktree", "add", wt, branch)
	return err
}

// removeWorktree removes the session's worktree and its local branch (whatever the agent pushed stays on the remote),
// then the session's directory. It keeps going on errors and returns the first one.
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
			wt := m.Worktree
			if wt == "" {
				wt = filepath.Join(sessionDir, "worktree")
			}
			if !ownedWorktree(m.Repo, sessionDir, wt) {
				keep(fmt.Errorf("%s: worktree %q is not a session worktree; left alone", metaFile, wt))
			} else if _, statErr := os.Stat(wt); statErr == nil {
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
