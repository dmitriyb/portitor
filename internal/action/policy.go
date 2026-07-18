package action

// Verbs is the closed mechanism set of gated GitHub actions. WHO may perform
// each verb is per-repo config (action_roles), never code — portitor ships no
// role names.
var Verbs = []string{"fetch", "comment", "review", "merge", "close"}

// KnownVerb reports whether act is one of the closed mechanism verbs.
func KnownVerb(act string) bool {
	for _, v := range Verbs {
		if v == act {
			return true
		}
	}
	return false
}

// RoleCan reports whether a role may perform an action under the config's
// action_roles map. Default-deny: an action not listed — or listed with no
// roles, or a nil map altogether — is refused for everyone; every action is
// privileged.
func RoleCan(actionRoles map[string][]string, role, act string) bool {
	if role == "" || !KnownVerb(act) {
		return false
	}
	for _, r := range actionRoles[act] {
		if r == role {
			return true
		}
	}
	return false
}
