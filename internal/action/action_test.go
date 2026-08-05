package action

import (
	"encoding/json"
	"fmt"
	"os"
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

// TestReviewSubmitsRealEvent pins the transparent-passthrough behavior
// (2026-08-05-transparent-approve): GH.Review submits the caller's REAL
// GitHub review event — approve/request-changes/comment — never a forced
// COMMENT.
func TestReviewSubmitsRealEvent(t *testing.T) {
	cases := []struct {
		verdict  string
		wantFlag string
	}{
		{"approve", "--approve"},
		{"request-changes", "--request-changes"},
		{"comment", "--comment"},
	}
	for _, c := range cases {
		t.Run(c.verdict, func(t *testing.T) {
			run, last := stub("", nil)
			if err := (GH{Repo: "o/r", Run: run}).Review(3, c.verdict, "lgtm"); err != nil {
				t.Fatal(err)
			}
			got := strings.Join(*last, " ")
			if !strings.Contains(got, "pr review 3") || !strings.Contains(got, c.wantFlag) || !strings.Contains(got, "--body lgtm") {
				t.Fatalf("args = %q, want %q", got, c.wantFlag)
			}
			for _, other := range []string{"--approve", "--request-changes", "--comment"} {
				if other != c.wantFlag && strings.Contains(got, other) {
					t.Fatalf("args %q must carry only %q, not %q", got, c.wantFlag, other)
				}
			}
		})
	}
}

// TestReviewUnknownVerdict pins that an unrecognized verdict is rejected
// before any gh call is attempted.
func TestReviewUnknownVerdict(t *testing.T) {
	run, last := stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).Review(3, "aprove", "lgtm"); err == nil {
		t.Fatal("unknown verdict must error")
	}
	if *last != nil {
		t.Fatalf("gh must not be called for an unknown verdict, got args %v", *last)
	}
}

// TestReviewForgeRefusalPropagates pins that a forge refusal (e.g. HTTP 422
// on self-approval) is returned verbatim, not swallowed — review is a
// transparent passthrough that fails loudly.
func TestReviewForgeRefusalPropagates(t *testing.T) {
	wantErr := fmt.Errorf("gh pr review: HTTP 422: Unprocessable Entity: Can not approve your own pull request")
	run, _ := stub("", wantErr)
	err := (GH{Repo: "o/r", Run: run}).Review(3, "approve", "lgtm")
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("want the 422 error propagated verbatim, got %v", err)
	}
}

// readInputPayloadEvent extracts the "event" field from a create-review
// call's --input payload file. It must be called from WITHIN the fake
// Runner, at call time: ReviewInline removes the temp payload file (defer
// os.Remove) before returning, so reading it back afterward always fails.
func readInputPayloadEvent(args []string) (string, error) {
	for i, a := range args {
		if a == "--input" && i+1 < len(args) {
			b, err := os.ReadFile(args[i+1])
			if err != nil {
				return "", err
			}
			var payload struct {
				Event string `json:"event"`
			}
			if err := json.Unmarshal(b, &payload); err != nil {
				return "", err
			}
			return payload.Event, nil
		}
	}
	return "", fmt.Errorf("no --input flag in args %v", args)
}

// TestReviewInline pins the --inline submission shape: one REST create-review
// call carrying the caller's real event + comments[], then a review-comments
// lookup and a reviewThreads GraphQL query to correlate the created threads.
func TestReviewInline(t *testing.T) {
	var calls [][]string
	var gotEvent string
	run := func(args ...string) (string, error) {
		calls = append(calls, args)
		switch {
		case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "/reviews") && !strings.Contains(args[1], "/comments"):
			ev, everr := readInputPayloadEvent(args)
			if everr != nil {
				t.Fatalf("read --input payload: %v", everr)
			}
			gotEvent = ev
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
	ids, err := g.ReviewInline(7, "request-changes", "please fix", []InlineComment{{Path: "a.go", Line: 3, Body: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "PRT_1" {
		t.Fatalf("thread ids = %v, want [PRT_1] (PRT_2 belongs to a different, human comment)", ids)
	}
	// First call must be the REST create-review with --input (a payload file)
	// carrying the caller's real event, not a forced COMMENT.
	if calls[0][0] != "api" || !strings.Contains(strings.Join(calls[0], " "), "--input") {
		t.Fatalf("first call = %v, want a REST create-review with --input", calls[0])
	}
	if !strings.HasSuffix(calls[0][1], "/pulls/7/reviews") {
		t.Fatalf("endpoint = %q", calls[0][1])
	}
	if gotEvent != "REQUEST_CHANGES" {
		t.Fatalf("payload event = %q, want REQUEST_CHANGES", gotEvent)
	}
}

func TestReviewInlineNoComments(t *testing.T) {
	var gotEvent string
	run := func(args ...string) (string, error) {
		ev, err := readInputPayloadEvent(args)
		if err != nil {
			t.Fatalf("read --input payload: %v", err)
		}
		gotEvent = ev
		return `{"id":1,"node_id":"REV_1"}`, nil
	}
	ids, err := (GH{Repo: "o/r", Run: run}).ReviewInline(7, "approve", "just a comment", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("no comments should raise no threads, got %v", ids)
	}
	if gotEvent != "APPROVE" {
		t.Fatalf("payload event = %q, want APPROVE", gotEvent)
	}
}

// TestReviewInlineUnknownVerdict pins that an unrecognized verdict is
// rejected before any gh call is attempted.
func TestReviewInlineUnknownVerdict(t *testing.T) {
	run, last := stub("", nil)
	if _, err := (GH{Repo: "o/r", Run: run}).ReviewInline(7, "aprove", "x", nil); err == nil {
		t.Fatal("unknown verdict must error")
	}
	if *last != nil {
		t.Fatalf("gh must not be called for an unknown verdict, got args %v", *last)
	}
}

// TestReviewInlineForgeRefusalPropagates pins that a forge refusal from the
// REST create-review call is returned verbatim.
func TestReviewInlineForgeRefusalPropagates(t *testing.T) {
	wantErr := fmt.Errorf("gh api: HTTP 422: Unprocessable Entity: Can not approve your own pull request")
	run, _ := stub("", wantErr)
	_, err := (GH{Repo: "o/r", Run: run}).ReviewInline(7, "approve", "x", nil)
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("want the 422 error propagated verbatim, got %v", err)
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

// TestCurrentLogin pins the `gh api user --jq .login` call shape used to
// identify the gate's own account, now that reviews_log is retired.
func TestCurrentLogin(t *testing.T) {
	run, last := stub("portitor-bot\n", nil)
	login, err := (GH{Repo: "o/r", Run: run}).CurrentLogin()
	if err != nil {
		t.Fatal(err)
	}
	if login != "portitor-bot" {
		t.Fatalf("login = %q, want portitor-bot", login)
	}
	got := strings.Join(*last, " ")
	for _, want := range []string{"api", "user", "--jq", ".login"} {
		if !strings.Contains(got, want) {
			t.Fatalf("args %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "-R o/r") {
		t.Fatalf("gh api user has no -R flag; args must not carry one: %q", got)
	}

	empty, _ := stub("\n", nil)
	if _, err := (GH{Repo: "o/r", Run: empty}).CurrentLogin(); err == nil {
		t.Fatal("an empty login must be an error")
	}
	fails, _ := stub("", fmt.Errorf("boom"))
	if _, err := (GH{Repo: "o/r", Run: fails}).CurrentLogin(); err == nil {
		t.Fatal("a runner failure must propagate")
	}
}

// TestGateAuthoredThreads pins the author-derived gate-thread identity that
// replaces reviews_log's GateThreadIDs: only threads whose opening comment
// was authored by the gate's own login are returned — a human-authored
// thread must never be included, regardless of resolved state (filtering by
// resolved-state is the caller's job, see resolveGateThreads in cmd/portitor).
func TestGateAuthoredThreads(t *testing.T) {
	threads := []ReviewThread{
		{ID: "PRT_1", IsResolved: false, Comments: []ThreadComment{{ID: "C1", Author: "portitor-bot"}}},
		{ID: "PRT_2", IsResolved: true, Comments: []ThreadComment{{ID: "C2", Author: "portitor-bot"}}},
		{ID: "PRT_3", IsResolved: false, Comments: []ThreadComment{{ID: "C3", Author: "human-reviewer"}}},
		{ID: "PRT_4", IsResolved: false, Comments: nil}, // no comments: must never match
	}
	got := GateAuthoredThreads(threads, "portitor-bot")
	var ids []string
	for _, th := range got {
		ids = append(ids, th.ID)
	}
	if strings.Join(ids, ",") != "PRT_1,PRT_2" {
		t.Fatalf("gate-authored threads = %v, want [PRT_1 PRT_2] (PRT_3 is human-authored, PRT_4 has no comments)", ids)
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
// git-content merge_gate.checks predicates were evaluated against that exact
// head.
func TestMergeIsHeadPinned(t *testing.T) {
	run, last := stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).Merge(9, "squash", "deadbeefcafe"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*last, " ")
	for _, want := range []string{"pr merge 9", "--squash", "--match-head-commit deadbeefcafe"} {
		if !strings.Contains(got, want) {
			t.Fatalf("merge args %q missing %q", got, want)
		}
	}
}

// TestMergeMethodFlag pins the merge_gate.merge_method -> gh pr merge flag
// mapping (2026-08-05-configurable-merge-method): each configured method
// issues its own flag, and --match-head-commit is present regardless of
// method — the TOCTOU close applies to every merge strategy.
func TestMergeMethodFlag(t *testing.T) {
	cases := []struct {
		method   string
		wantFlag string
	}{
		{"squash", "--squash"},
		{"merge", "--merge"},
		{"rebase", "--rebase"},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			run, last := stub("", nil)
			if err := (GH{Repo: "o/r", Run: run}).Merge(9, c.method, "deadbeefcafe"); err != nil {
				t.Fatal(err)
			}
			got := strings.Join(*last, " ")
			if !strings.Contains(got, "pr merge 9") || !strings.Contains(got, c.wantFlag) || !strings.Contains(got, "--match-head-commit deadbeefcafe") {
				t.Fatalf("merge args = %q, want %q + --match-head-commit", got, c.wantFlag)
			}
			for _, other := range []string{"--squash", "--merge", "--rebase"} {
				if other != c.wantFlag && strings.Contains(got, other) {
					t.Fatalf("args %q must carry only %q, not %q", got, c.wantFlag, other)
				}
			}
		})
	}
}

// TestMergeUnknownMethod pins that an unrecognized merge_method is rejected
// before any gh call is attempted — config.Validate should already have
// caught it, but Merge fails closed too rather than silently defaulting.
func TestMergeUnknownMethod(t *testing.T) {
	run, last := stub("", nil)
	if err := (GH{Repo: "o/r", Run: run}).Merge(9, "fast-forward", "deadbeefcafe"); err == nil {
		t.Fatal("unknown merge method must error")
	}
	if *last != nil {
		t.Fatalf("gh must not be called for an unknown merge method, got args %v", *last)
	}
}

// TestMergeMethodOrDefault pins the default-to-squash resolution
// (MergeGateConfig.MergeMethodOrDefault): a nil block, an empty field, and
// each explicit value all resolve as expected — byte-identical to the
// pre-configurable hardcoded --squash when merge_method is absent.
func TestMergeMethodOrDefault(t *testing.T) {
	var nilBlock *MergeGateConfig
	if got := nilBlock.MergeMethodOrDefault(); got != "squash" {
		t.Fatalf("nil block: got %q, want squash", got)
	}
	if got := (&MergeGateConfig{}).MergeMethodOrDefault(); got != "squash" {
		t.Fatalf("empty field: got %q, want squash", got)
	}
	for _, method := range []string{"squash", "merge", "rebase"} {
		if got := (&MergeGateConfig{MergeMethod: method}).MergeMethodOrDefault(); got != method {
			t.Fatalf("explicit %q: got %q", method, got)
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

// githubOK/noneOK are the minimal ReviewGateInput that satisfies each source,
// for building precondition-matrix test cases.
var (
	githubOK = ReviewGateInput{Source: "github"}
	noneOK   = ReviewGateInput{Source: "none"}
)

// TestUnmetMergePreconditionsMatrix is the merge precondition table: review
// source (github, none, default empty-string), CLEAN gate, required checks,
// and command predicates (met/unmet/broken). The retired "internal" source
// is gone (see 2026-08-05-transparent-approve).
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
		{name: "github approved + clean + checks green", st: clean, requiredChecks: []string{"ci/test"}, review: githubOK},
		{name: "github reviewDecision empty (stale/no review)", st: func() MergeState { s := clean; s.ReviewDecision = ""; return s }(), review: ReviewGateInput{Source: "github"}, wantUnmet: true},
		{name: "none skips review entirely, even with no approval evidence", st: clean, review: noneOK},
		{name: "default source (empty string) behaves like none", st: clean, review: ReviewGateInput{Source: ""}},
		{name: "unknown review source", st: clean, review: ReviewGateInput{Source: "bogus"}, wantUnmet: true},
		{name: "advisory empty required_checks", st: MergeState{ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"}, review: noneOK},
		{name: "missing required check", st: clean, requiredChecks: []string{"ci/other"}, review: noneOK, wantUnmet: true},
		{name: "predicate met", st: clean, review: noneOK, predicates: []PredicateResult{{Name: "p", Met: true}}},
		{name: "predicate unmet", st: clean, review: noneOK, predicates: []PredicateResult{{Name: "p", Met: false}}, wantUnmet: true},
		{name: "predicate broken (operational)", st: clean, review: noneOK, predicates: []PredicateResult{{Name: "p", Err: fmt.Errorf("boom")}}, wantErr: true},
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
		unmet, err := UnmetMergePreconditions(st, nil, noneOK, nil)
		if err != nil {
			t.Fatalf("merge state %q: unexpected error %v", state, err)
		}
		if len(unmet) == 0 {
			t.Errorf("merge state %q must be unmet", state)
		}
	}
	failed := clean
	failed.StatusCheckRollup = []CheckRun{{Name: "ci/test", Conclusion: "FAILURE"}}
	if unmet, err := UnmetMergePreconditions(failed, []string{"ci/test"}, noneOK, nil); err != nil || len(unmet) == 0 {
		t.Fatalf("failed required check must be unmet, got unmet=%v err=%v", unmet, err)
	}
	// Legacy status contexts (context/state shape) also count.
	legacy := clean
	legacy.StatusCheckRollup = []CheckRun{{Context: "ci/legacy", State: "SUCCESS"}}
	if unmet, err := UnmetMergePreconditions(legacy, []string{"ci/legacy"}, noneOK, nil); err != nil || len(unmet) != 0 {
		t.Fatalf("legacy status context should satisfy: unmet=%v err=%v", unmet, err)
	}
	// Deny-wins across duplicate same-name entries: one green + one red = unmet.
	dup := clean
	dup.StatusCheckRollup = []CheckRun{
		{Name: "ci/test", Conclusion: "SUCCESS"},
		{Name: "ci/test", Conclusion: "FAILURE"},
	}
	if unmet, err := UnmetMergePreconditions(dup, []string{"ci/test"}, noneOK, nil); err != nil || len(unmet) == 0 {
		t.Fatalf("a duplicate failing entry for a required check must be unmet, got unmet=%v err=%v", unmet, err)
	}
}
