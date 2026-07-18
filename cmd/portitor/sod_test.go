package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/config"
	"github.com/dmitriyb/portitor/internal/gate"
)

// TestRequesterSignedHead verifies the separation-of-duties source of truth:
// whether the requesting key signed any commit the PR introduces, derived from
// the LOCAL gated repo with the gate's own verification — and that an absent
// head ref fails closed.
func TestRequesterSignedHead(t *testing.T) {
	for _, bin := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "myrepo.git")
	work := filepath.Join(dir, "work")
	t.Setenv("PORTITOR_REPO_ROOT", dir)

	mustRun(t, "git", "-C", dir, "init", "-q", "--bare", "--initial-branch=main", bare)
	mustRun(t, "git", "-C", dir, "init", "-q", "--initial-branch=main", work)

	implKey := filepath.Join(dir, "impl")
	revKey := filepath.Join(dir, "rev")
	for _, k := range []string{implKey, revKey} {
		mustRun(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", k)
	}
	signers := filepath.Join(dir, "signers")
	var lines strings.Builder
	for _, k := range []string{implKey, revKey} {
		pub, err := os.ReadFile(k + ".pub")
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(string(pub))
		lines.WriteString(`p namespaces="git" ` + f[0] + " " + f[1] + "\n")
	}
	if err := os.WriteFile(signers, []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	sign := func(key string) {
		for _, c := range [][]string{
			{"config", "user.name", "t"}, {"config", "user.email", "t@x"},
			{"config", "gpg.format", "ssh"}, {"config", "user.signingkey", key},
			{"config", "commit.gpgsign", "true"},
		} {
			mustRun(t, "git", append([]string{"-C", work}, c...)...)
		}
	}
	commit := func(name string) {
		if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		mustRun(t, "git", "-C", work, "add", "-A")
		mustRun(t, "git", "-C", work, "commit", "-qm", name)
	}

	mustRun(t, "git", "-C", work, "remote", "add", "origin", bare)
	sign(implKey)
	commit("base")
	mustRun(t, "git", "-C", work, "push", "-q", "origin", "main")
	mustRun(t, "git", "-C", work, "checkout", "-qb", "feat")
	commit("feature-work") // signed by the implementer
	mustRun(t, "git", "-C", work, "push", "-q", "origin", "feat")

	fpOf := func(key string) string {
		out, err := exec.Command("ssh-keygen", "-lf", key+".pub").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(out))[1]
	}
	s := config.Settings{Config: gate.Config{DefaultBranch: "main", AllowedSigners: signers}}

	signed, err := requesterSignedHead(s, "myrepo", fpOf(implKey), "feat")
	if err != nil {
		t.Fatal(err)
	}
	if !signed {
		t.Fatal("the implementer signed the PR's commits; SoD must detect it")
	}
	signed, err = requesterSignedHead(s, "myrepo", fpOf(revKey), "feat")
	if err != nil {
		t.Fatal(err)
	}
	if signed {
		t.Fatal("the reviewer signed nothing on this PR")
	}
	// Fail-closed: a head ref portitor does not have locally is an error.
	if _, err := requesterSignedHead(s, "myrepo", fpOf(revKey), "no-such-branch"); err == nil {
		t.Fatal("an absent head ref must be an error, not a pass")
	}
	if _, err := requesterSignedHead(s, "myrepo", fpOf(revKey), ""); err == nil {
		t.Fatal("an empty head ref must be an error")
	}
}

func mustRun(t *testing.T, bin string, args ...string) {
	t.Helper()
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
}
