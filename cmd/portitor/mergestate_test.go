package main

import (
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/action"
)

func TestMergeStateReadsChecksOnlyForRequiredChecks(t *testing.T) {
	var calls []string
	gh := action.GH{Repo: "o/r", Run: func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		calls = append(calls, joined)
		switch {
		case args[0] == "pr":
			return `{"mergeStateStatus":"CLEAN","headRefOid":"abc"}`, nil
		case strings.Contains(joined, "actions/runs?head_sha=abc"):
			return `{"workflow_runs":[{"id":9,"name":"ci","run_attempt":1}]}`, nil
		case strings.Contains(joined, "actions/runs/9/jobs"):
			return `{"jobs":[{"id":1,"name":"test","status":"completed","conclusion":"success"}]}`, nil
		case strings.Contains(joined, "commits/abc/status"):
			return `{"statuses":[]}`, nil
		}
		t.Fatalf("unexpected call %q", joined)
		return "", nil
	}}

	st, err := mergeState(gh, 3, nil)
	if err != nil || len(calls) != 1 || st.Checks != nil {
		t.Fatalf("no required checks: state %+v, calls %v, err %v — want the one pr view and no checks read", st, calls, err)
	}

	calls = nil
	st, err = mergeState(gh, 3, []string{"test"})
	if err != nil || len(st.Checks) != 1 || st.Checks[0].Name != "test" || st.Checks[0].Conclusion != "SUCCESS" {
		t.Fatalf("required checks: state %+v, err %v — want the head's checks read", st, err)
	}
	unmet, err := action.UnmetMergePreconditions(st, []string{"test"}, action.ReviewGateInput{}, nil)
	if err != nil || len(unmet) != 0 {
		t.Fatalf("a green required check is unmet: %v %v", unmet, err)
	}
}
