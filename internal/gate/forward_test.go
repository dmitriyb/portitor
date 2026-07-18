package gate

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestForward verifies post-receive forwarding: accepted feature branches are
// pushed to the configured upstream remote; the default branch is not.
func TestForward(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	e := newTestEnv(t)

	// An "upstream" bare repo, wired as remote "upstream" on the receiving bare.
	upstream := filepath.Join(e.dir, "upstream.git")
	mustGit(t, e.dir, "init", "--bare", "--initial-branch=main", upstream)
	mustGit(t, e.bare, "remote", "add", "upstream", upstream)

	base := e.commitFile("README.md", "base")
	e.push("main")
	e.checkout("-b", "feature")
	feat := e.commitFile("a.txt", "a")
	e.push("feature")

	cfg := ForwardConfig{UpstreamRemote: "upstream", DefaultBranch: "main"}

	t.Run("feature branch forwarded", func(t *testing.T) {
		results, err := Forward(e.bare, []RefUpdate{{OldSHA: base, NewSHA: feat, Ref: "refs/heads/feature"}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].Err != nil {
			t.Fatalf("forward results = %+v", results)
		}
		got := strings.TrimSpace(mustGitOut(t, upstream, "rev-parse", "refs/heads/feature"))
		if got != feat {
			t.Fatalf("upstream feature = %s, want %s", got, feat)
		}
	})

	t.Run("non-branch ref not forwarded", func(t *testing.T) {
		results, err := Forward(e.bare, []RefUpdate{{OldSHA: base, NewSHA: feat, Ref: "refs/tags/v1"}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 0 {
			t.Fatalf("expected no forwards for a tag ref, got %+v", results)
		}
	})

	t.Run("invalid remote name refused", func(t *testing.T) {
		bad := ForwardConfig{UpstreamRemote: "--force", DefaultBranch: "main"}
		if _, err := Forward(e.bare, []RefUpdate{{OldSHA: base, NewSHA: feat, Ref: "refs/heads/feature"}}, bad); err == nil {
			t.Fatal("expected an error for a remote name shaped like an option")
		}
	})

	t.Run("default branch not forwarded", func(t *testing.T) {
		results, err := Forward(e.bare, []RefUpdate{{OldSHA: base, NewSHA: feat, Ref: "refs/heads/main"}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 0 {
			t.Fatalf("expected no forwards for the default branch, got %+v", results)
		}
		exists, err := refExists(upstream, "refs/heads/main")
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("upstream main should not have been forwarded")
		}
	})
}
