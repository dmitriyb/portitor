package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/check"
	"github.com/dmitriyb/portitor/internal/rules"
)

// TestMain intercepts the internal-check-exec re-exec: check.Records spawns
// os.Executable() — in tests, this test binary — so the trampoline entry must
// be handled before the test runner takes over.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "internal-check-exec" {
		os.Exit(check.TrampolineMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// contentEnv is a two-identity setup (implementer + reviewer, both trusted
// signers) with helpers for committing per identity.
type contentEnv struct {
	t          *testing.T
	dir        string
	bare, work string
	implKey    string
	revKey     string
	cfg        Config
}

func newContentEnv(t *testing.T, content *rules.ContentRules) *contentEnv {
	t.Helper()
	requireBins(t, "git", "ssh-keygen")
	dir := t.TempDir()
	e := &contentEnv{t: t, dir: dir,
		bare: filepath.Join(dir, "bare.git"), work: filepath.Join(dir, "work"),
		implKey: filepath.Join(dir, "impl"), revKey: filepath.Join(dir, "rev")}
	mustGit(t, dir, "init", "--bare", "--initial-branch=main", e.bare)
	mustGit(t, dir, "init", "--initial-branch=main", e.work)
	genKey(t, e.implKey, "impl@example.com")
	genKey(t, e.revKey, "reviewer@example.com")
	allowed := filepath.Join(dir, "allowed_signers")
	writeSigners(t, allowed, map[string]string{
		"impl@example.com":     e.implKey + ".pub",
		"reviewer@example.com": e.revKey + ".pub",
	})
	e.cfg = Config{
		AllowedSigners: allowed,
		Roles: map[string]string{
			fingerprint(t, e.implKey+".pub"): "implementer",
			fingerprint(t, e.revKey+".pub"):  "reviewer",
		},
		Content: content,
	}
	mustGit(t, e.work, "remote", "add", "origin", e.bare)
	return e
}

func (e *contentEnv) head() string {
	return strings.TrimSpace(mustGitOut(e.t, e.work, "rev-parse", "HEAD"))
}

func (e *contentEnv) identity(email, key string) {
	e.t.Helper()
	for _, c := range [][]string{
		{"config", "user.name", email},
		{"config", "user.email", email},
		{"config", "gpg.format", "ssh"},
		{"config", "user.signingkey", key},
		{"config", "commit.gpgsign", "true"},
	} {
		mustGit(e.t, e.work, c...)
	}
}

func (e *contentEnv) write(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.work, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *contentEnv) commitAll(msg string) string {
	e.t.Helper()
	mustGit(e.t, e.work, "add", "-A")
	mustGit(e.t, e.work, "commit", "-m", msg)
	return e.head()
}

// catCheck is the simplest possible seam filler: the repo file's content IS
// the record envelope, and the check command is plain `cat`. Proving the
// contract takes any filler is the point — no specific tool exists in this
// repo.
func catCheck() rules.CheckDef {
	return rules.CheckDef{
		Command:     []string{"cat", "records.json"},
		InputFile:   "records.json",
		RecordsPath: "records",
	}
}

// semanticGateRules: "moving a record's stage to approved requires
// reviewer/owner" — the canonical gate-form rule, all names from config.
func semanticGateRules() *rules.ContentRules {
	return &rules.ContentRules{
		Version: 1,
		Semantic: &rules.SemanticRules{Files: []rules.SemanticFile{{
			Path:  "registry/records.json",
			Check: catCheck(),
			Rules: []rules.SemanticRule{{
				Name:   "approval-requires-review",
				Match:  rules.Matcher{Type: "field", Field: "stage", To: "approved", HasTo: true},
				Roles:  &rules.RolePredicate{NotIn: []string{"reviewer", "owner"}},
				Effect: "deny",
			}},
		}}},
	}
}

// TestSemanticRules verifies fingerprint→role attribution + a semantic
// transition rule computed over check-command record deltas. Role is derived
// from the signing key, not a forgeable label.
func TestSemanticRules(t *testing.T) {
	e := newContentEnv(t, semanticGateRules())

	const draft = `{"records":[{"id":"r-1","stage":"draft","title":"task","noise":"a"}]}`
	const approved = `{"records":[{"id":"r-1","stage":"approved","title":"task","noise":"b"}]}`

	// Base: a DRAFT record on main, committed by the implementer.
	e.identity("impl@example.com", e.implKey)
	e.write("registry/records.json", draft)
	base := e.commitAll("draft record")
	mustGit(t, e.work, "push", "origin", "main")

	// Case A: implementer approves -> rejected.
	mustGit(t, e.work, "checkout", "-b", "impl-approve")
	e.write("registry/records.json", approved)
	implApprove := e.commitAll("approve r-1")
	mustGit(t, e.work, "push", "origin", "impl-approve")

	// Case B: reviewer approves -> accepted.
	mustGit(t, e.work, "checkout", "main")
	mustGit(t, e.work, "checkout", "-b", "rev-approve")
	e.identity("reviewer@example.com", e.revKey)
	e.write("registry/records.json", approved)
	revApprove := e.commitAll("approve r-1")
	mustGit(t, e.work, "push", "origin", "rev-approve")

	// Case C: implementer makes an unrelated change -> no violation.
	mustGit(t, e.work, "checkout", "main")
	mustGit(t, e.work, "checkout", "-b", "impl-code")
	e.identity("impl@example.com", e.implKey)
	e.write("code.go", "package x\n")
	implCode := e.commitAll("add code")
	mustGit(t, e.work, "push", "origin", "impl-code")

	tests := []struct {
		name string
		u    RefUpdate
		want []string
	}{
		{"implementer approval rejected", RefUpdate{base, implApprove, "refs/heads/impl-approve"}, []string{"approval-requires-review"}},
		{"reviewer approval accepted", RefUpdate{base, revApprove, "refs/heads/rev-approve"}, nil},
		{"implementer unrelated change accepted", RefUpdate{base, implCode, "refs/heads/impl-code"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs, err := Check(e.bare, []RefUpdate{tc.u}, e.cfg)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			assertRules(t, vs, tc.want)
		})
	}
}

func writeSigners(t *testing.T, path string, principalToPub map[string]string) {
	t.Helper()
	var b strings.Builder
	for principal, pubPath := range principalToPub {
		pub, err := os.ReadFile(pubPath)
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(string(pub))
		if len(f) < 2 {
			t.Fatalf("bad public key %q", pubPath)
		}
		b.WriteString(principal + ` namespaces="git" ` + f[0] + " " + f[1] + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fingerprint returns the key's SHA256 fingerprint as git reports it via %GF.
func fingerprint(t *testing.T, pubPath string) string {
	t.Helper()
	out, err := exec.Command("ssh-keygen", "-lf", pubPath).Output()
	if err != nil {
		t.Fatalf("ssh-keygen -lf: %v", err)
	}
	f := strings.Fields(string(out)) // "256 SHA256:xxxx comment (ED25519)"
	if len(f) < 2 {
		t.Fatalf("unexpected ssh-keygen -lf output: %q", out)
	}
	return f[1]
}
