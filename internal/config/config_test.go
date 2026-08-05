package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/action"
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

// TestReviewSource pins the default-to-none resolution: absent block, absent
// field, and an explicit value all resolve per
// action.MergeGateConfig.ReviewSource.
func TestReviewSource(t *testing.T) {
	if got := (Settings{}).ReviewSource(); got != "none" {
		t.Fatalf("absent merge_gate: %q, want none", got)
	}
	if got := (Settings{MergeGate: &action.MergeGateConfig{}}).ReviewSource(); got != "none" {
		t.Fatalf("absent review field: %q, want none", got)
	}
	for _, src := range []string{"github", "none"} {
		if got := (Settings{MergeGate: &action.MergeGateConfig{Review: src}}).ReviewSource(); got != src {
			t.Fatalf("explicit %q: got %q", src, got)
		}
	}
}

// TestMergeMethod pins the default-to-squash resolution: absent block, absent
// field, and an explicit value all resolve per
// action.MergeGateConfig.MergeMethodOrDefault.
func TestMergeMethod(t *testing.T) {
	if got := (Settings{}).MergeMethod(); got != "squash" {
		t.Fatalf("absent merge_gate: %q, want squash", got)
	}
	if got := (Settings{MergeGate: &action.MergeGateConfig{}}).MergeMethod(); got != "squash" {
		t.Fatalf("absent merge_method field: %q, want squash", got)
	}
	for _, method := range []string{"squash", "merge", "rebase"} {
		if got := (Settings{MergeGate: &action.MergeGateConfig{MergeMethod: method}}).MergeMethod(); got != method {
			t.Fatalf("explicit %q: got %q", method, got)
		}
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
	okEmails := good
	okEmails.AllowedCommitterEmails = []string{"dev@example.com"}
	if p := Validate(okEmails); len(p) != 0 {
		t.Fatalf("non-empty allowed_committer_emails entries should validate: %v", p)
	}
	emptyEmail := good
	emptyEmail.AllowedCommitterEmails = []string{"dev@example.com", ""}
	if p := Validate(emptyEmail); len(p) == 0 {
		t.Fatal("an empty allowed_committer_emails entry should be invalid (it can never match)")
	}

	// merge_gate.review: absent merge_gate, "github", and "none" are all valid;
	// the retired "internal" source is rejected (see
	// 2026-08-05-transparent-approve).
	githubSource := good
	githubSource.MergeGate = &action.MergeGateConfig{Review: "github"}
	if p := Validate(githubSource); len(p) != 0 {
		t.Fatalf("github review source should validate: %v", p)
	}
	noneSource := good
	noneSource.MergeGate = &action.MergeGateConfig{Review: "none"}
	if p := Validate(noneSource); len(p) != 0 {
		t.Fatalf("none review source should validate: %v", p)
	}
	badSource := good
	badSource.MergeGate = &action.MergeGateConfig{Review: "bogus"}
	if p := Validate(badSource); len(p) == 0 {
		t.Fatal("an unknown merge_gate.review value should be invalid")
	}
	retiredInternalSource := good
	retiredInternalSource.MergeGate = &action.MergeGateConfig{Review: "internal"}
	if p := Validate(retiredInternalSource); len(p) == 0 {
		t.Fatal("the retired internal review source should be invalid")
	}
	goodChecks := good
	goodChecks.MergeGate = &action.MergeGateConfig{Checks: []action.CheckPredicate{{Name: "bead-closed", Command: []string{"br", "--no-db"}}}}
	if p := Validate(goodChecks); len(p) != 0 {
		t.Fatalf("a well-formed check predicate should validate: %v", p)
	}
	noNameCheck := good
	noNameCheck.MergeGate = &action.MergeGateConfig{Checks: []action.CheckPredicate{{Command: []string{"br"}}}}
	if p := Validate(noNameCheck); len(p) == 0 {
		t.Fatal("a check predicate with an empty name should be invalid")
	}
	noCommandCheck := good
	noCommandCheck.MergeGate = &action.MergeGateConfig{Checks: []action.CheckPredicate{{Name: "x"}}}
	if p := Validate(noCommandCheck); len(p) == 0 {
		t.Fatal("a check predicate with an empty command should be invalid")
	}
	emptyArgvCheck := good
	emptyArgvCheck.MergeGate = &action.MergeGateConfig{Checks: []action.CheckPredicate{{Name: "x", Command: []string{"br", ""}}}}
	if p := Validate(emptyArgvCheck); len(p) == 0 {
		t.Fatal("a check predicate with an empty argv element should be invalid")
	}

	// merge_gate.merge_method: absent merge_gate and ""/squash/merge/rebase
	// are all valid; anything else is rejected (see
	// 2026-08-05-configurable-merge-method).
	if p := Validate(good); len(p) != 0 {
		t.Fatalf("absent merge_gate (default merge_method) should validate: %v", p)
	}
	for _, method := range []string{"", "squash", "merge", "rebase"} {
		okMethod := good
		okMethod.MergeGate = &action.MergeGateConfig{MergeMethod: method}
		if p := Validate(okMethod); len(p) != 0 {
			t.Fatalf("merge_method %q should validate: %v", method, p)
		}
	}
	badMethod := good
	badMethod.MergeGate = &action.MergeGateConfig{MergeMethod: "fast-forward"}
	if p := Validate(badMethod); len(p) == 0 {
		t.Fatal("an unknown merge_gate.merge_method value should be invalid")
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
	withEmails := `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{"` + fp + `":"reviewer"},"allowed_committer_emails":["dev@example.com"]}`
	if s, err := LoadFile(write(t, withEmails)); err != nil {
		t.Fatalf("allowed_committer_emails config: %v", err)
	} else if len(s.AllowedCommitterEmails) != 1 || s.AllowedCommitterEmails[0] != "dev@example.com" {
		t.Fatalf("allowed_committer_emails decoded as %v", s.AllowedCommitterEmails)
	}

	withMergeGate := `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{"` + fp + `":"reviewer"},` +
		`"merge_gate":{"review":"github","checks":[{"name":"bead-closed","command":["br","--no-db"]}]}}`
	if s, err := LoadFile(write(t, withMergeGate)); err != nil {
		t.Fatalf("merge_gate config: %v", err)
	} else if s.MergeGate == nil ||
		s.MergeGate.Review != "github" || len(s.MergeGate.Checks) != 1 ||
		s.MergeGate.Checks[0].Name != "bead-closed" || len(s.MergeGate.Checks[0].Command) != 2 {
		t.Fatalf("merge_gate decoded as %+v", s)
	}

	// merge_gate absent entirely, and merge_gate present with an absent
	// review field, must both decode fine (default resolved by ReviewSource,
	// not the decoder).
	minimalMergeGate := `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{"` + fp + `":"reviewer"},"merge_gate":{}}`
	if s, err := LoadFile(write(t, minimalMergeGate)); err != nil {
		t.Fatalf("empty merge_gate block: %v", err)
	} else if s.MergeGate == nil || s.MergeGate.Review != "" {
		t.Fatalf("empty merge_gate block decoded as %+v", s.MergeGate)
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
		// merge_gate is a schema object (not a data map): cased/duplicate keys,
		// including inside its checks[] entries, must be rejected the same way.
		"cased merge_gate key":       `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{},"merge_gate":{"Review":"internal"}}`,
		"duplicate merge_gate key":   `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{},"merge_gate":{"review":"none","review":"github"}}`,
		"cased merge_gate check key": `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{},"merge_gate":{"checks":[{"Name":"x","command":["a"]}]}}`,
		"non-object top":             `[1,2,3]`,
		"trailing content":           `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{}}{"roles":{"` + fp + `":"owner"}}`,
		// The retired reviews_log top-level key: strict decode's token-level
		// key check must refuse it as unknown, not silently accept and drop it
		// (see 2026-08-05-transparent-approve).
		"retired reviews_log key": `{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{},"reviews_log":"/var/lib/portitor/reviews.jsonl"}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadFile(write(t, body)); err == nil {
				t.Fatalf("config should be refused: %s", body)
			}
		})
	}
}
