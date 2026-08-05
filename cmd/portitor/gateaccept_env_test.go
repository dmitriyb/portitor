//go:build acceptance

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/action"
)

// patInURLRe matches the PAT embedded in an x-access-token clone URL (see
// cloneRepo) — used to scrub failure output so a failed git command can never
// print the live PAT into test logs / CI output.
var patInURLRe = regexp.MustCompile(`x-access-token:[^@]+@`)

// redactPAT replaces any embedded x-access-token:<pat>@ with a redacted form.
func redactPAT(s string) string {
	return patInURLRe.ReplaceAllString(s, "x-access-token:***@")
}

// This file holds the real-GitHub ENVIRONMENT helpers shared by the
// container-level gate acceptance suite (gate_accept_test.go): loading the
// gitignored testdata/realgh.local.json ({slug, pat_keychain_service}),
// reading the PAT from the macOS keychain via `security find-generic-password`
// (never the process environment) and handing it to `gh` via GH_TOKEN, a
// PAT-authenticated clone, and thin git/gh helpers.
//
// It lives behind the `acceptance` build tag, so a plain `go test` never even
// COMPILES it and therefore can never read the keychain — the entire
// real-GitHub tier runs only under an explicit `go test -tags acceptance ...`.
//
// The standalone real-GitHub research tests that once lived here (the
// TestRealGH_* suite from 2026-08-04-merge-gate-v2, which empirically verified
// GitHub's behavior: self-approve 422, reviewDecision states, thread resolve,
// squash merge) are retired. Their purpose was one-off verification — the
// answers are now baked into the design and its proposals — and their
// live-behavior assertions are subsumed by the container-level acceptance
// scenarios, which exercise the same facts through the real gate.

// realGHConfig is testdata/realgh.local.json's shape: the disposable repo's
// slug and the keychain service name holding its PAT. Gitignored — never
// committed (see .gitignore).
type realGHConfig struct {
	Slug               string `json:"slug"`
	PATKeychainService string `json:"pat_keychain_service"`
}

// loadRealGHConfig SKIPs the calling test when the local config is absent —
// the suite is opt-in infrastructure, not something CI or a casual `go test
// ./...` should ever need.
func loadRealGHConfig(t *testing.T) realGHConfig {
	t.Helper()
	path := filepath.Join("testdata", "realgh.local.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("real-GitHub suite not configured (%s absent): %v", path, err)
	}
	var cfg realGHConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if cfg.Slug == "" || cfg.PATKeychainService == "" {
		t.Fatalf("%s: slug and pat_keychain_service are both required", path)
	}
	return cfg
}

// realGHEnv bundles what every scenario needs: an authenticated GH.GH client
// plus the raw PAT (for the raw-gh probes that must bypass action.GH, e.g.
// demonstrating the self-approval 422 the whole redesign exists to route
// around) and the repo's owner/name.
type realGHEnv struct {
	GH    action.GH
	PAT   string
	Owner string
	Name  string
}

// setupRealGH loads the local config, reads the PAT from the keychain (never
// the environment — SKIP, not fail, when the keychain entry is unusable: an
// unprovisioned dev box is an infrastructure gap, not a code bug), verifies
// gh/git are on PATH, and returns a ready-to-use environment.
func setupRealGH(t *testing.T) realGHEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("real-GitHub suite: skipped in -short (network + a live disposable repo); run explicitly with go test -run RealGH ./...")
	}
	cfg := loadRealGHConfig(t)
	for _, bin := range []string{"gh", "git", "security"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	out, err := exec.Command("security", "find-generic-password", "-s", cfg.PATKeychainService, "-w").Output()
	if err != nil {
		t.Skipf("could not read a PAT from keychain service %q (real-GitHub infra not provisioned on this box): %v", cfg.PATKeychainService, err)
	}
	pat := strings.TrimSpace(string(out))
	if pat == "" {
		t.Skipf("keychain service %q returned an empty PAT", cfg.PATKeychainService)
	}
	t.Setenv("GH_TOKEN", pat)

	parts := strings.SplitN(cfg.Slug, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("testdata/realgh.local.json: slug %q is not owner/name", cfg.Slug)
	}
	return realGHEnv{GH: action.GH{Repo: cfg.Slug}, PAT: pat, Owner: parts[0], Name: parts[1]}
}

// runGit runs a real git command, failing the test loudly on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Redact every arg (the clone URL embeds x-access-token:<PAT>@, see
		// cloneRepo) AND the captured output (git itself echoes the URL back into
		// its own error messages) — no failure path here may print the live PAT.
		redactedArgs := make([]string, len(args))
		for i, a := range args {
			redactedArgs[i] = redactPAT(a)
		}
		t.Fatalf("git %s: %v\n%s", strings.Join(redactedArgs, " "), err, redactPAT(string(out)))
	}
	return string(out)
}

// rawGH runs the real gh CLI directly (bypassing internal/action.GH) — used
// for test-harness bookkeeping (scenario cleanup) that has no need to go
// through the GH client. This is test-harness code, not action code — the
// "gh only through the Runner seam" constraint scopes to internal/action.
func rawGH(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("gh", args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// cloneRepo clones env's repo into a fresh temp dir over an HTTPS URL
// carrying the PAT (ephemeral — nothing persisted to global git/gh config),
// and sets a throwaway committer identity.
func cloneRepo(t *testing.T, env realGHEnv) string {
	t.Helper()
	dir := t.TempDir()
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", env.PAT, env.GH.Repo)
	runGit(t, "", "clone", "-q", url, dir)
	runGit(t, dir, "config", "user.email", "portitor-realgh-test@example.com")
	runGit(t, dir, "config", "user.name", "portitor realgh test")
	// Disable commit signing for this throwaway identity — the ambient global
	// gitconfig (e.g. the operator's own SSH-signing setup) must not leak into
	// these disposable probe commits, which carry no signing key of their own.
	runGit(t, dir, "config", "commit.gpgsign", "false")
	return dir
}
