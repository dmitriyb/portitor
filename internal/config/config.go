// Package config loads, resolves, and validates portitor's per-repo configuration
// (the gate settings + forwarding fields). Centralizing it gives one coherent error
// model and one place for the registry-path conventions and repo-name validation.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dmitriyb/portitor/internal/action"
	"github.com/dmitriyb/portitor/internal/gate"
	"github.com/dmitriyb/portitor/internal/rules"
)

// SupportedFormatVersion is the only config format_version this binary
// operates with. Missing, lower, or higher refuses at load — never operate
// with a partially understood config (see spec/gate/arch_config.md).
const SupportedFormatVersion = 1

// Settings is portitor's per-repo configuration: gate checks + forwarding + the
// upstream slug the action API targets + the action policy and audit trail.
type Settings struct {
	// FormatVersion is the on-disk format stamp — independent of the
	// application version, bumped only on a real incompatible format change.
	FormatVersion int `json:"format_version"`
	gate.Config
	UpstreamRemote string `json:"upstream_remote"`
	UpstreamSlug   string `json:"upstream_slug"` // owner/name for gh; else derived from the upstream remote URL
	// ActionRoles maps each action verb (the closed set fetch|comment|review|
	// merge|close) to the roles allowed to invoke it. Default-deny: an action
	// not listed — or a nil map — is refused for everyone.
	ActionRoles map[string][]string `json:"action_roles"`
	// RequiredChecks lists check names that must be successful before a merge.
	// Empty = advisory (deliberate: repos without CI yet).
	RequiredChecks []string `json:"required_checks"`
	// AuditLog, when set, receives one JSON line per gate/action decision.
	// Empty disables the trail. Write failures never change a verdict.
	AuditLog string `json:"audit_log"`
}

// ReposDir is the registry holding one <repo>.json per managed repo. init-repo points
// each bare's hook config here too, so the gate and the action API read the same config.
func ReposDir() string {
	if d := os.Getenv("PORTITOR_REPOS_DIR"); d != "" {
		return d
	}
	return "/etc/portitor/repos.d"
}

// ReposRoot is where the bare repos live.
func ReposRoot() string {
	if d := os.Getenv("PORTITOR_REPO_ROOT"); d != "" {
		return d
	}
	return "/srv/git"
}

// ValidName guards a repo name used to build a config/repo path. Allowing only
// [A-Za-z0-9._-] (and rejecting "." / "..") keeps the name a single path component, so
// filepath.Join can never traverse out of ReposDir()/ReposRoot(). Used by both the git
// path (allowedRepoPath) and the pr/config path (Resolve).
func ValidName(repo string) bool {
	if repo == "" || repo == "." || repo == ".." {
		return false
	}
	for _, r := range repo {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// Load reads the config named by PORTITOR_CONFIG, applying the operational env
// overrides. The gate-integrity fields (default_branch, allowed_signers) have NO env
// override — they come solely from the file so a stray env var can't weaken the gate.
//
// An unset PORTITOR_CONFIG is an error: the hook shims bake it at provisioning,
// so its absence means a broken or bypassed provisioning — and a gate running
// with a zero config would not be uniformly fail-closed (a deletion-only push,
// for example, introduces no commits to distrust). Refuse loudly instead.
func Load() (Settings, error) {
	var s Settings
	path := os.Getenv("PORTITOR_CONFIG")
	if path == "" {
		return s, fmt.Errorf("PORTITOR_CONFIG is not set (hook not provisioned by init-repo/add-repo?); refusing to operate without a config")
	}
	if err := readInto(path, &s); err != nil {
		return s, err
	}
	if v := os.Getenv("PORTITOR_UPSTREAM_REMOTE"); v != "" {
		s.UpstreamRemote = v
	}
	if v := os.Getenv("PORTITOR_UPSTREAM_SLUG"); v != "" {
		s.UpstreamSlug = v
	}
	return s, nil
}

// LoadFile reads + parses a specific config file (no env overrides) — for validation.
func LoadFile(path string) (Settings, error) {
	var s Settings
	err := readInto(path, &s)
	return s, err
}

// Resolve loads one repo's config from the registry by name (its roles + upstream slug
// + rules), so `portitor pr --repo X` acts on X with X's settings.
func Resolve(repo string) (Settings, error) {
	var s Settings
	if !ValidName(repo) {
		return s, fmt.Errorf("invalid repo name %q (allowed: letters, digits, '.', '_', '-')", repo)
	}
	err := readInto(filepath.Join(ReposDir(), repo+".json"), &s)
	return s, err
}

func readInto(path string, s *Settings) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	// Token-level key discipline first (exact top-level keys, duplicates,
	// lowercase schema keys), then a strict decode, then the version guard —
	// all fail-closed before any consumer sees the config.
	if err := checkRawKeys(b); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(s); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	if dec.More() {
		// Trailing content (a concatenated or merge-damaged file) would be
		// silently dropped by a single Decode — the mis-keyed-file class this
		// discipline exists to refuse.
		return fmt.Errorf("config %s: trailing content after the config object", path)
	}
	if s.FormatVersion != SupportedFormatVersion {
		return fmt.Errorf("config %s: format_version %d is not supported by this binary (want %d); refusing to operate with a partially understood config",
			path, s.FormatVersion, SupportedFormatVersion)
	}
	return nil
}

// topLevelKeys is the exact (byte-exact, case-sensitive) allowed key set of the
// config's top-level object. The retired role_rules stays listed so its
// dedicated migration message fires instead of a generic unknown-key error.
var topLevelKeys = map[string]bool{
	"format_version":                  true,
	"default_branch":                  true,
	"allowed_signers":                 true,
	"roles":                           true,
	"content_rules":                   true,
	"role_rules":                      true,
	"require_up_to_date_with_default": true,
	"upstream_remote":                 true,
	"upstream_slug":                   true,
	"action_roles":                    true,
	"required_checks":                 true,
	"audit_log":                       true,
}

// dataMapKeys names the top-level keys whose object values are DATA maps
// (fingerprints, verb names) rather than schema objects: they keep the
// duplicate-key check but are exempt from the lowercase-key rule.
var dataMapKeys = map[string]bool{"roles": true, "action_roles": true}

// checkRawKeys walks the raw JSON tokens and rejects, fail-closed: a non-object
// top level, unknown or non-byte-exact top-level keys, duplicate keys in any
// object (JSON's silent last-wins is how a stale binding shadows a live one),
// and non-lowercase keys in schema objects (Go's case-insensitive field
// matching must never let a stale "Roles" resurrect a revoked binding).
func checkRawKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("top level must be a JSON object")
	}
	return walkObject(dec, true, false)
}

func walkObject(dec *json.Decoder, top, dataMap bool) error {
	seen := map[string]bool{}
	for {
		t, err := dec.Token()
		if err != nil {
			return fmt.Errorf("parse: %w", err)
		}
		if d, ok := t.(json.Delim); ok && d == '}' {
			return nil
		}
		key, ok := t.(string)
		if !ok {
			return fmt.Errorf("parse: unexpected token %v", t)
		}
		if seen[key] {
			return fmt.Errorf("duplicate key %q (silent last-wins could shadow a live value)", key)
		}
		seen[key] = true
		switch {
		case top && !topLevelKeys[key]:
			return fmt.Errorf("unknown top-level key %q (keys are byte-exact; known: lowercase snake_case)", key)
		case !top && !dataMap && key != strings.ToLower(key):
			return fmt.Errorf("key %q must be lowercase (a differently-cased key can shadow the real field)", key)
		}
		if err := walkValue(dec, dataMapKeys[key] && top); err != nil {
			return err
		}
	}
}

func walkValue(dec *json.Decoder, dataMap bool) error {
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if d, ok := t.(json.Delim); ok {
		switch d {
		case '{':
			return walkObject(dec, false, dataMap)
		case '[':
			for dec.More() {
				if err := walkValue(dec, false); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return fmt.Errorf("parse: %w", err)
			}
		}
	}
	return nil
}

// fingerprintRe matches a signer-key fingerprint as git reports it via %GF:
// "SHA256:" followed by 43 chars of unpadded base64 (a SHA-256 digest).
var fingerprintRe = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// ValidFingerprint reports whether fp has the shape git reports via %GF. A
// roles key with any other shape can never match a real signer — dead weight
// that hides a typo'd grant or revocation.
func ValidFingerprint(fp string) bool { return fingerprintRe.MatchString(fp) }

// Validate returns the problems with a config (empty slice = valid): the format
// version must be the supported one, the gate-integrity fields must be present,
// allowed_signers must be readable, roles must be non-empty with
// fingerprint-shaped keys and non-empty values, the retired role_rules key must
// be absent, action_roles verbs must be known, and content_rules must compile.
func Validate(s Settings) []string {
	var problems []string
	if s.FormatVersion != SupportedFormatVersion {
		problems = append(problems, fmt.Sprintf("format_version %d is not supported by this binary (want %d)", s.FormatVersion, SupportedFormatVersion))
	}
	if s.DefaultBranch == "" {
		problems = append(problems, "default_branch is empty")
	}
	if s.AllowedSigners == "" {
		problems = append(problems, "allowed_signers is empty")
	} else if _, err := os.Stat(s.AllowedSigners); err != nil {
		problems = append(problems, fmt.Sprintf("allowed_signers not readable (%s): %v", s.AllowedSigners, err))
	}
	if len(s.Roles) == 0 {
		problems = append(problems, "roles map is empty (no signer could ever be authorized)")
	}
	for fp, role := range s.Roles {
		if !ValidFingerprint(fp) {
			problems = append(problems, fmt.Sprintf("roles key %q is not a fingerprint (want SHA256: + 43 base64 chars); it can never match a real signer", fp))
		}
		if role == "" {
			problems = append(problems, fmt.Sprintf("roles[%q] has an empty role name", fp))
		}
	}
	if len(s.RetiredRoleRules) > 0 {
		problems = append(problems, "role_rules is retired; migrate to content_rules (see spec/gate/arch_content_rules.md)")
	}
	for verb := range s.ActionRoles {
		if !action.KnownVerb(verb) {
			problems = append(problems, fmt.Sprintf("action_roles: unknown action %q (known: %v)", verb, action.Verbs))
		}
	}
	_, ruleProblems := rules.Compile(s.Content)
	problems = append(problems, ruleProblems...)
	return problems
}
