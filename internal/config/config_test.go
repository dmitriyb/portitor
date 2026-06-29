package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dmitriyb/portitor/internal/gate"
)

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", dir)
	cfg := `{"upstream_slug":"o/r","roles":{"SHA256:abc":"implementer","SHA256:def":"reviewer"}}`
	if err := os.WriteFile(filepath.Join(dir, "myrepo.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Resolve("myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if s.UpstreamSlug != "o/r" {
		t.Fatalf("slug = %q", s.UpstreamSlug)
	}
	if s.Roles["SHA256:abc"] != "implementer" || s.Roles["SHA256:def"] != "reviewer" {
		t.Fatalf("roles = %v", s.Roles)
	}
	if _, err := Resolve("does-not-exist"); err == nil {
		t.Fatal("expected error for a missing repo config")
	}
	// A traversing / invalid --repo must be rejected before touching the FS.
	for _, bad := range []string{"../../etc/hostname", "a/b", "..", ".", "", "foo bar", "foo/../bar"} {
		if _, err := Resolve(bad); err == nil {
			t.Fatalf("expected error for invalid repo name %q", bad)
		}
	}
}

func TestValidName(t *testing.T) {
	good := []string{"myrepo", "my-repo", "my_repo", "Repo.2", "a", "v1.2.3"}
	bad := []string{"", ".", "..", "../x", "a/b", "a b", "x;y", "naïve"}
	for _, g := range good {
		if !ValidName(g) {
			t.Errorf("ValidName(%q) = false, want true", g)
		}
	}
	for _, b := range bad {
		if ValidName(b) {
			t.Errorf("ValidName(%q) = true, want false", b)
		}
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	signers := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(signers, []byte("principal ssh-ed25519 AAAA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := Settings{Config: gate.Config{
		DefaultBranch:  "main",
		AllowedSigners: signers,
		Roles:          map[string]string{"SHA256:a": "reviewer"},
	}}
	if p := Validate(good); len(p) != 0 {
		t.Fatalf("valid config returned problems: %v", p)
	}
	if p := Validate(Settings{}); len(p) == 0 {
		t.Fatal("empty config (no branch/signers/roles) should be invalid")
	}
	noSigners := good
	noSigners.AllowedSigners = "/no/such/file"
	if p := Validate(noSigners); len(p) == 0 {
		t.Fatal("unreadable allowed_signers should be invalid")
	}
	badRegex := good
	badRegex.RoleRules = []gate.RoleRule{{Name: "r", AddedRegex: "(", AllowedRoles: []string{"reviewer"}}}
	if p := Validate(badRegex); len(p) == 0 {
		t.Fatal("bad added_regex should be invalid")
	}
}
