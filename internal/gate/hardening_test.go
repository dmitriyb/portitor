package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRefNamespace verifies that only refs/heads/* updates are evaluated; every
// other namespace — tags, notes, and especially refs/replace/* — is refused
// outright (creates, updates, and deletions alike).
func TestRefNamespace(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	e := newTestEnv(t)

	base := e.commitFile("README.md", "base")
	e.push("main")
	e.checkout("-b", "feature")
	feat := e.commitFile("a.txt", "a")
	e.push("feature")

	cfg := Config{AllowedSigners: e.allowedSigners}

	tests := []struct {
		name   string
		update RefUpdate
	}{
		{"tag create", RefUpdate{OldSHA: zeroSHA, NewSHA: feat, Ref: "refs/tags/v1"}},
		{"replace ref create", RefUpdate{OldSHA: zeroSHA, NewSHA: feat, Ref: "refs/replace/" + base}},
		{"notes update", RefUpdate{OldSHA: base, NewSHA: feat, Ref: "refs/notes/commits"}},
		{"tag delete", RefUpdate{OldSHA: feat, NewSHA: zeroSHA, Ref: "refs/tags/v1"}},
		{"bare refs/heads/", RefUpdate{OldSHA: base, NewSHA: feat, Ref: "refs/heads/"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs, err := Check(e.bare, []RefUpdate{tc.update}, cfg)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			assertRules(t, vs, []string{"ref-namespace"})
		})
	}
}

// TestReplaceObjectsIgnored pins the deterministic-environment invariant: a
// refs/replace/* ref present in the receiving repo must not substitute objects
// during verification. Here an unsigned commit carries a replace ref pointing at
// a signed one; with substitution active, rev-list would see the introduced
// commit as already-present history (zero commits checked) and the push would
// be accepted unchecked.
func TestReplaceObjectsIgnored(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	e := newTestEnv(t)

	base := e.commitFile("README.md", "base")
	e.push("main")
	e.checkout("-b", "feature")
	featUnsigned := e.commitFile("a.txt", "a", "--no-gpg-sign")
	e.push("feature")

	// Map the unsigned commit to the signed base via a replace ref on the bare side.
	mustGit(t, e.bare, "replace", featUnsigned, base)

	cfg := Config{AllowedSigners: e.allowedSigners}
	vs, err := Check(e.bare, []RefUpdate{{OldSHA: base, NewSHA: featUnsigned, Ref: "refs/heads/feature"}}, cfg)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertRules(t, vs, []string{"unsigned-or-untrusted-commit"})
}

// TestPoisonedGlobalConfig pins the determinism property (§5.7 of the review):
// Check's verdict must be identical whether or not an ambient global git config
// supplies a trust root. A signer outside the configured allowed_signers must be
// rejected even when a poisoned ~/.gitconfig-style file trusts it — and an empty
// AllowedSigners must distrust everyone rather than fall back to ambient config.
func TestPoisonedGlobalConfig(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	e := newTestEnv(t)

	base := e.commitFile("README.md", "base")
	e.push("main")

	// A commit signed by a key NOT in the gate's allowed_signers.
	wrongKey := filepath.Join(e.dir, "wrong_ed25519")
	genKey(t, wrongKey, "evil@example.com")
	e.checkout("-b", "feat-wrongkey")
	e.signWith(wrongKey)
	featWrong := e.commitFile("c.txt", "c")
	e.signWith(e.goodKey)
	e.push("feat-wrongkey")

	// A poisoned "global" config whose allowed_signers trusts the wrong key.
	wrongSigners := writeAllowedSigners(t, filepath.Join(e.dir, "wrong_signers"), "evil@example.com", wrongKey+".pub")
	poison := filepath.Join(e.dir, "poisoned-gitconfig")
	if err := os.WriteFile(poison, []byte("[gpg \"ssh\"]\n\tallowedSignersFile = "+wrongSigners+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", poison)

	t.Run("untrusted signer still rejected", func(t *testing.T) {
		cfg := Config{AllowedSigners: e.allowedSigners}
		vs, err := Check(e.bare, []RefUpdate{{OldSHA: base, NewSHA: featWrong, Ref: "refs/heads/feat-wrongkey"}}, cfg)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		assertRules(t, vs, []string{"unsigned-or-untrusted-commit"})
	})

	t.Run("empty allowed_signers distrusts everyone", func(t *testing.T) {
		// Even a good-key commit must be rejected when no trust root is
		// configured — never silently accepted via the ambient config.
		e.checkout("main")
		e.checkout("-b", "feat-good")
		featGood := e.commitFile("d.txt", "d")
		e.push("feat-good")

		cfg := Config{AllowedSigners: ""}
		vs, err := Check(e.bare, []RefUpdate{{OldSHA: base, NewSHA: featGood, Ref: "refs/heads/feat-good"}}, cfg)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		assertRules(t, vs, []string{"unsigned-or-untrusted-commit"})
	})
}

// TestUntrustedSignerFingerprintInMessage (L-P3c): a commit signed by a key not
// in allowed_signers (git verdict "U") is rejected with the signer's fingerprint
// in the detail — the value the operator needs to fix the grant.
func TestUntrustedSignerFingerprintInMessage(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	e := newTestEnv(t)

	base := e.commitFile("README.md", "base")
	e.push("main")

	wrongKey := filepath.Join(e.dir, "wrong_ed25519")
	genKey(t, wrongKey, "evil@example.com")
	e.checkout("-b", "feat-wrongkey")
	e.signWith(wrongKey)
	featWrong := e.commitFile("c.txt", "c")
	e.signWith(e.goodKey)
	e.push("feat-wrongkey")

	wantFP := fingerprint(t, wrongKey+".pub")
	cfg := Config{AllowedSigners: e.allowedSigners}
	vs, err := Check(e.bare, []RefUpdate{{OldSHA: base, NewSHA: featWrong, Ref: "refs/heads/feat-wrongkey"}}, cfg)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(vs) != 1 || vs[0].Rule != "unsigned-or-untrusted-commit" {
		t.Fatalf("violations = %+v", vs)
	}
	if !strings.Contains(vs[0].Detail, wantFP) {
		t.Fatalf("detail should carry the signer fingerprint %s: %q", wantFP, vs[0].Detail)
	}
}

// TestRefUpdateValidate covers the hook-stdin shape check (fail-closed parsing).
func TestRefUpdateValidate(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	valid := []RefUpdate{
		{OldSHA: sha, NewSHA: sha, Ref: "refs/heads/feature"},
		{OldSHA: zeroSHA, NewSHA: sha, Ref: "refs/heads/x"},
		{OldSHA: sha, NewSHA: zeroSHA, Ref: "refs/tags/v1"}, // shape-valid; namespace is Check's job
	}
	for _, u := range valid {
		if err := u.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", u, err)
		}
	}
	invalid := []RefUpdate{
		{OldSHA: sha[:39], NewSHA: sha, Ref: "refs/heads/x"},                                   // short SHA
		{OldSHA: sha, NewSHA: "0123456789ABCDEF0123456789abcdef01234567", Ref: "refs/heads/x"}, // uppercase
		{OldSHA: sha, NewSHA: sha + "0", Ref: "refs/heads/x"},                                  // long SHA
		{OldSHA: sha, NewSHA: sha, Ref: "heads/x"},                                             // no refs/ prefix
		{OldSHA: sha, NewSHA: sha, Ref: "refs/"},                                               // empty remainder
		{OldSHA: sha, NewSHA: sha, Ref: "refs/heads/a\x01b"},                                   // control byte
		{OldSHA: "", NewSHA: sha, Ref: "refs/heads/x"},                                         // empty SHA
	}
	for _, u := range invalid {
		if err := u.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want error", u)
		}
	}
}
