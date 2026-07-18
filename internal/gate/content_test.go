package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/rules"
)

// structuralRules: delete/rename of anything under registry/ requires
// reviewer/owner.
func structuralRules() *rules.ContentRules {
	return &rules.ContentRules{
		Version: 1,
		Structural: &rules.StructuralRules{Rules: []rules.StructuralRule{{
			Name:       "registry-ops-review-only",
			Paths:      []string{"registry/**"},
			Operations: []string{"delete", "rename"},
			Roles:      &rules.RolePredicate{NotIn: []string{"reviewer", "owner"}},
			Effect:     "deny",
		}}},
	}
}

// TestStructuralRules covers delete/rename gating with rename double-visibility
// (renames into AND out of the protected path both trip the rule).
func TestStructuralRules(t *testing.T) {
	e := newContentEnv(t, structuralRules())

	e.identity("impl@example.com", e.implKey)
	e.write("registry/records.json", "{}")
	e.write("free.txt", "x")
	base := e.commitAll("base")
	mustGit(t, e.work, "push", "origin", "main")

	branch := func(name string, mutate func()) string {
		t.Helper()
		mustGit(t, e.work, "checkout", "main")
		mustGit(t, e.work, "checkout", "-b", name)
		mutate()
		mustGit(t, e.work, "add", "-A")
		mustGit(t, e.work, "commit", "-m", name)
		tip := e.head()
		mustGit(t, e.work, "push", "origin", name)
		return tip
	}

	implDelete := branch("impl-delete", func() {
		e.identity("impl@example.com", e.implKey)
		mustGit(t, e.work, "rm", "-q", "registry/records.json")
	})
	implRenameAway := branch("impl-rename-away", func() {
		e.identity("impl@example.com", e.implKey)
		mustGit(t, e.work, "mv", "registry/records.json", "elsewhere.json")
	})
	implRenameIn := branch("impl-rename-in", func() {
		e.identity("impl@example.com", e.implKey)
		mustGit(t, e.work, "mv", "free.txt", "registry/free.txt")
	})
	implModify := branch("impl-modify", func() {
		e.identity("impl@example.com", e.implKey)
		e.write("registry/records.json", "{\"changed\":true}")
	})
	revDelete := branch("rev-delete", func() {
		e.identity("reviewer@example.com", e.revKey)
		mustGit(t, e.work, "rm", "-q", "registry/records.json")
	})

	tests := []struct {
		name string
		u    RefUpdate
		want []string
	}{
		{"implementer delete rejected", RefUpdate{base, implDelete, "refs/heads/impl-delete"}, []string{"registry-ops-review-only"}},
		{"implementer rename away rejected", RefUpdate{base, implRenameAway, "refs/heads/impl-rename-away"}, []string{"registry-ops-review-only"}},
		{"implementer rename into rejected", RefUpdate{base, implRenameIn, "refs/heads/impl-rename-in"}, []string{"registry-ops-review-only"}},
		{"implementer modify allowed (op not gated)", RefUpdate{base, implModify, "refs/heads/impl-modify"}, nil},
		{"reviewer delete allowed", RefUpdate{base, revDelete, "refs/heads/rev-delete"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs, err := Check(e.bare, []RefUpdate{tc.u}, e.cfg)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			assertRules(t, vs, tc.want)
		})
	}
}

// TestSemanticBornAtGatedValue: a record ADDED already at the gated value trips
// the arrival gate (the delete-and-re-add / born-approved evasion is closed).
func TestSemanticBornAtGatedValue(t *testing.T) {
	e := newContentEnv(t, semanticGateRules())

	e.identity("impl@example.com", e.implKey)
	e.write("registry/records.json", `{"records":[]}`)
	base := e.commitAll("empty registry")
	mustGit(t, e.work, "push", "origin", "main")

	mustGit(t, e.work, "checkout", "-b", "impl-born-approved")
	e.write("registry/records.json", `{"records":[{"id":"r-9","stage":"approved"}]}`)
	tip := e.commitAll("add approved record")
	mustGit(t, e.work, "push", "origin", "impl-born-approved")

	vs, err := Check(e.bare, []RefUpdate{{OldSHA: base, NewSHA: tip, Ref: "refs/heads/impl-born-approved"}}, e.cfg)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertRules(t, vs, []string{"approval-requires-review"})
}

// TestSemanticCheckFailure covers the two fail-closed classes: a check command
// that runs and rejects the content is a violation; a command that cannot run
// at all is an operational error. Both directions reject the push.
func TestSemanticCheckFailure(t *testing.T) {
	requireBins(t, "sh")

	t.Run("command rejects content -> violation", func(t *testing.T) {
		scriptDir := t.TempDir()
		reject := filepath.Join(scriptDir, "reject.sh")
		if err := os.WriteFile(reject, []byte("#!/bin/sh\necho content refused >&2\nexit 3\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		content := semanticGateRules()
		content.Semantic.Files[0].Check = rules.CheckDef{Command: []string{reject}, RecordsPath: "records"}
		e := newContentEnv(t, content)

		e.identity("impl@example.com", e.implKey)
		e.write("registry/records.json", "whatever")
		base := e.commitAll("base")
		mustGit(t, e.work, "push", "origin", "main")
		mustGit(t, e.work, "checkout", "-b", "feat")
		e.write("registry/records.json", "changed")
		tip := e.commitAll("change")
		mustGit(t, e.work, "push", "origin", "feat")

		vs, err := Check(e.bare, []RefUpdate{{OldSHA: base, NewSHA: tip, Ref: "refs/heads/feat"}}, e.cfg)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		assertRules(t, vs, []string{"semantic-check-failed"})
		if !strings.Contains(vs[0].Detail, "content refused") {
			t.Fatalf("violation detail should carry the command's stderr excerpt: %q", vs[0].Detail)
		}
	})

	t.Run("command not runnable -> operational error", func(t *testing.T) {
		content := semanticGateRules()
		content.Semantic.Files[0].Check = rules.CheckDef{Command: []string{"/nonexistent/check-tool"}, RecordsPath: "records"}
		e := newContentEnv(t, content)

		e.identity("impl@example.com", e.implKey)
		e.write("registry/records.json", `{"records":[]}`)
		base := e.commitAll("base")
		mustGit(t, e.work, "push", "origin", "main")
		mustGit(t, e.work, "checkout", "-b", "feat")
		e.write("registry/records.json", `{"records":[{"id":"r-1","stage":"draft"}]}`)
		tip := e.commitAll("change")
		mustGit(t, e.work, "push", "origin", "feat")

		if _, err := Check(e.bare, []RefUpdate{{OldSHA: base, NewSHA: tip, Ref: "refs/heads/feat"}}, e.cfg); err == nil {
			t.Fatal("an unrunnable check command must be an operational error (fail-closed)")
		}
	})
}

// TestStructuralMergeRenameEvasion: a crafted merge that deletes a protected
// file while the first-parent diff relabels it as a rename must still be gated
// (the combined-diff introduced set retains the delete-side event). Regression
// for the merge+rename structural evasion.
func TestStructuralMergeRenameEvasion(t *testing.T) {
	e := newContentEnv(t, structuralRules()) // deny delete/rename under registry/ for non-reviewers

	e.identity("impl@example.com", e.implKey)
	e.write("registry/records.json", "secret")
	e.write("keep.txt", "k")
	base := e.commitAll("base")
	mustGit(t, e.work, "push", "origin", "main")

	// Side branch: add an identical-content file elsewhere (primes rename
	// detection), delete nothing.
	mustGit(t, e.work, "checkout", "-qb", "side")
	e.write("elsewhere.json", "secret")
	e.commitAll("add elsewhere")

	// Merge side into a feature branch off base, and delete the protected file
	// in the merge commit itself.
	mustGit(t, e.work, "checkout", "-q", "main")
	mustGit(t, e.work, "checkout", "-qb", "feature")
	mustGit(t, e.work, "merge", "-q", "--no-ff", "--no-commit", "side")
	mustGit(t, e.work, "rm", "-q", "registry/records.json")
	mergeTip := e.commitAll("evil merge deletes protected file")
	mustGit(t, e.work, "push", "origin", "feature")

	vs, err := Check(e.bare, []RefUpdate{{OldSHA: base, NewSHA: mergeTip, Ref: "refs/heads/feature"}}, e.cfg)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertRules(t, vs, []string{"registry-ops-review-only"})
}

// TestSemanticCleanMerge: a merge whose protected-file blob equals a parent's
// introduces nothing — the merge signer is not charged for transitions that
// arrived through an already-gated parent line.
func TestSemanticCleanMerge(t *testing.T) {
	e := newContentEnv(t, semanticGateRules())

	const draft = `{"records":[{"id":"r-1","stage":"draft"}]}`
	const approved = `{"records":[{"id":"r-1","stage":"approved"}]}`

	e.identity("impl@example.com", e.implKey)
	e.write("registry/records.json", draft)
	e.commitAll("draft")
	mustGit(t, e.work, "push", "origin", "main")

	// Reviewer approves on a feature branch (legitimately).
	mustGit(t, e.work, "checkout", "-b", "feat")
	e.identity("reviewer@example.com", e.revKey)
	e.write("registry/records.json", approved)
	featTip := e.commitAll("approve")
	mustGit(t, e.work, "push", "origin", "feat")

	// Main moves on with an unrelated change.
	mustGit(t, e.work, "checkout", "main")
	e.identity("impl@example.com", e.implKey)
	e.write("other.txt", "x")
	e.commitAll("unrelated")
	mustGit(t, e.work, "push", "origin", "main")

	// Implementer merges main INTO feat: the protected blob equals feat's tip
	// (first parent) — nothing introduced, no violation for the implementer.
	mustGit(t, e.work, "checkout", "feat")
	mustGit(t, e.work, "merge", "--no-ff", "-m", "merge main", "main")
	mergeTip := e.head()
	mustGit(t, e.work, "push", "origin", "feat")

	vs, err := Check(e.bare, []RefUpdate{{OldSHA: featTip, NewSHA: mergeTip, Ref: "refs/heads/feat"}}, e.cfg)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertRules(t, vs, nil)
}
