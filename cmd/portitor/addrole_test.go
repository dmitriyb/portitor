package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/config"
)

// genKey creates an ephemeral ed25519 keypair at <dir>/<name> and returns the pub
// file path and the key's SHA256 fingerprint.
func genKey(t *testing.T, dir, name string) (pubPath, fp string) {
	t.Helper()
	key := filepath.Join(dir, name)
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", name, "-f", key)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pubPath = key + ".pub"
	f, err := sshKeygenFingerprint(pubPath)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return pubPath, f
}

// seedRepo writes a repos.d/<name>.json (pointed at by PORTITOR_REPOS_DIR) plus its
// allowed_signers file, and returns the repos dir, config path, and signers path.
func seedRepo(t *testing.T, roles, extraSignerLines string) (reposDir, cfgPath, signersPath string) {
	t.Helper()
	reposDir = t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	signersPath = filepath.Join(reposDir, "allowed_signers")
	if err := os.WriteFile(signersPath, []byte(extraSignerLines), 0o644); err != nil {
		t.Fatal(err)
	}
	if roles == "" {
		roles = "{}"
	}
	cfgPath = filepath.Join(reposDir, "myrepo.json")
	// identity_only_roles is config, not code: the fixtures declare "merger"
	// landing-only, matching the recommended deployment policy.
	body := `{"format_version":1,"default_branch":"main","allowed_signers":"` + signersPath + `","identity_only_roles":["merger"],"roles":` + roles + `}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return reposDir, cfgPath, signersPath
}

func readCfg(t *testing.T, path string) config.Settings {
	t.Helper()
	s, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	return s
}

const goodFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// 1. malformed fingerprint rejected → exit 2, config untouched.
func TestAddRole_BadFingerprint(t *testing.T) {
	_, cfg, _ := seedRepo(t, "{}", "")
	before, _ := os.ReadFile(cfg)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", "deadbeef"}); rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Fatal("config was modified")
	}
}

// 2. empty or invalid role rejected → exit 2.
func TestAddRole_BadRole(t *testing.T) {
	seedRepo(t, "{}", "")
	for _, r := range []string{"", "bad name", "a/b"} {
		if rc := addRole([]string{"--repo", "myrepo", "--role", r, "--fingerprint", goodFP}); rc != 2 {
			t.Fatalf("role %q: rc = %d, want 2", r, rc)
		}
	}
}

// 3. missing config rejected → exit 1, no file created.
func TestAddRole_MissingConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", dir)
	if rc := addRole([]string{"--repo", "nope", "--role", "implementer", "--fingerprint", goodFP}); rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, "nope.json")); !os.IsNotExist(err) {
		t.Fatal("a config file was created")
	}
}

// 4. repo name is path-guarded → exit 2, nothing written outside repos.d.
func TestAddRole_RepoTraversal(t *testing.T) {
	seedRepo(t, "{}", "")
	if rc := addRole([]string{"--repo", "../escape", "--role", "implementer", "--fingerprint", goodFP}); rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if rc := addRole([]string{"--role", "implementer", "--fingerprint", goodFP}); rc != 2 {
		t.Fatalf("missing --repo: rc = %d, want 2", rc)
	}
}

// 5. add a new binding → roles[fp]==role, other fields unchanged, exit 0.
func TestAddRole_AddNewBinding(t *testing.T) {
	_, cfg, signers := seedRepo(t, `{"SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB":"reviewer"}`, "")
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", goodFP}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	s := readCfg(t, cfg)
	if s.Roles[goodFP] != "implementer" {
		t.Fatalf("roles[fp] = %q, want implementer", s.Roles[goodFP])
	}
	if s.Roles["SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"] != "reviewer" {
		t.Fatal("existing binding was lost")
	}
	if s.DefaultBranch != "main" || s.AllowedSigners != signers {
		t.Fatalf("other fields changed: %+v", s)
	}
}

// 6. overwrite an existing binding → roles[fp] becomes the new role, exit 0.
func TestAddRole_Overwrite(t *testing.T) {
	_, cfg, _ := seedRepo(t, `{"`+goodFP+`":"reviewer"}`, "")
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", goodFP}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if s := readCfg(t, cfg); s.Roles[goodFP] != "implementer" {
		t.Fatalf("roles[fp] = %q, want implementer", s.Roles[goodFP])
	}
}

// 7. idempotent no-op → exit 0, config bytes unchanged, reported unchanged.
func TestAddRole_Idempotent(t *testing.T) {
	_, cfg, _ := seedRepo(t, `{"`+goodFP+`":"implementer"}`, "")
	before, _ := os.ReadFile(cfg)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", goodFP}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Fatalf("config bytes changed on no-op:\nbefore=%s\nafter=%s", before, after)
	}
}

// 8. signing role appends its key → an allowed_signers line is added, exit 0.
func TestAddRole_SigningAppends(t *testing.T) {
	dir := t.TempDir()
	pub, fp := genKey(t, dir, "impl")
	_, cfg, signers := seedRepo(t, "{}", "")
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", fp, "--pub", pub}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if s := readCfg(t, cfg); s.Roles[fp] != "implementer" {
		t.Fatalf("roles[fp] = %q", s.Roles[fp])
	}
	b, _ := os.ReadFile(signers)
	if !strings.Contains(string(b), "implementer namespaces=\"git\" ") {
		t.Fatalf("allowed_signers missing appended line:\n%s", b)
	}
	pubBytes, _ := os.ReadFile(pub)
	keyData := strings.Fields(string(pubBytes))[1]
	if !strings.Contains(string(b), keyData) {
		t.Fatal("appended line does not carry the key blob")
	}
}

// 9. dedup on re-run → repeating scenario 8 does not add a second line.
func TestAddRole_Dedup(t *testing.T) {
	dir := t.TempDir()
	pub, fp := genKey(t, dir, "impl")
	_, _, signers := seedRepo(t, "{}", "")
	args := []string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", fp, "--pub", pub}
	if rc := addRole(args); rc != 0 {
		t.Fatalf("first run rc = %d", rc)
	}
	first, _ := os.ReadFile(signers)
	if rc := addRole(args); rc != 0 {
		t.Fatalf("second run rc = %d", rc)
	}
	second, _ := os.ReadFile(signers)
	if string(first) != string(second) {
		t.Fatalf("dedup failed; allowed_signers grew:\n%s", second)
	}
	if n := strings.Count(string(second), "namespaces=\"git\""); n != 1 {
		t.Fatalf("want 1 signer line, got %d", n)
	}
}

// 10. fingerprint/pub mismatch refused → exit 1, nothing modified.
func TestAddRole_PubMismatch(t *testing.T) {
	dir := t.TempDir()
	pubA, _ := genKey(t, dir, "a")
	_, fpB := genKey(t, dir, "b")
	_, cfg, signers := seedRepo(t, "{}", "")
	beforeCfg, _ := os.ReadFile(cfg)
	beforeSig, _ := os.ReadFile(signers)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", fpB, "--pub", pubA}); rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	afterCfg, _ := os.ReadFile(cfg)
	afterSig, _ := os.ReadFile(signers)
	if string(beforeCfg) != string(afterCfg) || string(beforeSig) != string(afterSig) {
		t.Fatal("mismatch must not modify config or allowed_signers")
	}
}

// 11. identity-only role with --pub refused → exit 1, allowed_signers and roles
// both untouched (fails before writing).
func TestAddRole_IdentityOnlyWithPub(t *testing.T) {
	dir := t.TempDir()
	pub, fp := genKey(t, dir, "merger")
	_, cfg, signers := seedRepo(t, "{}", "")
	beforeCfg, _ := os.ReadFile(cfg)
	beforeSig, _ := os.ReadFile(signers)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "merger", "--fingerprint", fp, "--pub", pub}); rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	afterCfg, _ := os.ReadFile(cfg)
	afterSig, _ := os.ReadFile(signers)
	if string(beforeCfg) != string(afterCfg) {
		t.Fatal("config (roles) must not be written")
	}
	if string(beforeSig) != string(afterSig) {
		t.Fatal("allowed_signers must not be touched")
	}
}

// 12. --pub omitted leaves allowed_signers alone.
func TestAddRole_NoPubLeavesSigners(t *testing.T) {
	_, cfg, signers := seedRepo(t, "{}", "principal ssh-ed25519 AAAAexisting\n")
	before, _ := os.ReadFile(signers)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", goodFP}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	after, _ := os.ReadFile(signers)
	if string(before) != string(after) {
		t.Fatal("allowed_signers changed with no --pub")
	}
	if s := readCfg(t, cfg); s.Roles[goodFP] != "implementer" {
		t.Fatal("role binding not written")
	}
}

// 13. atomic write → after add-role the config parses cleanly.
func TestAddRole_AtomicParses(t *testing.T) {
	_, cfg, _ := seedRepo(t, "{}", "")
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", goodFP}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if _, err := config.LoadFile(cfg); err != nil {
		t.Fatalf("config does not parse after write: %v", err)
	}
	// No temp files left behind in repos.d.
	dir := filepath.Dir(cfg)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

// 14. post-write validation surfaces problems → unreadable allowed_signers path.
func TestAddRole_PostWriteValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", dir)
	cfg := filepath.Join(dir, "myrepo.json")
	body := `{"format_version":1,"default_branch":"main","allowed_signers":"/no/such/allowed_signers","roles":{}}`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// No --pub, so allowed_signers is not created; Validate sees it unreadable.
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", goodFP}); rc == 0 {
		t.Fatal("expected non-zero from post-write validation")
	}
}

// 15. first append creates a missing allowed_signers file (mode 0644).
func TestAddRole_CreatesMissingSigners(t *testing.T) {
	dir := t.TempDir()
	pub, fp := genKey(t, dir, "impl")
	reposDir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	signers := filepath.Join(reposDir, "sub", "allowed_signers") // parent missing too
	cfg := filepath.Join(reposDir, "myrepo.json")
	body := `{"format_version":1,"default_branch":"main","allowed_signers":"` + signers + `","roles":{}}`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", fp, "--pub", pub}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	fi, err := os.Stat(signers)
	if err != nil {
		t.Fatalf("allowed_signers not created: %v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(signers)
	if strings.Count(string(b), "namespaces=\"git\"") != 1 {
		t.Fatalf("want 1 line, got:\n%s", b)
	}
}

// 16. unreadable/malformed --pub file → exit 1, nothing modified.
func TestAddRole_BadPubFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "not-a-key")
	if err := os.WriteFile(bad, []byte("this is not a public key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, cfg, signers := seedRepo(t, "{}", "")
	beforeCfg, _ := os.ReadFile(cfg)
	beforeSig, _ := os.ReadFile(signers)
	// nonexistent path
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", goodFP, "--pub", filepath.Join(dir, "nope.pub")}); rc != 1 {
		t.Fatalf("nonexistent pub: rc = %d, want 1", rc)
	}
	// malformed key file
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", goodFP, "--pub", bad}); rc != 1 {
		t.Fatalf("malformed pub: rc = %d, want 1", rc)
	}
	afterCfg, _ := os.ReadFile(cfg)
	afterSig, _ := os.ReadFile(signers)
	if string(beforeCfg) != string(afterCfg) || string(beforeSig) != string(afterSig) {
		t.Fatal("bad --pub must not modify config or allowed_signers")
	}
}

// 17. overwrite onto identity-only role with a trusted key is refused.
func TestAddRole_OverwriteToMergerTrusted(t *testing.T) {
	dir := t.TempDir()
	pub, fp := genKey(t, dir, "impl")
	// Seed: fp is bound to a signing role AND trusted in allowed_signers.
	pubBytes, _ := os.ReadFile(pub)
	f := strings.Fields(string(pubBytes))
	signerLine := "implementer namespaces=\"git\" " + f[0] + " " + f[1] + "\n"
	_, cfg, signers := seedRepo(t, `{"`+fp+`":"implementer"}`, signerLine)
	beforeCfg, _ := os.ReadFile(cfg)
	beforeSig, _ := os.ReadFile(signers)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "merger", "--fingerprint", fp}); rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	afterCfg, _ := os.ReadFile(cfg)
	afterSig, _ := os.ReadFile(signers)
	if string(beforeCfg) != string(afterCfg) {
		t.Fatal("roles must be left unchanged")
	}
	if string(beforeSig) != string(afterSig) {
		t.Fatal("allowed_signers must be left unchanged")
	}
	if s := readCfg(t, cfg); s.Roles[fp] != "implementer" {
		t.Fatalf("binding changed to %q", s.Roles[fp])
	}
}

// 18. an overwrite preserves every non-roles field's value (content_rules, upstream_*).
func TestAddRole_PreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", dir)
	signers := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(signers, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "myrepo.json")
	const contentRules = `{"version":1,"structural":{"rules":[{"name":"protect-gate","paths":["gate/**"],"operations":["delete"],"roles":{"not_in":["reviewer"]},"effect":"deny"}]}}`
	body := `{"format_version":1,"default_branch":"main","allowed_signers":"` + signers + `",` +
		`"upstream_remote":"origin","upstream_slug":"acme/widgets",` +
		`"content_rules":` + contentRules + `,` +
		`"roles":{"` + goodFP + `":"reviewer"}}`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Overwrite the existing binding (reviewer -> implementer) so the file is rewritten.
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", goodFP}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	// The roles field changed as intended.
	s := readCfg(t, cfg)
	if s.Roles[goodFP] != "implementer" {
		t.Fatalf("roles[fp] = %q, want implementer", s.Roles[goodFP])
	}
	// Every other field's value survived intact.
	if s.UpstreamRemote != "origin" || s.UpstreamSlug != "acme/widgets" {
		t.Fatalf("upstream fields lost: remote=%q slug=%q", s.UpstreamRemote, s.UpstreamSlug)
	}
	if s.DefaultBranch != "main" || s.AllowedSigners != signers {
		t.Fatalf("gate fields changed: %+v", s)
	}
	if s.Content == nil || s.Content.Structural == nil || len(s.Content.Structural.Rules) != 1 ||
		s.Content.Structural.Rules[0].Name != "protect-gate" {
		t.Fatalf("content_rules not preserved: %+v", s.Content)
	}
	// The content_rules value is byte-identical (RawMessage round-trip), even though
	// top-level key order/whitespace may differ.
	raw, _ := os.ReadFile(cfg)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	var got, want interface{}
	_ = json.Unmarshal(obj["content_rules"], &got)
	_ = json.Unmarshal([]byte(contentRules), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("content_rules value drifted: got %s", obj["content_rules"])
	}
}

// 19. the identity-only rebind guard is not fooled by a keytype-shaped principal.
// A signing role literally named "sk-agent" (passes roleNameRe; "sk-" is a real
// OpenSSH FIDO keytype prefix) writes its principal as field[0] of the signer line.
// A naive "first keytype token" scan would read the principal AS the keytype, fail to
// fingerprint the real key, and wrongly report the fp untrusted — letting the merger
// rebind slip through. Positional parsing (principal = field[0]) must still detect it.
func TestAddRole_MergerGuardKeytypeShapedPrincipal(t *testing.T) {
	dir := t.TempDir()
	pub, fp := genKey(t, dir, "agent")
	pubBytes, _ := os.ReadFile(pub)
	f := strings.Fields(string(pubBytes))
	// Principal name is keytype-shaped ("sk-agent"); the real key follows.
	signerLine := "sk-agent namespaces=\"git\" " + f[0] + " " + f[1] + "\n"
	_, cfg, signers := seedRepo(t, `{"`+fp+`":"sk-agent"}`, signerLine)
	beforeCfg, _ := os.ReadFile(cfg)
	beforeSig, _ := os.ReadFile(signers)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "merger", "--fingerprint", fp}); rc != 1 {
		t.Fatalf("rc = %d, want 1 (guard must refuse rebinding a trusted key to merger)", rc)
	}
	afterCfg, _ := os.ReadFile(cfg)
	afterSig, _ := os.ReadFile(signers)
	if string(beforeCfg) != string(afterCfg) || string(beforeSig) != string(afterSig) {
		t.Fatal("refused rebind must leave config and allowed_signers untouched")
	}
	if s := readCfg(t, cfg); s.Roles[fp] != "sk-agent" {
		t.Fatalf("binding changed to %q", s.Roles[fp])
	}
}

// Rebinding a fingerprint NOT in allowed_signers to merger is an ordinary overwrite.
func TestAddRole_OverwriteToMergerUntrusted(t *testing.T) {
	_, cfg, _ := seedRepo(t, `{"`+goodFP+`":"implementer"}`, "")
	if rc := addRole([]string{"--repo", "myrepo", "--role", "merger", "--fingerprint", goodFP}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if s := readCfg(t, cfg); s.Roles[goodFP] != "merger" {
		t.Fatalf("roles[fp] = %q, want merger", s.Roles[goodFP])
	}
}
