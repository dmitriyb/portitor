package action

// MergeGateConfig is the config's merge_gate block (spec/proposals/
// 2026-08-04-merge-gate-v2.md): the merge review-precondition source plus
// command predicates run under the check package's hermetic contract before a
// merge. It lives here (not internal/config) because UnmetMergePreconditions
// needs the type and internal/config already depends on internal/action.
type MergeGateConfig struct {
	// Review selects the merge review-precondition source: "github" (native
	// reviewDecision == APPROVED — separated-account deployments) or "none"
	// (no review precondition here; express it as a merge_gate.checks git-
	// content predicate instead). Empty defaults to "none" — see ReviewSource.
	// The retired "internal" source (a gate-owned reviews_log verdict) is no
	// longer valid (see 2026-08-05-transparent-approve).
	Review string `json:"review"`
	// Checks are named command predicates run in the bare repo dir with the PR
	// number and head SHA appended as the final two argv elements.
	Checks []CheckPredicate `json:"checks"`
	// MergeMethod selects the `gh pr merge` strategy: "squash" (default),
	// "merge", or "rebase". Empty defaults to "squash" — see
	// MergeMethodOrDefault. The repo must allow the chosen method (GitHub
	// rejects e.g. --merge when merge commits are disabled); that error is
	// forwarded verbatim rather than second-guessed here (see
	// 2026-08-05-configurable-merge-method).
	MergeMethod string `json:"merge_method"`
}

// CheckPredicate is one merge_gate.checks entry: a named, explicit-argv
// command run as a merge precondition under check.RunPredicate — the same
// hermetic execution contract (no shell, deadline, bounded output) as
// content-rule check commands.
type CheckPredicate struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
}

// ReviewSource returns the effective merge_gate.review source. A nil block or
// an empty review field both default to "none": portitor invents no gate — an
// absent merge_gate means only the mandatory CLEAN check and required_checks
// apply, and a deployment that wants a review precondition declares "github"
// or a merge_gate.checks predicate (see 2026-08-05-transparent-approve).
func (m *MergeGateConfig) ReviewSource() string {
	if m == nil || m.Review == "" {
		return "none"
	}
	return m.Review
}

// MergeMethodOrDefault returns the effective merge_gate.merge_method. A nil
// block or an empty field both default to "squash" — today's hardcoded
// behavior — so an absent field is byte-identical to before this was
// configurable (see 2026-08-05-configurable-merge-method).
func (m *MergeGateConfig) MergeMethodOrDefault() string {
	if m == nil || m.MergeMethod == "" {
		return "squash"
	}
	return m.MergeMethod
}
