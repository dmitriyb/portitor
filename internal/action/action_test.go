package action

import (
	"strings"
	"testing"
)

// stub returns a Runner that records the last args and replies with out/err.
func stub(out string, err error) (Runner, *[]string) {
	var last []string
	return func(args ...string) (string, error) {
		last = args
		return out, err
	}, &last
}

func TestOpenPR(t *testing.T) {
	run, last := stub("https://github.com/o/r/pull/42\n", nil)
	g := GH{Repo: "o/r", Run: run}
	n, url, err := g.OpenPR("feature", "main", "Title", "Body")
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 || url != "https://github.com/o/r/pull/42" {
		t.Fatalf("got n=%d url=%q", n, url)
	}
	got := strings.Join(*last, " ")
	for _, want := range []string{"pr create", "-R o/r", "--head feature", "--base main", "--title Title", "--body Body"} {
		if !strings.Contains(got, want) {
			t.Fatalf("args %q missing %q", got, want)
		}
	}
}

func TestOpenPRNumberIdempotency(t *testing.T) {
	run, _ := stub("7\n", nil)
	if n, err := (GH{Repo: "o/r", Run: run}).OpenPRNumber("feature"); err != nil || n != 7 {
		t.Fatalf("existing PR: n=%d err=%v", n, err)
	}
	run, _ = stub("\n", nil) // jq "// empty" => blank when none
	if n, err := (GH{Repo: "o/r", Run: run}).OpenPRNumber("feature"); err != nil || n != 0 {
		t.Fatalf("no PR: n=%d err=%v", n, err)
	}
}

func TestReviewEvents(t *testing.T) {
	for ev, flag := range map[string]string{"approve": "--approve", "request-changes": "--request-changes", "comment": "--comment"} {
		run, last := stub("", nil)
		if err := (GH{Repo: "o/r", Run: run}).Review(3, ev, "lgtm"); err != nil {
			t.Fatalf("%s: %v", ev, err)
		}
		got := strings.Join(*last, " ")
		if !strings.Contains(got, "pr review 3") || !strings.Contains(got, flag) || !strings.Contains(got, "--body lgtm") {
			t.Fatalf("%s args = %q", ev, got)
		}
	}
	run, _ := stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).Review(3, "bogus", ""); err == nil {
		t.Fatal("expected error for unknown review event")
	}
}

func TestMergeAndClose(t *testing.T) {
	run, last := stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).Merge(9); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*last, " "); !strings.Contains(got, "pr merge 9") || !strings.Contains(got, "--squash") {
		t.Fatalf("merge args = %q", got)
	}
	run, last = stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).ClosePR(9); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*last, " "); !strings.Contains(got, "pr close 9") {
		t.Fatalf("close args = %q", got)
	}
}

func TestFetchMergeState(t *testing.T) {
	run, last := stub(`{"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN","headRefName":"feat","statusCheckRollup":[{"name":"ci/test","conclusion":"SUCCESS"}]}`, nil)
	st, err := (GH{Repo: "o/r", Run: run}).FetchMergeState(5)
	if err != nil {
		t.Fatal(err)
	}
	if st.ReviewDecision != "APPROVED" || st.MergeStateStatus != "CLEAN" || st.HeadRefName != "feat" || len(st.StatusCheckRollup) != 1 {
		t.Fatalf("state = %+v", st)
	}
	if got := strings.Join(*last, " "); !strings.Contains(got, "pr view 5") || !strings.Contains(got, "mergeStateStatus") {
		t.Fatalf("args = %q", got)
	}
}

// TestUnmetMergePreconditions pins the fail-closed evaluation: every
// non-APPROVED decision (empty included) and every non-CLEAN state is unmet,
// and required checks must be present AND successful.
func TestUnmetMergePreconditions(t *testing.T) {
	clean := MergeState{ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		StatusCheckRollup: []CheckRun{{Name: "ci/test", Conclusion: "SUCCESS"}}}

	if unmet := UnmetMergePreconditions(clean, []string{"ci/test"}); len(unmet) != 0 {
		t.Fatalf("clean state should merge: %v", unmet)
	}
	// Empty advisory list: checks not enforced.
	if unmet := UnmetMergePreconditions(MergeState{ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"}, nil); len(unmet) != 0 {
		t.Fatalf("advisory checks should not block: %v", unmet)
	}
	for _, decision := range []string{"", "REVIEW_REQUIRED", "CHANGES_REQUESTED"} {
		st := clean
		st.ReviewDecision = decision
		if unmet := UnmetMergePreconditions(st, nil); len(unmet) == 0 {
			t.Errorf("decision %q must be unmet", decision)
		}
	}
	for _, state := range []string{"", "BEHIND", "DIRTY", "BLOCKED", "UNSTABLE", "UNKNOWN"} {
		st := clean
		st.MergeStateStatus = state
		if unmet := UnmetMergePreconditions(st, nil); len(unmet) == 0 {
			t.Errorf("merge state %q must be unmet", state)
		}
	}
	// A required check that is missing, or present but failed, blocks.
	if unmet := UnmetMergePreconditions(clean, []string{"ci/other"}); len(unmet) == 0 {
		t.Error("missing required check must be unmet")
	}
	failed := clean
	failed.StatusCheckRollup = []CheckRun{{Name: "ci/test", Conclusion: "FAILURE"}}
	if unmet := UnmetMergePreconditions(failed, []string{"ci/test"}); len(unmet) == 0 {
		t.Error("failed required check must be unmet")
	}
	// Legacy status contexts (context/state shape) also count.
	legacy := clean
	legacy.StatusCheckRollup = []CheckRun{{Context: "ci/legacy", State: "SUCCESS"}}
	if unmet := UnmetMergePreconditions(legacy, []string{"ci/legacy"}); len(unmet) != 0 {
		t.Errorf("legacy status context should satisfy: %v", unmet)
	}
	// Deny-wins across duplicate same-name entries: one green + one red = unmet.
	dup := clean
	dup.StatusCheckRollup = []CheckRun{
		{Name: "ci/test", Conclusion: "SUCCESS"},
		{Name: "ci/test", Conclusion: "FAILURE"},
	}
	if unmet := UnmetMergePreconditions(dup, []string{"ci/test"}); len(unmet) == 0 {
		t.Error("a duplicate failing entry for a required check must be unmet")
	}
}
