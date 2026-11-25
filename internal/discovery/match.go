package discovery

import (
	"sort"
	"strings"

	"github.com/collies-ai/collie-agent/internal/machine"
)

// Candidate is a clone of a codebase's repo found on this machine.
type Candidate struct {
	Repo Repo
	// Remote is the name of the remote that matched the codebase: "origin", or another one (e.g. "upstream" of a
	// fork) when origin points elsewhere.
	Remote string
}

// ViaOtherRemote tells whether the clone matched through a remote other than origin (a fork, usually).
func (c Candidate) ViaOtherRemote() bool { return c.Remote != "origin" }

// CodebaseMatch is a codebase with every clone of it found on the machine, the chosen one first.
type CodebaseMatch struct {
	CodebaseID int64
	Candidates []Candidate
}

// Chosen is the clone collie-agent proposes for the codebase.
func (m CodebaseMatch) Chosen() Candidate { return m.Candidates[0] }

// Match finds, for each codebase with a repo_full_name, the repos whose github.com remote equals it
// (case-insensitive). A remote on another host never matches, even with the same owner/name; an ssh Host alias counts
// when ~/.ssh/config resolves it to github.com. Within a repo, origin wins over other remotes. A repo matching several
// codebases (the same repo in two projects) is a candidate of each. Repos appearing twice (same Path) count once,
// keeping the latest LastUsed.
//
// Several clones of one codebase are ranked: a clone matched through origin before one matched through another
// remote, a main checkout before a worktree, the most recently used first, then the one on its default branch; the
// path breaks ties. Codebases come out in the order given, those with no clone left out.
func Match(repos []Repo, codebases []machine.Codebase) []CodebaseMatch {
	hosts := newHostResolver()
	repos = dedupe(repos)

	// For each repo, the remote that names each owner/name: origin first, then the others in config order.
	type hit struct {
		repo   int
		remote string
	}
	byName := map[string][]hit{}
	for i, r := range repos {
		found := map[string]bool{}
		for _, rem := range orderRemotes(r.Remotes) {
			name := hosts.GitHubRepo(rem.URL)
			if name == "" || found[name] {
				continue
			}
			found[name] = true
			byName[name] = append(byName[name], hit{repo: i, remote: rem.Name})
		}
	}

	var out []CodebaseMatch
	for _, c := range codebases {
		name := strings.ToLower(strings.TrimSpace(c.RepoFullName))
		hits := byName[name]
		if name == "" || len(hits) == 0 {
			continue
		}
		cands := make([]Candidate, 0, len(hits))
		for _, h := range hits {
			cands = append(cands, Candidate{Repo: repos[h.repo], Remote: h.remote})
		}
		sort.SliceStable(cands, func(i, j int) bool { return better(cands[i], cands[j]) })
		out = append(out, CodebaseMatch{CodebaseID: c.CodebaseID, Candidates: cands})
	}
	return out
}

// better tells whether clone a ranks before clone b.
func better(a, b Candidate) bool {
	if a.ViaOtherRemote() != b.ViaOtherRemote() {
		return !a.ViaOtherRemote()
	}
	if a.Repo.Worktree != b.Repo.Worktree {
		return !a.Repo.Worktree
	}
	if !a.Repo.LastUsed.Equal(b.Repo.LastUsed) {
		return a.Repo.LastUsed.After(b.Repo.LastUsed)
	}
	if da, db := a.Repo.onDefaultBranch(a.Remote), b.Repo.onDefaultBranch(b.Remote); da != db {
		return da
	}
	return a.Repo.Path < b.Repo.Path
}

// orderRemotes puts origin first and keeps the others in config order.
func orderRemotes(remotes []Remote) []Remote {
	out := make([]Remote, 0, len(remotes))
	for _, r := range remotes {
		if r.Name == "origin" {
			out = append(out, r)
		}
	}
	for _, r := range remotes {
		if r.Name != "origin" {
			out = append(out, r)
		}
	}
	return out
}

// dedupe keeps one repo per Path, in first-seen order, with the latest LastUsed of its copies.
func dedupe(repos []Repo) []Repo {
	idx := map[string]int{}
	out := make([]Repo, 0, len(repos))
	for _, r := range repos {
		if i, ok := idx[r.Path]; ok {
			if r.LastUsed.After(out[i].LastUsed) {
				out[i].LastUsed = r.LastUsed
			}
			continue
		}
		idx[r.Path] = len(out)
		out = append(out, r)
	}
	return out
}

// Inventory flattens the matches into the inventory sent to collie: every clone of each codebase, the chosen one
// first.
func Inventory(matches []CodebaseMatch) []machine.InventoryRepo {
	var out []machine.InventoryRepo
	for _, m := range matches {
		for _, c := range m.Candidates {
			out = append(out, machine.InventoryRepo{CodebaseID: m.CodebaseID, Path: c.Repo.Path})
		}
	}
	return out
}
