package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestSignerLineGrammar pins parseSignerLine against the consumer's grammar.
func TestSignerLineGrammar(t *testing.T) {
	if _, ok := parseSignerLine(""); ok {
		t.Error("blank line is not an entry")
	}
	if _, ok := parseSignerLine("# comment ssh-ed25519 AAAA"); ok {
		t.Error("comment is not an entry")
	}
	if _, ok := parseSignerLine("principal-only"); ok {
		t.Error("line with no key blob is not an entry")
	}
	e, ok := parseSignerLine(`role namespaces="git" ssh-ed25519 KEYDATA`)
	if !ok || !e.live() || e.keyData != "KEYDATA" {
		t.Fatalf("plain git entry should be live: %+v ok=%v", e, ok)
	}
	e, _ = parseSignerLine(`role namespaces="file,ssh" ssh-ed25519 KEYDATA`)
	if e.gitNamespace || e.live() {
		t.Fatalf("non-git namespaces must not be live: %+v", e)
	}
	e, _ = parseSignerLine(`role namespaces="file,GIT" ssh-ed25519 KEYDATA`)
	if !e.gitNamespace {
		t.Fatalf("namespace matching is case-insensitive: %+v", e)
	}
	e, _ = parseSignerLine(`role valid-before="20301231" ssh-ed25519 KEYDATA`)
	if !e.timeBoxed || e.live() {
		t.Fatalf("time-boxed entry must not count as live: %+v", e)
	}
	e, _ = parseSignerLine(`role cert-authority ssh-ed25519 KEYDATA`)
	if !e.certAuthority || e.live() {
		t.Fatalf("cert-authority must be flagged: %+v", e)
	}
	// cert-authority in the standard comma-joined options form must still flag.
	e, _ = parseSignerLine(`ca cert-authority,namespaces="git" ssh-ed25519 KEYDATA`)
	if !e.certAuthority {
		t.Fatalf("comma-joined cert-authority must be flagged: %+v", e)
	}
	// A quoted namespaces value containing a comma stays one option.
	e, _ = parseSignerLine(`role namespaces="git,file" ssh-ed25519 KEYDATA`)
	if !e.gitNamespace {
		t.Fatalf("quoted multi-namespace should include git: %+v", e)
	}
	e, _ = parseSignerLine(`role namespaces="file,other" ssh-ed25519 KEYDATA`)
	if e.gitNamespace {
		t.Fatalf("non-git quoted namespaces must not be git: %+v", e)
	}
	// A keytype-shaped principal must not shift the positional parse.
	e, _ = parseSignerLine(`sk-agent namespaces="git" ssh-ed25519 REALKEY`)
	if e.keyType != "ssh-ed25519" || e.keyData != "REALKEY" {
		t.Fatalf("keytype-shaped principal mis-parsed: %+v", e)
	}
}

// TestDedupCountsOnlyLiveEntries (PORT-6a): a key blob visible only in lines
// the consumer ignores — comments, non-git namespaces, validity windows — must
// not suppress a needed append.
func TestDedupCountsOnlyLiveEntries(t *testing.T) {
	dir := t.TempDir()
	pub, fp := genKey(t, dir, "impl")
	pubBytes, _ := os.ReadFile(pub)
	f := strings.Fields(string(pubBytes))
	keyType, keyData := f[0], f[1]

	ignored := strings.Join([]string{
		"# retired: old " + keyType + " " + keyData,
		`other namespaces="file" ` + keyType + " " + keyData,
		`other valid-before="20200101" ` + keyType + " " + keyData,
	}, "\n") + "\n"

	_, _, signers := seedRepo(t, "{}", ignored)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", fp, "--pub", pub}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	b, _ := os.ReadFile(signers)
	if !strings.Contains(string(b), `implementer namespaces="git" `+keyType+" "+keyData) {
		t.Fatalf("the durable live line was not appended despite ignored occurrences:\n%s", b)
	}

	// Re-run: the now-live line dedups; nothing grows.
	before, _ := os.ReadFile(signers)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", fp, "--pub", pub}); rc != 0 {
		t.Fatalf("re-run rc = %d", rc)
	}
	after, _ := os.ReadFile(signers)
	if string(before) != string(after) {
		t.Fatalf("live dedup failed:\n%s", after)
	}
}

// TestGuardRefusesCertAuthority (PORT-6b): a cert-authority entry anywhere makes
// the identity-only rebind guard refuse conservatively — certified keys cannot
// be enumerated.
func TestGuardRefusesCertAuthority(t *testing.T) {
	dir := t.TempDir()
	_, fp := genKey(t, dir, "somekey")
	caLine := `* cert-authority ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGhpZGRlbmNha2V5aGlkZGVuY2FrZXloaWRkZW5jYQ` + "\n"
	_, cfg, _ := seedRepo(t, `{"`+fp+`":"implementer"}`, caLine)
	before, _ := os.ReadFile(cfg)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "merger", "--fingerprint", fp}); rc != 1 {
		t.Fatalf("rc = %d, want 1 (cert-authority must refuse the rebind)", rc)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Fatal("config was modified despite the refusal")
	}
}

// TestGuardRefusesCommaJoinedCertAuthority: the cert-authority option in the
// standard comma-joined form (cert-authority,namespaces="git") must still make
// the guard refuse — it previously failed open on the compound token.
func TestGuardRefusesCommaJoinedCertAuthority(t *testing.T) {
	dir := t.TempDir()
	_, fp := genKey(t, dir, "somekey")
	caLine := `ca cert-authority,namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGhpZGRlbmNha2V5aGlkZGVuY2FrZXloaWRkZW5jYQ` + "\n"
	_, cfg, _ := seedRepo(t, `{"`+fp+`":"implementer"}`, caLine)
	before, _ := os.ReadFile(cfg)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "merger", "--fingerprint", fp}); rc != 1 {
		t.Fatalf("rc = %d, want 1 (comma-joined cert-authority must refuse)", rc)
	}
	if after, _ := os.ReadFile(cfg); string(before) != string(after) {
		t.Fatal("config was modified despite the refusal")
	}
}

// TestGuardSubprocessFailureIsFatal (L-P1b): an entry whose key blob cannot be
// fingerprinted (ssh-keygen failure) must fail the guard loudly — a skipped
// line is exactly a wrongly-passing guard.
func TestGuardSubprocessFailureIsFatal(t *testing.T) {
	dir := t.TempDir()
	_, fp := genKey(t, dir, "somekey")
	garbage := "other ssh-ed25519 not!valid!base64!at!all\n"
	_, cfg, _ := seedRepo(t, `{"`+fp+`":"implementer"}`, garbage)
	before, _ := os.ReadFile(cfg)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "merger", "--fingerprint", fp}); rc != 1 {
		t.Fatalf("rc = %d, want 1 (unfingerprintable entry must be fatal, not skipped)", rc)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Fatal("config was modified despite the failure")
	}
}

// TestGuardIsBroad: the rebind guard counts entries the DEDUP would ignore
// (time-boxed, non-git namespaces) — broad where dedup is narrow, because the
// guard's failure direction is a wrongly-permitted rebind.
func TestGuardIsBroad(t *testing.T) {
	dir := t.TempDir()
	pub, fp := genKey(t, dir, "somekey")
	pubBytes, _ := os.ReadFile(pub)
	f := strings.Fields(string(pubBytes))
	boxed := `other valid-before="20991231" ` + f[0] + " " + f[1] + "\n"
	_, cfg, _ := seedRepo(t, `{"`+fp+`":"implementer"}`, boxed)
	before, _ := os.ReadFile(cfg)
	if rc := addRole([]string{"--repo", "myrepo", "--role", "merger", "--fingerprint", fp}); rc != 1 {
		t.Fatalf("rc = %d, want 1 (a time-boxed trusted entry still blocks the rebind)", rc)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Fatal("config was modified despite the refusal")
	}
}

// TestNoIdentityOnlyRolesMeansAllSigning: with identity_only_roles absent,
// every role is a signing role — --pub works even for a role named "merger"
// (classification is config, not code; portitor ships no role names).
func TestNoIdentityOnlyRolesMeansAllSigning(t *testing.T) {
	dir := t.TempDir()
	pub, fp := genKey(t, dir, "m")
	reposDir := t.TempDir()
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	signers := filepath.Join(reposDir, "allowed_signers")
	if err := os.WriteFile(signers, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	body := `{"format_version":1,"default_branch":"main","allowed_signers":"` + signers + `","roles":{}}`
	if err := os.WriteFile(filepath.Join(reposDir, "myrepo.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := addRole([]string{"--repo", "myrepo", "--role", "merger", "--fingerprint", fp, "--pub", pub}); rc != 0 {
		t.Fatalf("rc = %d, want 0 (no identity_only_roles declared => signing role)", rc)
	}
	b, _ := os.ReadFile(signers)
	if !strings.Contains(string(b), "merger ") {
		t.Fatalf("signer line missing:\n%s", b)
	}
}

// TestAddRoleConcurrent (PORT-7a / L-P2f): concurrent runs serialize under the
// lock — no binding is lost to a read-modify-write race.
func TestAddRoleConcurrent(t *testing.T) {
	_, cfg, _ := seedRepo(t, "{}", "")
	const n = 8
	fps := make([]string, n)
	for i := range fps {
		fps[i] = fmt.Sprintf("SHA256:%c%s", 'B'+i, strings.Repeat("A", 42))
	}
	var wg sync.WaitGroup
	for _, fp := range fps {
		wg.Add(1)
		go func(fp string) {
			defer wg.Done()
			if rc := addRole([]string{"--repo", "myrepo", "--role", "implementer", "--fingerprint", fp}); rc != 0 {
				t.Errorf("fp %s: rc = %d", fp, rc)
			}
		}(fp)
	}
	wg.Wait()
	s := readCfg(t, cfg)
	for _, fp := range fps {
		if s.Roles[fp] != "implementer" {
			t.Errorf("binding for %s lost (roles=%v)", fp, s.Roles)
		}
	}
}
