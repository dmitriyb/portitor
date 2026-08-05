// This file used to hold the separation-of-duties (SoD) unit test
// (TestRequesterSignedHead). That hardcoded check — and the
// requesterSignedPR/requesterSignedHead helpers it pinned — is removed (see
// spec/proposals/2026-08-05-transparent-approve.md): reviewer-≠-author
// integrity now lives in GitHub branch protection (separated accounts) or
// the push-time content_rules that already gate a bead-close to the
// reviewer key (single-account git-content). The remaining tests here
// (bakedHookConfig / init-repo registry defaults) are unrelated to SoD and
// stay; mustRun is also used by e2e_test.go.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustRun(t *testing.T, bin string, args ...string) {
	t.Helper()
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
}

// TestBakedHookConfig verifies the add-role divergence cross-check helpers: the
// baked PORTITOR_CONFIG is extracted from the shim (shell-quoting reversed),
// and absence (no repo / no shim / no export line) reports ok=false.
func TestBakedHookConfig(t *testing.T) {
	bare := t.TempDir()
	if _, ok := bakedHookConfig(bare); ok {
		t.Fatal("missing shim should report ok=false")
	}
	hooks := filepath.Join(bare, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nexport PORTITOR_CONFIG=" + shellQuote("/etc/portitor/repos.d/it's.json") + "\nexec portitor pre-receive\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := bakedHookConfig(bare)
	if !ok || got != "/etc/portitor/repos.d/it's.json" {
		t.Fatalf("baked = %q ok=%v", got, ok)
	}
	if !samePath("/a/b/../c", "/a/c") {
		t.Fatal("samePath should normalize")
	}
	if samePath("/a/c", "/a/d") {
		t.Fatal("distinct paths must differ")
	}
}

// TestInitRepoDefaultsToRegistry pins PORT-4: a plain init-repo (no --config)
// bakes the REGISTRY path into the hook shims — the same file add-role and
// `portitor pr` read — never a per-bare private location.
func TestInitRepoDefaultsToRegistry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	reposDir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	bare := filepath.Join(root, "myrepo.git")
	if rc := initRepo([]string{"--bare", bare}); rc != 0 {
		t.Fatalf("init-repo rc = %d", rc)
	}
	baked, ok := bakedHookConfig(bare)
	if !ok {
		t.Fatal("no baked hook config found")
	}
	want := filepath.Join(reposDir, "myrepo.json")
	if !samePath(baked, want) {
		t.Fatalf("baked hook config = %q, want registry path %q", baked, want)
	}
}
