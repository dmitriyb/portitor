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

func TestMergeApproved(t *testing.T) {
	run, _ := stub("APPROVED\n", nil)
	if ok, err := (GH{Repo: "o/r", Run: run}).MergeApproved(1); err != nil || !ok {
		t.Fatalf("approved: ok=%v err=%v", ok, err)
	}
	run, _ = stub("REVIEW_REQUIRED\n", nil)
	if ok, _ := (GH{Repo: "o/r", Run: run}).MergeApproved(1); ok {
		t.Fatal("should not be approved")
	}
}
