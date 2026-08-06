package main

import (
	"bytes"
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

// TestReviewRejectsUnknownEvent pins that prRun's `review` verb refuses an
// unrecognized --event (e.g. a misspelling like "aprove") before doing
// anything else in that case — no GitHub post at all. A silent fall-through
// would post a review to GitHub under a verdict nobody asked for.
func TestReviewRejectsUnknownEvent(t *testing.T) {
	reposDir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)

	fp := "SHA256:" + strings.Repeat("a", 43)
	// upstream_slug is set explicitly so prRun reaches the "review" case
	// without deriving a slug from a git remote (there is none in this test) —
	// an empty/unresolvable slug would fail earlier, at the "no upstream slug
	// configured" check, and never exercise the event validation this test
	// targets. action_roles grants "reviewer" the review verb so the role
	// check above the switch does not short-circuit first either.
	cfg := `{"format_version":1,"default_branch":"main","allowed_signers":"",` +
		`"roles":{"` + fp + `":"reviewer"},"action_roles":{"review":["reviewer"]},` +
		`"upstream_slug":"acme/repo"}`
	if err := os.WriteFile(filepath.Join(reposDir, "myrepo.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errw bytes.Buffer
	rc := prRun(fp, []string{"review"}, prOptions{PR: 1, Event: "aprove", Repo: "myrepo"},
		strings.NewReader("looks good"), &out, &errw)
	if rc == 0 {
		t.Fatalf("misspelled --event should be refused, got rc=0 (stderr=%q)", errw.String())
	}
	if !strings.Contains(errw.String(), "review:") || !strings.Contains(errw.String(), `"aprove"`) {
		t.Fatalf("expected a clear usage-style error naming the bad event, got %q", errw.String())
	}
}

// TestDescribeDeniedWithoutRole pins that prRun's "describe" verb is a known,
// gate-checkable action — reaching the role.RoleCan default-deny branch (not
// the earlier "unknown action" usage error) — and is refused for a caller
// whose role is absent from action_roles["describe"]. If `describe` were
// missing from action.Verbs (the closed mechanism set), prRun would reject it
// at the args check with "unknown action" instead, before ever consulting
// action_roles; if the switch's "describe" case were removed but the verb
// left in Verbs/action_roles, this test would still pass (it never reaches
// the switch), which is exactly why TestDescribeGrantedRoleReachesGH below
// covers the granted path separately.
func TestDescribeDeniedWithoutRole(t *testing.T) {
	reposDir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)

	fp := "SHA256:" + strings.Repeat("a", 43)
	// action_roles grants "describe" to nobody at all (absent key ==
	// default-deny) — "reviewer" is a real, known role, just not one this
	// policy lists under "describe".
	cfg := `{"format_version":1,"default_branch":"main","allowed_signers":"",` +
		`"roles":{"` + fp + `":"reviewer"},"action_roles":{"comment":["reviewer"]},` +
		`"upstream_slug":"acme/repo"}`
	if err := os.WriteFile(filepath.Join(reposDir, "myrepo.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errw bytes.Buffer
	rc := prRun(fp, []string{"describe"}, prOptions{PR: 1, Repo: "myrepo"},
		strings.NewReader("new description"), &out, &errw)
	if rc == 0 {
		t.Fatalf("describe should be denied when action_roles[\"describe\"] does not list the caller's role, got rc=0")
	}
	if !strings.Contains(errw.String(), `may not "describe"`) {
		t.Fatalf("expected the role-based default-deny message naming \"describe\", got %q", errw.String())
	}
	if strings.Contains(errw.String(), "unknown action") {
		t.Fatalf("describe must be a known verb (dispatched to RoleCan, not rejected as unrecognized), got %q", errw.String())
	}
	if out.Len() != 0 {
		t.Fatalf("a denied describe must never write a gh response to stdout, got %q", out.String())
	}
}

// TestDescribeGrantedRoleReachesGH pins that a role GRANTED action_roles
// ["describe"] clears the RoleCan gate and reaches the gh dispatch — proven
// without any git/gh call by setting upstream_slug to a deliberately
// malformed value ("noSlash", no "/"). ghClientFor only falls back to
// deriving a slug from the git remote when upstream_slug is EMPTY (see
// main.go's ghClientFor) — an explicit-but-malformed value skips remote
// derivation entirely and fails validSlug, so gh.Repo == "" and prRun refuses
// with "no upstream slug configured" immediately before the switch, with no
// git subprocess run at all (this package's tests must never shell to the
// real git/gh — this test's own working tree has a real "origin" remote, and
// deriving from it would be exactly the unsafe network call to avoid). A
// denial here (the "may not" message) would mean describe was not actually
// granted; an "unknown action" here would mean describe is missing from the
// closed verb set — this test rules out both, isolating the remaining gap
// (the switch case itself, exercised in the acceptance tier where gh is
// real).
func TestDescribeGrantedRoleReachesGH(t *testing.T) {
	reposDir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)

	fp := "SHA256:" + strings.Repeat("b", 43)
	cfg := `{"format_version":1,"default_branch":"main","allowed_signers":"",` +
		`"roles":{"` + fp + `":"implementer"},"action_roles":{"describe":["implementer"]},` +
		`"upstream_slug":"noSlash"}`
	if err := os.WriteFile(filepath.Join(reposDir, "myrepo.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errw bytes.Buffer
	rc := prRun(fp, []string{"describe"}, prOptions{PR: 1, Repo: "myrepo"},
		strings.NewReader("new description"), &out, &errw)
	if rc == 0 {
		t.Fatalf("expected a failure past the role check (no upstream slug configured), got rc=0")
	}
	if strings.Contains(errw.String(), "may not") {
		t.Fatalf("a granted role must not be denied by RoleCan, got %q", errw.String())
	}
	if strings.Contains(errw.String(), "unknown action") {
		t.Fatalf("describe must be a known verb, got %q", errw.String())
	}
	if !strings.Contains(errw.String(), "no upstream slug configured") {
		t.Fatalf("expected the granted role to reach the upstream-slug check, got %q", errw.String())
	}
}

// TestDescribeCaseDispatchesToGH pins that prRun's switch actually HAS a
// "describe" case that calls through to gh.Describe — the one thing
// TestDescribeGrantedRoleReachesGH above cannot prove (it deliberately stops
// one step short of the switch, at the upstream-slug check, to avoid a real
// gh subprocess). This test reaches INSIDE the case without ever shelling gh,
// by combining a validly-shaped-but-fake upstream_slug (so gh.Repo is
// non-empty and prRun proceeds past the slug check into the switch, like
// TestReviewRejectsUnknownEvent's "acme/repo") with an EMPTY stdin body: with
// action.GH.Describe's empty-body guard (it refuses "" before running gh —
// see TestDescribeRefusesEmptyBody in internal/action), the case is entered,
// readBody(in) is called, Describe is invoked, and it fails INSIDE Describe
// before any subprocess runs — all observable, all hermetic.
//
// This also exercises prRun's outer-switch `default:` backstop by contrast:
// if the "describe" case were removed (while "describe" stayed in
// action.Verbs and action_roles), execution would fall to `default: return
// fail(fmt.Errorf("unhandled action %q", act))` instead, and the assertion
// below (which requires the Describe-specific "refusing to overwrite"
// message, not "unhandled action") would fail — so this test catches a
// dropped "describe" case exactly as TestReviewRejectsUnknownEvent already
// catches a dropped "review" case (its "review:" event-validation message is
// equally distinct from "unhandled action").
func TestDescribeCaseDispatchesToGH(t *testing.T) {
	reposDir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)

	fp := "SHA256:" + strings.Repeat("c", 43)
	// upstream_slug is validly shaped (owner/name) but points nowhere real —
	// gh is never actually invoked, because the empty body is refused inside
	// Describe before g.run (and thus any subprocess) is reached.
	cfg := `{"format_version":1,"default_branch":"main","allowed_signers":"",` +
		`"roles":{"` + fp + `":"implementer"},"action_roles":{"describe":["implementer"]},` +
		`"upstream_slug":"acme/repo"}`
	if err := os.WriteFile(filepath.Join(reposDir, "myrepo.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errw bytes.Buffer
	rc := prRun(fp, []string{"describe"}, prOptions{PR: 1, Repo: "myrepo"},
		strings.NewReader(""), &out, &errw)
	if rc == 0 {
		t.Fatalf("an empty describe body should be refused, got rc=0")
	}
	if !strings.Contains(errw.String(), "describe: refusing to overwrite the PR body with empty content") {
		t.Fatalf("expected Describe's empty-body refusal to surface verbatim, got %q", errw.String())
	}
	if strings.Contains(errw.String(), "unhandled action") {
		t.Fatalf("the \"describe\" case must be present in prRun's switch, got %q", errw.String())
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
		"comment":  {"implementer", "fixer", "reviewer", "merger", "owner"},
		"fetch":    {"implementer", "fixer", "reviewer", "merger", "owner"},
		"review":   {"reviewer", "owner"},
		"describe": {"implementer", "owner"},
		"merge":    {"merger", "owner"},
		"close":    {"merger", "owner"},
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
