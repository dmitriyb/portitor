// Package action mediates the GitHub actions portitor performs on the upstream
// with its own credential — PR open/comment/review/merge/close and read-side
// fetch. It is NOT a passthrough: callers pass structured, validated requests
// (the gh arguments are constructed here, never forwarded from the agent), so
// the agent can never run arbitrary gh. Authority is decided by the caller
// (post-receive for auto-open; the role-checked `portitor pr` handler for the
// rest); this package just executes the narrow, allowed operation.
package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ghTimeout bounds every gh subprocess (network calls to GitHub). A hung gh
// must never block a hook or an action indefinitely.
const ghTimeout = 2 * time.Minute

// Runner executes gh with the given args and returns stdout. Swapped in tests.
type Runner func(args ...string) (string, error)

// defaultRunner runs the real gh binary.
func defaultRunner(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.WaitDelay = 5 * time.Second
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return out.String(), fmt.Errorf("gh %s: timed out after %s", strings.Join(args, " "), ghTimeout)
		}
		return out.String(), fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// GH performs actions against a single repository (owner/name slug).
type GH struct {
	Repo string // owner/name
	Run  Runner // nil => the real gh binary
}

func (g GH) run(args ...string) (string, error) {
	r := g.Run
	if r == nil {
		r = defaultRunner
	}
	// -R <repo> pins every call to the managed repo (gh has no repo context in a
	// bare dir); appended at the end so it lands after the full subcommand.
	all := make([]string, 0, len(args)+2)
	all = append(all, args...)
	all = append(all, "-R", g.Repo)
	return r(all...)
}

// runAPI runs a `gh api ...` invocation directly, through the same swappable
// Runner but WITHOUT the trailing "-R <repo>" g.run appends: `gh api` has no
// -R/--repo flag (unlike the `pr`/`issue` subcommands) — the owner/repo goes
// into the REST path or the GraphQL variables instead (see ownerName).
func (g GH) runAPI(args ...string) (string, error) {
	r := g.Run
	if r == nil {
		r = defaultRunner
	}
	return r(args...)
}

// ownerName splits Repo ("owner/name") for REST paths and GraphQL variables.
func (g GH) ownerName() (owner, name string, err error) {
	parts := strings.SplitN(g.Repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed repo slug %q (want owner/name)", g.Repo)
	}
	return parts[0], parts[1], nil
}

var prNumberRe = regexp.MustCompile(`/pull/(\d+)`)

// OpenPR creates a PR from head into base and returns its number + URL.
func (g GH) OpenPR(head, base, title, body string) (int, string, error) {
	out, err := g.run("pr", "create", "--head", head, "--base", base, "--title", title, "--body", body)
	if err != nil {
		return 0, "", err
	}
	url := strings.TrimSpace(out)
	m := prNumberRe.FindStringSubmatch(url)
	if m == nil {
		return 0, url, fmt.Errorf("could not parse PR number from %q", url)
	}
	n, _ := strconv.Atoi(m[1])
	return n, url, nil
}

// OpenPRNumber returns the number of an existing open PR for head, or 0 if none.
// Used to make auto-open idempotent across self-correction re-pushes.
func (g GH) OpenPRNumber(head string) (int, error) {
	out, err := g.run("pr", "list", "--head", head, "--state", "open", "--json", "number", "--jq", ".[0].number // empty")
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(out)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse pr number %q: %w", s, err)
	}
	return n, nil
}

// Comment posts a top-level comment on a PR.
func (g GH) Comment(pr int, body string) error {
	_, err := g.run("pr", "comment", strconv.Itoa(pr), "--body", body)
	return err
}

// Review posts a COMMENT-type review with a plain markdown body, toward
// GitHub, regardless of the caller's review verdict (approve/request-changes/
// comment) — same-account safe: the PAT is typically the PR author's account,
// and GitHub refuses self-approval (HTTP 422) but never a comment review. The
// verdict itself lives only in the gate's own reviews_log (see ReviewRecord);
// GH.Review is purely the GitHub-facing half of `review`.
func (g GH) Review(pr int, body string) error {
	_, err := g.run("pr", "review", strconv.Itoa(pr), "--comment", "--body", body)
	return err
}

// InlineComment is one comment of an --inline review submission: a path/line
// anchor plus body, raising a real review thread on GitHub.
type InlineComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

// ReviewSubmission is the --inline stdin document: {"body", "comments"}.
type ReviewSubmission struct {
	Body     string          `json:"body"`
	Comments []InlineComment `json:"comments"`
}

// ReviewInline posts a COMMENT-type review carrying inline comments — each
// comments[] entry raises a real review thread — via one REST call (POST
// .../pulls/:pr/reviews with comments[]), then returns the ids of the review
// threads it created (via a GraphQL reviewThreads query filtered to the
// comments this review just posted), so the caller can record them.
func (g GH) ReviewInline(pr int, body string, comments []InlineComment) ([]string, error) {
	owner, name, err := g.ownerName()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(struct {
		Body     string          `json:"body"`
		Event    string          `json:"event"`
		Comments []InlineComment `json:"comments"`
	}{Body: body, Event: "COMMENT", Comments: comments})
	if err != nil {
		return nil, fmt.Errorf("review --inline: marshal payload: %w", err)
	}
	f, err := os.CreateTemp("", "portitor-review-*.json")
	if err != nil {
		return nil, fmt.Errorf("review --inline: temp payload file: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(payload); err != nil {
		f.Close()
		return nil, fmt.Errorf("review --inline: write payload: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("review --inline: close payload: %w", err)
	}

	endpoint := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, name, pr)
	out, err := g.runAPI("api", endpoint, "--input", f.Name())
	if err != nil {
		return nil, err
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		return nil, fmt.Errorf("review --inline: parse response: %w", err)
	}
	if len(comments) == 0 {
		return nil, nil // no inline comments => no threads raised
	}
	commentIDs, err := g.reviewCommentNodeIDs(pr, created.ID)
	if err != nil {
		return nil, fmt.Errorf("review --inline: created comments: %w", err)
	}
	threads, err := g.FetchReviewThreads(pr)
	if err != nil {
		return nil, fmt.Errorf("review --inline: review threads: %w", err)
	}
	return threadsContaining(threads, commentIDs), nil
}

// reviewCommentNodeIDs lists the GraphQL node ids of the review comments a
// just-created review posted (REST create-review does not echo them back).
func (g GH) reviewCommentNodeIDs(pr int, reviewID int64) ([]string, error) {
	owner, name, err := g.ownerName()
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%d/comments", owner, name, pr, reviewID)
	out, err := g.runAPI("api", endpoint)
	if err != nil {
		return nil, err
	}
	var comments []struct {
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal([]byte(out), &comments); err != nil {
		return nil, fmt.Errorf("parse review comments: %w", err)
	}
	ids := make([]string, 0, len(comments))
	for _, c := range comments {
		ids = append(ids, c.NodeID)
	}
	return ids, nil
}

// threadsContaining returns the ids of threads whose comment chain includes
// any of wantCommentIDs.
func threadsContaining(threads []ReviewThread, wantCommentIDs []string) []string {
	want := make(map[string]bool, len(wantCommentIDs))
	for _, id := range wantCommentIDs {
		want[id] = true
	}
	var out []string
	for _, th := range threads {
		for _, c := range th.Comments {
			if want[c.ID] {
				out = append(out, th.ID)
				break
			}
		}
	}
	return out
}

// replyMutation answers into an existing review thread.
const replyMutation = `mutation($threadId: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: {pullRequestReviewThreadId: $threadId, body: $body}) {
    comment { id }
  }
}`

// Reply answers into an existing review thread (GraphQL
// addPullRequestReviewThreadReply) — the thread id is a global GraphQL node
// id, so no PR number is needed.
func (g GH) Reply(threadID, body string) error {
	if threadID == "" {
		return fmt.Errorf("reply: empty thread id")
	}
	_, err := g.runAPI("api", "graphql", "-f", "query="+replyMutation,
		"-F", "threadId="+threadID, "-f", "body="+body)
	return err
}

// resolveMutation resolves an existing review thread.
const resolveMutation = `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`

// Resolve resolves one review thread (GraphQL resolveReviewThread).
func (g GH) Resolve(threadID string) error {
	if threadID == "" {
		return fmt.Errorf("resolve: empty thread id")
	}
	_, err := g.runAPI("api", "graphql", "-f", "query="+resolveMutation, "-F", "threadId="+threadID)
	return err
}

// Merge squash-merges a PR (the landing action; the caller has already
// enforced the config's action policy and the merge preconditions), pinned to
// headSHA via --match-head-commit. GitHub's own atomic re-check at merge time
// covers only GitHub-enforced rules (branch protection, required checks); the
// internal review verdict (reviews_log) is portitor's own and was recorded
// against a specific head, so the merge must land exactly the head the
// preconditions were evaluated against — an unpinned merge would be a
// TOCTOU: a new commit landing on the branch between the precondition
// evaluation and this call would ride in on an approval that never covered it.
func (g GH) Merge(pr int, headSHA string) error {
	_, err := g.run("pr", "merge", strconv.Itoa(pr), "--squash", "--match-head-commit", headSHA)
	return err
}

// ClosePR closes a PR without merging.
func (g GH) ClosePR(pr int) error {
	_, err := g.run("pr", "close", strconv.Itoa(pr))
	return err
}

// Fetch returns a PR's review state as JSON (branch refs + reviews/comments/
// commits + reviewThreads) — the read-side that feeds the review/fix bundle
// in place of direct gh access. headRefName lets the prelude check out the PR
// branch. reviewThreads (id, isResolved, path, line, comment chain) is merged
// in from one additional GraphQL query, so a fix step sees inline feedback
// deterministically — addressed-state is data, not inference.
func (g GH) Fetch(pr int) (string, error) {
	out, err := g.run("pr", "view", strconv.Itoa(pr), "--json",
		"number,title,body,state,headRefName,baseRefName,reviews,comments,commits")
	if err != nil {
		return "", err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &root); err != nil {
		return "", fmt.Errorf("fetch: parse pr view: %w", err)
	}
	threads, err := g.FetchReviewThreads(pr)
	if err != nil {
		return "", fmt.Errorf("fetch: review threads: %w", err)
	}
	tb, err := json.Marshal(threads)
	if err != nil {
		return "", fmt.Errorf("fetch: marshal review threads: %w", err)
	}
	root["reviewThreads"] = tb
	merged, err := json.Marshal(root)
	if err != nil {
		return "", fmt.Errorf("fetch: marshal merged result: %w", err)
	}
	return string(merged), nil
}

// ReviewThread is one PR review thread: id, resolved-state, the path/line it
// anchors to, and its comment chain.
type ReviewThread struct {
	ID         string          `json:"id"`
	IsResolved bool            `json:"isResolved"`
	Path       string          `json:"path"`
	Line       int             `json:"line"`
	Comments   []ThreadComment `json:"comments"`
}

// ThreadComment is one comment in a review thread's chain.
type ThreadComment struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Body   string `json:"body"`
}

// reviewThreadsQuery fetches a PR's review threads: id, resolved-state,
// path/line, and the comment chain (author, body). first: 100 covers any
// realistic PR; pagination is out of scope (portitor targets small,
// single-account repos, not high-thread-count monorepo PRs).
const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100) {
        nodes {
          id
          isResolved
          path
          line
          comments(first: 100) {
            nodes { id body author { login } }
          }
        }
      }
    }
  }
}`

// FetchReviewThreads runs the reviewThreads GraphQL query for pr.
func (g GH) FetchReviewThreads(pr int) ([]ReviewThread, error) {
	owner, name, err := g.ownerName()
	if err != nil {
		return nil, err
	}
	// owner/name go through -f (raw string): gh's -F type-coerces
	// true|false|null|123-looking values, which would corrupt a legitimate
	// repo/owner name against the String! variables here. -F is reserved for
	// number, the one genuinely integer variable (Int!).
	out, err := g.runAPI("api", "graphql", "-f", "query="+reviewThreadsQuery,
		"-f", "owner="+owner, "-f", "name="+name, "-F", "number="+strconv.Itoa(pr))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Path       string `json:"path"`
							Line       int    `json:"line"`
							Comments   struct {
								Nodes []struct {
									ID     string `json:"id"`
									Body   string `json:"body"`
									Author struct {
										Login string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("parse reviewThreads: %w", err)
	}
	nodes := resp.Data.Repository.PullRequest.ReviewThreads.Nodes
	threads := make([]ReviewThread, 0, len(nodes))
	for _, n := range nodes {
		th := ReviewThread{ID: n.ID, IsResolved: n.IsResolved, Path: n.Path, Line: n.Line}
		for _, c := range n.Comments.Nodes {
			th.Comments = append(th.Comments, ThreadComment{ID: c.ID, Author: c.Author.Login, Body: c.Body})
		}
		threads = append(threads, th)
	}
	return threads, nil
}

// MergeState is the authoritative GitHub-side state a merge decision re-derives
// from — fetched in one query, never trusted from the request.
type MergeState struct {
	ReviewDecision    string     `json:"reviewDecision"`
	MergeStateStatus  string     `json:"mergeStateStatus"`
	HeadRefName       string     `json:"headRefName"`
	HeadSHA           string     `json:"headRefOid"` // the PR's current head commit — reviews_log lookup key
	StatusCheckRollup []CheckRun `json:"statusCheckRollup"`
}

// CheckRun is one entry of statusCheckRollup. GitHub mixes two shapes (check
// runs and legacy status contexts); Name/Context and Conclusion/State are the
// respective pairs.
type CheckRun struct {
	Name       string `json:"name"`
	Context    string `json:"context"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

// checkName returns the entry's identifying name across both shapes.
func (c CheckRun) checkName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Context
}

// succeeded reports a green conclusion across both shapes.
func (c CheckRun) succeeded() bool {
	v := c.Conclusion
	if v == "" {
		v = c.State
	}
	return strings.EqualFold(v, "SUCCESS")
}

// FetchMergeState re-derives the PR's merge-relevant state in one query.
func (g GH) FetchMergeState(pr int) (MergeState, error) {
	out, err := g.run("pr", "view", strconv.Itoa(pr), "--json",
		"reviewDecision,mergeStateStatus,headRefName,headRefOid,statusCheckRollup")
	if err != nil {
		return MergeState{}, err
	}
	var st MergeState
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return MergeState{}, fmt.Errorf("parse merge state: %w", err)
	}
	return st, nil
}

// ReviewGateInput bundles the merge_gate.review precondition's already-
// resolved evidence: the effective source (see MergeGateConfig.ReviewSource)
// and, for "internal", whether reviews_log holds a qualifying approval.
// Resolving InternalApproved is I/O (reviews_log) — UnmetMergePreconditions
// stays a pure function over pre-fetched/pre-computed inputs.
type ReviewGateInput struct {
	Source           string // "internal" | "github" | "none"
	InternalApproved bool   // only meaningful when Source == "internal"
}

// PredicateResult is one merge_gate.checks entry's already-run outcome (see
// check.RunPredicate). Running the command is I/O — UnmetMergePreconditions
// stays pure over the result.
type PredicateResult struct {
	Name string
	Met  bool  // the command exited 0
	Err  error // non-nil: the command could not be run at all (operational)
}

// UnmetMergePreconditions evaluates the re-derived state plus the review-gate
// and command-predicate evidence the caller already resolved (pure,
// testable) and returns every unmet precondition — empty means the merge may
// proceed to the atomic gh gate. requiredChecks is the config's list; empty =
// advisory. A non-nil error means a predicate could not be run at all — an
// operational failure distinct from an unmet (but runnable) precondition;
// fail-closed, the caller must refuse the merge either way.
func UnmetMergePreconditions(st MergeState, requiredChecks []string, review ReviewGateInput, predicates []PredicateResult) ([]string, error) {
	var unmet []string
	switch review.Source {
	case "", "internal":
		if !review.InternalApproved {
			unmet = append(unmet, "internal review: no reviews_log approval for the current head from a role action_roles allows to review")
		}
	case "github":
		if st.ReviewDecision != "APPROVED" {
			unmet = append(unmet, fmt.Sprintf("review decision is %q, want APPROVED", st.ReviewDecision))
		}
	case "none":
		// No review precondition configured.
	default:
		unmet = append(unmet, fmt.Sprintf("merge_gate.review has an unknown source %q (want internal|github|none)", review.Source))
	}
	if st.MergeStateStatus != "CLEAN" {
		unmet = append(unmet, fmt.Sprintf("merge state is %q, want CLEAN (covers behind-base, conflicts, blocked)", st.MergeStateStatus))
	}
	for _, want := range requiredChecks {
		// Deny-wins across duplicates: if several rollup entries share the
		// required name (e.g. two apps reporting one context), every one of
		// them must be green.
		found, failed := false, false
		for _, c := range st.StatusCheckRollup {
			if c.checkName() == want {
				found = true
				if !c.succeeded() {
					failed = true
				}
			}
		}
		switch {
		case !found:
			unmet = append(unmet, fmt.Sprintf("required check %q is missing from the PR's checks", want))
		case failed:
			unmet = append(unmet, fmt.Sprintf("required check %q is not successful", want))
		}
	}
	for _, p := range predicates {
		if p.Err != nil {
			// Operational failure: fail-closed immediately rather than folding it
			// into the unmet list — it is not "this precondition failed to hold",
			// it is "the gate could not evaluate the precondition at all".
			return unmet, fmt.Errorf("merge_gate check %q: %w", p.Name, p.Err)
		}
		if !p.Met {
			unmet = append(unmet, fmt.Sprintf("merge_gate check %q did not pass", p.Name))
		}
	}
	return unmet, nil
}
