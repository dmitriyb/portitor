//go:build acceptance

package main

import (
	"fmt"
	"os"
	"os/exec"
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

// This file holds the real-GitHub ENVIRONMENT helpers of the acceptance suite
// (gate_accept_test.go): the disposable repo and its PAT from the environment
// CI gives the acceptance step (PORTITOR_ACCEPT_REPO, PORTITOR_ACCEPT_PAT), a
// PAT-authenticated clone, and thin git/gh helpers. The suite runs in CI only
// (spec/delivery/arch_ci.md); without both variables it skips — except under CI,
// where their absence fails, so a misconfigured workflow cannot pass silently.
//
// It lives behind the `acceptance` build tag, so a plain `go test` never even
// COMPILES it.

const (
	acceptPATEnv  = "PORTITOR_ACCEPT_PAT"
	acceptRepoEnv = "PORTITOR_ACCEPT_REPO"
)

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

// setupRealGH reads the disposable repo and its PAT from the environment,
// verifies gh/git are on PATH, and returns a ready-to-use environment.
func setupRealGH(t *testing.T) realGHEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("real-GitHub suite: skipped in -short (network + a live disposable repo)")
	}
	pat, slug := os.Getenv(acceptPATEnv), os.Getenv(acceptRepoEnv)
	if pat == "" || slug == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI is set but %s or %s is missing: the acceptance workflow must provide both", acceptPATEnv, acceptRepoEnv)
		}
		t.Skipf("real-GitHub suite not configured (%s and %s unset); it runs in CI", acceptPATEnv, acceptRepoEnv)
	}
	for _, bin := range []string{"gh", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s not available", bin)
		}
	}
	t.Setenv("GH_TOKEN", pat)

	parts := strings.SplitN(slug, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("%s=%q is not owner/name", acceptRepoEnv, slug)
	}
	return realGHEnv{GH: action.GH{Repo: slug}, PAT: pat, Owner: parts[0], Name: parts[1]}
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
