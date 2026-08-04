package action

import (
	"encoding/json"
	"fmt"
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

// TestReviewAlwaysComment pins the v2 same-account-safe behavior: GH.Review
// posts a COMMENT-type review toward GitHub regardless of the caller's
// verdict — the verdict (approve/request-changes/comment) lives only in the
// gate's own reviews_log, never as a GitHub approve/request-changes call
// (which the PAT's own account cannot use to self-approve).
func TestReviewAlwaysComment(t *testing.T) {
	run, last := stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).Review(3, "lgtm"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*last, " ")
	if !strings.Contains(got, "pr review 3") || !strings.Contains(got, "--comment") || !strings.Contains(got, "--body lgtm") {
		t.Fatalf("args = %q", got)
	}
	if strings.Contains(got, "--approve") || strings.Contains(got, "--request-changes") {
		t.Fatalf("Review must never post approve/request-changes to GitHub: %q", got)
	}
}

// TestReviewInline pins the --inline submission shape: one REST create-review
// call carrying comments[], then a review-comments lookup and a
// reviewThreads GraphQL query to correlate the created threads.
func TestReviewInline(t *testing.T) {
	var calls [][]string
	run := func(args ...string) (string, error) {
		calls = append(calls, args)
		switch {
		case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "/reviews") && !strings.Contains(args[1], "/comments"):
			return `{"id":99,"node_id":"REV_1"}`, nil
		case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "/reviews/99/comments"):
			return `[{"node_id":"PRRC_1"},{"node_id":"PRRC_2"}]`, nil
		case len(args) >= 2 && args[0] == "api" && args[1] == "graphql":
			return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
				{"id":"PRT_1","isResolved":false,"path":"a.go","line":3,"comments":{"nodes":[{"id":"PRRC_1","body":"x","author":{"login":"bot"}}]}},
				{"id":"PRT_2","isResolved":false,"path":"b.go","line":9,"comments":{"nodes":[{"id":"PRRC_9","body":"y","author":{"login":"human"}}]}}
			]}}}}}`, nil
		default:
			t.Fatalf("unexpected call: %v", args)
			return "", nil
		}
	}
	g := GH{Repo: "o/r", Run: run}
	ids, err := g.ReviewInline(7, "please fix", []InlineComment{{Path: "a.go", Line: 3, Body: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "PRT_1" {
		t.Fatalf("thread ids = %v, want [PRT_1] (PRT_2 belongs to a different, human comment)", ids)
	}
	// First call must be the REST create-review with --input (a payload file).
	if calls[0][0] != "api" || !strings.Contains(strings.Join(calls[0], " "), "--input") {
		t.Fatalf("first call = %v, want a REST create-review with --input", calls[0])
	}
	if !strings.HasSuffix(calls[0][1], "/pulls/7/reviews") {
		t.Fatalf("endpoint = %q", calls[0][1])
	}
}

func TestReviewInlineNoComments(t *testing.T) {
	run, _ := stub(`{"id":1,"node_id":"REV_1"}`, nil)
	ids, err := (GH{Repo: "o/r", Run: run}).ReviewInline(7, "just a comment", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("no comments should raise no threads, got %v", ids)
	}
}

func TestReply(t *testing.T) {
	run, last := stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).Reply("PRT_1", "answered"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*last, " ")
	for _, want := range []string{"api", "graphql", "threadId=PRT_1", "body=answered", "addPullRequestReviewThreadReply"} {
		if !strings.Contains(got, want) {
			t.Fatalf("args %q missing %q", got, want)
		}
	}
	if err := (GH{Repo: "o/r", Run: run}).Reply("", "x"); err == nil {
		t.Fatal("empty thread id must error")
	}
}

func TestResolve(t *testing.T) {
	run, last := stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).Resolve("PRT_1"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*last, " ")
	for _, want := range []string{"api", "graphql", "threadId=PRT_1", "resolveReviewThread"} {
		if !strings.Contains(got, want) {
			t.Fatalf("args %q missing %q", got, want)
		}
	}
	if err := (GH{Repo: "o/r", Run: run}).Resolve(""); err == nil {
		t.Fatal("empty thread id must error")
	}
}

// TestFetchReviewThreads pins the GraphQL call shape and the JSON->struct
// flattening (author.login -> Author).
func TestFetchReviewThreads(t *testing.T) {
	run, last := stub(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
		{"id":"PRT_1","isResolved":true,"path":"a.go","line":5,"comments":{"nodes":[{"id":"C1","body":"hi","author":{"login":"alice"}}]}}
	]}}}}}`, nil)
	threads, err := (GH{Repo: "o/r", Run: run}).FetchReviewThreads(4)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].ID != "PRT_1" || !threads[0].IsResolved || threads[0].Path != "a.go" || threads[0].Line != 5 {
		t.Fatalf("threads = %+v", threads)
	}
	if len(threads[0].Comments) != 1 || threads[0].Comments[0].Author != "alice" || threads[0].Comments[0].Body != "hi" {
		t.Fatalf("comments = %+v", threads[0].Comments)
	}
	got := strings.Join(*last, " ")
	// owner/name must go through -f (raw string): gh's -F type-coerces
	// true|false|null|123-looking values, which would corrupt a legitimate
	// repo/owner name against the String! variables. -F is reserved for
	// number, the one genuinely integer (Int!) variable.
	for _, want := range []string{"api", "graphql", "-f owner=o", "-f name=r", "-F number=4", "reviewThreads"} {
		if !strings.Contains(got, want) {
			t.Fatalf("args %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "-F owner=") || strings.Contains(got, "-F name=") {
		t.Fatalf("owner/name must not use -F (type coercion risk): %q", got)
	}
}

// TestFetchMergesReviewThreads pins that Fetch's returned JSON carries the
// original pr-view fields AND a merged-in reviewThreads array from the
// second GraphQL call.
func TestFetchMergesReviewThreads(t *testing.T) {
	calls := 0
	run := func(args ...string) (string, error) {
		calls++
		if args[0] == "pr" {
			return `{"number":9,"title":"t","reviews":[]}`, nil
		}
		return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
			{"id":"PRT_1","isResolved":false,"path":"a.go","line":1,"comments":{"nodes":[]}}
		]}}}}}`, nil
	}
	out, err := (GH{Repo: "o/r", Run: run}).Fetch(9)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 gh calls (pr view + graphql), got %d", calls)
	}
	var parsed struct {
		Number        int            `json:"number"`
		ReviewThreads []ReviewThread `json:"reviewThreads"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("Fetch output not valid JSON: %v\n%s", err, out)
	}
	if parsed.Number != 9 {
		t.Fatalf("original pr-view field lost: %+v", parsed)
	}
	if len(parsed.ReviewThreads) != 1 || parsed.ReviewThreads[0].ID != "PRT_1" {
		t.Fatalf("reviewThreads = %+v", parsed.ReviewThreads)
	}
}

// TestMergeIsHeadPinned pins the TOCTOU fix: Merge must append
// --match-head-commit <headSHA> so GitHub's own atomic re-check refuses if the
// head moved since the caller evaluated the merge preconditions — the
// internal review verdict (reviews_log) was recorded against that exact head.
func TestMergeIsHeadPinned(t *testing.T) {
	run, last := stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).Merge(9, "deadbeefcafe"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*last, " ")
	for _, want := range []string{"pr merge 9", "--squash", "--match-head-commit deadbeefcafe"} {
		if !strings.Contains(got, want) {
			t.Fatalf("merge args %q missing %q", got, want)
		}
	}
}

func TestClosePR(t *testing.T) {
	run, last := stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).ClosePR(9); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*last, " "); !strings.Contains(got, "pr close 9") {
		t.Fatalf("close args = %q", got)
	}
}

func TestFetchMergeState(t *testing.T) {
	run, last := stub(`{"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN","headRefName":"feat","headRefOid":"deadbeef","statusCheckRollup":[{"name":"ci/test","conclusion":"SUCCESS"}]}`, nil)
	st, err := (GH{Repo: "o/r", Run: run}).FetchMergeState(5)
	if err != nil {
		t.Fatal(err)
	}
	if st.ReviewDecision != "APPROVED" || st.MergeStateStatus != "CLEAN" || st.HeadRefName != "feat" || st.HeadSHA != "deadbeef" || len(st.StatusCheckRollup) != 1 {
		t.Fatalf("state = %+v", st)
	}
	if got := strings.Join(*last, " "); !strings.Contains(got, "pr view 5") || !strings.Contains(got, "mergeStateStatus") || !strings.Contains(got, "headRefOid") {
		t.Fatalf("args = %q", got)
	}
}

// internalOK/githubOK/noneOK are the minimal ReviewGateInput that satisfies
// each source, for building precondition-matrix test cases.
var (
	internalOK = ReviewGateInput{Source: "internal", InternalApproved: true}
	githubOK   = ReviewGateInput{Source: "github"}
	noneOK     = ReviewGateInput{Source: "none"}
)

// TestUnmetMergePreconditionsMatrix is the merge precondition table: review
// source (internal approved/absent/stale-head via the caller-resolved
// ReviewGateInput, github, none), CLEAN gate, required checks, and command
// predicates (met/unmet/broken).
func TestUnmetMergePreconditionsMatrix(t *testing.T) {
	clean := MergeState{ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		StatusCheckRollup: []CheckRun{{Name: "ci/test", Conclusion: "SUCCESS"}}}

	cases := []struct {
		name           string
		st             MergeState
		requiredChecks []string
		review         ReviewGateInput
		predicates     []PredicateResult
		wantUnmet      bool
		wantErr        bool
	}{
		{name: "internal approved + clean + checks green", st: clean, requiredChecks: []string{"ci/test"}, review: internalOK},
		{name: "internal not approved", st: clean, review: ReviewGateInput{Source: "internal", InternalApproved: false}, wantUnmet: true},
		{name: "internal default source (empty string)", st: clean, review: ReviewGateInput{Source: "", InternalApproved: false}, wantUnmet: true},
		{name: "internal default source approved", st: clean, review: ReviewGateInput{Source: "", InternalApproved: true}},
		{name: "github approved", st: clean, review: githubOK},
		{name: "github reviewDecision empty (stale/no review)", st: func() MergeState { s := clean; s.ReviewDecision = ""; return s }(), review: ReviewGateInput{Source: "github"}, wantUnmet: true},
		{name: "none skips review entirely, even with no approval evidence", st: clean, review: noneOK},
		{name: "unknown review source", st: clean, review: ReviewGateInput{Source: "bogus"}, wantUnmet: true},
		{name: "advisory empty required_checks", st: MergeState{ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"}, review: internalOK},
		{name: "missing required check", st: clean, requiredChecks: []string{"ci/other"}, review: internalOK, wantUnmet: true},
		{name: "predicate met", st: clean, review: internalOK, predicates: []PredicateResult{{Name: "p", Met: true}}},
		{name: "predicate unmet", st: clean, review: internalOK, predicates: []PredicateResult{{Name: "p", Met: false}}, wantUnmet: true},
		{name: "predicate broken (operational)", st: clean, review: internalOK, predicates: []PredicateResult{{Name: "p", Err: fmt.Errorf("boom")}}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unmet, err := UnmetMergePreconditions(c.st, c.requiredChecks, c.review, c.predicates)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an operational error, got unmet=%v", unmet)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := len(unmet) > 0; got != c.wantUnmet {
				t.Fatalf("unmet = %v (len %d>0 = %v), want wantUnmet=%v", unmet, len(unmet), got, c.wantUnmet)
			}
		})
	}
}

// TestUnmetMergePreconditionsCleanAndChecks pins the mandatory/non-
// configurable CLEAN gate and the required-checks semantics (present AND
// successful, legacy status-context shape, deny-wins across duplicates) —
// unaffected by the review source.
func TestUnmetMergePreconditionsCleanAndChecks(t *testing.T) {
	clean := MergeState{ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		StatusCheckRollup: []CheckRun{{Name: "ci/test", Conclusion: "SUCCESS"}}}

	for _, state := range []string{"", "BEHIND", "DIRTY", "BLOCKED", "UNSTABLE", "UNKNOWN"} {
		st := clean
		st.MergeStateStatus = state
		unmet, err := UnmetMergePreconditions(st, nil, internalOK, nil)
		if err != nil {
			t.Fatalf("merge state %q: unexpected error %v", state, err)
		}
		if len(unmet) == 0 {
			t.Errorf("merge state %q must be unmet", state)
		}
	}
	failed := clean
	failed.StatusCheckRollup = []CheckRun{{Name: "ci/test", Conclusion: "FAILURE"}}
	if unmet, err := UnmetMergePreconditions(failed, []string{"ci/test"}, internalOK, nil); err != nil || len(unmet) == 0 {
		t.Fatalf("failed required check must be unmet, got unmet=%v err=%v", unmet, err)
	}
	// Legacy status contexts (context/state shape) also count.
	legacy := clean
	legacy.StatusCheckRollup = []CheckRun{{Context: "ci/legacy", State: "SUCCESS"}}
	if unmet, err := UnmetMergePreconditions(legacy, []string{"ci/legacy"}, internalOK, nil); err != nil || len(unmet) != 0 {
		t.Fatalf("legacy status context should satisfy: unmet=%v err=%v", unmet, err)
	}
	// Deny-wins across duplicate same-name entries: one green + one red = unmet.
	dup := clean
	dup.StatusCheckRollup = []CheckRun{
		{Name: "ci/test", Conclusion: "SUCCESS"},
		{Name: "ci/test", Conclusion: "FAILURE"},
	}
	if unmet, err := UnmetMergePreconditions(dup, []string{"ci/test"}, internalOK, nil); err != nil || len(unmet) == 0 {
		t.Fatalf("a duplicate failing entry for a required check must be unmet, got unmet=%v err=%v", unmet, err)
	}
}
