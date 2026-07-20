package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/gate"
	"github.com/dmitriyb/portitor/internal/rules"
)

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", dir)
	fpA := "SHA256:" + strings.Repeat("a", 43)
	fpB := "SHA256:" + strings.Repeat("b", 43)
	cfg := `{"format_version":1,"upstream_slug":"o/r","roles":{"` + fpA + `":"implementer","` + fpB + `":"reviewer"}}`
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
	if s.Roles[fpA] != "implementer" || s.Roles[fpB] != "reviewer" {
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

// TestIdentityOnly: classification is config, not code; absent list = every
// role is a signing role.
func TestIdentityOnly(t *testing.T) {
	s := Settings{IdentityOnlyRoles: []string{"lander", "bot"}}
	if !s.IdentityOnly("lander") || !s.IdentityOnly("bot") {
		t.Fatal("listed roles must classify as identity-only")
	}
	if s.IdentityOnly("reviewer") || s.IdentityOnly("") {
		t.Fatal("unlisted roles are signing roles")
	}
	if (Settings{}).IdentityOnly("lander") {
		t.Fatal("absent list means every role is a signing role")
	}
	// The key parses through the strict decode path.
	p := filepath.Join(t.TempDir(), "cfg.json")
	body := `{"format_version":1,"default_branch":"m","allowed_signers":"x","roles":{},"identity_only_roles":["lander"]}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !s2.IdentityOnly("lander") {
		t.Fatal("identity_only_roles lost through decode")
	}
}

// TestLoadRequiresConfigPath: the hook consumers must refuse to run without a
// config — a gate with a zero config is not uniformly fail-closed.
func TestLoadRequiresConfigPath(t *testing.T) {
	t.Setenv("PORTITOR_CONFIG", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load with unset PORTITOR_CONFIG must refuse")
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
	fp := "SHA256:" + strings.Repeat("a", 43)
	good := Settings{FormatVersion: SupportedFormatVersion, Config: gate.Config{
		DefaultBranch:  "main",
		AllowedSigners: signers,
		Roles:          map[string]string{fp: "reviewer"},
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
	retired := good
	retired.RetiredRoleRules = []byte(`[{"name":"old"}]`)
	if p := Validate(retired); len(p) == 0 {
		t.Fatal("the retired role_rules key should be invalid (never silently dropped)")
	}
	badRules := good
	badRules.Content = &rules.ContentRules{Version: 99}
	if p := Validate(badRules); len(p) == 0 {
		t.Fatal("an unsupported content_rules version should be invalid")
	}
	badVerb := good
	badVerb.ActionRoles = map[string][]string{"deploy": {"owner"}}
	if p := Validate(badVerb); len(p) == 0 {
		t.Fatal("an unknown action verb in action_roles should be invalid")
	}
	okVerbs := good
	okVerbs.ActionRoles = map[string][]string{"merge": {"merger"}, "fetch": {"implementer"}}
	if p := Validate(okVerbs); len(p) != 0 {
		t.Fatalf("known verbs should validate: %v", p)
	}
	badVersion := good
	badVersion.FormatVersion = 2
	if p := Validate(badVersion); len(p) == 0 {
		t.Fatal("an unsupported format_version should be invalid")
	}
}

// TestDecodeDiscipline pins the token-level strict decode: exact top-level
// keys, duplicate rejection everywhere, lowercase schema keys, and the
// fingerprint-keyed data maps exempt from the lowercase rule.
func TestDecodeDiscipline(t *testing.T) {
	fp := "SHA256:" + strings.Repeat("A", 43) // mixed-case data key, legitimate
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "cfg.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	good := `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{"` + fp + `":"reviewer"}}`
	if _, err := LoadFile(write(t, good)); err != nil {
		t.Fatalf("good config: %v", err)
	}

	bad := map[string]string{
		"missing format_version": `{"default_branch":"main","allowed_signers":"x","roles":{}}`,
		"higher format_version":  `{"format_version":2,"default_branch":"main","allowed_signers":"x","roles":{}}`,
		"unknown top-level key":  `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{},"surprise":1}`,
		// Go's case-insensitive field matching would accept a lone "Roles";
		// the byte-exact top-level set must not.
		"cased top-level key": `{"format_version":1,"default_branch":"main","allowed_signers":"x","Roles":{"` + fp + `":"owner"}}`,
		// Coexisting cased twins: the resurrection scenario.
		"cased twin keys": `{"format_version":1,"default_branch":"main","allowed_signers":"x","Roles":{"` + fp + `":"owner"},"roles":{}}`,
		// Silent last-wins shadowing.
		"duplicate top-level key": `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{},"roles":{"` + fp + `":"owner"}}`,
		"duplicate data-map key":  `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{"` + fp + `":"a","` + fp + `":"b"}}`,
		// Nested schema objects must be lowercase too.
		"cased nested key": `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{},"content_rules":{"Version":1}}`,
		"non-object top":   `[1,2,3]`,
		"trailing content": `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{}}{"roles":{"` + fp + `":"owner"}}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadFile(write(t, body)); err == nil {
				t.Fatalf("config should be refused: %s", body)
			}
		})
	}
}
