//go:build acceptance

// This file is the container-level gate acceptance suite (spec/proposals/
// 2026-08-05-gate-acceptance-suite.md): it builds the gate image from THIS
// working tree, stands up a real container on a throwaway docker network
// with ephemeral role keys, and drives it through its actual front door —
// git push over SSH (pre/post-receive) and `portitor pr` over SSH (the
// forced-command action API) — against the disposable repo testdata/
// realgh.local.json points at, asserting the gate's decisions AND the
// resulting real GitHub state. It complements realgh_test.go (which drives
// internal/action.GH directly, no gate/container in the loop) and the fast
// unit suite (pure functions, a stubbed gh runner) — this tier is the only
// one that exercises config validation at container BOOT, the SSH forced-
// command dispatch, and push-time content-rule enforcement over
// git-receive-pack together, through the real binary built from this tree.
//
// Run explicitly:
//
//	go test -tags acceptance -run GateAccept ./cmd/portitor/...
//
// It is gated behind the "acceptance" build tag (never compiled by a plain
// `go test ./...`, so it can never run under CI or a casual local run) AND,
// at every top-level test, by loadRealGHConfig + the keychain-held PAT
// (realgh_test.go's setupRealGH, reused verbatim here) AND a docker
// availability probe — any of the three missing SKIPs, never fails. Like
// realgh_test.go, the PAT is read only from the keychain (never the process
// environment) and every docker/git invocation that could embed it redacts
// it before it ever reaches a log or a failure message (see
// gateaccept_harness_test.go's runDockerSafe).
//
// v1 implements the harness end-to-end plus scenario 3 (the headline single-
// account merge model) in full. Scenarios 1, 2, 4, 5, 6 are stubbed
// (t.Skip("TODO: ...")) rather than half-implemented — see each stub's
// comment for exactly what is missing and why scenario 3 already demonstrates
// scenario 4's core assertion as a side effect.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// setupGateAccept is every scenario's shared gate: the realgh config/PAT
// (setupRealGH) plus a docker probe. It never builds an image or touches
// docker itself — that happens lazily inside standUpGate, called only once a
// test body is actually running (so `-run <nothing-matches>` never shells
// out to docker at all).
func setupGateAccept(t *testing.T) realGHEnv {
	t.Helper()
	env := setupRealGH(t)
	if !dockerAvailable() {
		t.Skip("docker not available/reachable — gate acceptance suite not runnable on this box")
	}
	return env
}

var prNumberFromPushRe = regexp.MustCompile(`PR #(\d+)`)

// parsePRNumber extracts the auto-opened PR number from a gate push's
// output (post-receive's "portitor: PR #<n> <url>" line, relayed to the
// pusher as a `remote:`-prefixed sideband message).
func parsePRNumber(t *testing.T, pushOutput string) int {
	t.Helper()
	m := prNumberFromPushRe.FindStringSubmatch(pushOutput)
	if m == nil {
		t.Fatalf("could not find a PR number in the push output:\n%s", pushOutput)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- scenario 3: the single-account merge model (the headline case) ----

// TestGateAccept_SingleAccountMergeModel drives scenario 3 of the matrix
// (spec/proposals/2026-08-05-gate-acceptance-suite.md): merge_gate.review
// "none" + merge_gate.checks [bead-closed] + content_rules gating a
// bead-close to the reviewer role. It also demonstrates scenario 4
// (separation-of-duties is gone) as a direct side effect of the same flow:
// the reviewer's signed bead-close commit does not block the merger (who
// signs nothing) from merging — there is no hardcoded requester-signed check
// left anywhere in this path (2026-08-05-transparent-approve §3).
//
// Flow, each step asserting BOTH the gate's own verdict (push accept/reject,
// `pr merge` exit code) and the resulting real GitHub state:
//  1. implementer pushes a feature branch -> gate accepts, forwards
//     upstream, auto-opens a PR.
//  2. merger attempts `pr merge` while the bead is open -> refused
//     (merge_gate check "bead-closed" unmet).
//  3. implementer (non-reviewer) attempts the bead-close -> REJECTED at
//     push (content_rules semantic rule "bead-close-reviewer-only").
//  4. reviewer signs the SAME bead-close -> accepted at push.
//  5. merger's `pr merge` now succeeds and lands on GitHub.
func TestGateAccept_SingleAccountMergeModel(t *testing.T) {
	env := setupGateAccept(t)
	resetDisposableRepo(t, env)

	tmp := t.TempDir()
	roles := genRoleKeys(t, filepath.Join(tmp, "keys"), "implementer", "reviewer", "merger")

	cfgDir := filepath.Join(tmp, "portitor-config")
	if err := os.MkdirAll(filepath.Join(cfgDir, "repos.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// allowed_signers trusts implementer + reviewer only — merger is
	// identity_only (landing-only): it must never gain commit-signing trust.
	writeAllowedSigners(t, filepath.Join(cfgDir, "allowed_signers"), roles["implementer"], roles["reviewer"])

	const repoName = "gateaccept"
	settings := scenario3Settings(roles, env.GH.Repo, "/etc/portitor/allowed_signers")
	writeJSON(t, filepath.Join(cfgDir, "repos.d", repoName+".json"), settings)
	chmodTreeReadable(t, cfgDir) // readable by the container's "git" user regardless of host/container uid mapping

	gi := standUpGate(t, cfgDir, roles, env.PAT)
	gi.addRepo(t, repoName, "https://github.com/"+env.GH.Repo+".git")

	// ---- step 1: implementer pushes a feature branch ----
	implDir := gi.cloneAsRole(t, roles["implementer"], repoName)
	gitConfigSigning(t, implDir, roles["implementer"], "implementer@gateaccept.test")
	mustRun(t, "git", "-C", implDir, "checkout", "-q", "-b", "feature/gateaccept")
	if err := os.WriteFile(filepath.Join(implDir, probeFile), []byte("hello from the implementer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", implDir, "add", "-A")
	mustRun(t, "git", "-C", implDir, "commit", "-q", "-S", "-m", "gateaccept: implement")
	out, err := gi.pushAsRole(t, roles["implementer"], implDir, "feature/gateaccept")
	if err != nil {
		t.Fatalf("implementer's signed feature push should be accepted:\n%s", out)
	}
	pr := parsePRNumber(t, out)
	t.Logf("opened PR #%d", pr)

	// ---- step 2: merge refused while the bead is open ----
	waitMergeClean(t, env.GH.Repo, pr) // so the refusal is the bead-closed check, not a transient UNKNOWN merge state
	_, mergeStderr, err := gi.runPRAs(t, roles["merger"], "", "merge", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err == nil {
		t.Fatalf("merge should be refused while the bead is open; stderr:\n%s", mergeStderr)
	}
	if !strings.Contains(mergeStderr, mergeCheckBeadClosed) {
		t.Errorf("refusal should name the unmet %q check; stderr:\n%s", mergeCheckBeadClosed, mergeStderr)
	}

	// ---- step 3: a non-reviewer bead-close is rejected at push ----
	closeBead(t, implDir)
	out, err = gi.pushAsRole(t, roles["implementer"], implDir, "feature/gateaccept")
	if err == nil {
		t.Fatalf("a non-reviewer (implementer) bead-close should be rejected at push:\n%s", out)
	}
	if !strings.Contains(out, semanticRuleBeadClose) {
		t.Errorf("rejection should name the %q rule; push output:\n%s", semanticRuleBeadClose, out)
	}

	// ---- step 4: a reviewer-signed bead-close is accepted ----
	revDir := gi.cloneAsRole(t, roles["reviewer"], repoName)
	mustRun(t, "git", "-C", revDir, "checkout", "-q", "feature/gateaccept")
	gitConfigSigning(t, revDir, roles["reviewer"], "reviewer@gateaccept.test")
	closeBead(t, revDir)
	out, err = gi.pushAsRole(t, roles["reviewer"], revDir, "feature/gateaccept")
	if err != nil {
		t.Fatalf("a reviewer-signed bead-close should be accepted:\n%s", out)
	}

	// ---- step 5: merge now succeeds (the merger signed nothing — scenario 4) ----
	waitMergeClean(t, env.GH.Repo, pr) // step 4's push re-triggered GitHub's async mergeability compute; wait for CLEAN
	mergeOut, mergeErrOut, err := gi.runPRAs(t, roles["merger"], "", "merge", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err != nil {
		t.Fatalf("merge should succeed once the bead is reviewer-closed: %v\nstdout:\n%s\nstderr:\n%s", err, mergeOut, mergeErrOut)
	}

	// ---- assert the resulting real GitHub state ----
	var prState struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	b := ghAPI(t, "api", fmt.Sprintf("repos/%s/pulls/%d", env.GH.Repo, pr))
	if err := json.Unmarshal(b, &prState); err != nil {
		t.Fatalf("parse PR state: %v\nraw: %s", err, b)
	}
	if !prState.Merged || prState.State != "closed" {
		t.Fatalf("PR #%d should be merged on GitHub; state=%q merged=%v", pr, prState.State, prState.Merged)
	}
}

// ---- stubs: scenarios 1, 2, 4, 5, 6 ----
// Left as clearly-marked TODOs per the task's own priority call (a working
// harness + scenario 3 end-to-end beats a broken whole matrix). Each has
// everything it needs already in gateaccept_harness_test.go /
// gateaccept_seed_test.go (standUpGate, genRoleKeys, pushAsRole, runPRAs,
// resetDisposableRepo) — filling these in is wiring a new per-repo config +
// driving sequence, not new harness plumbing.

// TestGateAccept_TransparentApprove is scenario 1: `review --event comment`
// posts a real COMMENT review (assert via gh api the PR's reviews); `review
// --event approve` from the gate's own PAT account 422s and the gate
// forwards the error verbatim (non-zero exit, the 422 text on stderr) — see
// realgh_test.go's TestRealGH_SelfApproveRefused, which pins the same
// GitHub-side fact directly against internal/action.GH without the
// container/SSH layer this scenario adds.
func TestGateAccept_TransparentApprove(t *testing.T) {
	t.Skip("TODO(scenario 1 — transparent approve): drive `portitor pr review --event comment|approve` over SSH as the reviewer role against a config with merge_gate.review \"none\"; assert the comment review lands on GitHub (gh api .../reviews) and the approve attempt fails loudly with the 422 forwarded verbatim on stderr.")
}

// TestGateAccept_ConfigBootRejection is scenario 2: a per-repo config
// carrying the retired `reviews_log` key or `merge_gate.review: "internal"`
// makes the gate refuse at boot (deploy/entrypoint.sh's validate-config loop
// over every repos.d/*.json) — the container must exit non-zero and never
// start sshd, rather than silently serving with a config it strict-decodes
// as invalid.
func TestGateAccept_ConfigBootRejection(t *testing.T) {
	t.Skip("TODO(scenario 2 — config boot rejection): write a repos.d/<repo>.json carrying \"reviews_log\" (or merge_gate.review:\"internal\"), start the container via a standUpGate variant that tolerates/expects a non-running state, and assert docker inspect shows it exited non-zero with entrypoint's refusal message in `docker logs` — never reaching `exec sshd`.")
}

// TestGateAccept_SeparationOfDutiesGone is scenario 4, as a STANDALONE
// scenario (TestGateAccept_SingleAccountMergeModel above already
// demonstrates its core assertion as a side effect: the reviewer's signed
// bead-close does not block the merger, who signs nothing, from merging —
// there is no hardcoded requester-signed check left in either the approve or
// merge path). A dedicated scenario would additionally assert that a
// reviewer who ALSO authored the feature branch's other commits is still not
// blocked — i.e. probe the specific "same key both requests and reviews"
// shape the old separation-of-duties check used to refuse.
func TestGateAccept_SeparationOfDutiesGone(t *testing.T) {
	t.Skip("TODO(scenario 4 — separation-of-duties gone): reuse scenario 3's harness with the reviewer key ALSO signing an earlier commit on the same feature branch (the shape the old requester-signed check refused), and assert merge still succeeds — TestGateAccept_SingleAccountMergeModel already covers the simpler reviewer-signed-bead-close/merger-merges case.")
}

// TestGateAccept_GateThreadAutoResolveByAuthor is scenario 5: after a gate
// inline review (`portitor pr review --inline`, raising a gate-authored
// thread) and a human reply thread (posted directly via gh/PAT, bypassing
// the gate), `resolve --gate-threads` must resolve only the gate-authored
// thread and leave the human one untouched (author-derived identity,
// 2026-08-05-transparent-approve §4 — reviews_log is retired, so gate
// threads are no longer looked up from gate-owned state).
func TestGateAccept_GateThreadAutoResolveByAuthor(t *testing.T) {
	t.Skip("TODO(scenario 5 — gate-thread auto-resolve by author): drive `pr review --inline` as reviewer to raise a gate thread, open a second thread directly via gh api as a distinct \"human\" comment, run `pr resolve --gate-threads` as reviewer, and assert via gh api reviewThreads that only the gate-authored thread's isResolved flipped true.")
}

// TestGateAccept_HeadPinning is scenario 6: `merge` is refused when the PR's
// head advanced (a new push landed) between UnmetMergePreconditions'
// evaluation and the final gh merge call — the --match-head-commit TOCTOU
// close (main.go's prRun "merge" case; the closing comment there explains
// the pin).
func TestGateAccept_HeadPinning(t *testing.T) {
	t.Skip("TODO(scenario 6 — head-pinning): open a PR that already meets every merge_gate precondition, race a second signed push onto the same branch between the merge command's own precondition read and its `gh pr merge --match-head-commit` call (e.g. a config check predicate that sleeps), and assert the merge is refused GitHub-side (head moved) rather than silently landing the stale head.")
}
