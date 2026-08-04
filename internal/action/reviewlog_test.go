package action

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendReviewAndLastReview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "reviews.jsonl")

	if err := AppendReview(path, ReviewRecord{PR: 1, HeadSHA: "aaa", Fingerprint: "SHA256:x", Role: "reviewer", Event: "comment"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendReview(path, ReviewRecord{PR: 1, HeadSHA: "aaa", Fingerprint: "SHA256:x", Role: "reviewer", Event: "approve", Threads: []string{"T1", "T2"}}); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if fi2, err := os.Stat(filepath.Dir(path)); err != nil || fi2.Mode().Perm()&0o700 != 0o700 {
		t.Fatalf("parent dir mode wrong: %v %v", fi2, err)
	}

	rec, ok, err := LastReview(path, 1, "aaa")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || rec.Event != "approve" {
		t.Fatalf("last-wins record = %+v ok=%v, want the second (approve) record", rec, ok)
	}
	if rec.Time == "" {
		t.Fatal("AppendReview must fill Time when empty")
	}

	// A different head SHA (a new push) must not match — the record is
	// invalidated by construction.
	if _, ok, err := LastReview(path, 1, "bbb"); err != nil || ok {
		t.Fatalf("a different head sha must not match: ok=%v err=%v", ok, err)
	}
	// A different PR must not match either.
	if _, ok, err := LastReview(path, 2, "aaa"); err != nil || ok {
		t.Fatalf("a different PR must not match: ok=%v err=%v", ok, err)
	}
}

func TestLastReviewAbsent(t *testing.T) {
	if _, ok, err := LastReview("", 1, "aaa"); err != nil || ok {
		t.Fatalf("empty path: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	if _, ok, err := LastReview(filepath.Join(t.TempDir(), "no-such-file.jsonl"), 1, "aaa"); err != nil || ok {
		t.Fatalf("missing file: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestAppendReviewDisabled(t *testing.T) {
	if err := AppendReview("", ReviewRecord{PR: 1}); err != nil {
		t.Fatalf("empty path should be a no-op, got %v", err)
	}
}

func TestLastReviewMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LastReview(path, 1, "aaa"); err == nil {
		t.Fatal("a malformed line must be a hard error, not silently skipped")
	}
}

func TestInternalApproval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.jsonl")
	actionRoles := map[string][]string{"review": {"reviewer", "owner"}}

	// No record yet: not approved.
	ok, err := InternalApproval(path, 1, "aaa", actionRoles)
	if err != nil || ok {
		t.Fatalf("no record: ok=%v err=%v", ok, err)
	}

	// A comment (not approve) must not count as approval.
	if err := AppendReview(path, ReviewRecord{PR: 1, HeadSHA: "aaa", Role: "reviewer", Event: "comment"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := InternalApproval(path, 1, "aaa", actionRoles); err != nil || ok {
		t.Fatalf("comment event: ok=%v err=%v, want false", ok, err)
	}

	// An approve from a role NOT allowed to review must not count.
	if err := AppendReview(path, ReviewRecord{PR: 1, HeadSHA: "aaa", Role: "implementer", Event: "approve"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := InternalApproval(path, 1, "aaa", actionRoles); err != nil || ok {
		t.Fatalf("approve from a disallowed role: ok=%v err=%v, want false", ok, err)
	}

	// An approve from an allowed role counts.
	if err := AppendReview(path, ReviewRecord{PR: 1, HeadSHA: "aaa", Role: "reviewer", Event: "approve"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := InternalApproval(path, 1, "aaa", actionRoles); err != nil || !ok {
		t.Fatalf("approve from an allowed role: ok=%v err=%v, want true", ok, err)
	}

	// A stale head (the PR moved on): the old approval no longer matches.
	if ok, err := InternalApproval(path, 1, "ccc-new-head", actionRoles); err != nil || ok {
		t.Fatalf("stale head must not be approved: ok=%v err=%v", ok, err)
	}

	// Last-wins: a later request-changes from the same allowed role
	// supersedes the earlier approve for the SAME head.
	if err := AppendReview(path, ReviewRecord{PR: 1, HeadSHA: "aaa", Role: "reviewer", Event: "request-changes"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := InternalApproval(path, 1, "aaa", actionRoles); err != nil || ok {
		t.Fatalf("last-wins request-changes should un-approve: ok=%v err=%v", ok, err)
	}
}

func TestGateThreadIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.jsonl")
	if ids, err := GateThreadIDs(path, 1); err != nil || len(ids) != 0 {
		t.Fatalf("no file: ids=%v err=%v", ids, err)
	}
	if err := AppendReview(path, ReviewRecord{PR: 1, HeadSHA: "aaa", Threads: []string{"T1", "T2"}}); err != nil {
		t.Fatal(err)
	}
	if err := AppendReview(path, ReviewRecord{PR: 1, HeadSHA: "bbb", Threads: []string{"T2", "T3"}}); err != nil {
		t.Fatal(err)
	}
	if err := AppendReview(path, ReviewRecord{PR: 2, HeadSHA: "aaa", Threads: []string{"T9"}}); err != nil {
		t.Fatal(err)
	}
	ids, err := GateThreadIDs(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "T1,T2,T3" {
		t.Fatalf("ids = %v, want the union across every head for PR 1, deduplicated: [T1 T2 T3]", ids)
	}
	// A different PR's threads must never leak in.
	for _, id := range ids {
		if id == "T9" {
			t.Fatal("PR 2's thread leaked into PR 1's gate thread ids")
		}
	}
}
