// Package action mediates the GitHub actions portitor performs on the upstream
// with its own credential — PR open/comment/review/merge/close and read-side
// fetch. It is NOT a passthrough: callers pass structured, validated requests
// (the gh arguments are constructed here, never forwarded from the agent), so
// the agent can never run arbitrary gh. Authority is decided by the caller
// (post-receive for auto-open; the role-checked `portitor pr` handler for the
// rest); this package just executes the narrow, allowed operation.
package action

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Runner executes gh with the given args and returns stdout. Swapped in tests.
type Runner func(args ...string) (string, error)

// defaultRunner runs the real gh binary.
func defaultRunner(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
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

// Review submits a review verdict; event is approve|request-changes|comment.
func (g GH) Review(pr int, event, body string) error {
	var flag string
	switch event {
	case "approve":
		flag = "--approve"
	case "request-changes":
		flag = "--request-changes"
	case "comment":
		flag = "--comment"
	default:
		return fmt.Errorf("unknown review event %q (want approve|request-changes|comment)", event)
	}
	args := []string{"pr", "review", strconv.Itoa(pr), flag}
	if body != "" {
		args = append(args, "--body", body)
	}
	_, err := g.run(args...)
	return err
}

// Merge squash-merges a PR (the landing action; merger-only at the caller).
func (g GH) Merge(pr int) error {
	_, err := g.run("pr", "merge", strconv.Itoa(pr), "--squash")
	return err
}

// ClosePR closes a PR without merging.
func (g GH) ClosePR(pr int) error {
	_, err := g.run("pr", "close", strconv.Itoa(pr))
	return err
}

// Fetch returns a PR's review state as JSON (branch refs + reviews/comments/
// commits) — the read-side that feeds the review/fix bundle in place of direct
// gh access. headRefName lets the prelude check out the PR branch.
func (g GH) Fetch(pr int) (string, error) {
	return g.run("pr", "view", strconv.Itoa(pr), "--json",
		"number,title,body,state,headRefName,baseRefName,reviews,comments,commits")
}

// MergeApproved reports whether the PR has at least one APPROVED review and no
// pending CHANGES_REQUESTED — portitor re-derives this before a merger merge,
// rather than trusting the request.
func (g GH) MergeApproved(pr int) (bool, error) {
	out, err := g.run("pr", "view", strconv.Itoa(pr), "--json", "reviewDecision", "--jq", ".reviewDecision")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "APPROVED", nil
}
