package action

// The Actions proxy: the checks/rerun/logs verbs' config, pure evaluators, and
// gh I/O (spec/proposals/2026-08-12-actions-proxy.md). The governing rule is
// "the box names the PR; the gate resolves the run": no function here accepts
// a caller-supplied run id, job id, or workflow name — runs are derived from
// the PR's head SHA, and the enforcement (attempt cap, all-failed switch, log
// cap, head match) lives gate-side because box-side limits are advisory.
// Mirrors the merge-gate split: pure evaluators (BuildChecksReport, PlanRerun)
// over state the I/O methods (Checks, FetchRunState, ExecuteRerun,
// FailedJobLogs) fetched.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Defaults for an absent checks block (or absent fields): the verbs stay
// usable — bounded, fail-safe — without forcing every deployment to spell the
// block out. The attempt cap in particular must bound EVERY case, so its
// default is a small number, never "unlimited".
const (
	DefaultCheckBudget  = 5 * time.Minute
	DefaultMaxAttempts  = 3
	DefaultLogTailBytes = 65536
)

// ChecksConfig is the config's checks block (beside merge_gate): the
// Actions-proxy policy for the checks/rerun/logs verbs. It lives here (not
// internal/config) for the same reason MergeGateConfig does — the evaluators
// need the type and internal/config already depends on internal/action.
type ChecksConfig struct {
	// Budgets are per-check-name stuck budgets: how long a check may stay
	// pending before a caller should count it stuck. Data surfaced by the
	// checks verb (budgetSeconds), never a gate-enforced trigger — deciding
	// WHEN to re-run is client-side policy. An array of {name, budget}
	// objects (the merge_gate.checks shape), so check names live in values
	// and the config's lowercase-schema-key rule is untouched.
	Budgets []CheckBudget `json:"budgets"`
	// DefaultBudget is the budget for check names Budgets does not list (a Go
	// duration string). Empty defaults to DefaultCheckBudget.
	DefaultBudget string `json:"default_budget"`
	// MaxAttempts caps a run's re-runs: rerun refuses a run whose run_attempt
	// has reached it. 0/absent defaults to DefaultMaxAttempts — the cap bounds
	// every case, so there is deliberately no "unlimited" setting.
	MaxAttempts int `json:"max_attempts"`
	// AllowRerunFailed permits re-running a run whose jobs ALL completed
	// FAILURE. Default false (fail-closed): a wholly red run usually means
	// the change is broken, not the infrastructure.
	AllowRerunFailed bool `json:"allow_rerun_failed"`
	// LogTailBytes caps each failed job's log tail returned by the logs verb,
	// enforced gate-side. 0/absent defaults to DefaultLogTailBytes.
	LogTailBytes int `json:"log_tail_bytes"`
}

// CheckBudget is one per-check-name stuck budget.
type CheckBudget struct {
	Name   string `json:"name"`
	Budget string `json:"budget"` // Go duration string, e.g. "90s", "3m"
}

// DefaultBudgetOrDefault returns the effective default budget. A nil block, an
// empty field, or a malformed/non-positive value all fall back to
// DefaultCheckBudget (Validate flags the malformed value so a typo surfaces at
// validate-config time rather than silently falling back here).
func (c *ChecksConfig) DefaultBudgetOrDefault() time.Duration {
	if c == nil || c.DefaultBudget == "" {
		return DefaultCheckBudget
	}
	d, err := time.ParseDuration(c.DefaultBudget)
	if err != nil || d <= 0 {
		return DefaultCheckBudget
	}
	return d
}

// BudgetFor resolves a check name's stuck budget: its Budgets entry when
// listed (first match wins), else the default.
func (c *ChecksConfig) BudgetFor(name string) time.Duration {
	if c != nil {
		for _, b := range c.Budgets {
			if b.Name == name {
				if d, err := time.ParseDuration(b.Budget); err == nil && d > 0 {
					return d
				}
				break // malformed entry: fall through to the default
			}
		}
	}
	return c.DefaultBudgetOrDefault()
}

// MaxAttemptsOrDefault returns the effective re-run attempt cap (0/absent =>
// DefaultMaxAttempts).
func (c *ChecksConfig) MaxAttemptsOrDefault() int {
	if c == nil || c.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return c.MaxAttempts
}

// LogTailBytesOrDefault returns the effective per-job log tail cap (0/absent
// => DefaultLogTailBytes).
func (c *ChecksConfig) LogTailBytesOrDefault() int {
	if c == nil || c.LogTailBytes <= 0 {
		return DefaultLogTailBytes
	}
	return c.LogTailBytes
}

// ---- the checks verb ----

// CheckStatus is one check of a ChecksReport: the widened statusCheckRollup
// fields plus the owning run/job ids the gate parsed out of detailsUrl (so a
// caller never parses URLs in shell), the run's attempt number, and the
// check's resolved stuck budget.
type CheckStatus struct {
	Name         string `json:"name"`
	Status       string `json:"status"`     // QUEUED | IN_PROGRESS | COMPLETED (empty for legacy status contexts)
	Conclusion   string `json:"conclusion"` // conclusion, or the legacy context's state
	StartedAt    string `json:"startedAt"`
	CompletedAt  string `json:"completedAt"`
	WorkflowName string `json:"workflowName"`
	RunID        int64  `json:"runId"` // 0 when the check has no resolvable Actions run (legacy contexts, external apps)
	JobID        int64  `json:"jobId"`
	RunAttempt   int    `json:"runAttempt"`
	// BudgetSeconds is the check's resolved stuck budget — data for the
	// caller's stuck detection, not a gate-enforced trigger.
	BudgetSeconds int `json:"budgetSeconds"`
}

// ChecksReport is the checks verb's response: per-check CI state plus the
// merge-readiness fields, in one call — so a stateless caller reads
// everything it needs (including run_attempt) from one place.
type ChecksReport struct {
	MergeStateStatus string        `json:"mergeStateStatus"`
	HeadRefOid       string        `json:"headRefOid"`
	Checks           []CheckStatus `json:"checks"`
}

// actionsRunJobRe extracts the run and job ids a check run's detailsUrl
// embeds (…/actions/runs/<run>/job/<job>). Legacy status contexts and
// external-app checks carry other URLs and simply resolve to 0/0.
var actionsRunJobRe = regexp.MustCompile(`/actions/runs/(\d+)/job/(\d+)`)

// runJobIDs parses a detailsUrl into its run/job ids (0, 0 when the URL is
// not an Actions job URL).
func runJobIDs(detailsURL string) (runID, jobID int64) {
	m := actionsRunJobRe.FindStringSubmatch(detailsURL)
	if m == nil {
		return 0, 0
	}
	runID, _ = strconv.ParseInt(m[1], 10, 64)
	jobID, _ = strconv.ParseInt(m[2], 10, 64)
	return runID, jobID
}

// BuildChecksReport assembles the checks verb's response from the re-derived
// merge state and the per-run attempt numbers (pure — the I/O lives in
// GH.Checks).
func BuildChecksReport(st MergeState, attempts map[int64]int, cfg *ChecksConfig) ChecksReport {
	report := ChecksReport{
		MergeStateStatus: st.MergeStateStatus,
		HeadRefOid:       st.HeadSHA,
		Checks:           make([]CheckStatus, 0, len(st.StatusCheckRollup)),
	}
	for _, c := range st.StatusCheckRollup {
		runID, jobID := runJobIDs(c.DetailsURL)
		conclusion := c.Conclusion
		if conclusion == "" {
			conclusion = c.State
		}
		name := c.checkName()
		report.Checks = append(report.Checks, CheckStatus{
			Name:          name,
			Status:        c.Status,
			Conclusion:    conclusion,
			StartedAt:     c.StartedAt,
			CompletedAt:   c.CompletedAt,
			WorkflowName:  c.WorkflowName,
			RunID:         runID,
			JobID:         jobID,
			RunAttempt:    attempts[runID],
			BudgetSeconds: int(cfg.BudgetFor(name) / time.Second),
		})
	}
	return report
}

// Checks returns the checks verb's JSON: per-check state (statusCheckRollup,
// widened) folded together with each owning run's run_attempt (from the run
// object — the rollup lacks it) and the PR's mergeStateStatus/headRefOid.
func (g GH) Checks(pr int, cfg *ChecksConfig) (string, error) {
	st, err := g.FetchMergeState(pr)
	if err != nil {
		return "", err
	}
	attempts := map[int64]int{}
	for _, c := range st.StatusCheckRollup {
		runID, _ := runJobIDs(c.DetailsURL)
		if runID == 0 {
			continue // no resolvable Actions run behind this check
		}
		if _, done := attempts[runID]; done {
			continue
		}
		run, err := g.fetchRun(runID)
		if err != nil {
			return "", fmt.Errorf("checks: %w", err)
		}
		attempts[runID] = run.RunAttempt
	}
	b, err := json.Marshal(BuildChecksReport(st, attempts, cfg))
	if err != nil {
		return "", fmt.Errorf("checks: marshal report: %w", err)
	}
	return string(b), nil
}

// ---- run/job state (shared by rerun + logs) ----

// WorkflowRun is one GitHub Actions workflow run, as the runs API reports it,
// plus its jobs (fetched separately — the run object does not embed them).
type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"` // the workflow's name
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"` // queued | in_progress | completed
	Conclusion string `json:"conclusion"`
	RunAttempt int    `json:"run_attempt"`
	Jobs       []WorkflowJob
}

// WorkflowJob is one job of a workflow run's latest attempt.
type WorkflowJob struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// RunState is the Actions-side state rerun/logs decide over: the PR's head
// runs and — crucially — the PR head RE-DERIVED after the run/job queries, so
// PlanRerun's stale-head comparison catches a branch that moved while the
// state was being assembled (the same never-trust-the-request posture as the
// merge gate; the Actions API has no --match-head-commit equivalent to pin
// with, so the window is shrunk, not closed).
type RunState struct {
	HeadSHA string
	Runs    []WorkflowRun
}

// fetchRun reads one run object (run_attempt lives here).
func (g GH) fetchRun(runID int64) (WorkflowRun, error) {
	owner, name, err := g.ownerName()
	if err != nil {
		return WorkflowRun{}, err
	}
	out, err := g.runAPI("api", fmt.Sprintf("repos/%s/%s/actions/runs/%d", owner, name, runID))
	if err != nil {
		return WorkflowRun{}, err
	}
	var run WorkflowRun
	if err := json.Unmarshal([]byte(out), &run); err != nil {
		return WorkflowRun{}, fmt.Errorf("parse run %d: %w", runID, err)
	}
	return run, nil
}

// prHeadSHA reads the PR's current head commit.
func (g GH) prHeadSHA(pr int) (string, error) {
	out, err := g.run("pr", "view", strconv.Itoa(pr), "--json", "headRefOid", "--jq", ".headRefOid")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", fmt.Errorf("PR #%d has no head commit", pr)
	}
	return sha, nil
}

// FetchRunState resolves the PR's workflow runs from its head SHA (never from
// a caller-supplied id), fetches each run's latest-attempt jobs, then
// re-derives the PR head for the stale comparison.
func (g GH) FetchRunState(pr int) (RunState, error) {
	owner, name, err := g.ownerName()
	if err != nil {
		return RunState{}, err
	}
	head, err := g.prHeadSHA(pr)
	if err != nil {
		return RunState{}, err
	}
	out, err := g.runAPI("api", fmt.Sprintf("repos/%s/%s/actions/runs?head_sha=%s&per_page=100", owner, name, head))
	if err != nil {
		return RunState{}, err
	}
	var listed struct {
		WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		return RunState{}, fmt.Errorf("parse workflow runs: %w", err)
	}
	runs := listed.WorkflowRuns
	for i := range runs {
		jobsOut, err := g.runAPI("api", fmt.Sprintf("repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=100", owner, name, runs[i].ID))
		if err != nil {
			return RunState{}, err
		}
		var jobs struct {
			Jobs []WorkflowJob `json:"jobs"`
		}
		if err := json.Unmarshal([]byte(jobsOut), &jobs); err != nil {
			return RunState{}, fmt.Errorf("parse jobs of run %d: %w", runs[i].ID, err)
		}
		runs[i].Jobs = jobs.Jobs
	}
	// Re-derive the head AFTER the queries above: a push landing meanwhile
	// makes the runs stale, and PlanRerun must see that.
	headNow, err := g.prHeadSHA(pr)
	if err != nil {
		return RunState{}, err
	}
	return RunState{HeadSHA: headNow, Runs: runs}, nil
}

// ---- the rerun verb ----

// runCompleted reports whether a run has finished (any conclusion).
func runCompleted(r WorkflowRun) bool { return r.Status == "completed" }

// runGreen reports a run that completed entirely successfully — never a rerun
// target (its jobs have nothing to re-run).
func runGreen(r WorkflowRun) bool { return runCompleted(r) && r.Conclusion == "success" }

// allJobsFailed reports whether every job of the run's latest attempt
// completed FAILURE — the shape the allow_rerun_failed switch guards (a
// wholly red run usually means the change is broken, not the infrastructure).
func allJobsFailed(r WorkflowRun) bool {
	if len(r.Jobs) == 0 {
		return false
	}
	for _, j := range r.Jobs {
		if j.Status != "completed" || j.Conclusion != "failure" {
			return false
		}
	}
	return true
}

// PlanRerun decides which of the PR head's runs a rerun may act on (pure —
// the caller fetched state via FetchRunState and executes via ExecuteRerun).
// Targets are the non-green runs; refusals — each a distinct, attributable
// message — abort the whole verb (fail-closed: one refusing run refuses the
// operation, so a caller never half-succeeds silently). An empty refusal list
// with a non-empty target list means the rerun may proceed.
func PlanRerun(prHead string, runs []WorkflowRun, cfg *ChecksConfig) (targets []WorkflowRun, refusals []string) {
	if len(runs) == 0 {
		return nil, []string{fmt.Sprintf("no workflow run found for the PR head %s — nothing to re-run", prHead)}
	}
	for _, r := range runs {
		if !runGreen(r) {
			targets = append(targets, r)
		}
	}
	if len(targets) == 0 {
		return nil, []string{fmt.Sprintf("nothing to re-run: every workflow run for head %s completed successfully", prHead)}
	}
	maxAttempts := cfg.MaxAttemptsOrDefault()
	for _, r := range targets {
		switch {
		case r.HeadSHA != prHead:
			refusals = append(refusals, fmt.Sprintf("run %d (%s) is stale: its head %s is no longer the PR head %s", r.ID, r.Name, r.HeadSHA, prHead))
		case r.RunAttempt >= maxAttempts:
			refusals = append(refusals, fmt.Sprintf("run %d (%s) is at attempt %d of the configured maximum %d (checks.max_attempts)", r.ID, r.Name, r.RunAttempt, maxAttempts))
		case allJobsFailed(r) && (cfg == nil || !cfg.AllowRerunFailed):
			refusals = append(refusals, fmt.Sprintf("run %d (%s): every job completed FAILURE and checks.allow_rerun_failed is false", r.ID, r.Name))
		}
	}
	if len(refusals) > 0 {
		return nil, refusals
	}
	return targets, nil
}

// RerunOutcome is one re-run run's receipt: the attempt number moved from
// PreviousAttempt to NewAttempt, so a (stateless) caller can confirm the
// re-run took.
type RerunOutcome struct {
	RunID           int64  `json:"runId"`
	WorkflowName    string `json:"workflowName"`
	PreviousAttempt int    `json:"previousAttempt"`
	NewAttempt      int    `json:"newAttempt"`
}

// Polling bounds for ExecuteRerun's two waits. Each individual gh call is
// already bounded by ghTimeout; these bound how long the verb keeps polling.
const (
	cancelWaitPolls  = 45 // ~90s at the default interval: a cancelled run can take a while to settle
	attemptWaitPolls = 15 // ~30s: the attempt number increments almost immediately after the rerun POST
)

// pollInterval returns the delay between Actions-API polls (GH.Poll, default
// 2s; tests set a tiny value).
func (g GH) pollInterval() time.Duration {
	if g.Poll > 0 {
		return g.Poll
	}
	return 2 * time.Second
}

// cancelRun requests a run's cancellation (async on GitHub's side).
func (g GH) cancelRun(runID int64) error {
	owner, name, err := g.ownerName()
	if err != nil {
		return err
	}
	_, err = g.runAPI("api", "-X", "POST", fmt.Sprintf("repos/%s/%s/actions/runs/%d/cancel", owner, name, runID))
	return err
}

// rerunFailedJobs re-runs a completed run's failed/cancelled jobs (its
// completed green jobs are not re-run — the run-level mechanism's one mercy).
func (g GH) rerunFailedJobs(runID int64) error {
	owner, name, err := g.ownerName()
	if err != nil {
		return err
	}
	_, err = g.runAPI("api", "-X", "POST", fmt.Sprintf("repos/%s/%s/actions/runs/%d/rerun-failed-jobs", owner, name, runID))
	return err
}

// waitRunCompleted polls a run until it reports completed (bounded).
func (g GH) waitRunCompleted(runID int64) error {
	for i := 0; i < cancelWaitPolls; i++ {
		run, err := g.fetchRun(runID)
		if err != nil {
			return err
		}
		if runCompleted(run) {
			return nil
		}
		time.Sleep(g.pollInterval())
	}
	return fmt.Errorf("run %d did not reach completed after cancellation (still in flight after %d polls)", runID, cancelWaitPolls)
}

// waitAttemptBeyond polls a run until its attempt number exceeds prev
// (bounded), returning the new attempt — the caller's confirmation that the
// re-run took.
func (g GH) waitAttemptBeyond(runID int64, prev int) (int, error) {
	for i := 0; i < attemptWaitPolls; i++ {
		run, err := g.fetchRun(runID)
		if err != nil {
			return 0, err
		}
		if run.RunAttempt > prev {
			return run.RunAttempt, nil
		}
		time.Sleep(g.pollInterval())
	}
	return 0, fmt.Errorf("re-run of run %d did not take: attempt still %d after %d polls", runID, prev, attemptWaitPolls)
}

// ExecuteRerun acts on PlanRerun's targets: an in-flight run is cancelled
// first (run-level — GitHub has no per-job cancel) and awaited, then the
// run's failed/cancelled jobs are re-run, and the run is polled until its
// attempt number moves so the receipt reports a confirmed new attempt.
// Returns the receipts as JSON.
func (g GH) ExecuteRerun(targets []WorkflowRun) (string, error) {
	outcomes := make([]RerunOutcome, 0, len(targets))
	for _, run := range targets {
		if !runCompleted(run) {
			// The cancel POST can race the run finishing on its own (GitHub
			// answers 409); either way the wait below settles it — only when
			// the run never completes is the cancel error the diagnostic.
			cancelErr := g.cancelRun(run.ID)
			if err := g.waitRunCompleted(run.ID); err != nil {
				if cancelErr != nil {
					return "", fmt.Errorf("rerun: cancel run %d: %w", run.ID, cancelErr)
				}
				return "", fmt.Errorf("rerun: %w", err)
			}
		}
		if err := g.rerunFailedJobs(run.ID); err != nil {
			return "", fmt.Errorf("rerun: run %d: %w", run.ID, err)
		}
		newAttempt, err := g.waitAttemptBeyond(run.ID, run.RunAttempt)
		if err != nil {
			return "", fmt.Errorf("rerun: %w", err)
		}
		outcomes = append(outcomes, RerunOutcome{
			RunID: run.ID, WorkflowName: run.Name,
			PreviousAttempt: run.RunAttempt, NewAttempt: newAttempt,
		})
	}
	b, err := json.Marshal(struct {
		Reruns []RerunOutcome `json:"reruns"`
	}{Reruns: outcomes})
	if err != nil {
		return "", fmt.Errorf("rerun: marshal receipts: %w", err)
	}
	return string(b), nil
}

// ---- the logs verb ----

// JobLog is one failed job's log tail.
type JobLog struct {
	RunID        int64  `json:"runId"`
	JobID        int64  `json:"jobId"`
	JobName      string `json:"jobName"`
	WorkflowName string `json:"workflowName"`
	// Truncated reports that the log exceeded the cap and only its tail is
	// carried — the caller knows it is looking at an end, not a whole.
	Truncated bool   `json:"truncated"`
	Log       string `json:"log"`
}

// tailString returns at most n trailing bytes of s and whether it truncated.
// The cap is bytes, not lines — a caller may land mid-line at the cut and
// must treat the first line as possibly partial.
func tailString(s string, n int) (string, bool) {
	if n <= 0 {
		return "", s != ""
	}
	if len(s) <= n {
		return s, false
	}
	return s[len(s)-n:], true
}

// FailedJobLogs returns the failed jobs' logs for the PR head's runs, each
// tailed gate-side to tailBytes (the config's checks.log_tail_bytes — the cap
// is enforced here, where the caller cannot override it, because Actions logs
// reach tens of megabytes and an unbounded response would blow an agent's
// context budget). Bytes only: portitor never parses or classifies them.
func (g GH) FailedJobLogs(pr int, tailBytes int) (string, error) {
	owner, name, err := g.ownerName()
	if err != nil {
		return "", err
	}
	state, err := g.FetchRunState(pr)
	if err != nil {
		return "", err
	}
	logs := make([]JobLog, 0)
	for _, run := range state.Runs {
		for _, job := range run.Jobs {
			if job.Status != "completed" || job.Conclusion != "failure" {
				continue
			}
			out, err := g.runAPI("api", fmt.Sprintf("repos/%s/%s/actions/jobs/%d/logs", owner, name, job.ID))
			if err != nil {
				return "", fmt.Errorf("logs: job %d (%s): %w", job.ID, job.Name, err)
			}
			tail, truncated := tailString(out, tailBytes)
			logs = append(logs, JobLog{
				RunID: run.ID, JobID: job.ID, JobName: job.Name,
				WorkflowName: run.Name, Truncated: truncated, Log: tail,
			})
		}
	}
	b, err := json.Marshal(struct {
		HeadRefOid string   `json:"headRefOid"`
		Jobs       []JobLog `json:"jobs"`
	}{HeadRefOid: state.HeadSHA, Jobs: logs})
	if err != nil {
		return "", fmt.Errorf("logs: marshal: %w", err)
	}
	return string(b), nil
}
