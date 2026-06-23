package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testEnv is a throwaway git setup: a bare "server" repo, a work repo that signs
// with an ephemeral key, and an allowed_signers listing that key. No YubiKey,
// no real credentials — fully self-contained.
type testEnv struct {
	t              *testing.T
	dir            string
	bare           string
	work           string
	allowedSigners string
	goodKey        string // private key trusted by allowed_signers
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "bare.git")
	work := filepath.Join(dir, "work")

	mustGit(t, dir, "init", "--bare", "--initial-branch=main", bare)
	mustGit(t, dir, "init", "--initial-branch=main", work)

	goodKey := filepath.Join(dir, "good_ed25519")
	genKey(t, goodKey, "test@example.com")
	allowed := writeAllowedSigners(t, filepath.Join(dir, "allowed_signers"), "test@example.com", goodKey+".pub")

	for _, c := range [][]string{
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.com"},
		{"config", "gpg.format", "ssh"},
		{"config", "user.signingkey", goodKey},
		{"config", "commit.gpgsign", "true"},
		{"config", "remote.origin.url", bare},
	} {
		mustGit(t, work, c...)
	}

	return &testEnv{t: t, dir: dir, bare: bare, work: work, allowedSigners: allowed, goodKey: goodKey}
}

func (e *testEnv) head() string {
	return strings.TrimSpace(mustGitOut(e.t, e.work, "rev-parse", "HEAD"))
}

// commitFile writes a file in the work repo and commits it; extra args are passed
// to `git commit` (e.g. "--no-gpg-sign"). Returns the new commit SHA.
func (e *testEnv) commitFile(name, content string, commitArgs ...string) string {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.work, name), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	mustGit(e.t, e.work, "add", name)
	mustGit(e.t, e.work, append([]string{"commit", "-m", "add " + name}, commitArgs...)...)
	return e.head()
}

func (e *testEnv) checkout(args ...string) { mustGit(e.t, e.work, append([]string{"checkout"}, args...)...) }
func (e *testEnv) push(refspec string)     { mustGit(e.t, e.work, "push", "origin", refspec) }
func (e *testEnv) signWith(key string)     { mustGit(e.t, e.work, "config", "user.signingkey", key) }

func TestCheck(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	e := newTestEnv(t)

	// Base commit on main (signed), pushed so the bare has the shared history.
	base := e.commitFile("README.md", "base")
	e.push("main")

	// Signed feature branch off main.
	e.checkout("-b", "feature")
	featSigned := e.commitFile("a.txt", "a")
	e.push("feature")

	// Unsigned feature.
	e.checkout("main")
	e.checkout("-b", "feat-unsigned")
	featUnsigned := e.commitFile("b.txt", "b", "--no-gpg-sign")
	e.push("feat-unsigned")

	// Feature signed by a key NOT in allowed_signers.
	wrongKey := filepath.Join(e.dir, "wrong_ed25519")
	genKey(t, wrongKey, "evil@example.com")
	e.checkout("main")
	e.checkout("-b", "feat-wrongkey")
	e.signWith(wrongKey)
	featWrong := e.commitFile("c.txt", "c")
	e.signWith(e.goodKey) // restore for any later commits
	e.push("feat-wrongkey")

	cfg := Config{AllowedSigners: e.allowedSigners} // DefaultBranch derived from bare HEAD (main)

	tests := []struct {
		name      string
		update    RefUpdate
		wantRules []string
	}{
		{
			name:      "signed feature update accepted",
			update:    RefUpdate{OldSHA: base, NewSHA: featSigned, Ref: "refs/heads/feature"},
			wantRules: nil,
		},
		{
			name:      "push to default branch rejected",
			update:    RefUpdate{OldSHA: base, NewSHA: featSigned, Ref: "refs/heads/main"},
			wantRules: []string{"no-push-to-default"},
		},
		{
			name:      "unsigned commit rejected",
			update:    RefUpdate{OldSHA: base, NewSHA: featUnsigned, Ref: "refs/heads/feat-unsigned"},
			wantRules: []string{"unsigned-or-untrusted-commit"},
		},
		{
			name:      "untrusted signer rejected",
			update:    RefUpdate{OldSHA: base, NewSHA: featWrong, Ref: "refs/heads/feat-wrongkey"},
			wantRules: []string{"unsigned-or-untrusted-commit"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs, err := Check(e.bare, []RefUpdate{tc.update}, cfg)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			assertRules(t, vs, tc.wantRules)
		})
	}
}

// TestCheckCreateBranch exercises the branch-creation path (old = zero), where
// newCommits uses `rev-list <new> --not --all`. To mimic pre-receive's pre-update
// state (the ref doesn't exist yet), the branch is pushed then its ref deleted on
// the bare side, leaving the objects but not the ref.
func TestCheckCreateBranch(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	e := newTestEnv(t)

	e.commitFile("README.md", "base") // shared history so --not --all excludes it
	e.push("main")

	e.checkout("-b", "feat-new")
	newSigned := e.commitFile("d.txt", "d")
	e.push("feat-new")
	mustGit(t, e.bare, "update-ref", "-d", "refs/heads/feat-new") // keep objects, drop ref

	cfg := Config{AllowedSigners: e.allowedSigners}
	vs, err := Check(e.bare, []RefUpdate{{OldSHA: zeroSHA, NewSHA: newSigned, Ref: "refs/heads/feat-new"}}, cfg)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertRules(t, vs, nil)
}

// --- helpers ---

func assertRules(t *testing.T, vs []Violation, want []string) {
	t.Helper()
	var got []string
	for _, v := range vs {
		got = append(got, v.Rule)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("violations = %v, want rules %v\nfull: %+v", got, want, vs)
	}
}

func genKey(t *testing.T, path, comment string) {
	t.Helper()
	out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
}

func writeAllowedSigners(t *testing.T, path, principal, pubPath string) string {
	t.Helper()
	pub, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(pub)) // keytype keydata [comment]
	if len(f) < 2 {
		t.Fatalf("bad public key %q: %q", pubPath, pub)
	}
	line := principal + ` namespaces="git" ` + f[0] + " " + f[1] + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func mustGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func requireBins(t *testing.T, bins ...string) {
	t.Helper()
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not available: %v", b, err)
		}
	}
}
