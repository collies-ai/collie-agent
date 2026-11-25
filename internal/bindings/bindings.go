// Package bindings turns collie's bindings into the ones this machine actually serves. The machine has the last word:
// pinned bindings in the local config override collie's, and a folder outside the root folders is always refused.
package bindings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/machine"
)

// Binding is a binding as this machine serves it. Refused is non-empty when the machine will not serve it.
type Binding struct {
	machine.Binding
	// Pinned says the local config changed this binding.
	Pinned bool
	// Refused is why the machine refuses it (folder outside the root folders, missing, disabled locally...).
	Refused string
}

// Usable reports whether dispatches for this binding may be accepted.
func (b Binding) Usable() bool { return b.Refused == "" }

// Resolve applies the local settings to collie's bindings: pinned fields override collie's, and every folder is
// checked against the root folders. Pins that match no binding from collie are ignored: without a runner_id from
// collie there is nothing to dispatch.
func Resolve(server []machine.Binding, s *config.Settings) []Binding {
	if s == nil {
		s = &config.Settings{}
	}
	out := make([]Binding, 0, len(server))
	for _, sb := range server {
		b := Binding{Binding: sb}
		b.Agents = copyMap(sb.Agents)
		if pin := findPin(s.Bindings, sb); pin != nil {
			b.Pinned = true
			apply(&b, pin)
			if pin.Disabled {
				b.Refused = "disabled in the local config"
			}
		}
		if b.Refused == "" {
			if folder, err := Within(b.Folder, s.RootFolders); err != nil {
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
	}
	if p.MaxConcurrency > 0 {
		b.MaxConcurrency = p.MaxConcurrency
	}
}

// ErrNoRoots means no root folders are configured, so no folder can be allowed.
var ErrNoRoots = errors.New("no root folders configured (collie-agent roots add <folder>)")

// Within returns folder resolved (absolute, symlinks followed) when it is an existing directory inside one of the
// root folders, and an error otherwise. Symlinks are resolved on both sides, so a link inside a root cannot point
// the agent outside it.
func Within(folder string, roots []string) (string, error) {
	if len(roots) == 0 {
		return "", ErrNoRoots
	}
	if strings.TrimSpace(folder) == "" {
		return "", errors.New("the binding has no folder")
	}
	if !filepath.IsAbs(folder) {
		return "", fmt.Errorf("folder %q is not an absolute path", folder)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(folder))
	if err != nil {
		return "", fmt.Errorf("folder %q does not exist on this machine", folder)
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return "", fmt.Errorf("folder %q is not a directory", folder)
	}
	for _, root := range roots {
		r, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(r, real)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return real, nil
		}
	}
	return "", fmt.Errorf("folder %q is outside the root folders", folder)
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

// Root is a root folder and where it resolved (symlinks followed) when the bindings were synced.
type Root struct {
	Path     string
	Resolved string
}

// PinRoots resolves the root folders now. Roots that do not resolve are left out.
func PinRoots(roots []string) []Root {
	out := make([]Root, 0, len(roots))
	for _, r := range roots {
		if res, err := filepath.EvalSymlinks(filepath.Clean(r)); err == nil {
			out = append(out, Root{Path: r, Resolved: res})
		}
	}
	return out
}

// WithinPinned is Within against roots pinned at sync time: a root (or an ancestor of it) that now resolves
// somewhere else was redirected and no longer counts, so a swap of the root itself cannot move the boundary.
func WithinPinned(folder string, roots []Root) (string, error) {
	var still []string
	redirected := false
	for _, r := range roots {
		res, err := filepath.EvalSymlinks(filepath.Clean(r.Path))
		if err != nil || res != r.Resolved {
			redirected = true
			continue
		}
		still = append(still, r.Resolved)
	}
	if len(still) == 0 {
		if redirected {
			return "", errors.New("the root folders changed since the bindings were synced")
		}
		return "", ErrNoRoots
	}
	return Within(folder, still)
}
