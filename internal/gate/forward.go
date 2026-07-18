package gate

import (
	"fmt"
	"strings"

	"github.com/dmitriyb/portitor/internal/git"
)

// ForwardConfig controls post-receive forwarding to the real upstream.
type ForwardConfig struct {
	// UpstreamRemote is the git remote (configured in the receiving repo) that
	// accepted refs are forwarded to. Defaults to "upstream".
	UpstreamRemote string
	// DefaultBranch is never forwarded. If empty, derived from the repo HEAD.
	DefaultBranch string
}

// ForwardStatus classifies the outcome of one ref update.
type ForwardStatus string

const (
	// StatusForwarded — the ref was pushed to upstream.
	StatusForwarded ForwardStatus = "forwarded"
	// StatusAlreadyUpstream — the push was rejected but upstream already
	// contains the new tip (a later, containing push landed first). Success:
	// the content the update carried is upstream.
	StatusAlreadyUpstream ForwardStatus = "already-upstream"
	// StatusSkippedDefault — the ref is (now) the default branch, never
	// forwarded. Reported so a default-branch change between pre- and
	// post-receive does not silently drop an accepted ref.
	StatusSkippedDefault ForwardStatus = "skipped-default"
	// StatusSkippedNonBranch — a non-refs/heads ref (the gate refuses these;
	// defense in depth).
	StatusSkippedNonBranch ForwardStatus = "skipped-non-branch"
	// StatusSkippedDeletion — a deletion (not forwarded in this version).
	StatusSkippedDeletion ForwardStatus = "skipped-deletion"
	// StatusFailed — the push failed and upstream does not contain the tip.
	StatusFailed ForwardStatus = "failed"
)

// ForwardResult is the outcome of forwarding one ref.
type ForwardResult struct {
	Ref    string
	Status ForwardStatus
	Err    error
	Output string
}

// Forward pushes each accepted, non-default, non-deletion branch ref to the
// configured upstream remote, using credentials available to the proxy. It runs
// from post-receive — after pre-receive has accepted the push — so the refs
// already exist in the receiving repo.
//
// Every update yields a result with a status (nothing is silently dropped): a
// ref that is now the default branch, a non-branch ref, or a deletion is
// reported as skipped; a push rejected because upstream already contains the
// tip (out-of-order forwarding) is success.
func Forward(repoDir string, updates []RefUpdate, cfg ForwardConfig) ([]ForwardResult, error) {
	def := cfg.DefaultBranch
	if def == "" {
		d, err := defaultBranch(repoDir)
		if err != nil {
			return nil, fmt.Errorf("determine default branch: %w", err)
		}
		def = d
	}
	defRef := "refs/heads/" + def
	remote := cfg.UpstreamRemote
	if remote == "" {
		remote = "upstream"
	}
	if !git.ValidRemoteName(remote) {
		return nil, fmt.Errorf("invalid upstream remote name %q", remote)
	}

	var results []ForwardResult
	for _, u := range updates {
		if branch, ok := strings.CutPrefix(u.Ref, "refs/heads/"); !ok || branch == "" {
			results = append(results, ForwardResult{Ref: u.Ref, Status: StatusSkippedNonBranch})
			continue
		}
		if u.IsDelete() {
			results = append(results, ForwardResult{Ref: u.Ref, Status: StatusSkippedDeletion})
			continue
		}
		if u.Ref == defRef {
			results = append(results, ForwardResult{Ref: u.Ref, Status: StatusSkippedDefault})
			continue
		}
		if !ValidSHA(u.NewSHA) {
			results = append(results, ForwardResult{Ref: u.Ref, Status: StatusFailed, Err: fmt.Errorf("malformed new object id %q", u.NewSHA)})
			continue
		}
		results = append(results, forwardOne(repoDir, remote, u))
	}
	return results, nil
}

// forwardOne pushes a single ref, resolving a rejected push against whether the
// upstream already contains the tip.
func forwardOne(repoDir, remote string, u RefUpdate) ForwardResult {
	out, err := git.OutputNetwork(repoDir, "push", "--", remote, u.NewSHA+":"+u.Ref)
	if err == nil {
		return ForwardResult{Ref: u.Ref, Status: StatusForwarded, Output: strings.TrimSpace(out)}
	}
	// A rejected push may just mean a later push already carried this tip
	// upstream (out-of-order forwarding). If upstream's branch contains NewSHA,
	// the content landed — report success rather than a spurious failure.
	if contained, cerr := upstreamContains(repoDir, remote, u.Ref, u.NewSHA); cerr == nil && contained {
		return ForwardResult{Ref: u.Ref, Status: StatusAlreadyUpstream, Output: strings.TrimSpace(out)}
	}
	return ForwardResult{Ref: u.Ref, Status: StatusFailed, Err: err, Output: strings.TrimSpace(out)}
}

// upstreamContains reports whether the upstream branch's current tip already
// contains newSHA (newSHA is an ancestor of, or equal to, the remote tip). The
// remote tip object is present locally whenever it arrived via a push this repo
// received, which is exactly the out-of-order-forwarding case.
func upstreamContains(repoDir, remote, ref, newSHA string) (bool, error) {
	remoteTip, err := upstreamRefTip(repoDir, remote, ref)
	if err != nil {
		return false, err
	}
	if remoteTip == "" {
		return false, nil // upstream doesn't have the branch
	}
	if remoteTip == newSHA {
		return true, nil
	}
	// merge-base --is-ancestor needs both objects locally; the remote tip is
	// local iff it came through a push we received. If it isn't, we cannot
	// prove containment — fail closed (report not-contained → the push failure
	// stands).
	_, err = git.OutputHermetic(repoDir, "rev-parse", "--verify", "--quiet", remoteTip+"^{commit}")
	if err != nil {
		return false, nil
	}
	anc, err := isAncestor(repoDir, newSHA, remoteTip)
	if err != nil {
		return false, err
	}
	return anc, nil
}

// upstreamRefTip returns the upstream branch's current tip SHA, or "" if the
// branch does not exist upstream. The ref is matched **exactly**: ls-remote
// patterns suffix-match (so `refs/heads/x` also lists a decoy
// `refs/other/refs/heads/x`), and the output is refname-sorted — using the
// first line's SHA blindly would let a decoy fake containment (a dropped real
// forward failure) or mask a stranded branch. Only the line whose refname
// equals ref counts.
func upstreamRefTip(repoDir, remote, ref string) (string, error) {
	out, err := git.OutputNetwork(repoDir, "ls-remote", "--", remote, ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && ValidSHA(f[0]) && f[1] == ref {
			return f[0], nil
		}
	}
	return "", nil
}

// LocalBranches lists the repo's local branch refs (refs/heads/*) with their
// tips — the reconcile path's source of what should exist upstream.
func LocalBranches(repoDir string) (map[string]string, error) {
	out, err := git.OutputHermetic(repoDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	branches := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && ValidSHA(f[1]) {
			branches[f[0]] = f[1]
		}
	}
	return branches, nil
}

// Reconcile re-forwards every local non-default branch to upstream that is not
// already contained there — the recovery path after an upstream-forward
// failure (a re-push cannot re-trigger post-receive, so the operator runs
// reconcile). Idempotent: a branch already upstream is reported already-upstream.
func Reconcile(repoDir string, cfg ForwardConfig) ([]ForwardResult, error) {
	def := cfg.DefaultBranch
	if def == "" {
		d, err := defaultBranch(repoDir)
		if err != nil {
			return nil, fmt.Errorf("determine default branch: %w", err)
		}
		def = d
	}
	remote := cfg.UpstreamRemote
	if remote == "" {
		remote = "upstream"
	}
	if !git.ValidRemoteName(remote) {
		return nil, fmt.Errorf("invalid upstream remote name %q", remote)
	}
	branches, err := LocalBranches(repoDir)
	if err != nil {
		return nil, err
	}
	defRef := "refs/heads/" + def

	var results []ForwardResult
	for ref, tip := range branches {
		if ref == defRef {
			continue // the default is upstream/owner territory
		}
		remoteTip, err := upstreamRefTip(repoDir, remote, ref)
		if err != nil {
			results = append(results, ForwardResult{Ref: ref, Status: StatusFailed, Err: err})
			continue
		}
		if remoteTip == tip {
			results = append(results, ForwardResult{Ref: ref, Status: StatusAlreadyUpstream})
			continue
		}
		results = append(results, forwardOne(repoDir, remote, RefUpdate{NewSHA: tip, Ref: ref}))
	}
	return results, nil
}
