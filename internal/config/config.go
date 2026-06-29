// Package config loads, resolves, and validates portitor's per-repo configuration
// (the gate settings + forwarding fields). Centralizing it gives one coherent error
// model and one place for the registry-path conventions and repo-name validation.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/dmitriyb/portitor/internal/gate"
)

// Settings is portitor's per-repo configuration: gate checks + forwarding + the
// upstream slug the action API targets.
type Settings struct {
	gate.Config
	UpstreamRemote string `json:"upstream_remote"`
	UpstreamSlug   string `json:"upstream_slug"` // owner/name for gh; else derived from the upstream remote URL
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
func Load() (Settings, error) {
	var s Settings
	if path := os.Getenv("PORTITOR_CONFIG"); path != "" {
		if err := readInto(path, &s); err != nil {
			return s, err
		}
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
	if err := json.Unmarshal(b, s); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

// Validate returns the problems with a config (empty slice = valid): the gate-integrity
// fields must be present, allowed_signers must be readable, roles must be non-empty, and
// every role rule's regex must compile + name an allowed role.
func Validate(s Settings) []string {
	var problems []string
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
	for _, r := range s.RoleRules {
		if r.AddedRegex != "" {
			if _, err := regexp.Compile(r.AddedRegex); err != nil {
				problems = append(problems, fmt.Sprintf("role_rule %q: bad added_regex: %v", r.Name, err))
			}
		}
		if len(r.AllowedRoles) == 0 {
			problems = append(problems, fmt.Sprintf("role_rule %q: allowed_roles is empty", r.Name))
		}
	}
	return problems
}
