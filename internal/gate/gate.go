// Package gate implements portitor's push verification: the checks a git
// pre-receive runs against an incoming push, inspecting the *result* (the
// objects/refs being landed) rather than any command. It is the hard gate —
// every path to "landing" goes through it.
//
// The crypto is delegated to git itself (`git verify-commit` against an
// allowed_signers file); this package orchestrates and decides.
package gate

import (
	"bufio"
	"fmt"
	"os/exec"
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

// Config controls the checks.
type Config struct {
	// DefaultBranch is the protected branch (e.g. "main"). If empty, it is derived
	// from the receiving repo's HEAD symref.
	DefaultBranch string
	// AllowedSigners is the path to an OpenSSH allowed_signers file listing the
	// commit signers portitor trusts. If empty, signatures cannot be verified and
	// every commit is treated as untrusted.
	AllowedSigners string
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
			continue // nothing to sign-check on a deletion
		}

		// Rule: every newly-introduced commit must carry a good signature from an
		// allowed signer. Checking the result means it holds regardless of how the
		// commit was produced (git, plumbing, libgit2, ...).
		commits, err := newCommits(repoDir, u)
		if err != nil {
			return nil, fmt.Errorf("list commits for %s: %w", u.Ref, err)
		}
		for _, c := range commits {
			if ok, reason := verifyCommit(repoDir, c, cfg.AllowedSigners); !ok {
				vs = append(vs, Violation{
					Ref:    u.Ref,
					Rule:   "unsigned-or-untrusted-commit",
					Detail: fmt.Sprintf("commit %s is not signed by an allowed signer (%s)", shortSHA(c), reason),
				})
			}
		}
	}
	return vs, nil
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
// For a branch creation it excludes commits reachable from existing refs (so old
// history isn't re-verified); otherwise it walks old..new.
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

// verifyCommit reports whether sha has a good signature from a signer listed in
// allowedSigners. git does the cryptographic verification.
func verifyCommit(repoDir, sha, allowedSigners string) (ok bool, reason string) {
	args := []string{"-C", repoDir}
	if allowedSigners != "" {
		args = append(args, "-c", "gpg.ssh.allowedSignersFile="+allowedSigners)
	}
	args = append(args, "verify-commit", sha)
	cmd := exec.Command("git", args...)
	var errb strings.Builder
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if r := strings.TrimSpace(firstLine(errb.String())); r != "" {
			return false, r
		}
		return false, "no valid signature"
	}
	return true, ""
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

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
