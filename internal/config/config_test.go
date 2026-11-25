package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveDefaultAndNamedProfiles(t *testing.T) {
	base := t.TempDir()
	t.Setenv("COLLIE_AGENT_CONFIG_DIR", base)

	def, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	work, err := Resolve("work")
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != DefaultProfile || def.Dir != filepath.Join(base, "collie-agent") {
		t.Errorf("default profile = %+v", def)
	}
	if work.Dir != filepath.Join(base, "collie-agent-work") {
		t.Errorf("work profile dir = %s", work.Dir)
	}
	if def.CredentialsPath() == work.CredentialsPath() || def.LockPath() == work.LockPath() || def.StatePath() == work.StatePath() {
		t.Error("profiles share files")
	}
}

func TestResolveUsesXDGThenHome(t *testing.T) {
	t.Setenv("COLLIE_AGENT_CONFIG_DIR", "")
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	p, _ := Resolve("")
	if p.Dir != filepath.Join(xdg, "collie-agent") {
		t.Errorf("dir = %s", p.Dir)
	}
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	p, _ = Resolve("")
	if p.Dir != filepath.Join(home, ".config", "collie-agent") {
		t.Errorf("dir = %s", p.Dir)
	}
}

func TestValidateProfile(t *testing.T) {
	for _, ok := range []string{"default", "work", "a", "team-2"} {
		if err := ValidateProfile(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Work", "../x", "a/b", "-x", "a b", "x.y", "0123456789012345678901234567890123"} {
		if ValidateProfile(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCredentialsAreWrittenWith0600(t *testing.T) {
	t.Setenv("COLLIE_AGENT_CONFIG_DIR", t.TempDir())
	p, _ := Resolve("")
	if _, err := p.LoadCredentials(); err != ErrNotPaired {
		t.Fatalf("before pairing: %v, want ErrNotPaired", err)
	}
	c := &Credentials{Server: "https://collies.sh", MachineID: "42", MachineToken: "secret", Name: "laptop", PairedAt: time.Now().UTC()}
	if err := p.SaveCredentials(c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.CredentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials mode = %o, want 600", perm)
	}
	dir, _ := os.Stat(p.Dir)
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %o, want 700", perm)
	}
	got, err := p.LoadCredentials()
	if err != nil || got.MachineToken != "secret" || got.MachineID != "42" {
		t.Fatalf("loaded %+v, %v", got, err)
	}

	// Overwriting keeps 0600 even if someone loosened the file.
	_ = os.Chmod(p.CredentialsPath(), 0o644)
	if err := p.SaveCredentials(c); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(p.CredentialsPath())
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("after overwrite mode = %o, want 600", perm)
	}
	entries, _ := os.ReadDir(p.Dir)
	if len(entries) != 1 {
		t.Errorf("leftover temp files: %v", entries)
	}

	if err := p.DeleteCredentials(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.LoadCredentials(); err != ErrNotPaired {
		t.Errorf("after delete: %v", err)
	}
}

func TestNormalizeServer(t *testing.T) {
	good := map[string]string{
		"https://collies.sh":         "https://collies.sh",
		"https://collies.sh/":        "https://collies.sh",
		" https://collie.example/x/": "https://collie.example/x",
		"http://localhost:3000":      "http://localhost:3000",
		"http://127.0.0.1:3000/":     "http://127.0.0.1:3000",
	}
	for in, want := range good {
		got, err := NormalizeServer(in)
		if err != nil || got != want {
			t.Errorf("NormalizeServer(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "collies.sh", "http://collies.sh", "ftp://collies.sh", "https://collies.sh?x=1"} {
		if _, err := NormalizeServer(bad); err == nil {
			t.Errorf("NormalizeServer(%q) accepted", bad)
		}
	}
}
