package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
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

// codexFolders reads ~/.codex/config.toml: the [projects."<path>"] tables whose trust_level is "trusted". It is not
// a full TOML parser, but it tracks where a statement can start (outside strings, multiline strings and arrays), so
// text inside a value is never read as a table or a key, and it compares keys and table names decoded (a quoted and a
// bare spelling are the same key, as in TOML). Nothing is trusted until the whole file has been read, and a file it
// cannot read unambiguously trusts nothing: a table or a trust_level defined twice (which TOML forbids), a key it
// cannot decode, an array of tables under projects, or a project's trust set any other way (a dotted key or an inline
// table).
func codexFolders(home string) []Folder {
	b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		return nil
	}
	var sc tomlScanner
	seenTables := map[string]bool{} // decoded table paths, so a table defined twice is caught
	trust := map[string]string{}    // project path -> its trust_level
	var order []string
	var table []string // the decoded path of the current table
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if !sc.atStatement() {
			if !sc.scan(line) {
				return nil
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			continue
		case strings.HasPrefix(trimmed, "[["):
			path, ok := tomlHeader(trimmed[2:], "]]")
			if !ok || (len(path) > 0 && path[0] == "projects") || !sc.scan(line) || !sc.atStatement() {
				return nil
			}
			table = path
			continue
		case strings.HasPrefix(trimmed, "["):
			path, ok := tomlHeader(trimmed[1:], "]")
			if !ok || !sc.scan(line) || !sc.atStatement() {
				return nil // a header never spans lines
			}
			id := strings.Join(path, "\x00")
			if seenTables[id] {
				return nil
			}
			seenTables[id] = true
			if len(path) > 2 && path[0] == "projects" {
				return nil // a sub-table of a project: not a shape Codex writes
			}
			if len(path) == 2 && path[0] == "projects" {
				order = append(order, path[1])
			}
			table = path
			continue
		}
		key, rest, ok := tomlKeyPath(trimmed)
		if !ok || !strings.HasPrefix(rest, "=") {
			return nil
		}
		full := append(append([]string{}, table...), key...)
		if len(full) > 0 && full[0] == "projects" {
			switch {
			case len(full) == 3 && full[2] == "trust_level" && len(table) == 2:
				p := full[1]
				if _, dup := trust[p]; dup {
					return nil
				}
				v, ok := tomlString(strings.TrimSpace(rest[1:]))
				if !ok {
					v = "" // present but not a plain string: not trusted, and still counts for duplicates
				}
				trust[p] = v
			case len(full) <= 3:
				// projects = ..., projects."<p>" = {...} or a dotted projects."<p>".trust_level: trust set another way.
				return nil
			}
		}
		if !sc.scan(line) {
			return nil
		}
	}
	if !sc.atStatement() {
		return nil // a string or an array left open at the end of the file
	}
	var out []Folder
	for _, p := range order {
		if trust[p] == "trusted" && usable(p) {
			out = append(out, Folder{Path: filepath.Clean(p), Source: SourceCodex})
		}
	}
	return out
}

// tomlHeader decodes the key path of a table header whose opening bracket was already read, up to closing; only a
// comment may follow.
func tomlHeader(s, closing string) ([]string, bool) {
	path, rest, ok := tomlKeyPath(s)
	if !ok {
		return nil, false
	}
	rest, ok = strings.CutPrefix(rest, closing)
	if !ok || !onlyComment(rest) {
		return nil, false
	}
	return path, true
}

// tomlKeyPath decodes a TOML key (bare, "basic" or 'literal' parts joined by dots, spaces allowed around them) at the
// start of s and returns its parts and the rest of s after the key and the spaces that follow it.
func tomlKeyPath(s string) ([]string, string, bool) {
	var parts []string
	for {
		s = strings.TrimLeft(s, " \t")
		var part string
		switch {
		case strings.HasPrefix(s, `"`):
			end := closingQuote(s[1:], '"')
			if end < 0 {
				return nil, "", false
			}
			v, err := strconv.Unquote(s[:end+2])
			if err != nil {
				return nil, "", false
			}
			part, s = v, s[end+2:]
		case strings.HasPrefix(s, "'"):
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return nil, "", false
			}
			part, s = s[1:end+1], s[end+2:]
		default:
			n := 0
			for n < len(s) && (s[n] >= 'a' && s[n] <= 'z' || s[n] >= 'A' && s[n] <= 'Z' || s[n] >= '0' && s[n] <= '9' || s[n] == '_' || s[n] == '-') {
				n++
			}
			if n == 0 {
				return nil, "", false
			}
			part, s = s[:n], s[n:]
		}
		parts = append(parts, part)
		s = strings.TrimLeft(s, " \t")
		rest, more := strings.CutPrefix(s, ".")
		if !more {
			return parts, s, true
		}
		s = rest
	}
}

// tomlScanner follows TOML values across lines: whether the next line is inside a multiline string or an array, or
// starts a new statement.
type tomlScanner struct {
	multiline string // the open multiline string's delimiter (""" or '''), or ""
	depth     int    // open [ of arrays (or a header) not closed yet
}

func (t *tomlScanner) atStatement() bool { return t.multiline == "" && t.depth == 0 }

// scan reads one line, updating the state. It returns false on what it cannot read: a one-line string left open or a
// ] without its [.
func (t *tomlScanner) scan(line string) bool {
	for i := 0; i < len(line); {
		if t.multiline != "" {
			end := closingDelimiter(line[i:], t.multiline)
			if end < 0 {
				return true // the string goes on in the next line
			}
			i += end + len(t.multiline)
			t.multiline = ""
			continue
		}
		switch c := line[i]; {
		case c == '#':
			return true
		case strings.HasPrefix(line[i:], `"""`), strings.HasPrefix(line[i:], "'''"):
			t.multiline = line[i : i+3]
			i += 3
		case c == '"' || c == '\'':
			end := closingQuote(line[i+1:], c)
			if end < 0 {
				return false
			}
			i += end + 2
		case c == '[':
			t.depth++
			i++
		case c == ']':
			t.depth--
			if t.depth < 0 {
				return false
			}
			i++
		default:
			i++
		}
	}
	return true
}

// closingDelimiter finds the end of a multiline string in s: the index of the closing delimiter (an unescaped """ for
// basic strings), or -1.
func closingDelimiter(s, delim string) int {
	for i := 0; i+len(delim) <= len(s); i++ {
		if delim == `"""` && s[i] == '\\' {
			i++
			continue
		}
		if strings.HasPrefix(s[i:], delim) {
			// A run of more quotes ends with the delimiter: """"" closes with the last three.
			for i+len(delim) < len(s) && s[i+len(delim)] == delim[0] {
				i++
			}
			return i
		}
	}
	return -1
}

// closingQuote finds the closing quote of a one-line string in s (escapes count in basic strings), or -1.
func closingQuote(s string, quote byte) int {
	for i := 0; i < len(s); i++ {
		if quote == '"' && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == quote {
			return i
		}
	}
	return -1
}

// tomlString reads a TOML basic ("...") or literal ('...') string that makes up the whole value, a trailing comment
// allowed.
func tomlString(v string) (string, bool) {
	if len(v) < 2 {
		return "", false
	}
	switch v[0] {
	case '\'':
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 || !onlyComment(v[end+2:]) {
			return "", false
		}
		return v[1 : end+1], true
	case '"':
		for i := 1; i < len(v); i++ {
			switch v[i] {
			case '\\':
				i++
			case '"':
				if !onlyComment(v[i+1:]) {
					return "", false
				}
				s, err := strconv.Unquote(v[:i+1])
				return s, err == nil
			}
		}
	}
	return "", false
}

func onlyComment(rest string) bool {
	rest = strings.TrimSpace(rest)
	return rest == "" || strings.HasPrefix(rest, "#")
}
