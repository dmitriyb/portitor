package action

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---- config accessors ----

// TestChecksConfigDefaults pins the nil-safe accessor defaults: an absent
// checks block (or absent fields) keeps the verbs usable and BOUNDED — the
// attempt cap in particular must never default to "unlimited".
func TestChecksConfigDefaults(t *testing.T) {
	var nilBlock *ChecksConfig
	if got := nilBlock.MaxAttemptsOrDefault(); got != DefaultMaxAttempts {
		t.Errorf("nil block MaxAttemptsOrDefault = %d, want %d", got, DefaultMaxAttempts)
	}
	if got := nilBlock.LogTailBytesOrDefault(); got != DefaultLogTailBytes {
		t.Errorf("nil block LogTailBytesOrDefault = %d, want %d", got, DefaultLogTailBytes)
	}
	if got := nilBlock.BudgetFor("anything"); got != DefaultCheckBudget {
		t.Errorf("nil block BudgetFor = %v, want %v", got, DefaultCheckBudget)
	}
	if got := (&ChecksConfig{}).MaxAttemptsOrDefault(); got != DefaultMaxAttempts {
		t.Errorf("empty block MaxAttemptsOrDefault = %d, want %d", got, DefaultMaxAttempts)
	}
	if got := (&ChecksConfig{MaxAttempts: 7}).MaxAttemptsOrDefault(); got != 7 {
		t.Errorf("explicit MaxAttempts: got %d, want 7", got)
	}
	if got := (&ChecksConfig{LogTailBytes: 100}).LogTailBytesOrDefault(); got != 100 {
		t.Errorf("explicit LogTailBytes: got %d, want 100", got)
	}
}

// TestBudgetFor pins the per-name -> default_budget -> built-in resolution,
// including the malformed-entry fallback (Validate flags it; runtime falls
// back rather than failing a read verb).
func TestBudgetFor(t *testing.T) {
	cfg := &ChecksConfig{
		Budgets: []CheckBudget{
			{Name: "test", Budget: "3m"},
			{Name: "broken", Budget: "not-a-duration"},
		},
		DefaultBudget: "90s",
	}
	if got := cfg.BudgetFor("test"); got != 3*time.Minute {
		t.Errorf("BudgetFor(test) = %v, want 3m", got)
	}
	if got := cfg.BudgetFor("unlisted"); got != 90*time.Second {
		t.Errorf("BudgetFor(unlisted) = %v, want the 90s default_budget", got)
	}
	if got := cfg.BudgetFor("broken"); got != 90*time.Second {
		t.Errorf("BudgetFor(broken entry) = %v, want the default_budget fallback", got)
	}
	noDefault := &ChecksConfig{DefaultBudget: "bogus"}
	if got := noDefault.BudgetFor("x"); got != DefaultCheckBudget {
		t.Errorf("malformed default_budget: got %v, want the built-in %v", got, DefaultCheckBudget)
	}
}

// ---- checks report ----

func TestRunJobIDs(t *testing.T) {
	run, job := runJobIDs("https://github.com/o/r/actions/runs/17032161986/job/48287654321")
	if run != 17032161986 || job != 48287654321 {
		t.Fatalf("got run=%d job=%d", run, job)
	}
	for _, url := range []string{"", "https://codecov.io/gh/o/r", "https://github.com/o/r/actions/runs/123"} {
		if r, j := runJobIDs(url); r != 0 || j != 0 {
			t.Errorf("runJobIDs(%q) = %d,%d, want 0,0", url, r, j)
		}
	}
}

// TestBuildChecksReport pins the report assembly: widened check-run fields,
// legacy status contexts (conclusion falls back to state; no Actions run so
// runId/jobId/runAttempt stay 0), the attempts fold-in, and the resolved
// per-name budget in seconds.
func TestBuildChecksReport(t *testing.T) {
	st := MergeState{
		MergeStateStatus: "UNSTABLE",
		HeadSHA:          "deadbeef",
		StatusCheckRollup: []CheckRun{
			{Name: "test", Status: "IN_PROGRESS", StartedAt: "2026-08-12T10:00:00Z", WorkflowName: "ci",
				DetailsURL: "https://github.com/o/r/actions/runs/42/job/420"},
			{Context: "legacy/ctx", State: "SUCCESS"},
		},
	}
	cfg := &ChecksConfig{Budgets: []CheckBudget{{Name: "test", Budget: "97s"}}, DefaultBudget: "2m"}
	report := BuildChecksReport(st, map[int64]int{42: 2}, cfg)

	if report.MergeStateStatus != "UNSTABLE" || report.HeadRefOid != "deadbeef" {
		t.Fatalf("report header = %+v", report)
	}
	if len(report.Checks) != 2 {
		t.Fatalf("checks = %+v", report.Checks)
	}
	c := report.Checks[0]
	if c.Name != "test" || c.Status != "IN_PROGRESS" || c.WorkflowName != "ci" ||
		c.RunID != 42 || c.JobID != 420 || c.RunAttempt != 2 || c.BudgetSeconds != 97 {
		t.Fatalf("check[0] = %+v", c)
	}
	legacy := report.Checks[1]
	if legacy.Name != "legacy/ctx" || legacy.Conclusion != "SUCCESS" ||
		legacy.RunID != 0 || legacy.JobID != 0 || legacy.RunAttempt != 0 || legacy.BudgetSeconds != 120 {
		t.Fatalf("legacy check = %+v", legacy)
	}
}

// TestChecks pins GH.Checks end-to-end over a stubbed runner: one pr-view
// call plus exactly ONE run fetch per unique run (two checks sharing a run
// must not fetch it twice), folded into a single JSON response.
func TestChecks(t *testing.T) {
	runFetches := 0
	run := func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "pr":
			return `{"mergeStateStatus":"UNSTABLE","headRefOid":"abc123","statusCheckRollup":[
				{"name":"build","status":"COMPLETED","conclusion":"SUCCESS","workflowName":"ci","detailsUrl":"https://github.com/o/r/actions/runs/42/job/1"},
				{"name":"test","status":"IN_PROGRESS","workflowName":"ci","detailsUrl":"https://github.com/o/r/actions/runs/42/job/2"}
			]}`, nil
		case args[0] == "api" && strings.Contains(joined, "actions/runs/42"):
			runFetches++
			return `{"id":42,"name":"ci","head_sha":"abc123","status":"in_progress","run_attempt":3}`, nil
		default:
			t.Fatalf("unexpected call: %v", args)
			return "", nil
		}
	}
	out, err := (GH{Repo: "o/r", Run: run}).Checks(7, nil)
	if err != nil {
		t.Fatal(err)
	}
	if runFetches != 1 {
		t.Fatalf("run fetched %d times, want 1 (two checks share run 42)", runFetches)
	}
	var report ChecksReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("Checks output not valid JSON: %v\n%s", err, out)
	}
	if report.MergeStateStatus != "UNSTABLE" || report.HeadRefOid != "abc123" || len(report.Checks) != 2 {
		t.Fatalf("report = %+v", report)
	}
	for _, c := range report.Checks {
		if c.RunAttempt != 3 {
			t.Fatalf("check %q runAttempt = %d, want 3 (from the run object)", c.Name, c.RunAttempt)
		}
		if c.BudgetSeconds != int(DefaultCheckBudget/time.Second) {
			t.Fatalf("check %q budgetSeconds = %d, want the default", c.Name, c.BudgetSeconds)
		}
	}
}

// ---- rerun planning ----

// completedRun/inFlightRun build PlanRerun inputs.
func completedRun(id int64, conclusion string, jobs ...WorkflowJob) WorkflowRun {
	return WorkflowRun{ID: id, Name: "ci", HeadSHA: "head", Status: "completed", Conclusion: conclusion, RunAttempt: 1, Jobs: jobs}
}

// TestPlanRerunMatrix is the rerun refusal table: each guard fires with its
// own distinct, attributable message, and the happy paths return targets.
func TestPlanRerunMatrix(t *testing.T) {
	failedJob := WorkflowJob{ID: 1, Name: "fail", Status: "completed", Conclusion: "failure"}
	passedJob := WorkflowJob{ID: 2, Name: "pass", Status: "completed", Conclusion: "success"}

	cases := []struct {
		name        string
		runs        []WorkflowRun
		cfg         *ChecksConfig
		wantTargets int
		wantRefusal string // "" = no refusal
	}{
		{name: "no runs at all", runs: nil, wantRefusal: "no workflow run found"},
		{name: "everything green", runs: []WorkflowRun{completedRun(1, "success", passedJob)}, wantRefusal: "nothing to re-run"},
		{name: "mixed failure re-runs", runs: []WorkflowRun{completedRun(1, "failure", failedJob, passedJob)}, wantTargets: 1},
		{name: "green run skipped beside a red one",
			runs:        []WorkflowRun{completedRun(1, "success", passedJob), completedRun(2, "failure", failedJob, passedJob)},
			wantTargets: 1},
		{name: "stale head",
			runs:        []WorkflowRun{{ID: 3, Name: "ci", HeadSHA: "old", Status: "completed", Conclusion: "failure", RunAttempt: 1, Jobs: []WorkflowJob{failedJob, passedJob}}},
			wantRefusal: "stale"},
		{name: "attempt cap reached",
			runs:        []WorkflowRun{{ID: 4, Name: "ci", HeadSHA: "head", Status: "completed", Conclusion: "failure", RunAttempt: 3, Jobs: []WorkflowJob{failedJob, passedJob}}},
			wantRefusal: "checks.max_attempts"},
		{name: "attempt cap honors config",
			runs:        []WorkflowRun{{ID: 4, Name: "ci", HeadSHA: "head", Status: "completed", Conclusion: "failure", RunAttempt: 3, Jobs: []WorkflowJob{failedJob, passedJob}}},
			cfg:         &ChecksConfig{MaxAttempts: 5},
			wantTargets: 1},
		{name: "all jobs failed, rerun-failed disallowed",
			runs:        []WorkflowRun{completedRun(5, "failure", failedJob)},
			wantRefusal: "allow_rerun_failed"},
		{name: "all jobs failed, rerun-failed allowed",
			runs:        []WorkflowRun{completedRun(5, "failure", failedJob)},
			cfg:         &ChecksConfig{AllowRerunFailed: true},
			wantTargets: 1},
		{name: "in-flight run is a target (jobs not all completed)",
			runs:        []WorkflowRun{{ID: 6, Name: "ci", HeadSHA: "head", Status: "in_progress", RunAttempt: 1, Jobs: []WorkflowJob{{ID: 9, Status: "in_progress"}}}},
			wantTargets: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			targets, refusals := PlanRerun("head", c.runs, c.cfg)
			if c.wantRefusal != "" {
				if len(refusals) == 0 {
					t.Fatalf("want a refusal containing %q, got targets %+v", c.wantRefusal, targets)
				}
				if !strings.Contains(strings.Join(refusals, "; "), c.wantRefusal) {
					t.Fatalf("refusals %v missing %q", refusals, c.wantRefusal)
				}
				if len(targets) != 0 {
					t.Fatalf("a refused plan must carry no targets, got %+v", targets)
				}
				return
			}
			if len(refusals) != 0 {
				t.Fatalf("unexpected refusals: %v", refusals)
			}
			if len(targets) != c.wantTargets {
				t.Fatalf("targets = %+v, want %d", targets, c.wantTargets)
			}
		})
	}
}

// TestPlanRerunDistinctMessages pins that the three configured guards carry
// DISTINCT messages (the acceptance criterion: each refusal attributable).
func TestPlanRerunDistinctMessages(t *testing.T) {
	failedJob := WorkflowJob{ID: 1, Status: "completed", Conclusion: "failure"}
	mk := func(runs []WorkflowRun, cfg *ChecksConfig) string {
		_, refusals := PlanRerun("head", runs, cfg)
		if len(refusals) != 1 {
			t.Fatalf("want exactly one refusal, got %v", refusals)
		}
		return refusals[0]
	}
	stale := mk([]WorkflowRun{{ID: 1, HeadSHA: "old", Status: "completed", Conclusion: "failure", RunAttempt: 1, Jobs: []WorkflowJob{failedJob, {ID: 2, Status: "completed", Conclusion: "success"}}}}, nil)
	atCap := mk([]WorkflowRun{{ID: 1, HeadSHA: "head", Status: "completed", Conclusion: "failure", RunAttempt: 3, Jobs: []WorkflowJob{failedJob, {ID: 2, Status: "completed", Conclusion: "success"}}}}, nil)
	allFailed := mk([]WorkflowRun{{ID: 1, HeadSHA: "head", Status: "completed", Conclusion: "failure", RunAttempt: 1, Jobs: []WorkflowJob{failedJob}}}, nil)
	if stale == atCap || atCap == allFailed || stale == allFailed {
		t.Fatalf("refusal messages must be distinct:\n%s\n%s\n%s", stale, atCap, allFailed)
	}
}

// ---- rerun execution ----

// TestExecuteRerunCompletedRun pins the completed-run path: no cancel, one
// rerun-failed-jobs POST, then the attempt poll confirms the new attempt.
func TestExecuteRerunCompletedRun(t *testing.T) {
	var calls []string
	rerunPosted := false
	run := func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		calls = append(calls, joined)
		switch {
		case strings.Contains(joined, "rerun-failed-jobs"):
			rerunPosted = true
			return "", nil
		case strings.Contains(joined, "actions/runs/42"):
			if rerunPosted {
				return `{"id":42,"name":"ci","status":"queued","run_attempt":2}`, nil
			}
			return `{"id":42,"name":"ci","status":"completed","conclusion":"failure","run_attempt":1}`, nil
		default:
			t.Fatalf("unexpected call: %v", args)
			return "", nil
		}
	}
	g := GH{Repo: "o/r", Run: run, Poll: time.Nanosecond}
	out, err := g.ExecuteRerun([]WorkflowRun{{ID: 42, Name: "ci", Status: "completed", Conclusion: "failure", RunAttempt: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var receipts struct {
		Reruns []RerunOutcome `json:"reruns"`
	}
	if err := json.Unmarshal([]byte(out), &receipts); err != nil {
		t.Fatalf("receipts not valid JSON: %v\n%s", err, out)
	}
	if len(receipts.Reruns) != 1 || receipts.Reruns[0].PreviousAttempt != 1 || receipts.Reruns[0].NewAttempt != 2 {
		t.Fatalf("receipts = %+v", receipts)
	}
	for _, c := range calls {
		if strings.Contains(c, "/cancel") {
			t.Fatalf("a completed run must not be cancelled; calls: %v", calls)
		}
	}
}

// TestExecuteRerunInFlightCancelsFirst pins the stuck path: an in-flight run
// is cancelled, AWAITED to completion, and only then re-run — the call order
// is the contract (rerun-failed-jobs on a live run would 409).
func TestExecuteRerunInFlightCancelsFirst(t *testing.T) {
	var order []string
	cancelled, rerunPosted := false, false
	run := func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "/cancel"):
			order = append(order, "cancel")
			cancelled = true
			return "", nil
		case strings.Contains(joined, "rerun-failed-jobs"):
			order = append(order, "rerun")
			rerunPosted = true
			return "", nil
		case strings.Contains(joined, "actions/runs/5"):
			switch {
			case rerunPosted:
				order = append(order, "get-queued")
				return `{"id":5,"name":"ci","status":"queued","run_attempt":2}`, nil
			case cancelled:
				order = append(order, "get-completed")
				return `{"id":5,"name":"ci","status":"completed","conclusion":"cancelled","run_attempt":1}`, nil
			default:
				order = append(order, "get-inflight")
				return `{"id":5,"name":"ci","status":"in_progress","run_attempt":1}`, nil
			}
		default:
			t.Fatalf("unexpected call: %v", args)
			return "", nil
		}
	}
	g := GH{Repo: "o/r", Run: run, Poll: time.Nanosecond}
	out, err := g.ExecuteRerun([]WorkflowRun{{ID: 5, Name: "ci", Status: "in_progress", RunAttempt: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"newAttempt":2`) {
		t.Fatalf("receipt missing the new attempt: %s", out)
	}
	// cancel must precede rerun, AND a poll must have observed the run
	// completed in between — dropping the wait (rerun-failed-jobs on a live
	// run 409s in production) must fail this test, not just reorder it.
	ci, done, ri := -1, -1, -1
	for i, o := range order {
		switch {
		case o == "cancel" && ci < 0:
			ci = i
		case o == "get-completed" && done < 0:
			done = i
		case o == "rerun" && ri < 0:
			ri = i
		}
	}
	if ci < 0 || done < 0 || ri < 0 || !(ci < done && done < ri) {
		t.Fatalf("call order = %v, want cancel -> a completed-run poll -> rerun-failed-jobs", order)
	}
}

// TestExecuteRerunConfirmsAttempt pins the receipt's honesty: if the attempt
// number never moves after the rerun POST, ExecuteRerun errors instead of
// reporting a re-run that never took.
func TestExecuteRerunConfirmsAttempt(t *testing.T) {
	run := func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "rerun-failed-jobs") {
			return "", nil
		}
		return `{"id":9,"name":"ci","status":"completed","conclusion":"failure","run_attempt":1}`, nil
	}
	g := GH{Repo: "o/r", Run: run, Poll: time.Nanosecond}
	_, err := g.ExecuteRerun([]WorkflowRun{{ID: 9, Name: "ci", Status: "completed", Conclusion: "failure", RunAttempt: 1}})
	if err == nil || !strings.Contains(err.Error(), "did not take") {
		t.Fatalf("want a did-not-take error, got %v", err)
	}
}

// ---- run-state resolution ----

// TestFetchRunStateRederivesHead pins the stale-head seam: the PR head is
// re-derived AFTER the run/job queries, so a branch that moved mid-resolution
// surfaces as PlanRerun's stale refusal instead of silently re-running runs
// of a superseded commit.
func TestFetchRunStateRederivesHead(t *testing.T) {
	prViews := 0
	run := func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "pr":
			prViews++
			if prViews == 1 {
				return "sha-one\n", nil
			}
			return "sha-two\n", nil
		case strings.Contains(joined, "actions/runs?head_sha=sha-one"):
			return `{"workflow_runs":[{"id":1,"name":"ci","head_sha":"sha-one","status":"completed","conclusion":"failure","run_attempt":1}]}`, nil
		case strings.Contains(joined, "actions/runs/1/jobs"):
			return `{"jobs":[{"id":11,"name":"fail","status":"completed","conclusion":"failure"},{"id":12,"name":"pass","status":"completed","conclusion":"success"}]}`, nil
		default:
			t.Fatalf("unexpected call: %v", args)
			return "", nil
		}
	}
	state, err := (GH{Repo: "o/r", Run: run}).FetchRunState(3)
	if err != nil {
		t.Fatal(err)
	}
	if prViews != 2 {
		t.Fatalf("pr head derived %d times, want 2 (before and after run resolution)", prViews)
	}
	if state.HeadSHA != "sha-two" {
		t.Fatalf("HeadSHA = %q, want the RE-derived sha-two", state.HeadSHA)
	}
	_, refusals := PlanRerun(state.HeadSHA, state.Runs, nil)
	if len(refusals) == 0 || !strings.Contains(refusals[0], "stale") {
		t.Fatalf("a moved head must plan as stale, got refusals %v", refusals)
	}
}

// ---- logs ----

func TestTailString(t *testing.T) {
	if got, trunc := tailString("hello", 100); got != "hello" || trunc {
		t.Fatalf("under cap: %q %v", got, trunc)
	}
	if got, trunc := tailString("0123456789", 4); got != "6789" || !trunc {
		t.Fatalf("over cap: %q %v (want the TAIL, truncated)", got, trunc)
	}
	if got, trunc := tailString("x", 0); got != "" || !trunc {
		t.Fatalf("zero cap: %q %v", got, trunc)
	}
}

// TestFailedJobLogs pins the logs verb: failed jobs only (green and in-flight
// jobs are never fetched), each log tailed to the cap with the truncation
// visible — the cap enforced gate-side, whatever the log's real size.
func TestFailedJobLogs(t *testing.T) {
	longLog := strings.Repeat("x", 5000) + "THE-END"
	var logFetches []string
	run := func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "pr":
			return "abc\n", nil
		case strings.Contains(joined, "actions/runs?head_sha=abc"):
			return `{"workflow_runs":[{"id":9,"name":"ci","head_sha":"abc","status":"completed","conclusion":"failure","run_attempt":1}]}`, nil
		case strings.Contains(joined, "actions/runs/9/jobs"):
			return `{"jobs":[
				{"id":70,"name":"pass","status":"completed","conclusion":"success"},
				{"id":71,"name":"running","status":"in_progress"},
				{"id":77,"name":"fail","status":"completed","conclusion":"failure"}
			]}`, nil
		case strings.Contains(joined, "actions/jobs/"):
			logFetches = append(logFetches, joined)
			return longLog, nil
		default:
			t.Fatalf("unexpected call: %v", args)
			return "", nil
		}
	}
	out, err := (GH{Repo: "o/r", Run: run}).FailedJobLogs(3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(logFetches) != 1 || !strings.Contains(logFetches[0], "actions/jobs/77/logs") {
		t.Fatalf("log fetches = %v, want exactly the failed job 77", logFetches)
	}
	var parsed struct {
		HeadRefOid string   `json:"headRefOid"`
		Jobs       []JobLog `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("logs output not valid JSON: %v\n%s", err, out)
	}
	if len(parsed.Jobs) != 1 {
		t.Fatalf("jobs = %+v", parsed.Jobs)
	}
	j := parsed.Jobs[0]
	if j.JobID != 77 || j.JobName != "fail" || !j.Truncated {
		t.Fatalf("job log = %+v", j)
	}
	if len(j.Log) != 100 || !strings.HasSuffix(j.Log, "THE-END") {
		t.Fatalf("log must be the 100-byte TAIL, got %d bytes ending %q", len(j.Log), j.Log[max(0, len(j.Log)-10):])
	}
}

// TestFailedJobLogsNoFailures pins the empty case: a green PR returns an
// empty jobs array (valid JSON, no error) — "no failed jobs" is an answer,
// not a failure.
func TestFailedJobLogsNoFailures(t *testing.T) {
	run := func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "pr":
			return "abc\n", nil
		case strings.Contains(joined, "actions/runs?head_sha=abc"):
			return `{"workflow_runs":[{"id":9,"name":"ci","head_sha":"abc","status":"completed","conclusion":"success","run_attempt":1}]}`, nil
		case strings.Contains(joined, "actions/runs/9/jobs"):
			return `{"jobs":[{"id":70,"name":"pass","status":"completed","conclusion":"success"}]}`, nil
		default:
			t.Fatalf("unexpected call: %v", args)
			return "", nil
		}
	}
	out, err := (GH{Repo: "o/r", Run: run}).FailedJobLogs(3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"jobs":[]`) {
		t.Fatalf("want an empty jobs array, got %s", out)
	}
}

// TestNoVerbAcceptsRunIDs is the acceptance-criterion tripwire at the API
// seam: the exported Actions-surface entry points take a PR number (plus
// config/state the GATE resolved), never a caller-supplied run id, job id, or
// workflow name. Compile-time shape assertions — if someone widens a
// signature to accept a caller-named run, this stops compiling or fails.
func TestNoVerbAcceptsRunIDs(t *testing.T) {
	var _ func(int, *ChecksConfig) (string, error) = GH{}.Checks
	var _ func(int) (RunState, error) = GH{}.FetchRunState
	var _ func(int, int) (string, error) = GH{}.FailedJobLogs
	// ExecuteRerun takes targets — but only RunState/PlanRerun (gate-resolved
	// from the PR) produce them; the prRun wiring never passes caller input.
	var _ func([]WorkflowRun) (string, error) = GH{}.ExecuteRerun
}
