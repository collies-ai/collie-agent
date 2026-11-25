package discovery

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// SSHConfigPath returns the ssh client config read to resolve Host aliases (git@github-work:owner/name). Tests
// replace it.
var SSHConfigPath = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "config")
}

// githubHosts are the hosts that serve github.com repos.
var githubHosts = map[string]bool{"github.com": true, "ssh.github.com": true, "www.github.com": true}

// ParseRemote splits a git remote URL into its host (lowercase, without user or port) and "owner/name" (lowercase).
// It understands https (with or without credentials), ssh:// and scp-like (git@host:owner/name) remotes, with or
// without .git. ssh reports whether the URL goes over ssh, where the host may be an alias of ~/.ssh/config. ok is
// false for a local path or a URL without owner/name.
func ParseRemote(remote string) (host, fullName string, ssh, ok bool) {
	r := strings.TrimSpace(remote)
	var authority, path string
	switch {
	case strings.Contains(r, "://"):
		scheme, rest, _ := strings.Cut(r, "://")
		a, p, found := strings.Cut(rest, "/")
		if !found {
			return "", "", false, false
		}
		authority, path = a, p
		scheme = strings.ToLower(scheme)
		ssh = scheme == "ssh" || scheme == "git+ssh" || scheme == "ssh+git"
		if scheme == "file" {
			return "", "", false, false
		}
	case strings.Contains(r, ":") && !strings.HasPrefix(r, "/"):
		authority, path, _ = strings.Cut(r, ":") // scp-like: [user@]host:path
		ssh = true
	default:
		return "", "", false, false // a local path
	}
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	if h, _, found := strings.Cut(authority, ":"); found {
		authority = h
	}
	host = strings.ToLower(authority)
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if host == "" || len(parts) < 2 {
		return "", "", false, false
	}
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || name == "" {
		return "", "", false, false
	}
	return host, strings.ToLower(owner + "/" + name), ssh, true
}

// RepoFullName turns a git remote URL into "owner/name" (lowercase), or "" when it does not look like one. It does
// not look at the host: use GitHubRepo to know whether the remote is a github.com repo.
func RepoFullName(remote string) string {
	_, name, _, _ := ParseRemote(remote)
	return name
}

// hostResolver tells whether a remote's host is github.com, applying ~/.ssh/config to ssh remotes.
type hostResolver struct {
	blocks []sshBlock
}

// sshBlock is a Host block of an ssh client config: its patterns and the HostName it sets ("" when none).
type sshBlock struct {
	patterns []string
	hostName string
}

// newHostResolver reads the Host/HostName pairs of the ssh config at SSHConfigPath. A missing file means none.
func newHostResolver() *hostResolver {
	return &hostResolver{blocks: readSSHConfig(SSHConfigPath())}
}

// GitHubRepo returns the remote's "owner/name" when it is a github.com repo, and "" otherwise. For an ssh remote the
// host is first resolved as ssh would (the HostName of the first matching Host block), so an alias pointing at
// github.com counts and a "Host github.com" pointing elsewhere does not.
func (h *hostResolver) GitHubRepo(remote string) string {
	host, name, ssh, ok := ParseRemote(remote)
	if !ok {
		return ""
	}
	if ssh && h != nil {
		host = h.hostName(host)
	}
	if githubHosts[host] {
		return name
	}
	return ""
}

// hostName is the host ssh connects to for host: the HostName of the first Host block that matches it and sets one
// (%h standing for host), or host itself.
func (h *hostResolver) hostName(host string) string {
	for _, b := range h.blocks {
		if b.hostName != "" && hostMatches(host, b.patterns) {
			return strings.ToLower(strings.ReplaceAll(b.hostName, "%h", host))
		}
	}
	return host
}

// hostMatches applies ssh's Host matching: any pattern (with * and ?) matches, and no negated (!) pattern does.
func hostMatches(host string, patterns []string) bool {
	matched := false
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		ok, err := path.Match(strings.TrimPrefix(p, "!"), host)
		if err != nil || !ok {
			continue
		}
		if neg {
			return false
		}
		matched = true
	}
	return matched
}

// readSSHConfig parses only the Host and HostName keywords of an ssh client config, in file order: each Host block's
// patterns (lowercase) and its first HostName. Match blocks and Include are ignored.
func readSSHConfig(file string) []sshBlock {
	if file == "" {
		return nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []sshBlock
	cur := -1 // index of the Host block being read; -1 before the first or inside a Match block
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value := sshKeyword(sc.Text())
		switch key {
		case "host":
			out = append(out, sshBlock{patterns: strings.Fields(strings.ToLower(value))})
			cur = len(out) - 1
		case "match":
			cur = -1
		case "hostname":
			if cur >= 0 && out[cur].hostName == "" {
				out[cur].hostName = strings.Trim(value, `"`)
			}
		}
	}
	return out
}

// sshKeyword splits an ssh config line into its lowercase keyword and value ("Key value" or "Key=value").
func sshKeyword(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", ""
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), ""
	}
	key := strings.ToLower(line[:i])
	value := strings.TrimLeft(line[i:], " \t")
	value = strings.TrimPrefix(value, "=")
	return key, strings.TrimSpace(value)
}
