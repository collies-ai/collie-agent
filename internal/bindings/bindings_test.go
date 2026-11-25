package bindings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/collies-ai/collie-agent/internal/config"
	"github.com/collies-ai/collie-agent/internal/machine"
)

func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFolderOutsideRootsIsRefusedEvenIfCollieSendsIt(t *testing.T) {
	root := realDir(t)
	outside := realDir(t)
	inside := filepath.Join(root, "collie")
	_ = os.MkdirAll(inside, 0o755)
	// A symlink inside the root that points outside it must not let the agent out.
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	server := []machine.Binding{
		{RunnerID: 1, Project: "collies", Codebase: "collie", Folder: inside, MaxConcurrency: 2},
		{RunnerID: 2, Project: "collies", Codebase: "other", Folder: outside},
		{RunnerID: 3, Project: "collies", Codebase: "etc", Folder: "/etc"},
		{RunnerID: 4, Project: "collies", Codebase: "esc", Folder: escape},
		{RunnerID: 5, Project: "collies", Codebase: "dots", Folder: filepath.Join(root, "collie", "..", "..")},
		{RunnerID: 6, Project: "collies", Codebase: "rel", Folder: "collie"},
		{RunnerID: 7, Project: "collies", Codebase: "missing", Folder: filepath.Join(root, "missing")},
		{RunnerID: 8, Project: "collies", Codebase: "empty", Folder: ""},
	}
	got := Resolve(server, &config.Settings{RootFolders: []string{root}}, roots(root))
	if !got[0].Usable() || got[0].Folder != inside || got[0].MaxConcurrency != 2 {
		t.Errorf("binding inside the root: %+v", got[0])
	}
	for _, b := range got[1:] {
		if b.Usable() {
			t.Errorf("binding %d (%s) was accepted", b.RunnerID, b.Folder)
		}
	}
}

func TestNoRootsRefusesEverything(t *testing.T) {
	dir := realDir(t)
	got := Resolve([]machine.Binding{{RunnerID: 1, Folder: dir}}, &config.Settings{}, Allowed{})
	if got[0].Usable() || got[0].Refused != ErrNoRoots.Error() {
		t.Errorf("got %+v", got[0])
	}
}

func TestRootItselfAndSiblingPrefix(t *testing.T) {
	parent := realDir(t)
	root := filepath.Join(parent, "code")
	sibling := filepath.Join(parent, "code-other") // shares the "code" prefix but is outside
	_ = os.MkdirAll(root, 0o755)
	_ = os.MkdirAll(sibling, 0o755)
	if _, err := Within(root, roots(root)); err != nil {
		t.Errorf("the root itself: %v", err)
	}
	if _, err := Within(sibling, roots(root)); err == nil {
		t.Error("a sibling with the root as name prefix was accepted")
	}
}

func TestPinnedBindingsOverrideCollie(t *testing.T) {
	root := realDir(t)
	collieDir := filepath.Join(root, "collie")
	localDir := filepath.Join(root, "collie-local")
	agentDir := filepath.Join(root, "agent")
	for _, d := range []string{collieDir, localDir, agentDir} {
		_ = os.MkdirAll(d, 0o755)
	}
	server := []machine.Binding{
		{RunnerID: 1, Project: "collies", Codebase: "collie", Purpose: "code", Folder: collieDir,
			Agents: map[string]string{"code": "claude"}, PermissionMode: "default", MaxConcurrency: 1},
		{RunnerID: 2, Project: "collies", Codebase: "collie-agent", Purpose: "code", Folder: agentDir,
			Agents: map[string]string{"code": "claude"}, MaxConcurrency: 1},
		{RunnerID: 3, Project: "collies", Codebase: "starter", Purpose: "code", Folder: "/somewhere/else"},
	}
	s := &config.Settings{
		RootFolders: []string{root},
		Bindings: []config.PinnedBinding{
			// by project + codebase: another folder, codex for code, more concurrency, a stricter mode
			{Project: "collies", Codebase: "collie", Folder: localDir, Agents: map[string]string{"code": "codex"}, PermissionMode: "plan", MaxConcurrency: 3},
			// by runner id: turned off on this machine
			{RunnerID: 2, Disabled: true},
			// a pin can also fix a folder collie got wrong
			{RunnerID: 3, Folder: agentDir},
			// a pin for a binding collie does not send is ignored
			{Project: "collies", Codebase: "ghost", Folder: agentDir},
		},
	}
	got := Resolve(server, s, roots(root))
	if len(got) != 3 {
		t.Fatalf("got %d bindings", len(got))
	}
	b := got[0]
	if !b.Pinned || !b.Usable() || b.Folder != localDir || b.Agents["code"] != "codex" || b.PermissionMode != "plan" || b.MaxConcurrency != 3 {
		t.Errorf("pinned by project+codebase: %+v", b)
	}
	if server[0].Agents["code"] != "claude" {
		t.Error("Resolve changed collie's binding in place")
	}
	if got[1].Usable() || got[1].Refused != "disabled in the local config" {
		t.Errorf("disabled pin: %+v", got[1])
	}
	if !got[2].Usable() || got[2].Folder != agentDir {
		t.Errorf("folder pin: %+v", got[2])
	}
}

func TestPinnedFolderMustStillBeInsideRoots(t *testing.T) {
	root := realDir(t)
	outside := realDir(t)
	got := Resolve(
		[]machine.Binding{{RunnerID: 1, Project: "p", Codebase: "c", Folder: root}},
		&config.Settings{RootFolders: []string{root}, Bindings: []config.PinnedBinding{{RunnerID: 1, Folder: outside}}},
		roots(root),
	)
	if got[0].Usable() {
		t.Error("a pin moved the binding outside the root folders")
	}
}

func TestARootRedirectedDuringTheCheckDoesNotMoveTheBoundary(t *testing.T) {
	root := realDir(t)
	outside := realDir(t)
	escaped := filepath.Join(outside, "collie")
	_ = os.MkdirAll(escaped, 0o755)
	pinned := PinRoots(roots(root))

	// The binding folder already resolves outside (e.g. swapped earlier); the root itself is redirected right after
	// its redirect check, so a second resolution of the root would land in the outside tree.
	orig := evalSymlinks
	defer func() { evalSymlinks = orig }()
	rootCalls := 0
	evalSymlinks = func(p string) (string, error) {
		if p == root {
			rootCalls++
			if rootCalls > 1 {
				return outside, nil
			}
		}
		return orig(p)
	}
	if got, err := WithinPinned(escaped, pinned); err == nil {
		t.Fatalf("WithinPinned accepted %s after the root was redirected mid-check", got)
	}
	if rootCalls != 1 {
		t.Errorf("the root was resolved %d times during one check, want 1", rootCalls)
	}
}

func TestTheMachinesPermissionModeOverridesCollieAndAPinOverridesIt(t *testing.T) {
	root := realDir(t)
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	_ = os.MkdirAll(a, 0o755)
	_ = os.MkdirAll(b, 0o755)
	server := []machine.Binding{
		{RunnerID: 1, Project: "p", Codebase: "a", Folder: a, PermissionMode: "auto"},
		{RunnerID: 2, Project: "p", Codebase: "b", Folder: b, PermissionMode: "full"},
	}

	got := Resolve(server, &config.Settings{RootFolders: []string{root}}, roots(root))
	if got[0].PermissionMode != "auto" || got[0].PermissionFrom != "collie" || got[1].PermissionMode != "full" {
		t.Errorf("without local settings: %+v", got)
	}

	s := &config.Settings{RootFolders: []string{root}, PermissionMode: "plan",
		Bindings: []config.PinnedBinding{{RunnerID: 2, PermissionMode: "auto"}}}
	got = Resolve(server, s, roots(root))
	if got[0].PermissionMode != "plan" || got[0].PermissionFrom != "machine" {
		t.Errorf("the machine's mode did not override collie's: %+v", got[0])
	}
	if got[1].PermissionMode != "auto" || got[1].PermissionFrom != "pin" {
		t.Errorf("the pin did not override the machine's mode: %+v", got[1])
	}
}

func roots(r ...string) Allowed { return Allowed{Roots: r} }

func TestExactFolderIsAllowedButNotItsChildren(t *testing.T) {
	repo := realDir(t)
	child := filepath.Join(repo, "sub")
	_ = os.MkdirAll(child, 0o755)
	link := filepath.Join(realDir(t), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	allowed := Allowed{Exact: []string{repo}}
	for _, ok := range []string{repo, link} {
		if got, err := Within(ok, allowed); err != nil || got != repo {
			t.Errorf("Within(%s) = %q, %v; want %s", ok, got, err, repo)
		}
	}
	if _, err := Within(child, allowed); err == nil {
		t.Error("a child of an exact folder was allowed")
	}
	if _, err := Within(filepath.Dir(repo), allowed); err == nil {
		t.Error("the parent of an exact folder was allowed")
	}
	got := Resolve([]machine.Binding{{RunnerID: 1, Folder: repo}, {RunnerID: 2, Folder: child}}, &config.Settings{}, allowed)
	if !got[0].Usable() || got[1].Usable() {
		t.Errorf("Resolve = %+v", got)
	}
	pinned := PinRoots(allowed)
	if _, err := WithinPinned(repo, pinned); err != nil {
		t.Errorf("WithinPinned(exact) = %v", err)
	}
	if _, err := WithinPinned(child, pinned); err == nil {
		t.Error("WithinPinned allowed a child of an exact folder")
	}
}

func TestWithoutExactFoldersOnlyRootsAreAllowed(t *testing.T) {
	root, repo := realDir(t), realDir(t)
	if _, err := Within(repo, roots(root)); err == nil {
		t.Error("a folder outside the roots was allowed with no exact folders")
	}
	if _, err := Within(repo, Allowed{Roots: []string{root}, Exact: []string{repo}}); err != nil {
		t.Errorf("exact folder next to roots: %v", err)
	}
}

func TestASwappedExactFolderIsRefused(t *testing.T) {
	parent := realDir(t)
	repo := filepath.Join(parent, "repo") // canonical, as discovery hands it over
	evil := filepath.Join(parent, "evil")
	_ = os.MkdirAll(repo, 0o755)
	_ = os.MkdirAll(evil, 0o755)
	swap := func() {
		t.Helper()
		_ = os.RemoveAll(repo)
		if err := os.Symlink(evil, repo); err != nil {
			t.Fatal(err)
		}
	}
	allowed := Allowed{Exact: []string{repo}}

	// Swapped after the sync: the pinned folder no longer resolves where it did.
	pinned := PinRoots(allowed)
	swap()
	for _, f := range []string{repo, evil} {
		if got, err := WithinPinned(f, pinned); err == nil {
			t.Errorf("WithinPinned accepted %s after the swap: %s", f, got)
		}
	}
	// Swapped before a later sync: the exact folder is not followed to its new target.
	for _, f := range []string{repo, evil} {
		if got, err := Within(f, allowed); err == nil {
			t.Errorf("Within accepted %s: %s", f, got)
		}
	}
	if p := PinRoots(allowed); len(p.Exact) != 0 {
		t.Errorf("a swapped exact folder was pinned: %+v", p.Exact)
	}
}
