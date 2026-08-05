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
	"time"

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

// This file is the opt-in, real-GitHub e2e suite (spec/proposals/
// 2026-08-04-merge-gate-v2.md "Real-GitHub test harness"): it drives the
// internal/action.GH adapter (a real Runner, no stub) against a disposable
// repo. It never runs under `go test -short` or the default `go test ./...`
// gate — every top-level test here starts by loading testdata/
// realgh.local.json and SKIPs when it, or the keychain-held PAT it points at,
// is absent. Run explicitly with: go test -run RealGH ./cmd/portitor/...
//
// The PAT is NEVER read from the process environment — only from the macOS
// keychain via `security find-generic-password`, exactly like an operator's
// own credential handling. It is then handed to the `gh` subprocess via
// GH_TOKEN (gh's own supported non-interactive credential mechanism) so the
// real internal/action.GH{Run: nil} default runner authenticates.

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

// pushBranch creates branch off the repo's default branch with one commit
// (writing content to path) and pushes it, returning the branch name and its
// head SHA. name is suffixed with the test's own uniqueness token so
// concurrent/rerun scenarios never collide.
func pushBranch(t *testing.T, env realGHEnv, dir, namePrefix, path, content string) (branch, headSHA string) {
	t.Helper()
	branch = fmt.Sprintf("%s-%d", namePrefix, time.Now().UnixNano())
	runGit(t, dir, "checkout", "-q", "-b", branch)
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "realgh test: "+branch)
	runGit(t, dir, "push", "-q", "origin", branch)
	headSHA = strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	return branch, headSHA
}

// openPR opens a PR from branch and registers cleanup (close + delete the
// remote branch) that runs even on failure, per the scenario-isolation
// requirement — every scenario owns and tears down its own branch/PR.
func openPR(t *testing.T, env realGHEnv, dir, branch string) int {
	t.Helper()
	n, _, err := env.GH.OpenPR(branch, "main", "portitor realgh test: "+branch, "Automated real-GitHub e2e scenario; safe to ignore/close.")
	if err != nil {
		t.Fatalf("open PR for %s: %v", branch, err)
	}
	t.Cleanup(func() {
		_ = env.GH.ClosePR(n) // best-effort: cleanup must not itself panic the suite
		_, _ = rawGH(t, "api", fmt.Sprintf("repos/%s/git/refs/heads/%s", env.GH.Repo, branch), "-X", "DELETE")
	})
	return n
}

// ---- scenario 1: reviewDecision is null without required-review protection ----

// TestRealGH_ReviewDecisionState pins the empirical finding that motivated
// the whole redesign: on this repo's actual branch-protection configuration,
// reviewDecision is whatever GitHub reports for an unreviewed PR — logged for
// visibility rather than hard-asserted either way, since the exact value
// depends on the repo's live protection rules (which this suite does not
// own) — but it must never be "APPROVED" for a PR nobody reviewed.
func TestRealGH_ReviewDecisionState(t *testing.T) {
	env := setupRealGH(t)
	dir := cloneRepo(t, env)
	branch, _ := pushBranch(t, env, dir, "realgh-reviewdecision", "realgh-probe.txt", "probe\n")
	pr := openPR(t, env, dir, branch)

	st, err := env.GH.FetchMergeState(pr)
	if err != nil {
		t.Fatalf("FetchMergeState: %v", err)
	}
	t.Logf("PR #%d reviewDecision = %q (empirically null on a repo without required-review protection)", pr, st.ReviewDecision)
	if st.ReviewDecision == "APPROVED" {
		t.Fatalf("an unreviewed PR must never report reviewDecision == APPROVED, got %q", st.ReviewDecision)
	}
}

// ---- scenario 2: self-approval is refused (422) and GH.Review fails loudly ----

// TestRealGH_SelfApproveRefused pins the empirical fact the whole redesign is
// built around, AND that the redesigned code path itself fails loudly on it
// (spec/proposals/2026-08-05-transparent-approve.md): review is now a
// transparent passthrough — GH.Review(pr, "approve", ...) submits the caller's
// real verdict, and when the PAT tries to approve its own PR, GitHub refuses
// (HTTP 422) and the error propagates verbatim out of GH.Review, not silently
// swallowed or downgraded to a no-op.
func TestRealGH_SelfApproveRefused(t *testing.T) {
	env := setupRealGH(t)
	dir := cloneRepo(t, env)
	branch, _ := pushBranch(t, env, dir, "realgh-selfapprove", "realgh-probe.txt", "probe\n")
	pr := openPR(t, env, dir, branch)

	err := env.GH.Review(pr, "approve", "self-approve probe")
	if err == nil {
		t.Fatal("self-approval should be refused by GitHub (422); GH.Review(approve) succeeded")
	}
	t.Logf("GH.Review(approve) failed loudly as expected: %v", err)
}

// ---- scenario 3+4: inline review raises a thread; reply lands in it ----

// TestRealGH_InlineReviewAndReply pins ReviewInline's thread-correlation
// logic and Reply's addPullRequestReviewThreadReply call against real
// GitHub: an --inline review with one comment must raise exactly one new
// thread, and Reply must add a comment that FetchReviewThreads then reports
// in that thread's chain.
func TestRealGH_InlineReviewAndReply(t *testing.T) {
	env := setupRealGH(t)
	dir := cloneRepo(t, env)
	branch, _ := pushBranch(t, env, dir, "realgh-inline", "realgh-probe.txt", "line one\nline two\n")
	pr := openPR(t, env, dir, branch)

	threadIDs, err := env.GH.ReviewInline(pr, "comment", "automated inline review", []action.InlineComment{
		{Path: "realgh-probe.txt", Line: 1, Body: "inline comment from the realgh suite"},
	})
	if err != nil {
		t.Fatalf("ReviewInline: %v", err)
	}
	if len(threadIDs) != 1 {
		t.Fatalf("want exactly 1 new thread, got %v", threadIDs)
	}
	tid := threadIDs[0]

	if err := env.GH.Reply(tid, "automated reply from the realgh suite"); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	threads, err := env.GH.FetchReviewThreads(pr)
	if err != nil {
		t.Fatalf("FetchReviewThreads: %v", err)
	}
	var found *action.ReviewThread
	for i := range threads {
		if threads[i].ID == tid {
			found = &threads[i]
		}
	}
	if found == nil {
		t.Fatalf("thread %s not found among %d threads", tid, len(threads))
	}
	if found.IsResolved {
		t.Fatal("a freshly created thread must not already be resolved")
	}
	var sawReply bool
	for _, c := range found.Comments {
		if strings.Contains(c.Body, "automated reply from the realgh suite") {
			sawReply = true
		}
	}
	if !sawReply {
		t.Fatalf("reply did not land in thread %s: comments = %+v", tid, found.Comments)
	}
}

// ---- scenario 5: resolve flips isResolved, and BLOCKED -> CLEAN when the ----
// ---- repo requires conversation resolution                              ----

// TestRealGH_ResolveUnblocksMergeState pins Resolve's effect on both the
// thread's own isResolved flag and, when the repo requires conversation
// resolution, mergeStateStatus (BLOCKED while any thread is open, CLEAN once
// every thread portitor raised is resolved and every other precondition is
// otherwise met).
func TestRealGH_ResolveUnblocksMergeState(t *testing.T) {
	env := setupRealGH(t)
	dir := cloneRepo(t, env)
	branch, _ := pushBranch(t, env, dir, "realgh-resolve", "realgh-probe.txt", "line one\n")
	pr := openPR(t, env, dir, branch)

	threadIDs, err := env.GH.ReviewInline(pr, "comment", "automated inline review", []action.InlineComment{
		{Path: "realgh-probe.txt", Line: 1, Body: "please address"},
	})
	if err != nil {
		t.Fatalf("ReviewInline: %v", err)
	}
	if len(threadIDs) != 1 {
		t.Fatalf("want exactly 1 new thread, got %v", threadIDs)
	}
	tid := threadIDs[0]

	stBefore, err := env.GH.FetchMergeState(pr)
	if err != nil {
		t.Fatalf("FetchMergeState (before resolve): %v", err)
	}
	t.Logf("mergeStateStatus before resolve = %q", stBefore.MergeStateStatus)

	if err := env.GH.Resolve(tid); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	threads, err := env.GH.FetchReviewThreads(pr)
	if err != nil {
		t.Fatalf("FetchReviewThreads: %v", err)
	}
	for _, th := range threads {
		if th.ID == tid && !th.IsResolved {
			t.Fatalf("thread %s still unresolved after Resolve", tid)
		}
	}

	stAfter, err := env.GH.FetchMergeState(pr)
	if err != nil {
		t.Fatalf("FetchMergeState (after resolve): %v", err)
	}
	t.Logf("mergeStateStatus after resolve = %q", stAfter.MergeStateStatus)
	if stBefore.MergeStateStatus == "BLOCKED" && stAfter.MergeStateStatus == "BLOCKED" {
		t.Fatal("resolving the only open thread must clear a conversation-resolution BLOCKED state")
	}
}

// ---- scenario 6: squash-merge works end to end ----

// TestRealGH_SquashMergeWorks exercises Merge directly: no review/checks
// preconditions are evaluated here (that is the merge-gate matrix test
// below) — this scenario only pins that a PR portitor considers mergeable
// actually lands via `gh pr merge --squash`.
func TestRealGH_SquashMergeWorks(t *testing.T) {
	env := setupRealGH(t)
	dir := cloneRepo(t, env)
	branch, headSHA := pushBranch(t, env, dir, "realgh-squash", "realgh-probe.txt", "squash me\n")
	pr := openPR(t, env, dir, branch)

	if err := env.GH.Merge(pr, headSHA); err != nil {
		t.Fatalf("Merge: %v (if this repo requires reviews/checks/conversation-resolution, the merge may be blocked GitHub-side even though this scenario does not evaluate portitor's own preconditions)", err)
	}
	st, err := env.GH.FetchMergeState(pr)
	if err != nil {
		t.Fatalf("FetchMergeState after merge: %v", err)
	}
	t.Logf("post-merge mergeStateStatus = %q", st.MergeStateStatus)
}

// ---- scenario 7: the single-account bead-closed-predicate merge pass ----

// TestRealGH_FullMergeGatePass drives the SINGLE-ACCOUNT world (spec/
// proposals/2026-08-05-transparent-approve.md): the PAT cannot natively
// approve its own PR, so the review gate is expressed as a merge_gate.checks
// predicate over git content instead (e.g. a reviewer-signed bead-close, read
// at the current head) — never a gate-owned reviews_log verdict, which is
// retired. This scenario opens a PR, raises + resolves an inline review
// thread (the gate's own account authored it, exactly as review --event
// approve's auto-resolve targets), then confirms UnmetMergePreconditions
// reports nothing unmet with review source "none" and a satisfied checks
// predicate — calling internal/action directly (rather than through the SSH/
// role-gating layer, which needs a provisioned repo config outside this
// suite's scope) against the re-derived real GitHub state.
func TestRealGH_FullMergeGatePass(t *testing.T) {
	env := setupRealGH(t)
	dir := cloneRepo(t, env)
	branch, headSHA := pushBranch(t, env, dir, "realgh-fullpass", "realgh-probe.txt", "full pass\n")
	pr := openPR(t, env, dir, branch)

	threadIDs, err := env.GH.ReviewInline(pr, "comment", "automated full-pass review", []action.InlineComment{
		{Path: "realgh-probe.txt", Line: 1, Body: "nit: automated"},
	})
	if err != nil {
		t.Fatalf("ReviewInline: %v", err)
	}
	for _, id := range threadIDs {
		if err := env.GH.Resolve(id); err != nil {
			t.Fatalf("Resolve %s: %v", id, err)
		}
	}

	st, err := env.GH.FetchMergeState(pr)
	if err != nil {
		t.Fatalf("FetchMergeState: %v", err)
	}
	if st.HeadSHA != headSHA {
		t.Fatalf("head moved between push and fetch (%s vs %s) — rerun", st.HeadSHA, headSHA)
	}

	// The reviewer's verdict, single-account style: a satisfied git-content
	// predicate (e.g. "bead-closed") — represented here as an already-run
	// PredicateResult, exactly what cmd/portitor's runMergeChecks produces
	// after actually invoking the configured command via check.RunPredicate
	// (that re-exec trampoline has its own dedicated test coverage in
	// internal/check; this scenario's scope is the merge-gate evaluation).
	predicates := []action.PredicateResult{{Name: "bead-closed", Met: true}}

	unmet, err := action.UnmetMergePreconditions(st, nil, action.ReviewGateInput{Source: "none"}, predicates)
	if err != nil {
		t.Fatalf("UnmetMergePreconditions: %v", err)
	}
	if len(unmet) != 0 {
		t.Fatalf("single-account bead-closed-predicate pass should have nothing unmet, got: %v (mergeStateStatus=%q)", unmet, st.MergeStateStatus)
	}

	// Pinned to st.HeadSHA — the exact head UnmetMergePreconditions was just
	// evaluated against — mirroring what prRun's merge verb does.
	if err := env.GH.Merge(pr, st.HeadSHA); err != nil {
		t.Fatalf("Merge: %v", err)
	}
}

// ---- scenario 8: a precise refusal list ----

// TestRealGH_PreciseRefusalList pins that an unreviewed, unresolved-thread PR
// against a real repo produces a named, non-empty refusal list — not a
// generic failure — from the same UnmetMergePreconditions the merge verb
// calls.
func TestRealGH_PreciseRefusalList(t *testing.T) {
	env := setupRealGH(t)
	dir := cloneRepo(t, env)
	branch, _ := pushBranch(t, env, dir, "realgh-refusal", "realgh-probe.txt", "refuse me\n")
	pr := openPR(t, env, dir, branch)

	st, err := env.GH.FetchMergeState(pr)
	if err != nil {
		t.Fatalf("FetchMergeState: %v", err)
	}

	unmet, err := action.UnmetMergePreconditions(st, []string{"definitely-not-a-configured-check"},
		action.ReviewGateInput{Source: "github"},
		[]action.PredicateResult{{Name: "always-unmet", Met: false}})
	if err != nil {
		t.Fatalf("UnmetMergePreconditions: %v", err)
	}
	if len(unmet) < 2 {
		t.Fatalf("want multiple precisely-named unmet preconditions (no reviewDecision APPROVED, missing required check, unmet predicate), got: %v", unmet)
	}
	joined := strings.Join(unmet, "\n")
	for _, want := range []string{"review decision", "definitely-not-a-configured-check", "always-unmet"} {
		if !strings.Contains(joined, want) {
			t.Errorf("refusal list missing a precise mention of %q:\n%s", want, joined)
		}
	}
}
