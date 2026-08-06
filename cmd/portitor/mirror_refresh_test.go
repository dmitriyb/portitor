package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gitOut runs git and returns trimmed stdout, failing the test loudly on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(full, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newSeededUpstreamAndMirror builds a local "upstream" bare repo (standing in
// for the real GitHub upstream) seeded with one commit on main, and a
// "mirror" bare repo (standing in for the gate's managed bare mirror) with an
// `upstream` remote pointed at it and refs/heads/main seeded to match —
// exactly the state add-repo/init-repo leaves a freshly provisioned repo in
// (main.go's initRepoRun).
func newSeededUpstreamAndMirror(t *testing.T, root string) (upstream, mirror, work string) {
	t.Helper()
	upstream = filepath.Join(root, "upstream.git")
	mustRun(t, "git", "init", "-q", "--bare", "--initial-branch=main", upstream)

	work = filepath.Join(root, "work")
	mustRun(t, "git", "init", "-q", "--initial-branch=main", work)
	for _, c := range [][]string{
		{"config", "user.name", "tester"},
		{"config", "user.email", "tester@x"},
		// The ambient global gitconfig (e.g. an operator's own commit-signing
		// setup) must not leak into these throwaway probe commits, which carry
		// no signing key of their own (cloneRepo, gate_accept_test's realGH
		// helpers, does the same for the same reason).
		{"config", "commit.gpgsign", "false"},
		{"remote", "add", "origin", upstream},
	} {
		mustRun(t, "git", append([]string{"-C", work}, c...)...)
	}
	if err := os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", work, "add", "-A")
	mustRun(t, "git", "-C", work, "commit", "-q", "-m", "seed")
	mustRun(t, "git", "-C", work, "push", "-q", "-u", "origin", "main")

	mirror = filepath.Join(root, "mirror.git")
	mustRun(t, "git", "init", "-q", "--bare", "--initial-branch=main", mirror)
	mustRun(t, "git", "-C", mirror, "remote", "add", "upstream", upstream)
	// Seed the mirror's default branch from upstream, the way init-repo does.
	mustRun(t, "git", "-C", mirror, "fetch", "-q", "upstream", "+main:refs/heads/main")
	return upstream, mirror, work
}

// writeRepoConfig writes the registry config refreshDefaultBranch's
// config.Resolve(name) call reads: format version + default_branch, and (when
// timeout != "") serve_refresh_timeout. Just enough to satisfy Parse;
// Validate's fuller requirements (allowed_signers, roles) are irrelevant to
// this helper, which never reaches the gate.
func writeRepoConfig(t *testing.T, reposDir, name, defaultBranch, timeout string) {
	t.Helper()
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"format_version":1,"default_branch":"` + defaultBranch + `"`
	if timeout != "" {
		body += `,"serve_refresh_timeout":"` + timeout + `"`
	}
	body += `}`
	if err := os.WriteFile(filepath.Join(reposDir, name+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeMinimalRepoConfig is writeRepoConfig with serve_refresh_timeout absent
// (the 30s default applies).
func writeMinimalRepoConfig(t *testing.T, reposDir, name, defaultBranch string) {
	t.Helper()
	writeRepoConfig(t, reposDir, name, defaultBranch, "")
}

// TestRefreshDefaultBranch_ForcesUpdate pins the core of gate/mirror-refresh-
// on-serve: a mirror seeded at upstream's old tip is force-updated to
// upstream's NEW tip once upstream advances directly (standing in for a
// landed merge, which — like this direct push — never touches the mirror on
// its own; see spec/cli/arch_machine_entrypoints.md "shell <fingerprint>").
func TestRefreshDefaultBranch_ForcesUpdate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	_, mirror, work := newSeededUpstreamAndMirror(t, root)

	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	writeMinimalRepoConfig(t, reposDir, "mirror", "main")

	oldSHA := gitOut(t, mirror, "rev-parse", "main")

	// Advance upstream directly (bypassing the mirror entirely).
	if err := os.WriteFile(filepath.Join(work, "advance.txt"), []byte("advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", work, "add", "-A")
	mustRun(t, "git", "-C", work, "commit", "-q", "-m", "advance")
	mustRun(t, "git", "-C", work, "push", "-q", "origin", "main")
	wantSHA := gitOut(t, work, "rev-parse", "HEAD")
	if wantSHA == oldSHA {
		t.Fatal("test setup: upstream did not actually advance")
	}

	// Before the refresh, the mirror is still stale (proves the fixture, not
	// the fix).
	if got := gitOut(t, mirror, "rev-parse", "main"); got != oldSHA {
		t.Fatalf("mirror main = %s before refresh, want unchanged seed %s", got, oldSHA)
	}

	if err := refreshDefaultBranch(mirror); err != nil {
		t.Fatalf("refreshDefaultBranch: %v", err)
	}

	if got := gitOut(t, mirror, "rev-parse", "main"); got != wantSHA {
		t.Fatalf("mirror main = %s after refresh, want upstream's new tip %s", got, wantSHA)
	}
}

// TestRefreshDefaultBranch_ForcesRewind pins the `+` refspec's actual
// semantics: a FORCE update, not merely a fast-forward. It rewinds upstream
// past the mirror's current tip (the new tip does not descend from it) and
// asserts refreshDefaultBranch still lands it — a plain (non-forced) fetch
// would instead be rejected as a non-fast-forward.
func TestRefreshDefaultBranch_ForcesRewind(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	_, mirror, work := newSeededUpstreamAndMirror(t, root)

	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	writeMinimalRepoConfig(t, reposDir, "mirror", "main")

	seedSHA := gitOut(t, mirror, "rev-parse", "main")

	// Advance upstream normally and bring the mirror's current tip along, so
	// the rewind below has somewhere non-trivial to rewind FROM.
	if err := os.WriteFile(filepath.Join(work, "advance.txt"), []byte("advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", work, "add", "-A")
	mustRun(t, "git", "-C", work, "commit", "-q", "-m", "advance")
	mustRun(t, "git", "-C", work, "push", "-q", "origin", "main")
	if err := refreshDefaultBranch(mirror); err != nil {
		t.Fatalf("refreshDefaultBranch (seed advance): %v", err)
	}
	advancedSHA := gitOut(t, mirror, "rev-parse", "main")
	if advancedSHA == seedSHA {
		t.Fatal("test setup: mirror did not advance past the seed")
	}

	// Rewrite upstream history: reset to the ORIGINAL seed and commit
	// different content, then force-push. The new tip does NOT have the
	// mirror's current ref (advancedSHA) as an ancestor — a genuine
	// non-fast-forward from the mirror's point of view.
	mustRun(t, "git", "-C", work, "reset", "-q", "--hard", seedSHA)
	if err := os.WriteFile(filepath.Join(work, "rewind.txt"), []byte("rewind\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", work, "add", "-A")
	mustRun(t, "git", "-C", work, "commit", "-q", "-m", "rewind")
	mustRun(t, "git", "-C", work, "push", "-q", "-f", "origin", "main")
	rewoundSHA := gitOut(t, work, "rev-parse", "HEAD")

	// Prove the fixture actually diverged (a plain fetch would be rejected
	// here), not just changed.
	if err := exec.Command("git", "-C", mirror, "merge-base", "--is-ancestor", advancedSHA, rewoundSHA).Run(); err == nil {
		t.Fatal("test setup: the rewound upstream tip should NOT descend from the mirror's current ref")
	}

	if err := refreshDefaultBranch(mirror); err != nil {
		t.Fatalf("refreshDefaultBranch should force through a non-fast-forward rewind: %v", err)
	}
	if got := gitOut(t, mirror, "rev-parse", "main"); got != rewoundSHA {
		t.Fatalf("mirror main = %s after rewind refresh, want upstream's rewound tip %s", got, rewoundSHA)
	}
}

// TestRefreshDefaultBranch_LeavesFeatureBranchesAlone pins the "default
// branch ONLY" contract: a feature branch that lives only in the mirror
// (arrived via a gated push, never pushed to upstream) survives a refresh
// untouched — the fetch refspec never mentions it and carries no --prune.
// Non-tautological: the default branch is advanced in the SAME test (a
// refresh that were a silent no-op would fail this, since a stale default
// wouldn't move either).
func TestRefreshDefaultBranch_LeavesFeatureBranchesAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	_, mirror, work := newSeededUpstreamAndMirror(t, root)

	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	writeMinimalRepoConfig(t, reposDir, "mirror", "main")

	// A feature branch living only in the mirror (never pushed to upstream).
	mustRun(t, "git", "-C", work, "checkout", "-q", "-b", "feature/x")
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", work, "add", "-A")
	mustRun(t, "git", "-C", work, "commit", "-q", "-m", "feature commit")
	featureSHA := gitOut(t, work, "rev-parse", "HEAD")
	mustRun(t, "git", "-C", mirror, "fetch", "-q", work, "feature/x:refs/heads/feature/x")

	// Advance the default branch too, so this test is non-tautological.
	mustRun(t, "git", "-C", work, "checkout", "-q", "main")
	oldMainSHA := gitOut(t, mirror, "rev-parse", "main")
	if err := os.WriteFile(filepath.Join(work, "advance.txt"), []byte("advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", work, "add", "-A")
	mustRun(t, "git", "-C", work, "commit", "-q", "-m", "advance main")
	mustRun(t, "git", "-C", work, "push", "-q", "origin", "main")
	wantMainSHA := gitOut(t, work, "rev-parse", "HEAD")
	if wantMainSHA == oldMainSHA {
		t.Fatal("test setup: upstream main did not actually advance")
	}

	if err := refreshDefaultBranch(mirror); err != nil {
		t.Fatalf("refreshDefaultBranch: %v", err)
	}

	if got := gitOut(t, mirror, "rev-parse", "main"); got != wantMainSHA {
		t.Fatalf("mirror main = %s after refresh, want upstream's advanced tip %s (refresh must not be a no-op)", got, wantMainSHA)
	}
	if got := gitOut(t, mirror, "rev-parse", "refs/heads/feature/x"); got != featureSHA {
		t.Fatalf("feature branch = %s after refresh, want untouched %s", got, featureSHA)
	}
}

// TestRefreshDefaultBranch_EmptyDefaultBranch pins the empty-default-branch
// guard: refreshDefaultBranch must refuse loudly BEFORE building the fetch
// refspec, never reaching git with the malformed "+:refs/heads/".
func TestRefreshDefaultBranch_EmptyDefaultBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	_, mirror, _ := newSeededUpstreamAndMirror(t, root)

	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	writeMinimalRepoConfig(t, reposDir, "mirror", "") // empty default_branch

	err := refreshDefaultBranch(mirror)
	if err == nil {
		t.Fatal("refreshDefaultBranch should fail loudly on an empty default_branch")
	}
	if !strings.Contains(err.Error(), "empty default_branch") {
		t.Errorf("error should name the empty default_branch guard, got: %v", err)
	}
}

// TestRefreshDefaultBranch_UpstreamMissingDefault pins the empty-upstream
// tolerance: an upstream that legitimately lacks the default branch yet (the
// same case initRepoRun tolerates at provisioning, main.go's hasDefault
// check) must serve the mirror's current state, NOT fail the clone.
func TestRefreshDefaultBranch_UpstreamMissingDefault(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()

	// A fresh, empty upstream: no commits, no branches at all.
	upstream := filepath.Join(root, "empty-upstream.git")
	mustRun(t, "git", "init", "-q", "--bare", upstream)

	work := filepath.Join(root, "work")
	mustRun(t, "git", "init", "-q", "--initial-branch=main", work)
	for _, c := range [][]string{
		{"config", "user.name", "tester"},
		{"config", "user.email", "tester@x"},
		{"config", "commit.gpgsign", "false"},
	} {
		mustRun(t, "git", append([]string{"-C", work}, c...)...)
	}
	if err := os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", work, "add", "-A")
	mustRun(t, "git", "-C", work, "commit", "-q", "-m", "seed")

	// A mirror with local-only history (seeded from work, not from the empty
	// upstream) and an `upstream` remote pointed at the empty repo — exactly
	// what a freshly add-repo'd repo against a not-yet-populated upstream
	// looks like.
	mirror := filepath.Join(root, "mirror.git")
	mustRun(t, "git", "init", "-q", "--bare", "--initial-branch=main", mirror)
	mustRun(t, "git", "-C", mirror, "remote", "add", "upstream", upstream)
	mustRun(t, "git", "-C", mirror, "fetch", "-q", work, "main:refs/heads/main")
	currentSHA := gitOut(t, mirror, "rev-parse", "main")

	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	writeMinimalRepoConfig(t, reposDir, "mirror", "main")

	if err := refreshDefaultBranch(mirror); err != nil {
		t.Fatalf("refreshDefaultBranch should tolerate an upstream missing the default branch, not fail the clone: %v", err)
	}
	if got := gitOut(t, mirror, "rev-parse", "main"); got != currentSHA {
		t.Fatalf("mirror main = %s after tolerated missing-upstream-default refresh, want unchanged current %s", got, currentSHA)
	}
}

// TestRefreshDefaultBranch_FlockTimeout pins the flock-serialization's
// timeout bound: with the per-repo lock held by someone else, a second
// caller configured with a small serve_refresh_timeout must fail loudly
// within roughly that bound — not hang for git.NetworkTimeout's 5m.
func TestRefreshDefaultBranch_FlockTimeout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	_, mirror, _ := newSeededUpstreamAndMirror(t, root)

	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	writeRepoConfig(t, reposDir, "mirror", "main", "200ms")

	// Hold the refresh lock via a SEPARATE open file description — the way a
	// concurrent portitor process would. flock(2) locks are per open file
	// description, so a second acquire blocks even from within this same
	// test process.
	lockPath := filepath.Join(mirror, mirrorRefreshLockFile)
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
		_ = holder.Close()
	}()

	start := time.Now()
	err = refreshDefaultBranch(mirror)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("refreshDefaultBranch should fail loudly when the flock cannot be acquired within serve_refresh_timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should name the flock timeout, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("refreshDefaultBranch took %s to fail on a held lock, want roughly the 200ms configured timeout", elapsed)
	}
}

// TestRefreshDefaultBranch_UpstreamUnreachable pins the fail-loud contract: a
// mirror whose upstream remote cannot be reached returns a wrapped error
// (never a silent no-op that would let the caller serve a stale clone).
func TestRefreshDefaultBranch_UpstreamUnreachable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	_, mirror, _ := newSeededUpstreamAndMirror(t, root)

	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	writeMinimalRepoConfig(t, reposDir, "mirror", "main")

	mustRun(t, "git", "-C", mirror, "remote", "set-url", "upstream", filepath.Join(root, "does-not-exist.git"))

	err := refreshDefaultBranch(mirror)
	if err == nil {
		t.Fatal("refreshDefaultBranch should fail loudly when upstream is unreachable")
	}
	if !strings.Contains(err.Error(), "fetch upstream main") {
		t.Errorf("error should be wrapped with a fetch diagnostic naming the remote and branch, got: %v", err)
	}
}

// TestRefreshDefaultBranch_MissingConfig pins that a mirror with no matching
// registry config fails loudly too (a broken/bypassed provisioning must not
// silently skip the refresh and serve whatever the mirror already has).
func TestRefreshDefaultBranch_MissingConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	_, mirror, _ := newSeededUpstreamAndMirror(t, root)

	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Deliberately no mirror.json written.

	err := refreshDefaultBranch(mirror)
	if err == nil {
		t.Fatal("refreshDefaultBranch should fail loudly when the repo has no registry config")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Errorf("error should be wrapped with a load-config diagnostic, got: %v", err)
	}
}

// TestShellCommand_RefreshOnlyOnUploadPack pins the git-upload-pack-only
// gating: the shell dispatcher's `git` route must trigger refreshDefaultBranch
// for git-upload-pack (serving a clone/fetch) and must NOT trigger it for
// git-receive-pack (a push), even though upstream has advanced in both cases.
// The real git-receive-pack/git-upload-pack binaries are stubbed with instant
// no-ops on PATH — this test is about the dispatch decision, not the pack
// protocol.
func TestShellCommand_RefreshOnlyOnUploadPack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	_, mirror, work := newSeededUpstreamAndMirror(t, root)

	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	writeMinimalRepoConfig(t, reposDir, "mirror", "main")
	t.Setenv("PORTITOR_REPO_ROOT", root)

	binDir := t.TempDir()
	for _, name := range []string{"git-receive-pack", "git-upload-pack"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Advance upstream so a triggered refresh is observable.
	if err := os.WriteFile(filepath.Join(work, "advance.txt"), []byte("advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", work, "add", "-A")
	mustRun(t, "git", "-C", work, "commit", "-q", "-m", "advance")
	mustRun(t, "git", "-C", work, "push", "-q", "origin", "main")
	advancedSHA := gitOut(t, work, "rev-parse", "HEAD")
	seedSHA := gitOut(t, mirror, "rev-parse", "main")
	if advancedSHA == seedSHA {
		t.Fatal("test setup: upstream did not actually advance")
	}

	// git-receive-pack (push): must NOT trigger a refresh.
	t.Setenv("SSH_ORIGINAL_COMMAND", "git-receive-pack '"+mirror+"'")
	if rc := shellCommand([]string{"testfp"}); rc != 0 {
		t.Fatalf("shellCommand(git-receive-pack) = %d, want 0", rc)
	}
	if got := gitOut(t, mirror, "rev-parse", "main"); got != seedSHA {
		t.Fatalf("mirror main = %s after a git-receive-pack dispatch, want unchanged %s (receive-pack must not trigger a refresh)", got, seedSHA)
	}

	// git-upload-pack (clone/fetch): MUST trigger a refresh.
	t.Setenv("SSH_ORIGINAL_COMMAND", "git-upload-pack '"+mirror+"'")
	if rc := shellCommand([]string{"testfp"}); rc != 0 {
		t.Fatalf("shellCommand(git-upload-pack) = %d, want 0", rc)
	}
	if got := gitOut(t, mirror, "rev-parse", "main"); got != advancedSHA {
		t.Fatalf("mirror main = %s after a git-upload-pack dispatch, want upstream's advanced tip %s (upload-pack should trigger a refresh)", got, advancedSHA)
	}
}
