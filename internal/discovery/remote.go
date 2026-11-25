package discovery

import (
	"bufio"
	"os"
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

// hostResolver tells whether a remote's host is github.com, resolving ssh Host aliases.
type hostResolver struct {
	aliases map[string]string // lowercase Host alias -> lowercase HostName
}

// newHostResolver reads the Host/HostName pairs of the ssh config at SSHConfigPath. A missing file means no aliases.
func newHostResolver() *hostResolver {
	return &hostResolver{aliases: readSSHAliases(SSHConfigPath())}
}

// GitHubRepo returns the remote's "owner/name" when it is a github.com repo (directly, or through an ssh Host alias
// whose HostName is github.com), and "" otherwise.
func (h *hostResolver) GitHubRepo(remote string) string {
	host, name, ssh, ok := ParseRemote(remote)
	if !ok {
		return ""
	}
	if githubHosts[host] {
		return name
	}
	if ssh && h != nil && githubHosts[h.aliases[host]] {
		return name
	}
	return ""
}

// readSSHAliases parses only the Host and HostName keywords of an ssh client config: for each exact (non-wildcard)
// Host name, the first HostName given in its block. Match blocks and Include are ignored.
func readSSHAliases(path string) map[string]string {
	out := map[string]string{}
	if path == "" {
		return out
	}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	var current []string // hosts of the block being read; nil inside a Match block
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value := sshKeyword(sc.Text())
		switch key {
		case "host":
			current = nil
			for _, h := range strings.Fields(value) {
				if strings.ContainsAny(h, "*?!") {
					continue
				}
				current = append(current, strings.ToLower(h))
			}
		case "match":
			current = nil
		case "hostname":
			hn := strings.ToLower(strings.Trim(value, `"`))
			for _, h := range current {
				if _, set := out[h]; !set {
					out[h] = hn
				}
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
