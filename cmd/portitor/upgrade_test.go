package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpgradeEmbeddedMatchesReleased is the security-critical identity
// check: the install.sh embedded into this binary (cmd/portitor/install.sh)
// MUST be byte-identical to the canonical repo-root install.sh that the release
// workflow uploads and the README documents. If they diverge, `upgrade` would
// run a script that is not the audited, signed installer — the whole "nothing
// to substitute" argument collapses. Regenerate with `go generate ./cmd/portitor`.
func TestUpgradeEmbeddedMatchesReleased(t *testing.T) {
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

// TestUpgradeChildEnvStripsAmbientVERSION proves the child env never inherits an
// ambient VERSION — nor the test-only origin seams PORTITOR_API_BASE /
// PORTITOR_DL_BASE — from the operator's shell. The embedded install.sh keys the
// explicit-release path vs the forward-only latest path solely on whether VERSION
// is set, so a stray VERSION (a very common name) would flip a plain `upgrade`
// onto the explicit path and silently defeat the non-overridable forward-only
// anomaly refusal; an ambient origin base would redirect where a production
// `upgrade` fetches from. upgradeChildEnv strips all three and sets VERSION only
// from the pin.
func TestUpgradeChildEnvStripsAmbientVERSION(t *testing.T) {
	t.Setenv("VERSION", "9.9.9")
	t.Setenv("PORTITOR_API_BASE", "http://attacker.example/api")
	t.Setenv("PORTITOR_DL_BASE", "http://attacker.example/dl")

	countVERSION := func(env []string) (n int, entry string) {
		for _, e := range env {
			if strings.HasPrefix(e, "VERSION=") {
				n++
				entry = e
			}
		}
		return n, entry
	}

	// hasBase reports whether any origin-base override survived into the env.
	hasBase := func(env []string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, "PORTITOR_API_BASE=") || strings.HasPrefix(e, "PORTITOR_DL_BASE=") {
				return true
			}
		}
		return false
	}

	t.Run("no pin: the ambient VERSION is stripped, none survives", func(t *testing.T) {
		env := upgradeChildEnv("")
		n, entry := countVERSION(env)
		if n != 0 {
			t.Errorf("ambient VERSION leaked onto the latest path: found %d (%q), want 0", n, entry)
		}
		if hasBase(env) {
			t.Errorf("an ambient origin base leaked onto the latest path: %v", env)
		}
	})

	t.Run("pinned: exactly the pinned VERSION is present, not the ambient one", func(t *testing.T) {
		env := upgradeChildEnv("v1.2.3")
		n, entry := countVERSION(env)
		if n != 1 || entry != "VERSION=v1.2.3" {
			t.Errorf("child env VERSION = %d entr(y/ies) (%q), want exactly VERSION=v1.2.3 (not the ambient 9.9.9)", n, entry)
		}
		if hasBase(env) {
			t.Errorf("an ambient origin base leaked onto the explicit path: %v", env)
		}
	})
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
	for _, want := range []string{"upgrade", "--check", "--version", "--rollback", "forward-only", "container image", "Dockerfile"} {
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
