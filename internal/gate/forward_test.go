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
		if len(results) != 1 || results[0].Status != StatusForwarded || results[0].Err != nil {
			t.Fatalf("forward results = %+v", results)
		}
		got := strings.TrimSpace(mustGitOut(t, upstream, "rev-parse", "refs/heads/feature"))
		if got != feat {
			t.Fatalf("upstream feature = %s, want %s", got, feat)
		}
	})

	t.Run("non-branch ref reported skipped", func(t *testing.T) {
		results, err := Forward(e.bare, []RefUpdate{{OldSHA: base, NewSHA: feat, Ref: "refs/tags/v1"}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].Status != StatusSkippedNonBranch {
			t.Fatalf("expected skipped-non-branch, got %+v", results)
		}
	})

	t.Run("invalid remote name refused", func(t *testing.T) {
		bad := ForwardConfig{UpstreamRemote: "--force", DefaultBranch: "main"}
		if _, err := Forward(e.bare, []RefUpdate{{OldSHA: base, NewSHA: feat, Ref: "refs/heads/feature"}}, bad); err == nil {
			t.Fatal("expected an error for a remote name shaped like an option")
		}
	})

	t.Run("default branch reported skipped", func(t *testing.T) {
		results, err := Forward(e.bare, []RefUpdate{{OldSHA: base, NewSHA: feat, Ref: "refs/heads/main"}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].Status != StatusSkippedDefault {
			t.Fatalf("expected skipped-default (never silently dropped), got %+v", results)
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

// TestForwardOutOfOrder (PORT-9a): forwarding an older tip after a newer,
// containing tip already landed upstream is success (already-upstream), not a
// spurious failure — the content the older update carried is upstream.
func TestForwardOutOfOrder(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	e := newTestEnv(t)
	upstream := filepath.Join(e.dir, "upstream.git")
	mustGit(t, e.dir, "init", "--bare", "--initial-branch=main", upstream)
	mustGit(t, e.bare, "remote", "add", "upstream", upstream)

	base := e.commitFile("README.md", "base")
	e.push("main")
	e.checkout("-b", "feature")
	c1 := e.commitFile("a.txt", "a1")
	e.push("feature")
	c2 := e.commitFile("b.txt", "b2") // c2 contains c1
	e.push("feature")

	cfg := ForwardConfig{UpstreamRemote: "upstream", DefaultBranch: "main"}

	// The newer, containing push (c2) forwards first.
	if r, err := Forward(e.bare, []RefUpdate{{OldSHA: c1, NewSHA: c2, Ref: "refs/heads/feature"}}, cfg); err != nil || r[0].Status != StatusForwarded {
		t.Fatalf("c2 forward: %+v err=%v", r, err)
	}
	// The older push (base..c1) now arrives; upstream already contains c1.
	r, err := Forward(e.bare, []RefUpdate{{OldSHA: base, NewSHA: c1, Ref: "refs/heads/feature"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 1 || r[0].Status != StatusAlreadyUpstream {
		t.Fatalf("out-of-order forward should be already-upstream, got %+v", r)
	}
}

// TestUpstreamRefTipExactMatch (PORT-9a hardening): ls-remote suffix-matches,
// so a decoy ref like refs/backup/refs/heads/feature (sorting before
// refs/heads/) must NOT be read as the branch's tip — else a diverged upstream
// could fake containment and drop a real forward failure.
func TestUpstreamRefTipExactMatch(t *testing.T) {
	requireBins(t, "git")
	e := newTestEnv(t)
	upstream := filepath.Join(e.dir, "upstream.git")
	mustGit(t, e.dir, "init", "--bare", "--initial-branch=main", upstream)
	mustGit(t, e.bare, "remote", "add", "upstream", upstream)

	e.commitFile("README.md", "base")
	e.push("main")
	e.checkout("-b", "feature")
	real := e.commitFile("a.txt", "real")
	e.push("feature")
	decoy := e.commitFile("b.txt", "decoy")
	e.push("feature")

	// Upstream carries the REAL tip at refs/heads/feature and the decoy tip at
	// a suffix-matching decoy namespace that sorts before "heads".
	mustGit(t, e.bare, "push", "upstream", real+":refs/heads/feature")
	mustGit(t, e.bare, "push", "upstream", decoy+":refs/backup/refs/heads/feature")

	tip, err := upstreamRefTip(e.bare, "upstream", "refs/heads/feature")
	if err != nil {
		t.Fatal(err)
	}
	if tip != real {
		t.Fatalf("upstreamRefTip returned %s, want the real branch tip %s (decoy leaked)", tip, real)
	}
}

// TestReconcile (L-P2g): a branch that never reached upstream is forwarded by
// reconcile; one already upstream is a no-op.
func TestReconcile(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	e := newTestEnv(t)
	upstream := filepath.Join(e.dir, "upstream.git")
	mustGit(t, e.dir, "init", "--bare", "--initial-branch=main", upstream)
	mustGit(t, e.bare, "remote", "add", "upstream", upstream)

	e.commitFile("README.md", "base")
	e.push("main")
	e.checkout("-b", "landed")
	landed := e.commitFile("a.txt", "a")
	e.push("landed")
	e.checkout("main")
	e.checkout("-b", "stranded")
	e.commitFile("b.txt", "b")
	e.push("stranded")

	cfg := ForwardConfig{UpstreamRemote: "upstream", DefaultBranch: "main"}
	// "landed" reaches upstream; "stranded" does not (simulating a forward failure).
	mustGit(t, e.bare, "push", "upstream", landed+":refs/heads/landed")

	results, err := Reconcile(e.bare, cfg)
	if err != nil {
		t.Fatal(err)
	}
	byRef := map[string]ForwardStatus{}
	for _, r := range results {
		byRef[r.Ref] = r.Status
	}
	if byRef["refs/heads/landed"] != StatusAlreadyUpstream {
		t.Fatalf("landed should be already-upstream, got %q", byRef["refs/heads/landed"])
	}
	if byRef["refs/heads/stranded"] != StatusForwarded {
		t.Fatalf("stranded should be forwarded, got %q", byRef["refs/heads/stranded"])
	}
	if exists, _ := refExists(upstream, "refs/heads/stranded"); !exists {
		t.Fatal("stranded should now exist upstream")
	}
}
