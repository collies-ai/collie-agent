package discovery

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MaxDepth is how deep under a root folder the scan looks for repos.
const MaxDepth = 4

// skipDirs are never descended into: dependencies and build output hold no repos of interest and can be huge.
var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "target": true, "dist": true, "build": true, "Library": true}

// Remote is a [remote "<name>"] of a repo's git config.
type Remote struct {
	Name string
	URL  string
}

// Repo is a git repo found on disk.
type Repo struct {
	// Path is the repo root with symlinks resolved: the main checkout, also when it was reached through one of its
	// worktrees, or the worktree itself when there is no main checkout to follow (a bare repo's worktree).
	Path     string
	Remotes  []Remote // as written in the repo's git config, in file order
	Worktree bool     // Path is a linked worktree, not a main checkout
	Branch   string   // the checked-out branch ("" when detached or unknown)
	// LastUsed is when the person last worked in the repo (zero when unknown). Matching ranks recently used clones
	// first.
	LastUsed time.Time

	commonDir string // the git dir holding config and refs (the main .git of a worktree)
}

// ScanRepos walks the root folders (up to MaxDepth) and returns the git repos it finds, resolved with ResolveRepo:
// a worktree is followed to its main checkout and every repo appears once, by its symlink-free root. It does not
// descend into a repo, hidden directories (collie's and Claude Code's worktrees live under .claude/worktrees) or
// dependency folders, and does not follow symlinks below a root. A repo resolved outside its root (a worktree of a
// clone kept elsewhere) is not reported.
func ScanRepos(roots []string) []Repo {
	var out []Repo
	seen := map[string]bool{}
	for _, root := range roots {
		root = filepath.Clean(root)
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		base := strings.Count(root, string(filepath.Separator))
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == root {
					return filepath.SkipDir
				}
				return nil // unreadable entry: skip it, keep walking
			}
			if !d.IsDir() {
				return nil
			}
			if path != root && (strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()]) {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
				// A worktree whose main checkout lies outside the root is left out: a binding there would be refused.
				if r, ok := ResolveRepo(path); ok && !seen[r.Path] && within(r.Path, realRoot) {
					seen[r.Path] = true
					out = append(out, r)
				}
				return filepath.SkipDir
			}
			if strings.Count(path, string(filepath.Separator))-base >= MaxDepth {
				return filepath.SkipDir
			}
			return nil
		})
	}
	return out
}

// within tells whether path is root or under it (both symlink-free).
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// ResolveRepo finds the git repo a folder belongs to, without running git: it walks up from the folder to the first
// .git. A .git directory makes that folder the main checkout. A .git file ("gitdir: ...") is a linked worktree, followed
// through its commondir to the main checkout; when the common git dir is not the .git of a checkout (a bare repo),
// the worktree itself is the repo, flagged Worktree. Symlinks are resolved, so two paths to one clone give the same
// Path. ok is false when the folder is in no repo or the .git file points nowhere.
func ResolveRepo(folder string) (Repo, bool) {
	dir, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return Repo{}, false
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return Repo{}, false
	}
	for {
		dotGit := filepath.Join(dir, ".git")
		if info, err := os.Stat(dotGit); err == nil {
			if info.IsDir() {
				return newRepo(dir, dotGit, dotGit, false), true
			}
			return resolveGitFile(dir, dotGit)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Repo{}, false
		}
		dir = parent
	}
}

// resolveGitFile follows a .git file (worktree or submodule) of the checkout at dir.
func resolveGitFile(dir, dotGit string) (Repo, bool) {
	gitDir := readPointer(dotGit, "gitdir:", dir)
	if gitDir == "" {
		return Repo{}, false
	}
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return Repo{}, false
	}
	common := readPointer(filepath.Join(gitDir, "commondir"), "", gitDir)
	if common == "" {
		return newRepo(dir, gitDir, gitDir, false), true // not a worktree (a submodule): its own git dir
	}
	if filepath.Base(common) == ".git" {
		main := filepath.Dir(common)
		if info, err := os.Stat(filepath.Join(main, ".git")); err == nil && info.IsDir() {
			return newRepo(main, common, common, false), true
		}
	}
	return newRepo(dir, gitDir, common, true), true
}

// readPointer reads a one-line file holding a path (after prefix, when given), resolved against base when relative
// and with symlinks resolved. "" when the file or the path it names is missing.
func readPointer(file, prefix, base string) string {
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimSpace(line)
	if prefix != "" {
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			return ""
		}
		line = strings.TrimSpace(rest)
	}
	if line == "" {
		return ""
	}
	if !filepath.IsAbs(line) {
		line = filepath.Join(base, line)
	}
	p, err := filepath.EvalSymlinks(line)
	if err != nil {
		return ""
	}
	return filepath.Clean(p)
}

// newRepo reads the repo at path: remotes from the common git dir's config, the branch from headDir's HEAD.
func newRepo(path, headDir, commonDir string, worktree bool) Repo {
	return Repo{
		Path:      path,
		Remotes:   readRemotes(filepath.Join(commonDir, "config")),
		Worktree:  worktree,
		Branch:    readHeadBranch(filepath.Join(headDir, "HEAD")),
		commonDir: commonDir,
	}
}

// readHeadBranch returns the branch a HEAD file points to, or "" when detached or unreadable.
func readHeadBranch(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref:")
	if !ok {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
}

// defaultBranch returns the default branch of a remote as recorded by git (refs/remotes/<remote>/HEAD), or "" when
// git has not recorded it.
func (r Repo) defaultBranch(remote string) string {
	if r.commonDir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(r.commonDir, "refs", "remotes", remote, "HEAD"))
	if err != nil {
		return ""
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref:")
	if !ok {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(ref), "refs/remotes/"+remote+"/")
}

// onDefaultBranch tells whether the repo has the remote's default branch checked out (main or master when git has
// not recorded the remote's default).
func (r Repo) onDefaultBranch(remote string) bool {
	if r.Branch == "" {
		return false
	}
	if d := r.defaultBranch(remote); d != "" {
		return r.Branch == d
	}
	return r.Branch == "main" || r.Branch == "master"
}

var (
	sectionRe = regexp.MustCompile(`^\[\s*remote\s+"([^"]*)"\s*\]$`)
	urlRe     = regexp.MustCompile(`^url\s*=\s*(.+)$`)
)

// readRemotes reads the name and url of every [remote "..."] section of a git config file, without running git.
func readRemotes(path string) []Remote {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Remote
	name := ""
	inRemote := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			m := sectionRe.FindStringSubmatch(line)
			inRemote = m != nil
			if inRemote {
				name = m[1]
			}
			continue
		}
		if inRemote {
			if m := urlRe.FindStringSubmatch(line); m != nil {
				out = append(out, Remote{Name: name, URL: strings.Trim(strings.TrimSpace(m[1]), `"`)})
			}
		}
	}
	return out
}
