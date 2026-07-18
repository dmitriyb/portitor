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

// ForwardResult is the outcome of forwarding one ref.
type ForwardResult struct {
	Ref    string
	Err    error
	Output string
}

// Forward pushes each accepted, non-default, non-deletion ref update to the
// configured upstream remote, using credentials available to the proxy. It runs
// from post-receive — after pre-receive has accepted the push — so the refs
// already exist in the receiving repo.
//
// The default branch is never forwarded (it is PR/owner territory; pre-receive
// rejects pushes to it anyway). Deletions are not forwarded in this version.
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
		// Only branch refs are ever forwarded. The gate refuses other namespaces
		// before post-receive can see them; this is defense in depth.
		if branch, ok := strings.CutPrefix(u.Ref, "refs/heads/"); !ok || branch == "" {
			continue
		}
		if u.Ref == defRef || u.IsDelete() {
			continue
		}
		// The refspec is built only from validated parts: a 40-hex SHA and a
		// refs/heads/-prefixed ref (checked above) can never read as options.
		if !ValidSHA(u.NewSHA) {
			results = append(results, ForwardResult{Ref: u.Ref, Err: fmt.Errorf("malformed new object id %q", u.NewSHA)})
			continue
		}
		out, err := git.OutputNetwork(repoDir, "push", "--", remote, u.NewSHA+":"+u.Ref)
		results = append(results, ForwardResult{Ref: u.Ref, Err: err, Output: strings.TrimSpace(out)})
	}
	return results, nil
}
