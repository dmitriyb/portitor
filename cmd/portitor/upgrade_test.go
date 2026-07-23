package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpgradeEmbeddedInstallMatchesCanonical is the security-critical identity
// check: the install.sh embedded into this binary (cmd/portitor/install.sh)
// MUST be byte-identical to the canonical repo-root install.sh that the release
// workflow uploads and the README documents. If they diverge, `upgrade` would
// run a script that is not the audited, signed installer — the whole "nothing
// to substitute" argument collapses. Regenerate with `go generate ./cmd/portitor`.
func TestUpgradeEmbeddedInstallMatchesCanonical(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatalf("read canonical install.sh: %v", err)
	}
	if !bytes.Equal(canonical, installScript) {
		t.Fatalf("embedded cmd/portitor/install.sh has drifted from the canonical ./install.sh "+
			"(embedded %d bytes, canonical %d bytes); run `go generate ./cmd/portitor`",
			len(installScript), len(canonical))
	}
}

// TestUpgradeHelpScopesToBinaryNotImage locks the documented scope into the
// command's own help: upgrade replaces the standalone operator binary and
// explicitly does NOT touch the container image (a separate Dockerfile artifact).
func TestUpgradeHelpScopesToBinaryNotImage(t *testing.T) {
	root := newRootCommand()
	var out, errb strings.Builder
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"upgrade", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("upgrade --help: %v", err)
	}
	got := out.String()
	for _, want := range []string{"upgrade", "--check", "--version", "--rollback", "--force", "container image", "Dockerfile"} {
		if !strings.Contains(got, want) {
			t.Errorf("upgrade help missing %q:\n%s", want, got)
		}
	}
}

// TestUpgradeFakeOriginHarness runs the fake-origin shell harness end to end so
// the upgrade + rollback + fail-closed proofs ride the same `go test ./...` CI
// gate as the rest of the suite. It is skipped only when the tools the harness
// needs are absent from the environment.
func TestUpgradeFakeOriginHarness(t *testing.T) {
	for _, tool := range []string{"sh", "curl", "ssh-keygen", "tar", "mktemp", "cut", "awk", "sed"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("harness needs %q, not found on PATH: %v", tool, err)
		}
	}
	harness := filepath.Join("..", "..", "scripts", "install_upgrade_test.sh")
	if _, err := os.Stat(harness); err != nil {
		t.Fatalf("harness not found: %v", err)
	}
	cmd := exec.Command("sh", harness)
	out, err := cmd.CombinedOutput()
	t.Logf("harness output:\n%s", out)
	if err != nil {
		t.Fatalf("fake-origin harness failed: %v", err)
	}
}
