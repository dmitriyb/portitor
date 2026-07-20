package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBakeHooksAtomicAndVersioned (L-P1a, PORT-14): baked shims carry the
// version marker, are executable, and the write is atomic (no leftover temp).
func TestBakeHooksAtomicAndVersioned(t *testing.T) {
	bare := t.TempDir()
	cfg := "/etc/portitor/repos.d/x.json"
	if err := bakeHooks(bare, cfg); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"pre-receive", "post-receive"} {
		p := filepath.Join(bare, "hooks", hook)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, hookMarker) {
			t.Fatalf("%s missing version marker:\n%s", hook, s)
		}
		if !strings.Contains(s, "export PORTITOR_CONFIG="+shellQuote(cfg)) {
			t.Fatalf("%s missing config export:\n%s", hook, s)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm()&0o100 == 0 {
			t.Fatalf("%s is not executable (mode %v)", hook, fi.Mode())
		}
	}
	// Atomic write leaves no temp files behind.
	entries, _ := os.ReadDir(filepath.Join(bare, "hooks"))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
	// The config path round-trips through the shim (baked → read back).
	baked, ok := bakedHookConfig(bare)
	if !ok || baked != cfg {
		t.Fatalf("baked config = %q ok=%v, want %q", baked, ok, cfg)
	}
}

// TestUpgradeRepo (PORT-14): upgrade-repo re-bakes hooks idempotently,
// preserving the config path already baked in.
func TestUpgradeRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	t.Setenv("PORTITOR_REPO_ROOT", root)
	t.Setenv("PORTITOR_REPOS_DIR", filepath.Join(root, "repos.d"))
	bare := filepath.Join(root, "myrepo.git")

	// Provision with a stale hand-written shim (no version marker).
	if err := os.MkdirAll(filepath.Join(bare, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := "#!/bin/sh\nexport PORTITOR_CONFIG='/etc/portitor/repos.d/myrepo.json'\nexec portitor pre-receive\n"
	if err := os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), []byte(stale), 0o755); err != nil {
		t.Fatal(err)
	}

	if rc := upgradeRepo([]string{"--repo", "myrepo"}); rc != 0 {
		t.Fatalf("upgrade-repo rc = %d", rc)
	}
	b, _ := os.ReadFile(filepath.Join(bare, "hooks", "pre-receive"))
	if !strings.Contains(string(b), hookMarker) {
		t.Fatalf("re-baked shim missing version marker:\n%s", b)
	}
	if !strings.Contains(string(b), "repos.d/myrepo.json") {
		t.Fatalf("re-baked shim lost the config path:\n%s", b)
	}
	// Idempotent second run.
	if rc := upgradeRepo([]string{"--repo", "myrepo"}); rc != 0 {
		t.Fatalf("second upgrade-repo rc = %d", rc)
	}
}

func TestValidSlug(t *testing.T) {
	ok := []string{"owner/name", "o-1/n_2", "Owner/Repo.git-ish"}
	bad := []string{"", "owner", "owner/name/extra", "/name", "owner/", "-owner/name", "a b/c", "owner/na me"}
	for _, s := range ok {
		if !validSlug(s) {
			t.Errorf("validSlug(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if validSlug(s) {
			t.Errorf("validSlug(%q) = true, want false", s)
		}
	}
}

func TestDeriveSlugValidation(t *testing.T) {
	// Shape-malformed inputs derive to "" (never a bogus gh -R target): a
	// single segment, a leading-dash segment, or whitespace.
	for _, url := range []string{"not-a-url", "https://github.com/-bad/name", "https://github.com/ow ner/name", ""} {
		if got := deriveSlug(url); got != "" {
			t.Errorf("deriveSlug(%q) = %q, want empty", url, got)
		}
	}
	if got := deriveSlug("git@github.com:owner/name.git"); got != "owner/name" {
		t.Errorf("deriveSlug scp form = %q", got)
	}
	if got := deriveSlug("https://github.com/owner/name.git"); got != "owner/name" {
		t.Errorf("deriveSlug https form = %q", got)
	}
}
