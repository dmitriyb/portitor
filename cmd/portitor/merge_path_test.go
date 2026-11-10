package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGHForMerge puts a gh on PATH that answers a merge's queries for PR 3 at
// head abc, with one Actions job "test" of the given conclusion, and logs every
// call.
func fakeGHForMerge(t *testing.T, conclusion string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >>'` + log + `'
case "$*" in
  "pr view 3 --json"*) echo '{"mergeStateStatus":"CLEAN","headRefName":"feat","headRefOid":"abc"}' ;;
  *"actions/runs?head_sha=abc"*) echo '{"workflow_runs":[{"id":9,"name":"ci","run_attempt":1}]}' ;;
  *"actions/runs/9/jobs"*) echo '{"jobs":[{"id":1,"name":"test","status":"completed","conclusion":"` + conclusion + `"}]}' ;;
  *"commits/abc/status"*) echo '{"statuses":[]}' ;;
  "pr merge 3"*) ;;
  *) echo "fake gh: unexpected $*" >&2; exit 9 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func mergeWithRequiredChecks(t *testing.T, conclusion string) (code int, stderr string, calls string) {
	t.Helper()
	reposDir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	fp := "SHA256:" + strings.Repeat("M", 43)
	cfg := `{"format_version":1,"default_branch":"main","upstream_slug":"o/r",` +
		`"roles":{"` + fp + `":"merger"},"action_roles":{"merge":["merger"]},"required_checks":["test"]}`
	if err := os.WriteFile(filepath.Join(reposDir, "r.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	log := fakeGHForMerge(t, conclusion)
	var out, errb bytes.Buffer
	code = prRun(fp, []string{"merge"}, prOptions{Repo: "r", PR: 3}, strings.NewReader(""), &out, &errb)
	b, _ := os.ReadFile(log)
	return code, errb.String(), string(b)
}

func TestMergeReadsRequiredChecksFromActionsJobs(t *testing.T) {
	code, stderr, calls := mergeWithRequiredChecks(t, "success")
	if code != 0 {
		t.Fatalf("merge with a green required check = %d; stderr:\n%s\ncalls:\n%s", code, stderr, calls)
	}
	if !strings.Contains(calls, "actions/runs?head_sha=abc") || !strings.Contains(calls, "pr merge 3") || !strings.Contains(calls, "--match-head-commit abc") {
		t.Fatalf("merge did not read the head's jobs and merge pinned to it; calls:\n%s", calls)
	}
	if strings.Contains(calls, "statusCheckRollup") {
		t.Fatalf("merge asked for statusCheckRollup; calls:\n%s", calls)
	}
}

func TestMergeRefusesAFailedRequiredCheck(t *testing.T) {
	code, stderr, calls := mergeWithRequiredChecks(t, "failure")
	if code == 0 || !strings.Contains(stderr, `required check "test" is not successful`) {
		t.Fatalf("merge with a red required check = %d; stderr:\n%s", code, stderr)
	}
	if strings.Contains(calls, "pr merge") {
		t.Fatalf("a refused merge still called gh pr merge; calls:\n%s", calls)
	}
}
