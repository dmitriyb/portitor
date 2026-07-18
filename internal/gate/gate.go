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
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/dmitriyb/portitor/internal/git"
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

// Validate checks the update's wire shape as pre-receive delivers it: each SHA
// exactly 40 hex chars (the all-zero id included), the ref "refs/"-prefixed with
// no control bytes. Fail-closed: the caller rejects the push on any error —
// input the gate cannot fully understand is never partially enforced.
func (u RefUpdate) Validate() error {
	if !ValidSHA(u.OldSHA) {
		return fmt.Errorf("malformed old object id %q", u.OldSHA)
	}
	if !ValidSHA(u.NewSHA) {
		return fmt.Errorf("malformed new object id %q", u.NewSHA)
	}
	if !ValidRef(u.Ref) {
		return fmt.Errorf("malformed ref name %q", u.Ref)
	}
	return nil
}

// ValidSHA reports whether s is a well-formed object id as hook stdin delivers
// them: exactly 40 lowercase hex chars (the all-zero id included).
func ValidSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// maxRefLen bounds a ref name from hook stdin; git imposes no hard limit, but
// anything near this is not a ref a supported flow produces.
const maxRefLen = 4096

// ValidRef reports whether s is a plausible ref name for hook input:
// "refs/"-prefixed, bounded, and free of space/control bytes (which could
// otherwise smuggle structure into later argv or reports).
func ValidRef(s string) bool {
	if !strings.HasPrefix(s, "refs/") || len(s) <= len("refs/") || len(s) > maxRefLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// RoleRule requires that any introduced commit whose diff to PathGlob adds a line
// matching AddedRegex be signed by a signer whose role is in AllowedRoles. It lets
// generic portitor enforce domain rules (e.g. "only reviewer/owner may close a
// bead") from config, without built-in knowledge of beads/spex.
type RoleRule struct {
	Name         string   `json:"name"`          // stable identifier, surfaced as the violation rule
	PathGlob     string   `json:"path_glob"`     // git pathspec the commit's diff must touch
	AddedRegex   string   `json:"added_regex"`   // regex; matched against ADDED lines in that path's diff
	AllowedRoles []string `json:"allowed_roles"` // the signer's role must be one of these
}

// Config controls the checks.
type Config struct {
	// DefaultBranch is the protected branch (e.g. "main"). If empty, it is derived
	// from the receiving repo's HEAD symref.
	DefaultBranch string `json:"default_branch"`
	// AllowedSigners is the path to an OpenSSH allowed_signers file listing the
	// commit signers portitor trusts. If empty, signatures cannot be trusted and
	// every commit is treated as untrusted.
	AllowedSigners string `json:"allowed_signers"`
	// Roles maps a signer key fingerprint (as git reports it via %GF, e.g.
	// "SHA256:...") to a role name. Unmapped signers have the empty role.
	Roles map[string]string `json:"roles"`
	// RoleRules are evaluated against every introduced commit.
	RoleRules []RoleRule `json:"role_rules"`
	// RequireUpToDateWithDefault, when true, rejects a feature-branch update whose
	// tip does not contain the current default-branch tip (i.e. it is based on a
	// stale default). The deterministic start-task wrapper branches from the current
	// default, so this is a backstop. Off by default.
	RequireUpToDateWithDefault bool `json:"require_up_to_date_with_default"`
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
		// Rule: only branch refs are accepted. Any other namespace — tags, notes,
		// and especially refs/replace/* (whose objects would substitute for others
		// in later git reads) — is refused outright, creates/updates/deletions
		// alike. A refused ref gets no further evaluation.
		if branch, ok := strings.CutPrefix(u.Ref, "refs/heads/"); !ok || branch == "" {
			vs = append(vs, Violation{
				Ref:    u.Ref,
				Rule:   "ref-namespace",
				Detail: "only branch refs (refs/heads/<name>) may be pushed",
			})
			continue
		}

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
			continue // nothing to sign-, ancestry-, or role-check on a deletion
		}

		// Rule: a feature branch must be based on the current default (not stale).
		if cfg.RequireUpToDateWithDefault && u.Ref != defRef {
			stale, err := staleBase(repoDir, defRef, u.NewSHA)
			if err != nil {
				return nil, fmt.Errorf("ancestry check for %s: %w", u.Ref, err)
			}
			if stale {
				vs = append(vs, Violation{
					Ref:    u.Ref,
					Rule:   "stale-base",
					Detail: fmt.Sprintf("branch is not based on the current %q — rebase onto it (git fetch && git rebase origin/%s)", def, def),
				})
			}
		}

		commits, err := newCommits(repoDir, u)
		if err != nil {
			return nil, fmt.Errorf("list commits for %s: %w", u.Ref, err)
		}
		for _, c := range commits {
			// Rule: every introduced commit must be signed by an allowed signer.
			// A failure of the verification subprocess itself is an operational
			// error (push rejected), distinct from a signature verdict.
			status, fp, _, err := commitSig(repoDir, c, cfg.AllowedSigners)
			if err != nil {
				return nil, err
			}
			if status != "G" {
				vs = append(vs, Violation{
					Ref:    u.Ref,
					Rule:   "unsigned-or-untrusted-commit",
					Detail: fmt.Sprintf("commit %s is not signed by an allowed signer (%s)", shortSHA(c), sigReason(status)),
				})
				continue // an untrusted signer's role can't be trusted; skip role rules
			}

			// Rule(s): role-gated content. The signer's role comes from its key
			// fingerprint (a credential, not a forgeable label). Cache the per-pathglob
			// diff for THIS commit so rules sharing a path don't re-shell git (the diff
			// for a given pathglob is identical regardless of which rule asked).
			role := cfg.Roles[fp]
			diffCache := map[string]string{}
			for _, r := range rules {
				diff, ok := diffCache[r.PathGlob]
				if !ok {
					d, err := git.OutputHermetic(repoDir, "show", "--format=", "--unified=0", c, "--", r.PathGlob)
					if err != nil {
						return nil, err
					}
					diff = d
					diffCache[r.PathGlob] = d
				}
				if diffAddsMatch(diff, r.re) && !contains(r.AllowedRoles, role) {
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
	out, err := git.OutputHermetic(repoDir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// newCommits lists the commits an update introduces that aren't already present.
func newCommits(repoDir string, u RefUpdate) ([]string, error) {
	var args []string
	if u.IsCreate() {
		args = []string{"rev-list", u.NewSHA, "--not", "--all", "--"}
	} else {
		args = []string{"rev-list", u.OldSHA + ".." + u.NewSHA, "--"}
	}
	out, err := git.OutputHermetic(repoDir, args...)
	if err != nil {
		return nil, err
	}
	// Split over the fully captured output — deliberately not a Scanner: a
	// scanner error here would silently truncate the commit list, and the
	// "every introduced commit is checked" invariant fails toward acceptance.
	var commits []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}

// commitSig returns git's signature verdict for a commit: status is the %G? code
// ("G" good+trusted, "U" good+untrusted, "B" bad, "N" none, "E" error), fingerprint
// is %GF (e.g. "SHA256:..."), and signer is %GS (the matched principal).
//
// The verification runs hermetically with the allowed-signers trust root pinned
// unconditionally: an empty allowedSigners path makes git report every signature
// as untrusted (%G? == U), so a missing trust root fails closed — it never falls
// back to an ambient allowedSignersFile from machine config. A failure of the
// subprocess itself is returned as err, distinct from a signature verdict.
func commitSig(repoDir, sha, allowedSigners string) (status, fingerprint, signer string, err error) {
	out, err := git.OutputHermetic(repoDir,
		"-c", "gpg.ssh.allowedSignersFile="+allowedSigners,
		"show", "-s", "--format=%G?%n%GF%n%GS", sha)
	if err != nil {
		return "", "", "", fmt.Errorf("signature check for %s: %w", shortSHA(sha), err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for len(lines) < 3 {
		lines = append(lines, "")
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), strings.TrimSpace(lines[2]), nil
}

// diffAddsMatch reports whether a unified diff adds (a "+" line, not the "+++"
// file header) a line matching re. Pure — the diff is produced by git with the
// rule's pathspec applied, so path filtering keeps git's exact semantics.
func diffAddsMatch(diff string, re *regexp.Regexp) bool {
	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // tolerate long diff lines
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			if re.MatchString(line) {
				return true
			}
		}
	}
	return false
}

// staleBase reports whether newSHA fails to contain the current default-branch tip
// (i.e. the branch is based on a stale default). If the default branch doesn't yet
// exist in the repo, nothing is stale.
func staleBase(repoDir, defRef, newSHA string) (bool, error) {
	exists, err := refExists(repoDir, defRef)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	tip, err := git.OutputHermetic(repoDir, "rev-parse", defRef)
	if err != nil {
		return false, err
	}
	anc, err := isAncestor(repoDir, strings.TrimSpace(tip), newSHA)
	if err != nil {
		return false, err
	}
	return !anc, nil
}

// refExists reports whether ref resolves to a commit. Exit status 1 means "does
// not exist"; any other failure (e.g. a timeout) is a real error — it must not
// silently read as "absent" and skip a check.
func refExists(repoDir, ref string) (bool, error) {
	_, err := git.OutputHermetic(repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// isAncestor reports whether ancestor is an ancestor of descendant.
func isAncestor(repoDir, ancestor, descendant string) (bool, error) {
	_, err := git.OutputHermetic(repoDir, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("merge-base --is-ancestor: %w", err)
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
