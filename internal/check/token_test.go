package check

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dmitriyb/portitor/internal/credential"
	"github.com/dmitriyb/portitor/internal/rules"
)

// pointCredential points the credential file at a temp file (nil: absent). The
// trampoline runs in a re-exec of this test binary, so the path travels to it
// through the environment hook TestMain reads.
func pointCredential(t *testing.T, content *string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh_token")
	if content != nil {
		if err := os.WriteFile(p, []byte(*content), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	old := credential.Path
	credential.Path = p
	t.Setenv(testCredentialEnv, p)
	t.Cleanup(func() { credential.Path = old })
}

func TestRunPredicateGetsGateToken(t *testing.T) {
	requireSh(t)
	tok := "ghp_gate"
	pointCredential(t, &tok)
	t.Setenv("GH_TOKEN", "ambient")
	s := script(t, `[ "$GH_TOKEN" = ghp_gate ]`+"\n")
	if err := RunPredicate([]string{s}, t.TempDir()); err != nil {
		t.Fatalf("predicate did not see the gate's token: %v", err)
	}
}

func TestRunPredicateWithoutCredentialFileGetsNoToken(t *testing.T) {
	requireSh(t)
	pointCredential(t, nil)
	t.Setenv("GH_TOKEN", "ambient")
	s := script(t, `[ -z "$GH_TOKEN" ]`+"\n")
	if err := RunPredicate([]string{s}, t.TempDir()); err != nil {
		t.Fatalf("predicate saw a GH_TOKEN with no credential file: %v", err)
	}
}

func TestRunPredicateFailsOnEmptyCredentialFile(t *testing.T) {
	requireSh(t)
	empty := ""
	pointCredential(t, &empty)
	s := script(t, "exit 0\n")
	if err := RunPredicate([]string{s}, t.TempDir()); err == nil {
		t.Fatal("empty credential file: predicate ran, want an operational error")
	}
}

func TestContentCheckNeverGetsGateToken(t *testing.T) {
	requireSh(t)
	tok := "ghp_gate"
	pointCredential(t, &tok)
	s := script(t, `if [ -n "$GH_TOKEN" ]; then echo '[{"id":"leaked"}]'; else echo '[{"id":"clean"}]'; fi`+"\n")
	recs, err := Records(rules.CheckDef{Command: []string{s}}, []byte("{}"))
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if _, ok := recs["clean"]; !ok {
		t.Fatalf("the gate's token reached a content check: %+v", recs)
	}
}
