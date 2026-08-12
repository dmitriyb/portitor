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
	"time"

	"github.com/dmitriyb/portitor/internal/action"
	"github.com/dmitriyb/portitor/internal/config"
	"github.com/dmitriyb/portitor/internal/gate"
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

	// ---- step 1b: `pr describe` overwrites the PR body; denied for a role
	// action_roles["describe"] does not list ----
	// reviewer is a real, known role in this config — just not one granted
	// "describe" (scenario3Settings grants it only to implementer) — so this
	// pins the default-deny gating on the new verb, not a role-lookup miss.
	newBody := "gateaccept: description set via `portitor pr describe`\n"
	_, descDenyStderr, err := gi.runPRAs(t, roles["reviewer"], newBody, "describe", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err == nil {
		t.Fatalf("reviewer should be denied describe (action_roles[\"describe\"] grants only implementer); stderr:\n%s", descDenyStderr)
	}
	if !strings.Contains(descDenyStderr, "may not") {
		t.Errorf("denial should name the role/action; stderr:\n%s", descDenyStderr)
	}
	descOut, descStderr, err := gi.runPRAs(t, roles["implementer"], newBody, "describe", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err != nil {
		t.Fatalf("implementer's describe should be granted: %v\nstdout:\n%s\nstderr:\n%s", err, descOut, descStderr)
	}
	var prBody struct {
		Body string `json:"body"`
	}
	bodyBytes := ghAPI(t, "api", fmt.Sprintf("repos/%s/pulls/%d", env.GH.Repo, pr))
	if err := json.Unmarshal(bodyBytes, &prBody); err != nil {
		t.Fatalf("parse PR body: %v\nraw: %s", err, bodyBytes)
	}
	if strings.TrimSpace(prBody.Body) != strings.TrimSpace(newBody) {
		t.Fatalf("PR #%d body should be overwritten by describe; got %q, want %q", pr, prBody.Body, newBody)
	}

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

// ---- mirror refresh on serve (gate/mirror-refresh-on-serve) ----

// mirrorRefreshSettings is the minimal config this scenario needs: one role,
// no merge_gate/content_rules, no explicit serve_refresh_timeout (the 30s
// default applies) — the behavior under test (force-updating the mirror's
// default branch before serving git-upload-pack) runs in the SSH shell
// dispatcher itself, ahead of any push-time or merge-time machinery.
func mirrorRefreshSettings(roles map[string]roleKey, allowedSignersPath string) config.Settings {
	rolesMap := make(map[string]string, len(roles))
	for _, rk := range roles {
		rolesMap[rk.fingerprint] = rk.role
	}
	return config.Settings{
		FormatVersion: config.SupportedFormatVersion,
		Config: gate.Config{
			DefaultBranch:  "main",
			AllowedSigners: allowedSignersPath,
			Roles:          rolesMap,
		},
	}
}

// TestGateAccept_MirrorRefreshOnServe pins the fix for the frozen-mirror bug
// (spec/cli/arch_machine_entrypoints.md "shell <fingerprint>" / the `git`
// route's git-upload-pack handling): the gate's bare mirror used to write
// refs/heads/<default> once, at add-repo time, and never again — a `pr merge`
// lands on GitHub only, so a box cloning the mirror saw a frozen default
// branch forever. Now the dispatcher force-updates the mirror's default
// branch from upstream (flock-serialized, bounded by serve_refresh_timeout)
// before serving every git-upload-pack (clone/fetch).
//
// Flow:
//  1. a clone through the gate shows the seed `main`.
//  2. upstream `main` advances via a plain PAT-authenticated push directly to
//     GitHub (not a PR merge, not `add-repo`/reset) — standing in for a
//     landed merge, which likewise never touches the mirror directly.
//  3. a FRESH clone through the gate now carries upstream's new tip — the
//     mirror advanced on its own before serving.
//  4. negative: with the mirror's upstream remote pointed at an unreachable
//     URL, the clone fails loudly (non-zero + portitor's wrapped diagnostic
//     on stderr), never silently serving the stale mirror.
func TestGateAccept_MirrorRefreshOnServe(t *testing.T) {
	env := setupGateAccept(t)
	resetDisposableRepo(t, env)

	tmp := t.TempDir()
	roles := genRoleKeys(t, filepath.Join(tmp, "keys"), "reader")

	cfgDir := filepath.Join(tmp, "portitor-config")
	if err := os.MkdirAll(filepath.Join(cfgDir, "repos.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAllowedSigners(t, filepath.Join(cfgDir, "allowed_signers"), roles["reader"])

	const repoName = "gateaccept-mirror-refresh"
	settings := mirrorRefreshSettings(roles, "/etc/portitor/allowed_signers")
	writeJSON(t, filepath.Join(cfgDir, "repos.d", repoName+".json"), settings)
	chmodTreeReadable(t, cfgDir)

	gi := standUpGate(t, cfgDir, roles, env.PAT)
	gi.addRepo(t, repoName, "https://github.com/"+env.GH.Repo+".git")

	// ---- step 1: a clone through the gate shows the seed state ----
	dir1 := gi.cloneAsRole(t, roles["reader"], repoName)
	seed, err := os.ReadFile(filepath.Join(dir1, probeFile))
	if err != nil {
		t.Fatalf("seed clone missing %s: %v", probeFile, err)
	}
	if strings.TrimSpace(string(seed)) != "gateaccept seed" {
		t.Fatalf("seed clone: %s = %q, want the seed content", probeFile, seed)
	}

	// ---- step 2: advance upstream main directly, bypassing the gate (a
	// stand-in for a landed merge — pr merge is a pure gh API call that lands
	// on GitHub only, never touching the mirror either) ----
	const advancedFile = "gateaccept-mirror-refresh-probe.txt"
	upDir := cloneRepo(t, env)
	if err := os.WriteFile(filepath.Join(upDir, advancedFile), []byte("advanced upstream directly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, upDir, "add", "-A")
	runGit(t, upDir, "commit", "-q", "-m", "gateaccept: advance upstream main directly")
	runGit(t, upDir, "push", "-q", "origin", "main")
	wantSHA := strings.TrimSpace(runGit(t, upDir, "rev-parse", "HEAD"))

	// ---- step 3: a FRESH clone through the gate reflects the advance ----
	dir2 := gi.cloneAsRole(t, roles["reader"], repoName)
	if _, err := os.Stat(filepath.Join(dir2, advancedFile)); err != nil {
		t.Fatalf("clone after upstream advanced should carry %s (the mirror should have force-updated before serving): %v", advancedFile, err)
	}
	gotSHA := strings.TrimSpace(runGit(t, dir2, "rev-parse", "HEAD"))
	if gotSHA != wantSHA {
		t.Fatalf("clone after upstream advanced: HEAD = %s, want upstream's new tip %s (mirror did not advance)", gotSHA, wantSHA)
	}

	// ---- negative: upstream unreachable -> the clone fails loudly ----
	runDocker(t, "exec", "-u", "git", gi.name, "git", "-C", "/srv/git/"+repoName+".git",
		"remote", "set-url", "upstream", "https://github.com/portitor-gateaccept-nonexistent/does-not-exist.git")
	dest := filepath.Join(t.TempDir(), "clone")
	out, err := gi.tryCloneAsRole(t, roles["reader"], repoName, dest)
	if err == nil {
		t.Fatalf("clone with an unreachable upstream should fail loudly, not serve stale:\n%s", out)
	}
	if !strings.Contains(out, "portitor:") {
		t.Errorf("clone failure should surface portitor's wrapped diagnostic; output:\n%s", out)
	}
}

// ---- the Actions-proxy rerun path (2026-08-12-actions-proxy) ----

// rerunSettings is the Actions-proxy scenario's per-repo config: the read
// verbs (checks/logs) granted to implementer + merger, rerun to the merger
// only (the recommended landing-identity isolation), and a checks block with
// a per-name budget, a tight attempt cap (2 — so the scenario can hit the cap
// with a single re-run), and a small log tail cap (600 bytes — real Actions
// job logs are KBs, so gate-side truncation is guaranteed observable).
func rerunSettings(roles map[string]roleKey, upstreamSlug, allowedSignersPath string) config.Settings {
	rolesMap := make(map[string]string, len(roles))
	for _, rk := range roles {
		rolesMap[rk.fingerprint] = rk.role
	}
	return config.Settings{
		FormatVersion: config.SupportedFormatVersion,
		Config: gate.Config{
			DefaultBranch:  "main",
			AllowedSigners: allowedSignersPath,
			Roles:          rolesMap,
		},
		UpstreamSlug: upstreamSlug,
		ActionRoles: map[string][]string{
			"checks": {"implementer", "merger"},
			"logs":   {"implementer", "merger"},
			"rerun":  {"merger"},
		},
		Checks: &action.ChecksConfig{
			Budgets:       []action.CheckBudget{{Name: "fail", Budget: "30s"}},
			DefaultBudget: "2m",
			MaxAttempts:   2,
			LogTailBytes:  600,
		},
	}
}

// rerunWorkflowYAML is the CI the scenario's feature branch carries: one
// passing and one deliberately failing job, so the run completes red without
// being ALL-failed (allow_rerun_failed stays false and untriggered — the
// all-failed guard is unit-tested; this scenario exercises the mixed case a
// flaky-CI re-run actually meets). The failure prints enough output that the
// 600-byte gate-side tail cap is guaranteed to truncate.
const rerunWorkflowYAML = `name: gateaccept-rerun
on: push
jobs:
  pass:
    runs-on: ubuntu-latest
    steps:
      - run: echo "gateaccept pass"
  fail:
    runs-on: ubuntu-latest
    steps:
      - run: |
          for i in $(seq 1 50); do echo "gateaccept intentional failure line $i"; done
          exit 1
`

// waitRunsCompleted polls the Actions API (directly, harness-side) until at
// least one workflow run exists for the head SHA and every run for it has
// completed. Hosted-runner queue time dominates here, hence the long
// deadline.
func waitRunsCompleted(t *testing.T, repo, headSHA string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		out, err := ghAPIOK("api", fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", repo, headSHA))
		if err == nil {
			var listed struct {
				WorkflowRuns []struct {
					Status string `json:"status"`
				} `json:"workflow_runs"`
			}
			if jerr := json.Unmarshal(out, &listed); jerr == nil && len(listed.WorkflowRuns) > 0 {
				done := true
				for _, r := range listed.WorkflowRuns {
					if r.Status != "completed" {
						done = false
					}
					last = r.Status
				}
				if done {
					return
				}
			} else {
				last = "no runs yet"
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("workflow runs for head %s did not complete within 10m (last status: %q)", headSHA, last)
}

// TestGateAccept_RerunPath drives the Actions-proxy verbs end-to-end through
// the real gate against real GitHub Actions (2026-08-12-actions-proxy),
// asserting the gate's decisions AND the resulting GitHub state at each step:
//
//  1. implementer pushes a feature branch carrying a pass+fail workflow ->
//     gate accepts, auto-opens a PR; the run completes red (mixed, not
//     all-failed). NOTE: the gate forwards the branch with its PAT — pushing
//     .github/workflows/** requires the PAT to carry the workflow scope, and
//     Actions must be enabled on the disposable repo.
//  2. `pr checks` (implementer) returns per-check state + runAttempt=1 +
//     the resolved budgets + mergeStateStatus/headRefOid in one response.
//  3. `pr logs` (implementer) returns the FAILED job's log only, tailed
//     gate-side to checks.log_tail_bytes.
//  4. `pr rerun` as implementer -> denied (action_roles grants merger only).
//  5. `pr rerun` as merger -> re-runs the failed job; the receipt reports
//     previousAttempt 1 -> newAttempt 2, confirmed against GitHub.
//  6. a second `pr rerun` -> refused at the checks.max_attempts cap (2), with
//     the attributable message.
func TestGateAccept_RerunPath(t *testing.T) {
	env := setupGateAccept(t)
	resetDisposableRepo(t, env)

	tmp := t.TempDir()
	roles := genRoleKeys(t, filepath.Join(tmp, "keys"), "implementer", "merger")

	cfgDir := filepath.Join(tmp, "portitor-config")
	if err := os.MkdirAll(filepath.Join(cfgDir, "repos.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// merger is landing-only by convention but signs nothing here anyway; only
	// the implementer's key needs commit-signing trust.
	writeAllowedSigners(t, filepath.Join(cfgDir, "allowed_signers"), roles["implementer"])

	const repoName = "gateaccept-rerun"
	settings := rerunSettings(roles, env.GH.Repo, "/etc/portitor/allowed_signers")
	writeJSON(t, filepath.Join(cfgDir, "repos.d", repoName+".json"), settings)
	chmodTreeReadable(t, cfgDir)

	gi := standUpGate(t, cfgDir, roles, env.PAT)
	gi.addRepo(t, repoName, "https://github.com/"+env.GH.Repo+".git")

	// ---- step 1: push a feature branch carrying the pass+fail workflow ----
	implDir := gi.cloneAsRole(t, roles["implementer"], repoName)
	gitConfigSigning(t, implDir, roles["implementer"], "implementer@gateaccept.test")
	mustRun(t, "git", "-C", implDir, "checkout", "-q", "-b", "feature/gateaccept-rerun")
	wfPath := filepath.Join(implDir, ".github", "workflows", "gateaccept-rerun.yml")
	if err := os.MkdirAll(filepath.Dir(wfPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wfPath, []byte(rerunWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", implDir, "add", "-A")
	mustRun(t, "git", "-C", implDir, "commit", "-q", "-S", "-m", "gateaccept: add pass+fail workflow")
	out, err := gi.pushAsRole(t, roles["implementer"], implDir, "feature/gateaccept-rerun")
	if err != nil {
		t.Fatalf("implementer's signed workflow push should be accepted (does the PAT carry the workflow scope?):\n%s", out)
	}
	pr := parsePRNumber(t, out)
	headSHA := strings.TrimSpace(runGit(t, implDir, "rev-parse", "HEAD"))
	t.Logf("opened PR #%d, head %s", pr, headSHA)
	waitRunsCompleted(t, env.GH.Repo, headSHA)

	// ---- step 2: `pr checks` returns the folded report ----
	checksOut, checksErr, err := gi.runPRAs(t, roles["implementer"], "", "checks", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err != nil {
		t.Fatalf("checks should be granted to implementer: %v\nstderr:\n%s", err, checksErr)
	}
	var report action.ChecksReport
	if err := json.Unmarshal([]byte(checksOut), &report); err != nil {
		t.Fatalf("checks output not valid JSON: %v\n%s", err, checksOut)
	}
	if report.HeadRefOid != headSHA {
		t.Errorf("checks headRefOid = %q, want the pushed head %s", report.HeadRefOid, headSHA)
	}
	if report.MergeStateStatus == "" {
		t.Error("checks response must carry mergeStateStatus")
	}
	var failCheck, passCheck *action.CheckStatus
	for i := range report.Checks {
		switch report.Checks[i].Name {
		case "fail":
			failCheck = &report.Checks[i]
		case "pass":
			passCheck = &report.Checks[i]
		}
	}
	if failCheck == nil || passCheck == nil {
		t.Fatalf("checks report must carry the workflow's pass+fail jobs; got %+v", report.Checks)
	}
	if failCheck.Conclusion != "FAILURE" || failCheck.RunAttempt != 1 || failCheck.RunID == 0 || failCheck.JobID == 0 {
		t.Errorf("fail check = %+v, want conclusion FAILURE, runAttempt 1, resolved run/job ids", failCheck)
	}
	if failCheck.BudgetSeconds != 30 {
		t.Errorf("fail check budgetSeconds = %d, want the per-name 30", failCheck.BudgetSeconds)
	}
	if passCheck.BudgetSeconds != 120 {
		t.Errorf("pass check budgetSeconds = %d, want the 120s default_budget", passCheck.BudgetSeconds)
	}

	// ---- step 3: `pr logs` returns the failed job's tail, capped ----
	logsOut, logsErr, err := gi.runPRAs(t, roles["implementer"], "", "logs", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err != nil {
		t.Fatalf("logs should be granted to implementer: %v\nstderr:\n%s", err, logsErr)
	}
	var logs struct {
		Jobs []action.JobLog `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(logsOut), &logs); err != nil {
		t.Fatalf("logs output not valid JSON: %v\n%s", err, logsOut)
	}
	var failLog *action.JobLog
	for i := range logs.Jobs {
		if logs.Jobs[i].JobName == "fail" {
			failLog = &logs.Jobs[i]
		}
		if logs.Jobs[i].JobName == "pass" {
			t.Errorf("logs must carry FAILED jobs only, got the pass job too")
		}
	}
	if failLog == nil {
		t.Fatalf("logs must carry the failed job, got %+v", logs.Jobs)
	}
	if len(failLog.Log) > 600 {
		t.Errorf("failed job log is %d bytes, must be capped at checks.log_tail_bytes 600", len(failLog.Log))
	}
	if !failLog.Truncated {
		t.Error("a real Actions job log exceeds 600 bytes; truncated should be true")
	}
	if !strings.Contains(failLog.Log, "intentional failure") {
		t.Errorf("the tail should carry the job's final output, got:\n%s", failLog.Log)
	}

	// ---- step 4: rerun denied for the implementer ----
	_, rerunDenyErr, err := gi.runPRAs(t, roles["implementer"], "", "rerun", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err == nil {
		t.Fatal("implementer should be denied rerun (action_roles grants merger only)")
	}
	if !strings.Contains(rerunDenyErr, "may not") {
		t.Errorf("denial should name the role/action; stderr:\n%s", rerunDenyErr)
	}

	// ---- step 5: rerun as merger takes; the receipt confirms attempt 2 ----
	rerunOut, rerunErr, err := gi.runPRAs(t, roles["merger"], "", "rerun", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err != nil {
		t.Fatalf("merger's rerun should succeed: %v\nstdout:\n%s\nstderr:\n%s", err, rerunOut, rerunErr)
	}
	var receipts struct {
		Reruns []action.RerunOutcome `json:"reruns"`
	}
	if err := json.Unmarshal([]byte(rerunOut), &receipts); err != nil {
		t.Fatalf("rerun receipt not valid JSON: %v\n%s", err, rerunOut)
	}
	var ourRerun *action.RerunOutcome
	for i := range receipts.Reruns {
		if receipts.Reruns[i].WorkflowName == "gateaccept-rerun" {
			ourRerun = &receipts.Reruns[i]
		}
	}
	if ourRerun == nil || ourRerun.PreviousAttempt != 1 || ourRerun.NewAttempt != 2 {
		t.Fatalf("rerun receipt = %+v, want gateaccept-rerun attempt 1 -> 2", receipts.Reruns)
	}
	// Confirm against GitHub directly: the run object's attempt moved.
	runObj := ghAPI(t, "api", fmt.Sprintf("repos/%s/actions/runs/%d", env.GH.Repo, ourRerun.RunID))
	var runState struct {
		RunAttempt int `json:"run_attempt"`
	}
	if err := json.Unmarshal(runObj, &runState); err != nil {
		t.Fatalf("parse run object: %v", err)
	}
	if runState.RunAttempt != 2 {
		t.Fatalf("GitHub reports run %d at attempt %d, want 2", ourRerun.RunID, runState.RunAttempt)
	}

	// ---- step 6: a second rerun is refused at the attempt cap ----
	_, capErr, err := gi.runPRAs(t, roles["merger"], "", "rerun", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err == nil {
		t.Fatal("a second rerun should be refused at checks.max_attempts = 2")
	}
	if !strings.Contains(capErr, "checks.max_attempts") {
		t.Errorf("the cap refusal should be attributable to checks.max_attempts; stderr:\n%s", capErr)
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
