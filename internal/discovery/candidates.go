package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Where a repo was found.
const (
	SourceRoot   = "root"   // under a root folder
	SourceClaude = "claude" // a folder Claude Code trusts (~/.claude.json)
	SourceCodex  = "codex"  // a project Codex trusts (~/.codex/config.toml)
	SourceScan   = "scan"   // the bounded scan of common folders, when ~/.claude.json cannot be read
)

// ScanFolders are the folders under the home directory scanned (up to MaxDepth) when the claude source is on but
// ~/.claude.json is missing or unreadable.
var ScanFolders = []string{"Projects", "code", "src", "dev", "work"}

// TempDirs are never taken from Claude Code or Codex: what was opened there is gone soon. Tests replace it.
var TempDirs = []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}

// Folder is a folder that may hold a repo, and where it was found.
type Folder struct {
	Path     string
	Source   string
	LastUsed time.Time // zero when unknown
}

// Candidates lists the folders to look for repos in: the repos under the root folders (source root) and, for each
// source in sources, the folders Claude Code or Codex trust. Missing paths, temp dirs and worktrees
// (*/.claude/worktrees/*, where Claude Code and collie put theirs) are skipped. ~/.claude.json is not a documented
// interface, so it is only a hint: when it is missing or unreadable, the common folders (ScanFolders) are scanned
// instead (source scan).
func Candidates(home string, sources, roots []string) []Folder {
	var out []Folder
	for _, r := range ScanRepos(roots) {
		out = append(out, Folder{Path: r.Path, Source: SourceRoot})
	}
	if slices.Contains(sources, SourceClaude) {
		folders, ok := claudeFolders(home)
		if ok {
			out = append(out, folders...)
		} else {
			var dirs []string
			for _, d := range ScanFolders {
				dirs = append(dirs, filepath.Join(home, d))
			}
			for _, r := range ScanRepos(dirs) {
				out = append(out, Folder{Path: r.Path, Source: SourceScan})
			}
		}
	}
	if slices.Contains(sources, SourceCodex) {
		out = append(out, codexFolders(home)...)
	}
	return out
}

// Discover resolves the candidates (Candidates) to repos: each folder is taken to the repo it belongs to
// (ResolveRepo), repos without remotes are left out, and every repo appears once, with the source it was first found
// by and its latest LastUsed.
func Discover(home string, sources, roots []string) []Repo {
	var out []Repo
	idx := map[string]int{}
	for _, f := range Candidates(home, sources, roots) {
		r, ok := ResolveRepo(f.Path)
		if !ok || len(r.Remotes) == 0 || (f.Source != SourceRoot && skipped(r.Path)) {
			continue
		}
		if i, seen := idx[r.Path]; seen {
			if f.LastUsed.After(out[i].LastUsed) {
				out[i].LastUsed = f.LastUsed
			}
			continue
		}
		r.Source, r.LastUsed = f.Source, f.LastUsed
		idx[r.Path] = len(out)
		out = append(out, r)
	}
	return out
}

// usable keeps a folder from Claude Code or Codex when it exists, is a directory and is neither a temp dir nor a
// worktree.
func usable(path string) bool {
	if !filepath.IsAbs(path) || skipped(path) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if real, err := filepath.EvalSymlinks(path); err != nil || skipped(real) {
		return false
	}
	return true
}

// skipped tells whether path is in a temp dir or a .claude/worktrees folder.
func skipped(path string) bool {
	path = filepath.Clean(path)
	for _, t := range TempDirs {
		if path == t || strings.HasPrefix(path, t+string(filepath.Separator)) {
			return true
		}
	}
	sep := string(filepath.Separator)
	marker := sep + filepath.Join(".claude", "worktrees") + sep
	return strings.Contains(path+sep, marker)
}

// claudeFolders reads ~/.claude.json: the keys of "projects" whose hasTrustDialogAccepted is true. LastUsed is the
// project's lastStartTime (milliseconds), or else the mtime of ~/.claude/projects/<encoded path>. ok is false when the
// file is missing or is not the JSON expected.
func claudeFolders(home string) (folders []Folder, ok bool) {
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return nil, false
	}
	var doc struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool    `json:"hasTrustDialogAccepted"`
			LastStartTime          float64 `json:"lastStartTime"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, false
	}
	paths := make([]string, 0, len(doc.Projects))
	for p := range doc.Projects {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		proj := doc.Projects[p]
		if !proj.HasTrustDialogAccepted || !usable(p) {
			continue
		}
		f := Folder{Path: filepath.Clean(p), Source: SourceClaude}
		if proj.LastStartTime > 0 {
			f.LastUsed = time.UnixMilli(int64(proj.LastStartTime))
		} else if info, err := os.Stat(filepath.Join(home, ".claude", "projects", claudeProjectDir(p))); err == nil {
			f.LastUsed = info.ModTime()
		}
		folders = append(folders, f)
	}
	return folders, true
}

// claudeProjectDir is the name Claude Code gives a folder's transcripts directory under ~/.claude/projects: the path
// with every character other than a letter or a digit turned into "-".
func claudeProjectDir(path string) string {
	b := []byte(path)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// codexFolders reads ~/.codex/config.toml with a conforming TOML decoder: the [projects."<path>"] tables whose
// trust_level is "trusted". A file TOML rejects (bad escapes, a key or a table defined twice...) trusts nothing, and so
// does a trust_level that is not a string.
func codexFolders(home string) []Folder {
	b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		return nil
	}
	var doc struct {
		Projects map[string]map[string]any `toml:"projects"`
	}
	if _, err := toml.Decode(string(b), &doc); err != nil {
		return nil
	}
	paths := make([]string, 0, len(doc.Projects))
	for p := range doc.Projects {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	var out []Folder
	for _, p := range paths {
		if level, ok := doc.Projects[p]["trust_level"].(string); ok && level == "trusted" && usable(p) {
			out = append(out, Folder{Path: filepath.Clean(p), Source: SourceCodex})
		}
	}
	return out
}
