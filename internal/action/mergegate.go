package action

// MergeGateConfig is the config's merge_gate block (spec/proposals/
// 2026-08-04-merge-gate-v2.md): the merge review-precondition source plus
// command predicates run under the check package's hermetic contract before a
// merge. It lives here (not internal/config) because UnmetMergePreconditions
// needs the type and internal/config already depends on internal/action.
type MergeGateConfig struct {
	// Review selects the merge review-precondition source: "internal" (a
	// reviews_log approval for the current head), "github" (legacy
	// reviewDecision == APPROVED), or "none" (no review precondition). Empty
	// defaults to "internal" — see ReviewSource.
	Review string `json:"review"`
	// Checks are named command predicates run in the bare repo dir with the PR
	// number and head SHA appended as the final two argv elements.
	Checks []CheckPredicate `json:"checks"`
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
// an empty review field both default to "internal": the old hardcoded
// reviewDecision == APPROVED check could never merge in the single-account
// deployments portitor targets (see the proposal's empirical findings), so
// the safer, always-satisfiable default is the gate's own cryptographically
// attributed verdict.
func (m *MergeGateConfig) ReviewSource() string {
	if m == nil || m.Review == "" {
		return "internal"
	}
	return m.Review
}
