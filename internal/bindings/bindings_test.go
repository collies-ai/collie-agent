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
	got := Resolve(server, &config.Settings{RootFolders: []string{root}})
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
	got := Resolve([]machine.Binding{{RunnerID: 1, Folder: dir}}, &config.Settings{})
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
	if _, err := Within(root, []string{root}); err != nil {
		t.Errorf("the root itself: %v", err)
	}
	if _, err := Within(sibling, []string{root}); err == nil {
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
	got := Resolve(server, s)
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
	)
	if got[0].Usable() {
		t.Error("a pin moved the binding outside the root folders")
	}
}
