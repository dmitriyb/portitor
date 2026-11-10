//go:build acceptance

// This file is the container-level gate acceptance suite (spec/proposals/
// 2026-08-05-gate-acceptance-suite.md, run in CI since
// 2026-11-09-hostless-deploy.md): it deploys a gate from THIS working tree
// through the real `portitor deploy` sequence, with ephemeral role keys, and
// drives it through its actual front door — git push over SSH
// (pre/post-receive) and `portitor pr` over SSH (the forced-command action
// API) — against the disposable repo PORTITOR_ACCEPT_REPO names, asserting the
// gate's decisions AND the resulting real GitHub state. It is the only tier
// that exercises the gate against real GitHub.
//
// Run (CI's acceptance workflow does):
//
//	PORTITOR_ACCEPT_PAT=… PORTITOR_ACCEPT_REPO=owner/name go test -tags acceptance -run GateAccept ./cmd/portitor/...
//
// It is gated behind the "acceptance" build tag AND, at every top-level test,
// by those two variables (setupRealGH: missing ones skip, or fail under CI) AND
// a docker availability probe. The PAT reaches the gate as the credential file
// deploy copies into the created container, never in a docker argv or env.
//
// Every scenario runs; what this tier does not cover, and why, is listed in
// spec/delivery/test_deploy.md.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
// "none" + merge_gate.checks [approved] + content_rules gating the
// approval of a change to the reviewer role. It also demonstrates scenario 4
// (separation-of-duties is gone) as a direct side effect of the same flow:
// the reviewer's signed approval commit does not block the merger (who
// signs nothing) from merging — there is no hardcoded requester-signed check
// left anywhere in this path (2026-08-05-transparent-approve §3).
//
// Flow, each step asserting BOTH the gate's own verdict (push accept/reject,
// `pr merge` exit code) and the resulting real GitHub state:
//  1. implementer pushes a feature branch -> gate accepts, forwards
//     upstream, auto-opens a PR.
//  2. merger attempts `pr merge` while the change is not approved -> refused
//     (merge_gate check "approved" unmet).
//  3. implementer (non-reviewer) attempts the approval -> REJECTED at
//     push (content_rules semantic rule "approve-reviewer-only").
//  4. reviewer signs the SAME approval -> accepted at push.
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
	chmodTreePrivate(t, cfgDir) // owner-only, like operator tooling leaves it: only the bake's mode normalization lets git read it

	// approvedScript calls grep, which the gate image's fixed part does not ship.
	gi := standUpGate(t, cfgDir, roles, env.PAT, "gnugrep")
	gi.addRepo(t, repoName)

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

	// ---- step 2: merge refused while the change is not approved ----
	// so the refusal is the approved check, not a transient UNKNOWN merge state
	waitMergeClean(t, env.GH.Repo, pr, strings.TrimSpace(gitOut(t, implDir, "rev-parse", "HEAD")))
	_, mergeStderr, err := gi.runPRAs(t, roles["merger"], "", "merge", "--repo", repoName, "--pr", strconv.Itoa(pr))
	logMergeViews(t, gi, env.GH.Repo, pr)
	if err == nil {
		t.Fatalf("merge should be refused while the change is not approved; stderr:\n%s", mergeStderr)
	}
	if !strings.Contains(mergeStderr, mergeCheckApproved) {
		t.Errorf("refusal should name the unmet %q check; stderr:\n%s", mergeCheckApproved, mergeStderr)
	}

	// ---- step 3: a non-reviewer approval is rejected at push ----
	approveChange(t, implDir)
	out, err = gi.pushAsRole(t, roles["implementer"], implDir, "feature/gateaccept")
	if err == nil {
		t.Fatalf("a non-reviewer (implementer) approval should be rejected at push:\n%s", out)
	}
	if !strings.Contains(out, semanticRuleApprove) {
		t.Errorf("rejection should name the %q rule; push output:\n%s", semanticRuleApprove, out)
	}

	// ---- step 4: a reviewer-signed approval is accepted ----
	revDir := gi.cloneAsRole(t, roles["reviewer"], repoName)
	mustRun(t, "git", "-C", revDir, "checkout", "-q", "feature/gateaccept")
	gitConfigSigning(t, revDir, roles["reviewer"], "reviewer@gateaccept.test")
	approveChange(t, revDir)
	out, err = gi.pushAsRole(t, roles["reviewer"], revDir, "feature/gateaccept")
	if err != nil {
		t.Fatalf("a reviewer-signed approval should be accepted:\n%s", out)
	}

	// ---- step 5: merge now succeeds (the merger signed nothing — scenario 4) ----
	// step 4's push re-triggered GitHub's async mergeability compute; wait for
	// CLEAN of the reviewer's head, not a lingering CLEAN of the previous one
	waitMergeClean(t, env.GH.Repo, pr, strings.TrimSpace(gitOut(t, revDir, "rev-parse", "HEAD")))
	mergeOut, mergeErrOut, err := gi.runPRAs(t, roles["merger"], "", "merge", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err != nil {
		logMergeViews(t, gi, env.GH.Repo, pr)
		t.Fatalf("merge should succeed once the reviewer approved the change: %v\nstdout:\n%s\nstderr:\n%s", err, mergeOut, mergeErrOut)
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
func mirrorRefreshSettings(roles map[string]roleKey, upstreamSlug, allowedSignersPath string) config.Settings {
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
	settings := mirrorRefreshSettings(roles, env.GH.Repo, "/etc/portitor/allowed_signers")
	writeJSON(t, filepath.Join(cfgDir, "repos.d", repoName+".json"), settings)
	chmodTreePrivate(t, cfgDir)

	gi := standUpGate(t, cfgDir, roles, env.PAT)
	gi.addRepo(t, repoName)

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
//  1. implementer pushes the branch the seed's pass+fail workflow runs on ->
//     gate accepts, auto-opens a PR; the run completes red (mixed, not
//     all-failed). The PAT needs Actions read/write and Actions enabled on
//     the disposable repo; the workflow itself is seed content.
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
	chmodTreePrivate(t, cfgDir)

	gi := standUpGate(t, cfgDir, roles, env.PAT)
	gi.addRepo(t, repoName)

	// ---- step 1: push the branch the seed's pass+fail workflow runs on ----
	// The workflow is seed content (testdata/acceptance-seed), so this push only
	// triggers it: the PAT needs Actions, never the Workflows permission.
	implDir := gi.cloneAsRole(t, roles["implementer"], repoName)
	gitConfigSigning(t, implDir, roles["implementer"], "implementer@gateaccept.test")
	mustRun(t, "git", "-C", implDir, "checkout", "-q", "-b", "feature/gateaccept-rerun")
	if err := os.WriteFile(filepath.Join(implDir, "rerun-probe.txt"), []byte("trigger the seed workflow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", implDir, "add", "-A")
	mustRun(t, "git", "-C", implDir, "commit", "-q", "-S", "-m", "gateaccept: trigger the pass+fail workflow")
	out, err := gi.pushAsRole(t, roles["implementer"], implDir, "feature/gateaccept-rerun")
	if err != nil {
		t.Fatalf("implementer's signed push should be accepted:\n%s", out)
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

// ---- review passthrough and gate threads (2026-08-05-transparent-approve) ----

// reviewSettings grants the reviewer the review-side verbs over a PR the
// implementer opens; merge_gate.review "none" — review is a pure passthrough.
func reviewSettings(roles map[string]roleKey, upstreamSlug, allowedSignersPath string) config.Settings {
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
			"fetch":   {"implementer", "reviewer"},
			"review":  {"reviewer"},
			"resolve": {"reviewer"},
		},
		MergeGate: &action.MergeGateConfig{Review: "none"},
	}
}

// openReviewPR stands a gate up with reviewSettings and has the implementer
// open a PR adding a small file, returning the gate, the PR and its head.
func openReviewPR(t *testing.T, env realGHEnv, repoName string) (*gateInstance, map[string]roleKey, int) {
	t.Helper()
	tmp := t.TempDir()
	roles := genRoleKeys(t, filepath.Join(tmp, "keys"), "implementer", "reviewer")
	cfgDir := filepath.Join(tmp, "portitor-config")
	if err := os.MkdirAll(filepath.Join(cfgDir, "repos.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAllowedSigners(t, filepath.Join(cfgDir, "allowed_signers"), roles["implementer"], roles["reviewer"])
	writeJSON(t, filepath.Join(cfgDir, "repos.d", repoName+".json"), reviewSettings(roles, env.GH.Repo, "/etc/portitor/allowed_signers"))
	chmodTreePrivate(t, cfgDir)

	gi := standUpGate(t, cfgDir, roles, env.PAT)
	gi.addRepo(t, repoName)
	dir := gi.cloneAsRole(t, roles["implementer"], repoName)
	gitConfigSigning(t, dir, roles["implementer"], "implementer@gateaccept.test")
	mustRun(t, "git", "-C", dir, "checkout", "-q", "-b", "feature/gateaccept-review")
	if err := os.WriteFile(filepath.Join(dir, "review-probe.txt"), []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", dir, "add", "-A")
	mustRun(t, "git", "-C", dir, "commit", "-q", "-S", "-m", "gateaccept: something to review")
	out, err := gi.pushAsRole(t, roles["implementer"], dir, "feature/gateaccept-review")
	if err != nil {
		t.Fatalf("implementer's signed feature push should be accepted:\n%s", out)
	}
	return gi, roles, parsePRNumber(t, out)
}

// TestGateAccept_TransparentApprove is scenario 1: `review --event comment`
// lands as a real COMMENTED review, and `review --event approve` from the
// PAT's own account — the PR's author on GitHub — is refused by GitHub, and
// the gate forwards GitHub's refusal verbatim instead of swallowing it.
func TestGateAccept_TransparentApprove(t *testing.T) {
	env := setupGateAccept(t)
	resetDisposableRepo(t, env)
	const repoName = "gateaccept-review"
	gi, roles, pr := openReviewPR(t, env, repoName)

	const body = "gateaccept: a comment review through the gate"
	if out, errOut, err := gi.runPRAs(t, roles["reviewer"], body, "review", "--event", "comment", "--repo", repoName, "--pr", strconv.Itoa(pr)); err != nil {
		t.Fatalf("a comment review should land: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	var reviews []struct {
		State string `json:"state"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(ghAPI(t, "api", fmt.Sprintf("repos/%s/pulls/%d/reviews", env.GH.Repo, pr)), &reviews); err != nil {
		t.Fatalf("parse reviews: %v", err)
	}
	landed := false
	for _, r := range reviews {
		landed = landed || (r.State == "COMMENTED" && strings.TrimSpace(r.Body) == body)
	}
	if !landed {
		t.Fatalf("GitHub holds no COMMENTED review with the body sent; reviews: %+v", reviews)
	}

	_, approveErr, err := gi.runPRAs(t, roles["reviewer"], "", "review", "--event", "approve", "--repo", repoName, "--pr", strconv.Itoa(pr))
	if err == nil {
		t.Fatal("an approve from the PR author's own account should be refused by GitHub")
	}
	if !strings.Contains(approveErr, "approve your own pull request") {
		t.Fatalf("the gate should forward GitHub's self-approval refusal verbatim; stderr:\n%s", approveErr)
	}
}

// TestGateAccept_GateThreadsResolve is the reachable half of scenario 5: a
// thread raised through the gate (`review --inline`) is resolved by `resolve
// --gate-threads`, proven on GitHub's own reviewThreads. The other half — a
// thread by someone else stays untouched — needs a second GitHub account, so
// this suite leaves it to the unit test of GateAuthoredThreads.
func TestGateAccept_GateThreadsResolve(t *testing.T) {
	env := setupGateAccept(t)
	resetDisposableRepo(t, env)
	const repoName = "gateaccept-review"
	gi, roles, pr := openReviewPR(t, env, repoName)

	inline := `{"body":"gateaccept: inline review","comments":[{"path":"review-probe.txt","line":2,"body":"gateaccept: a gate thread"}]}`
	if out, errOut, err := gi.runPRAs(t, roles["reviewer"], inline, "review", "--event", "comment", "--inline", "--repo", repoName, "--pr", strconv.Itoa(pr)); err != nil {
		t.Fatalf("an inline review should raise a thread: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	threads := func() []bool {
		q := `query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){reviewThreads(first:50){nodes{isResolved}}}}}`
		raw := ghAPI(t, "api", "graphql", "-f", "query="+q, "-f", "o="+env.Owner, "-f", "n="+env.Name, "-F", "p="+strconv.Itoa(pr))
		var resp struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewThreads struct {
							Nodes []struct{ IsResolved bool } `json:"nodes"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("parse reviewThreads: %v\n%s", err, raw)
		}
		var states []bool
		for _, n := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
			states = append(states, n.IsResolved)
		}
		return states
	}
	if got := threads(); len(got) != 1 || got[0] {
		t.Fatalf("after the inline review GitHub shows threads %v, want one unresolved", got)
	}
	if out, errOut, err := gi.runPRAs(t, roles["reviewer"], "", "resolve", "--gate-threads", "--repo", repoName, "--pr", strconv.Itoa(pr)); err != nil {
		t.Fatalf("resolve --gate-threads: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if got := threads(); len(got) != 1 || !got[0] {
		t.Fatalf("after resolve --gate-threads GitHub shows threads %v, want the gate thread resolved", got)
	}
}

// TestGateAccept_PATCannotWriteWorkflows pins the bound on a leaked acceptance
// PAT: it can push code and run the workflows the seed already has, but GitHub
// refuses any push that adds a workflow — so it cannot make the repo run code
// of its own choosing.
func TestGateAccept_PATCannotWriteWorkflows(t *testing.T) {
	env := setupRealGH(t) // no gate involved, so no docker either
	resetDisposableRepo(t, env)
	dir := cloneRepo(t, env)
	const branch = "feature/gateaccept-pat-probe"
	runGit(t, dir, "checkout", "-q", "-b", branch)
	wf := filepath.Join(dir, ".github", "workflows", "probe.yml")
	if err := os.MkdirAll(filepath.Dir(wf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wf, []byte("name: probe\non: push\njobs:\n  probe:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "gateaccept: try to add a workflow")
	cmd := exec.Command("git", "-C", dir, "push", "-q", "origin", branch)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("GitHub accepted a workflow pushed with the acceptance PAT: the PAT has the Workflows permission, so a leak could run arbitrary code in Actions")
	}
	// GitHub names the workflow file it refuses; any other rejection (a ruleset,
	// a protected branch) would not.
	if msg := redactPAT(string(out)); !strings.Contains(msg, "refusing to allow") || !strings.Contains(msg, ".github/workflows/probe.yml") {
		t.Fatalf("the push failed, but not on the workflow permission:\n%s", msg)
	}
	var branches []ghBranch
	if err := json.Unmarshal(ghAPI(t, "api", fmt.Sprintf("repos/%s/branches?per_page=100", env.GH.Repo)), &branches); err != nil {
		t.Fatalf("parse branches: %v", err)
	}
	for _, b := range branches {
		if b.Name == branch {
			t.Fatalf("the refused push still created %s on GitHub", branch)
		}
	}
}

// mergeViewFields is the query the gate's `pr merge` makes
// (internal/action GH.FetchMergeState).
const mergeViewFields = "reviewDecision,mergeStateStatus,headRefName,headRefOid,statusCheckRollup"

// logMergeViews records what GitHub answers about pr from the runner and from
// inside the gate — the gate's own query and the bare mergeStateStatus one, with
// both gh versions — so a merge state the test and the gate disagree on can be
// traced to the query, the client, or the timing.
func logMergeViews(t *testing.T, gi *gateInstance, repo string, pr int) {
	t.Helper()
	n := strconv.Itoa(pr)
	if v, err := exec.Command("gh", "--version").CombinedOutput(); err == nil {
		t.Logf("runner %s", strings.SplitN(string(v), "\n", 2)[0])
	}
	for _, fields := range []string{"mergeStateStatus", mergeViewFields} {
		out, err := ghAPIOK("pr", "view", n, "--repo", repo, "--json", fields)
		t.Logf("runner view %s (err=%v):\n%s", fields, err, out)
	}
	v, err := dockerOut("exec", "-u", "git", gi.name, "gh", "--version")
	t.Logf("gate %s (err=%v)", strings.SplitN(string(v), "\n", 2)[0], err)
	for _, fields := range []string{"mergeStateStatus", mergeViewFields} {
		out, err := dockerOut("exec", "-u", "git", gi.name, "sh", "-c",
			`GH_TOKEN=$(cat /run/secrets/gh_token) exec gh pr view "$1" --repo "$2" --json "$3"`, "sh", n, repo, fields)
		t.Logf("gate view %s (err=%v):\n%s", fields, err, out)
	}
}
