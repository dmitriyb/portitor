package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/action"
)

// Repo-name validation + config resolution moved to internal/config — tested there
// (config_test.go: TestResolve, TestValidName, TestValidate).

func TestValidateConfig(t *testing.T) {
	dir := t.TempDir()
	signers := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(signers, []byte("principal ssh-ed25519 AAAA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(body string) string {
		p := filepath.Join(dir, "cfg.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	fp := "SHA256:" + strings.Repeat("a", 43)
	ok := write(`{"format_version":1,"default_branch":"main","allowed_signers":"` + signers + `","roles":{"` + fp + `":"reviewer"}}`)
	if rc := validateConfig([]string{"--config", ok}); rc != 0 {
		t.Fatalf("valid config: rc = %d, want 0", rc)
	}

	// Missing format_version → non-zero (hard fail-closed at load).
	noVersion := write(`{"default_branch":"main","allowed_signers":"` + signers + `","roles":{"` + fp + `":"reviewer"}}`)
	if rc := validateConfig([]string{"--config", noVersion}); rc == 0 {
		t.Fatal("config without format_version should fail")
	}

	// Unknown top-level key → non-zero (strict decode).
	unknownKey := write(`{"format_version":1,"default_branch":"main","allowed_signers":"` + signers + `","roles":{"` + fp + `":"reviewer"},"surprise":true}`)
	if rc := validateConfig([]string{"--config", unknownKey}); rc == 0 {
		t.Fatal("config with an unknown key should fail")
	}

	// Missing required fields → non-zero.
	badFields := write(`{"format_version":1,"roles":{}}`)
	if rc := validateConfig([]string{"--config", badFields}); rc == 0 {
		t.Fatal("config with empty default_branch/allowed_signers/roles should fail")
	}

	// allowed_signers points at a non-existent file → non-zero.
	badSigners := write(`{"format_version":1,"default_branch":"main","allowed_signers":"/no/such/file","roles":{"` + fp + `":"reviewer"}}`)
	if rc := validateConfig([]string{"--config", badSigners}); rc == 0 {
		t.Fatal("config with unreadable allowed_signers should fail")
	}

	// A non-fingerprint roles key → non-zero.
	badKey := write(`{"format_version":1,"default_branch":"main","allowed_signers":"` + signers + `","roles":{"SHA256:short":"reviewer"}}`)
	if rc := validateConfig([]string{"--config", badKey}); rc == 0 {
		t.Fatal("config with a non-fingerprint roles key should fail")
	}

	// The retired role_rules key → non-zero (never silently dropped).
	retired := write(`{"format_version":1,"default_branch":"main","allowed_signers":"` + signers + `","roles":{"` + fp + `":"reviewer"},"role_rules":[{"name":"r"}]}`)
	if rc := validateConfig([]string{"--config", retired}); rc == 0 {
		t.Fatal("config with the retired role_rules key should fail")
	}

	// Malformed content_rules (unsupported version) → non-zero.
	badRules := write(`{"format_version":1,"default_branch":"main","allowed_signers":"` + signers + `","roles":{"` + fp + `":"reviewer"},"content_rules":{"version":99}}`)
	if rc := validateConfig([]string{"--config", badRules}); rc == 0 {
		t.Fatal("config with an unsupported content_rules version should fail")
	}

	// Missing path → exit 2.
	if rc := validateConfig([]string{"--config", filepath.Join(dir, "nope.json")}); rc != 1 {
		t.Fatalf("missing config file: rc = %d, want 1", rc)
	}
}

func TestParseUpdates(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	zero := strings.Repeat("0", 40)

	t.Run("valid lines parse", func(t *testing.T) {
		in := sha + " " + zero + " refs/heads/gone\n" + zero + " " + sha + " refs/heads/new\n"
		us, err := parseUpdates(strings.NewReader(in))
		if err != nil {
			t.Fatalf("parseUpdates: %v", err)
		}
		if len(us) != 2 || us[1].Ref != "refs/heads/new" {
			t.Fatalf("updates = %+v", us)
		}
	})

	bad := []string{
		sha + " " + sha,                        // two fields
		sha[:12] + " " + sha + " refs/heads/x", // short SHA
		sha + " not-a-sha refs/heads/x",        // garbage SHA
		sha + " " + sha + " heads/x",           // no refs/ prefix
		sha + " " + sha + " refs/heads/a\x01b", // control byte in ref
	}
	for _, line := range bad {
		t.Run("rejects "+line, func(t *testing.T) {
			if _, err := parseUpdates(strings.NewReader(line + "\n")); err == nil {
				t.Fatalf("parseUpdates(%q) accepted a malformed line", line)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		orig string
		kind string
		rest []string
		ok   bool
	}{
		{"git-receive-pack '/srv/git/repo.git'", "git", []string{"git-receive-pack", "/srv/git/repo.git"}, true},
		{"git-upload-pack '/srv/git/repo.git'", "git", []string{"git-upload-pack", "/srv/git/repo.git"}, true},
		{"git-upload-archive '/srv/git/repo.git'", "reject", nil, false}, // deliberately outside the closed table
		{"portitor pr comment --pr 5", "pr", []string{"comment", "--pr", "5"}, true},
		{"portitor pr fetch --pr 7", "pr", []string{"fetch", "--pr", "7"}, true},
		{"portitor shell deadbeef", "reject", nil, false},
		{"rm -rf /", "reject", nil, false},
		{"git-receive-pack a b", "reject", nil, false},
		{"", "reject", nil, false},
	}
	for _, c := range cases {
		kind, rest, err := classify(c.orig)
		if kind != c.kind {
			t.Errorf("classify(%q) kind=%q want %q", c.orig, kind, c.kind)
		}
		if c.ok && err != nil {
			t.Errorf("classify(%q) unexpected err %v", c.orig, err)
		}
		if !c.ok && err == nil {
			t.Errorf("classify(%q) expected err", c.orig)
		}
		if c.rest != nil && !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("classify(%q) rest=%v want %v", c.orig, rest, c.rest)
		}
	}
}

func TestRoleCan(t *testing.T) {
	policy := map[string][]string{
		"comment": {"implementer", "fixer", "reviewer", "merger", "owner"},
		"fetch":   {"implementer", "fixer", "reviewer", "merger", "owner"},
		"review":  {"reviewer", "owner"},
		"merge":   {"merger", "owner"},
		"close":   {"merger", "owner"},
	}
	allRoles := []string{"implementer", "fixer", "reviewer", "merger", "owner", "", "bogus"}
	for act, allowed := range policy {
		for _, role := range allRoles {
			want := role != "" && contains(allowed, role)
			if got := action.RoleCan(policy, role, act); got != want {
				t.Errorf("RoleCan(policy,%q,%q)=%v want %v", role, act, got, want)
			}
		}
	}
	// implementer must NOT review/merge/close under this policy (the teeth).
	for _, act := range []string{"review", "merge", "close"} {
		if action.RoleCan(policy, "implementer", act) {
			t.Errorf("implementer should not be able to %q", act)
		}
	}
	// Default-deny: nil map, missing verb, unknown verb — all refused.
	if action.RoleCan(nil, "owner", "merge") {
		t.Error("nil action_roles must deny everything")
	}
	if action.RoleCan(map[string][]string{"fetch": {"owner"}}, "owner", "merge") {
		t.Error("an unlisted action must be denied")
	}
	if action.RoleCan(policy, "anything", "unknown-action") {
		t.Error("unknown action must be denied")
	}
}

func TestShellSplit(t *testing.T) {
	got, err := shellSplit("git-receive-pack '/srv/git/my repo.git'")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"git-receive-pack", "/srv/git/my repo.git"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := shellSplit("unterminated 'quote"); err == nil {
		t.Fatal("expected unterminated-quote error")
	}
}

func TestDeriveSlug(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:dmitriyb/portitor.git":          "dmitriyb/portitor",
		"git@github.com-personal:dmitriyb/portitor.git": "dmitriyb/portitor",
		"https://github.com/dmitriyb/portitor.git":      "dmitriyb/portitor",
		"https://github.com/dmitriyb/portitor":          "dmitriyb/portitor",
	} {
		if got := deriveSlug(url); got != want {
			t.Errorf("deriveSlug(%q)=%q want %q", url, got, want)
		}
	}
}

func TestAllowedRepoPath(t *testing.T) {
	t.Setenv("PORTITOR_REPO_ROOT", "/srv/git")
	ok := []string{"/srv/git/repo.git", "/srv/git/team/repo.git"}
	bad := []string{"/etc/passwd", "/srv/git/../../etc/x.git", "/srv/git/repo", "/other/repo.git"}
	for _, p := range ok {
		if !allowedRepoPath(p) {
			t.Errorf("allowedRepoPath(%q) = false, want true", p)
		}
	}
	for _, p := range bad {
		if allowedRepoPath(p) {
			t.Errorf("allowedRepoPath(%q) = true, want false", p)
		}
	}
}

// contains is a tiny test helper (mirrors gate.contains).
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
