package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRoleRules verifies fingerprint→role attribution + a content-gated role rule:
// "only a reviewer/owner may close a bead" (a .beads/issues.jsonl transition to
// status closed). Role is derived from the signing key, not a forgeable label.
func TestRoleRules(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	dir := t.TempDir()
	bare := filepath.Join(dir, "bare.git")
	work := filepath.Join(dir, "work")
	mustGit(t, dir, "init", "--bare", "--initial-branch=main", bare)
	mustGit(t, dir, "init", "--initial-branch=main", work)

	implKey := filepath.Join(dir, "impl")
	revKey := filepath.Join(dir, "rev")
	genKey(t, implKey, "impl@example.com")
	genKey(t, revKey, "reviewer@example.com")

	allowed := filepath.Join(dir, "allowed_signers")
	writeSigners(t, allowed, map[string]string{
		"impl@example.com":     implKey + ".pub",
		"reviewer@example.com": revKey + ".pub",
	})

	cfg := Config{
		AllowedSigners: allowed,
		Roles: map[string]string{
			fingerprint(t, implKey+".pub"): "implementer",
			fingerprint(t, revKey+".pub"):  "reviewer",
		},
		RoleRules: []RoleRule{{
			Name:         "bead-close-requires-review",
			PathGlob:     ".beads/issues.jsonl",
			AddedRegex:   `"status"\s*:\s*"closed"`,
			AllowedRoles: []string{"reviewer", "owner"},
		}},
	}

	mustGit(t, work, "remote", "add", "origin", bare)
	head := func() string { return strings.TrimSpace(mustGitOut(t, work, "rev-parse", "HEAD")) }
	writeFile := func(rel, content string) {
		p := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setIdentity := func(email, key string) {
		for _, c := range [][]string{
			{"config", "user.name", email},
			{"config", "user.email", email},
			{"config", "gpg.format", "ssh"},
			{"config", "user.signingkey", key},
			{"config", "commit.gpgsign", "true"},
		} {
			mustGit(t, work, c...)
		}
	}

	const openBead = `{"id":"b1","status":"open","title":"task"}` + "\n"
	const closedBead = `{"id":"b1","status":"closed","title":"task"}` + "\n"

	// Base: an OPEN bead on main, signed by the implementer.
	setIdentity("impl@example.com", implKey)
	writeFile(".beads/issues.jsonl", openBead)
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-m", "open bead")
	base := head()
	mustGit(t, work, "push", "origin", "main")

	// Case A: implementer closes the bead -> rejected.
	mustGit(t, work, "checkout", "-b", "impl-close")
	writeFile(".beads/issues.jsonl", closedBead)
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-m", "close b1")
	implClose := head()
	mustGit(t, work, "push", "origin", "impl-close")

	// Case B: reviewer closes the bead -> accepted.
	mustGit(t, work, "checkout", "main")
	mustGit(t, work, "checkout", "-b", "rev-close")
	setIdentity("reviewer@example.com", revKey)
	writeFile(".beads/issues.jsonl", closedBead)
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-m", "close b1")
	revClose := head()
	mustGit(t, work, "push", "origin", "rev-close")

	// Case C: implementer makes a non-close change -> no role violation.
	mustGit(t, work, "checkout", "main")
	mustGit(t, work, "checkout", "-b", "impl-code")
	setIdentity("impl@example.com", implKey)
	writeFile("code.go", "package x\n")
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-m", "add code")
	implCode := head()
	mustGit(t, work, "push", "origin", "impl-code")

	tests := []struct {
		name string
		u    RefUpdate
		want []string
	}{
		{"implementer close rejected", RefUpdate{base, implClose, "refs/heads/impl-close"}, []string{"bead-close-requires-review"}},
		{"reviewer close accepted", RefUpdate{base, revClose, "refs/heads/rev-close"}, nil},
		{"implementer non-close accepted", RefUpdate{base, implCode, "refs/heads/impl-code"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs, err := Check(bare, []RefUpdate{tc.u}, cfg)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			assertRules(t, vs, tc.want)
		})
	}
}

func writeSigners(t *testing.T, path string, principalToPub map[string]string) {
	t.Helper()
	var b strings.Builder
	for principal, pubPath := range principalToPub {
		pub, err := os.ReadFile(pubPath)
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(string(pub))
		if len(f) < 2 {
			t.Fatalf("bad public key %q", pubPath)
		}
		b.WriteString(principal + ` namespaces="git" ` + f[0] + " " + f[1] + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fingerprint returns the key's SHA256 fingerprint as git reports it via %GF.
func fingerprint(t *testing.T, pubPath string) string {
	t.Helper()
	out, err := exec.Command("ssh-keygen", "-lf", pubPath).Output()
	if err != nil {
		t.Fatalf("ssh-keygen -lf: %v", err)
	}
	f := strings.Fields(string(out)) // "256 SHA256:xxxx comment (ED25519)"
	if len(f) < 2 {
		t.Fatalf("unexpected ssh-keygen -lf output: %q", out)
	}
	return f[1]
}
