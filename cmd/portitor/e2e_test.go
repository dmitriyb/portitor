package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEndToEndRealPush installs the built portitor binary as a real pre-receive
// hook on a bare repo and drives it with actual `git push` — confirming stdin
// parsing, GIT_DIR handling, the exit-code → push-rejection mapping, and that
// rejection reasons surface to the pusher as remote: lines (spec/gate/
// test_pre_receive.md "End-to-end (real push)").
func TestEndToEndRealPush(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	for _, b := range []string{"git", "ssh-keygen", "go"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not available", b)
		}
	}
	dir := t.TempDir()

	// Build the portitor binary under test.
	bin := filepath.Join(dir, "portitor")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Signing key + allowed_signers + a valid config.
	key := filepath.Join(dir, "signer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "signer@x", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	f := strings.Fields(string(pub))
	signers := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(signers, []byte("signer@x namespaces=\"git\" "+f[0]+" "+f[1]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fpOut, _ := exec.Command("ssh-keygen", "-lf", key+".pub").Output()
	fp := strings.Fields(string(fpOut))[1]
	cfg := filepath.Join(dir, "config.json")
	body := `{"format_version":1,"default_branch":"main","allowed_signers":"` + signers +
		`","roles":{"` + fp + `":"implementer"}}`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bare "server" with the real pre-receive hook installed.
	bare := filepath.Join(dir, "bare.git")
	mustRun(t, "git", "init", "-q", "--bare", "--initial-branch=main", bare)
	shim := "#!/bin/sh\nexport PORTITOR_CONFIG=" + shellQuote(cfg) + "\nexec " + shellQuote(bin) + " pre-receive\n"
	if err := os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	// Work repo signing with the trusted key.
	work := filepath.Join(dir, "work")
	mustRun(t, "git", "init", "-q", "--initial-branch=main", work)
	for _, c := range [][]string{
		{"config", "user.name", "Signer"}, {"config", "user.email", "signer@x"},
		{"config", "gpg.format", "ssh"}, {"config", "user.signingkey", key},
		{"config", "commit.gpgsign", "true"}, {"config", "remote.origin.url", bare},
	} {
		mustRun(t, "git", append([]string{"-C", work}, c...)...)
	}
	commit := func(name string, extra ...string) {
		if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		mustRun(t, "git", "-C", work, "add", name)
		args := append([]string{"-C", work, "commit", "-m", "add " + name}, extra...)
		mustRun(t, "git", args...)
	}
	push := func(refspec string) (string, error) {
		out, err := exec.Command("git", "-C", work, "push", "origin", refspec).CombinedOutput()
		return string(out), err
	}

	commit("base.txt")
	mustRun(t, "git", "-C", work, "branch", "-M", "main")

	t.Run("push to main rejected", func(t *testing.T) {
		out, err := push("main")
		if err == nil {
			t.Fatalf("push to main should be declined:\n%s", out)
		}
		if !strings.Contains(out, "no-push-to-default") {
			t.Fatalf("expected no-push-to-default in remote output:\n%s", out)
		}
	})

	// Land base on the bare out-of-band so feature has a parent there.
	mustRun(t, "git", "-C", bare, "fetch", work, "main:refs/heads/main")

	t.Run("signed feature accepted", func(t *testing.T) {
		mustRun(t, "git", "-C", work, "checkout", "-q", "-b", "feature")
		commit("a.txt")
		out, err := push("feature")
		if err != nil {
			t.Fatalf("signed feature push should be accepted:\n%s", out)
		}
	})

	t.Run("unsigned feature rejected", func(t *testing.T) {
		mustRun(t, "git", "-C", work, "checkout", "-q", "main")
		mustRun(t, "git", "-C", work, "checkout", "-q", "-b", "feat-unsigned")
		commit("b.txt", "--no-gpg-sign")
		out, err := push("feat-unsigned")
		if err == nil {
			t.Fatalf("unsigned feature push should be declined:\n%s", out)
		}
		if !strings.Contains(out, "unsigned-or-untrusted-commit") {
			t.Fatalf("expected unsigned-or-untrusted-commit in remote output:\n%s", out)
		}
	})
}
