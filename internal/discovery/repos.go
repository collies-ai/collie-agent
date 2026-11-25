package discovery

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/collies-ai/collie-agent/internal/machine"
)

// MaxDepth is how deep under a root folder the scan looks for repos.
const MaxDepth = 4

// skipDirs are never descended into: dependencies and build output hold no repos of interest and can be huge.
var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "target": true, "dist": true, "build": true, "Library": true}

// Repo is a git repo found on disk.
type Repo struct {
	Path    string
	Remotes []string // remote URLs as written in .git/config
}

// ScanRepos walks the root folders (up to MaxDepth) and returns the git repos it finds: directories with a .git
// directory. It does not descend into a repo, hidden directories or dependency folders, and does not follow
// symlinks. Worktrees (.git file) are skipped on purpose: collie-agent creates its own.
func ScanRepos(roots []string) []Repo {
	var out []Repo
	seen := map[string]bool{}
	for _, root := range roots {
		root = filepath.Clean(root)
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
			if info, err := os.Stat(filepath.Join(path, ".git")); err == nil && info.IsDir() {
				if !seen[path] {
					seen[path] = true
					out = append(out, Repo{Path: path, Remotes: readRemotes(filepath.Join(path, ".git", "config"))})
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

var (
	sectionRe = regexp.MustCompile(`^\[\s*remote\s+"([^"]*)"\s*\]$`)
	urlRe     = regexp.MustCompile(`^url\s*=\s*(.+)$`)
)

// readRemotes reads the url of every [remote "..."] section of a git config file, without running git.
func readRemotes(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var urls []string
	inRemote := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inRemote = sectionRe.MatchString(line)
			continue
		}
		if inRemote {
			if m := urlRe.FindStringSubmatch(line); m != nil {
				urls = append(urls, strings.Trim(strings.TrimSpace(m[1]), `"`))
			}
		}
	}
	return urls
}

// RepoFullName turns a git remote URL into "owner/name" (lowercase), or "" when it does not look like one. It
// understands https (with or without credentials), ssh:// and scp-like (git@host:owner/name) remotes, with or without
// .git, and any host: SSH aliases such as git@github-work:owner/name are common.
func RepoFullName(remote string) string {
	r := strings.TrimSpace(remote)
	var path string
	switch {
	case strings.Contains(r, "://"):
		_, rest, _ := strings.Cut(r, "://")
		_, p, ok := strings.Cut(rest, "/") // drop user@host:port
		if !ok {
			return ""
		}
		path = p
	case strings.Contains(r, ":") && !strings.HasPrefix(r, "/"):
		_, p, _ := strings.Cut(r, ":") // scp-like: [user@]host:path
		path = p
	default:
		return "" // a local path
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return ""
	}
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || name == "" {
		return ""
	}
	return strings.ToLower(owner + "/" + name)
}

// Match keeps the repos whose remote matches a codebase's repo_full_name and returns the inventory to send collie.
// A repo matching several codebases (the same repo in two projects) is reported once per codebase.
func Match(repos []Repo, codebases []machine.Codebase) []machine.InventoryRepo {
	byName := map[string][]int64{}
	for _, c := range codebases {
		if n := strings.ToLower(strings.TrimSpace(c.RepoFullName)); n != "" {
			byName[n] = append(byName[n], c.CodebaseID)
		}
	}
	var out []machine.InventoryRepo
	for _, r := range repos {
		reported := map[int64]bool{}
		for _, remote := range r.Remotes {
			for _, id := range byName[RepoFullName(remote)] {
				if !reported[id] {
					reported[id] = true
					out = append(out, machine.InventoryRepo{CodebaseID: id, Path: r.Path})
				}
			}
		}
	}
	return out
}
