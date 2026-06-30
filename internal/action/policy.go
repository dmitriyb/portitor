package action

// RoleActions is the single source of truth for which signer roles may perform each
// gated GitHub action via `portitor pr`. `owner` is the human override; `merger` is
// the dedicated, commit-less landing identity. Keep this in sync with the README
// policy table — a new action is added here once.
var RoleActions = map[string][]string{
	"fetch":   {"implementer", "fixer", "reviewer", "merger", "owner"},
	"comment": {"implementer", "fixer", "reviewer", "merger", "owner"},
	"review":  {"reviewer", "owner"},
	"merge":   {"merger", "owner"},
	"close":   {"merger", "owner"},
}

// RoleCan reports whether a role may perform an action.
func RoleCan(role, act string) bool {
	for _, r := range RoleActions[act] {
		if r == role {
			return true
		}
	}
	return false
}
