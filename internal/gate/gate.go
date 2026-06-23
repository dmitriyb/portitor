// Package gate implements portitor's push verification: the checks a git
// pre-receive runs against an incoming push, inspecting the *result* (the
// objects/refs being landed) rather than any command. It is the hard gate —
// every path to "landing" goes through it.
//
// The crypto is delegated to git itself (the %G? signature verdict against an
// allowed_signers file); this package orchestrates and decides.
package gate

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// zeroSHA is git's all-zero object id, used for branch create (old) and delete (new).
const zeroSHA = "0000000000000000000000000000000000000000"

// RefUpdate is one line of pre-receive stdin: "<old-sha> <new-sha> <ref>".
type RefUpdate struct {
	OldSHA string
	NewSHA string
	Ref    string
}

// IsCreate reports whether the update creates a new ref (old is all-zero).
func (u RefUpdate) IsCreate() bool { return isZero(u.OldSHA) }

// IsDelete reports whether the update deletes a ref (new is all-zero).
func (u RefUpdate) IsDelete() bool { return isZero(u.NewSHA) }

func isZero(s string) bool { return s == "" || strings.Trim(s, "0") == "" }

// RoleRule requires that any introduced commit whose diff to PathGlob adds a line
// matching AddedRegex be signed by a signer whose role is in AllowedRoles. It lets
// generic portitor enforce domain rules (e.g. "only reviewer/owner may close a
// bead") from config, without built-in knowledge of beads/spex.
type RoleRule struct {
	Name         string   // stable identifier, surfaced as the violation rule
	PathGlob     string   // git pathspec the commit's diff must touch
	AddedRegex   string   // regex; matched against ADDED lines in that path's diff
	AllowedRoles []string // the signer's role must be one of these
}

// Config controls the checks.
type Config struct {
	// DefaultBranch is the protected branch (e.g. "main"). If empty, it is derived
	// from the receiving repo's HEAD symref.
	DefaultBranch string
	// AllowedSigners is the path to an OpenSSH allowed_signers file listing the
	// commit signers portitor trusts. If empty, signatures cannot be trusted and
	// every commit is treated as untrusted.
	AllowedSigners string
	// Roles maps a signer key fingerprint (as git reports it via %GF, e.g.
	// "SHA256:...") to a role name. Unmapped signers have the empty role.
	Roles map[string]string
	// RoleRules are evaluated against every introduced commit.
	RoleRules []RoleRule
}

// Violation is a single rejected condition. Rule is a stable identifier; Detail
// is the human-facing reason surfaced to the agent over the push's stderr.
type Violation struct {
	Ref    string
	Rule   string
	Detail string
}

// Check evaluates every ref update against the policy and returns all violations
// (an empty slice means accept). repoDir is the receiving (bare) repository.
//
// It is atomic by contract: the caller rejects the whole push if any violation is
// returned. All violations are collected so the agent can fix everything in one pass.
func Check(repoDir string, updates []RefUpdate, cfg Config) ([]Violation, error) {
	def := cfg.DefaultBranch
	if def == "" {
		d, err := defaultBranch(repoDir)
		if err != nil {
			return nil, fmt.Errorf("determine default branch: %w", err)
		}
		def = d
	}
	defRef := "refs/heads/" + def

	rules, err := compileRules(cfg.RoleRules)
	if err != nil {
		return nil, err
	}

	var vs []Violation
	for _, u := range updates {
		// Rule: never push to (or delete) the default branch.
		if u.Ref == defRef {
			verb := "push to"
			if u.IsDelete() {
				verb = "deletion of"
			}
			vs = append(vs, Violation{
				Ref:    u.Ref,
				Rule:   "no-push-to-default",
				Detail: fmt.Sprintf("%s the default branch %q is not allowed — use a feature branch and open a PR", verb, def),
			})
		}

		if u.IsDelete() {
			continue // nothing to sign- or role-check on a deletion
		}

		commits, err := newCommits(repoDir, u)
		if err != nil {
			return nil, fmt.Errorf("list commits for %s: %w", u.Ref, err)
		}
		for _, c := range commits {
			// Rule: every introduced commit must be signed by an allowed signer.
			status, fp, _ := commitSig(repoDir, c, cfg.AllowedSigners)
			if status != "G" {
				vs = append(vs, Violation{
					Ref:    u.Ref,
					Rule:   "unsigned-or-untrusted-commit",
					Detail: fmt.Sprintf("commit %s is not signed by an allowed signer (%s)", shortSHA(c), sigReason(status)),
				})
				continue // an untrusted signer's role can't be trusted; skip role rules
			}

			// Rule(s): role-gated content. The signer's role comes from its key
			// fingerprint (a credential, not a forgeable label).
			role := cfg.Roles[fp]
			for _, r := range rules {
				match, err := commitMatchesRule(repoDir, c, r)
				if err != nil {
					return nil, err
				}
				if match && !contains(r.AllowedRoles, role) {
					vs = append(vs, Violation{
						Ref:    u.Ref,
						Rule:   r.Name,
						Detail: fmt.Sprintf("commit %s requires signer role in %v, but the signer's role is %s", shortSHA(c), r.AllowedRoles, roleLabel(role)),
					})
				}
			}
		}
	}
	return vs, nil
}

// compiledRule is a RoleRule with its regex pre-compiled.
type compiledRule struct {
	RoleRule
	re *regexp.Regexp
}

func compileRules(rules []RoleRule) ([]compiledRule, error) {
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		re, err := regexp.Compile(r.AddedRegex)
		if err != nil {
			return nil, fmt.Errorf("role rule %q: bad AddedRegex: %w", r.Name, err)
		}
		out = append(out, compiledRule{RoleRule: r, re: re})
	}
	return out, nil
}

// defaultBranch reads the repo's HEAD symref (e.g. "main").
func defaultBranch(repoDir string) (string, error) {
	out, err := git(repoDir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// newCommits lists the commits an update introduces that aren't already present.
func newCommits(repoDir string, u RefUpdate) ([]string, error) {
	var args []string
	if u.IsCreate() {
		args = []string{"rev-list", u.NewSHA, "--not", "--all"}
	} else {
		args = []string{"rev-list", u.OldSHA + ".." + u.NewSHA}
	}
	out, err := git(repoDir, args...)
	if err != nil {
		return nil, err
	}
	var commits []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}

// commitSig returns git's signature verdict for a commit: status is the %G? code
// ("G" good+trusted, "U" good+untrusted, "B" bad, "N" none, "E" error), fingerprint
// is %GF (e.g. "SHA256:..."), and signer is %GS (the matched principal).
func commitSig(repoDir, sha, allowedSigners string) (status, fingerprint, signer string) {
	args := []string{"-C", repoDir}
	if allowedSigners != "" {
		args = append(args, "-c", "gpg.ssh.allowedSignersFile="+allowedSigners)
	}
	args = append(args, "show", "-s", "--format=%G?%n%GF%n%GS", sha)
	out, _ := exec.Command("git", args...).Output() // verdict is in the output, not the exit code
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for len(lines) < 3 {
		lines = append(lines, "")
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), strings.TrimSpace(lines[2])
}

// commitMatchesRule reports whether the commit's diff to rule.PathGlob adds a line
// matching the rule's regex.
func commitMatchesRule(repoDir, sha string, r compiledRule) (bool, error) {
	out, err := git(repoDir, "show", "--format=", "--unified=0", sha, "--", r.PathGlob)
	if err != nil {
		return false, err
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			if r.re.MatchString(line) {
				return true, nil
			}
		}
	}
	return false, nil
}

// git runs `git -C repoDir <args...>` and returns stdout, wrapping errors with stderr.
func git(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func sigReason(status string) string {
	switch status {
	case "N":
		return "no signature"
	case "B":
		return "bad signature"
	case "U":
		return "signer not in allowed_signers"
	case "E":
		return "signature could not be checked"
	default:
		return "no valid signature"
	}
}

func roleLabel(role string) string {
	if role == "" {
		return "unknown (signer not mapped to a role)"
	}
	return fmt.Sprintf("%q", role)
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
