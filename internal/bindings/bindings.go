// Package bindings turns collie's bindings into the ones this machine actually serves. The machine has the last word:
// pinned bindings in the local config override collie's, and a folder outside the allowed folders (inside a root folder,
// or exactly a discovered repo) is always refused.
package bindings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/machine"
)

// Binding is a binding as this machine serves it. Refused is non-empty when the machine will not serve it.
type Binding struct {
	machine.Binding
	// Pinned says the local config changed this binding.
	Pinned bool
	// PermissionFrom says where PermissionMode comes from: "collie", "machine" (the config's permission_mode, which
	// overrides collie) or "pin" (the binding's pin, which overrides both).
	PermissionFrom string
	// Refused is why the machine refuses it (folder outside the root folders, missing, disabled locally...).
	Refused string
}

// Usable reports whether dispatches for this binding may be accepted.
func (b Binding) Usable() bool { return b.Refused == "" }

// Allowed is where a binding's folder may be: inside one of the Roots (the root folders), or exactly one of the Exact
// folders (repos discovered from Claude Code or Codex, never their children). Exact folders are the symlink-free paths
// discovery found: one that resolves anywhere else is ignored.
type Allowed struct {
	Roots []string
	Exact []string
}

func (a Allowed) empty() bool { return len(a.Roots) == 0 && len(a.Exact) == 0 }

// CoordinatorPurpose is the purpose of a binding that runs the project's coordinator (collie's goal #160): a
// project-level binding with no codebase and no folder, whose sessions plan goals over collie's MCP and touch no repo.
const CoordinatorPurpose = "coordinator"

// IsCoordinator reports whether the binding runs the project's coordinator.
func IsCoordinator(b machine.Binding) bool { return b.Purpose == CoordinatorPurpose }

// Resolve applies the local settings to collie's bindings: the machine's permission mode overrides collie's, pinned
// fields override both, and every folder is checked against the allowed folders. Pins that match no binding from
// collie are ignored: without a runner_id from collie there is nothing to dispatch.
func Resolve(server []machine.Binding, s *config.Settings, allowed Allowed) []Binding {
	if s == nil {
		s = &config.Settings{}
	}
	out := make([]Binding, 0, len(server))
	for _, sb := range server {
		b := Binding{Binding: sb, PermissionFrom: "collie"}
		b.Agents = copyMap(sb.Agents)
		if s.PermissionMode != "" {
			b.PermissionMode = s.PermissionMode
			b.PermissionFrom = "machine"
		}
		if pin := findPin(s.Bindings, sb); pin != nil {
			b.Pinned = true
			apply(&b, pin)
			if pin.Disabled {
				b.Refused = "disabled in the local config"
			}
		}
		switch {
		case b.Refused != "":
		case IsCoordinator(b.Binding):
			// A coordinator binding is project-level: no codebase and no folder. Its sessions run in a scratch dir of
			// their own with collie's MCP only, so there is no folder to check, and a pinned folder is ignored.
			b.Folder = ""
		default:
			if folder, err := Within(b.Folder, allowed); err != nil {
				b.Refused = err.Error()
			} else {
				b.Folder = folder
			}
		}
		if b.MaxConcurrency <= 0 {
			b.MaxConcurrency = 1
		}
		out = append(out, b)
	}
	return out
}

func findPin(pins []config.PinnedBinding, sb machine.Binding) *config.PinnedBinding {
	// A pin by runner_id wins over one by project + codebase.
	for i := range pins {
		if pins[i].RunnerID != 0 && pins[i].RunnerID == sb.RunnerID {
			return &pins[i]
		}
	}
	for i := range pins {
		p := &pins[i]
		if p.RunnerID != 0 || p.Project == "" || p.Codebase == "" {
			continue
		}
		if p.Project == sb.Project && p.Codebase == sb.Codebase && (p.Purpose == "" || p.Purpose == sb.Purpose) {
			return p
		}
	}
	return nil
}

func apply(b *Binding, p *config.PinnedBinding) {
	if p.Folder != "" {
		if abs, err := config.ExpandPath(p.Folder); err == nil {
			b.Folder = abs
		} else {
			b.Folder = p.Folder
		}
	}
	if len(p.Agents) > 0 {
		if b.Agents == nil {
			b.Agents = map[string]string{}
		}
		for k, v := range p.Agents {
			b.Agents[k] = v
		}
	}
	if p.PermissionMode != "" {
		b.PermissionMode = p.PermissionMode
		b.PermissionFrom = "pin"
	}
	if p.MaxConcurrency > 0 {
		b.MaxConcurrency = p.MaxConcurrency
	}
}

// ErrNoRoots means no folder can be allowed: no root folders are configured and no repo was discovered.
var ErrNoRoots = errors.New("no root folders configured (collie-agent roots add <folder>) and no discovered repos")

// Within returns folder resolved (absolute, symlinks followed) when it is an existing directory inside one of the
// root folders or exactly one of the exact folders, and an error otherwise. Symlinks are resolved on both sides, so a
// link inside a root cannot point the agent outside it.
func Within(folder string, allowed Allowed) (string, error) {
	if allowed.empty() {
		return "", ErrNoRoots
	}
	real, err := resolveFolder(folder)
	if err != nil {
		return "", err
	}
	if !inside(real, resolveAll(allowed.Roots)) && !slices.Contains(canonical(allowed.Exact), real) {
		return "", outside(folder, len(allowed.Exact) > 0)
	}
	return real, nil
}

// canonical keeps the exact folders that still resolve to themselves. Discovery hands over symlink-free paths, so one
// that now resolves elsewhere (replaced by a symlink) was never authorized where it points and does not count.
func canonical(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = filepath.Clean(p)
		if r, err := evalSymlinks(p); err == nil && r == p {
			out = append(out, p)
		}
	}
	return out
}

// outside is the refusal of a folder that is not allowed.
func outside(folder string, withExact bool) error {
	if withExact {
		return fmt.Errorf("folder %q is outside the root folders and is not a discovered repo", folder)
	}
	return fmt.Errorf("folder %q is outside the root folders", folder)
}

// resolveAll resolves the symlinks of every path, leaving out those that do not resolve.
func resolveAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if r, err := evalSymlinks(filepath.Clean(p)); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// evalSymlinks is filepath.EvalSymlinks; tests replace it to redirect a path between two checks.
var evalSymlinks = filepath.EvalSymlinks

// resolveFolder checks folder is an absolute path to an existing directory and returns it with symlinks resolved.
func resolveFolder(folder string) (string, error) {
	if strings.TrimSpace(folder) == "" {
		return "", errors.New("the binding has no folder")
	}
	if !filepath.IsAbs(folder) {
		return "", fmt.Errorf("folder %q is not an absolute path", folder)
	}
	real, err := evalSymlinks(filepath.Clean(folder))
	if err != nil {
		return "", fmt.Errorf("folder %q does not exist on this machine", folder)
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return "", fmt.Errorf("folder %q is not a directory", folder)
	}
	return real, nil
}

// inside reports whether the resolved path real is one of the resolved roots or below one. It resolves nothing.
func inside(real string, resolvedRoots []string) bool {
	for _, r := range resolvedRoots {
		rel, err := filepath.Rel(r, real)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Root is an allowed folder (a root folder or an exact one) and where it resolved (symlinks followed) when the
// bindings were synced.
type Root struct {
	Path     string
	Resolved string
}

// Pinned is Allowed resolved at sync time.
type Pinned struct {
	Roots []Root
	Exact []Root
}

// PinRoots resolves the allowed folders now. Roots that do not resolve are left out, and so are exact folders that no
// longer resolve to themselves.
func PinRoots(allowed Allowed) Pinned {
	exact := make([]Root, 0, len(allowed.Exact))
	for _, p := range canonical(allowed.Exact) {
		exact = append(exact, Root{Path: p, Resolved: p})
	}
	return Pinned{Roots: pin(allowed.Roots), Exact: exact}
}

func pin(paths []string) []Root {
	out := make([]Root, 0, len(paths))
	for _, r := range paths {
		if res, err := evalSymlinks(filepath.Clean(r)); err == nil {
			out = append(out, Root{Path: r, Resolved: res})
		}
	}
	return out
}

// stillPinned returns the resolved targets of the folders that still resolve where they did when pinned; redirected
// says some no longer do.
func stillPinned(roots []Root) (still []string, redirected bool) {
	for _, r := range roots {
		res, err := evalSymlinks(filepath.Clean(r.Path))
		if err != nil || res != r.Resolved {
			redirected = true
			continue
		}
		still = append(still, r.Resolved)
	}
	return still, redirected
}

// WithinPinned is Within against folders pinned at sync time: a folder (or an ancestor of it) that now resolves
// somewhere else was redirected and no longer counts, so a swap of the folder itself cannot move the boundary.
func WithinPinned(folder string, pinned Pinned) (string, error) {
	roots, rootsMoved := stillPinned(pinned.Roots)
	exact, exactMoved := stillPinned(pinned.Exact)
	if len(roots) == 0 && len(exact) == 0 {
		if exactMoved {
			return "", errors.New("the root folders or the discovered repos changed since the bindings were synced")
		}
		if rootsMoved {
			return "", errors.New("the root folders changed since the bindings were synced")
		}
		return "", ErrNoRoots
	}
	real, err := resolveFolder(folder)
	if err != nil {
		return "", err
	}
	// Compare against the targets pinned at sync, never against the folders resolved again: a redirect after the
	// check above cannot move the boundary.
	if !inside(real, roots) && !slices.Contains(exact, real) {
		return "", outside(folder, len(pinned.Exact) > 0)
	}
	return real, nil
}
