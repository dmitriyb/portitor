package action

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dmitriyb/portitor/internal/credential"
)

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
	t.Cleanup(func() { credential.Path = old })
}

func TestGHEnvAddsTokenFromCredentialFile(t *testing.T) {
	tok := "ghp_file"
	pointCredential(t, &tok)
	env, err := ghEnv([]string{"PATH=/bin", "GH_TOKEN=ambient"})
	if err != nil {
		t.Fatal(err)
	}
	if env[len(env)-1] != "GH_TOKEN=ghp_file" {
		t.Fatalf("last env entry = %q, want GH_TOKEN=ghp_file (last wins in os/exec)", env[len(env)-1])
	}
}

func TestGHEnvLeavesAmbientWithoutCredentialFile(t *testing.T) {
	pointCredential(t, nil)
	base := []string{"PATH=/bin", "HOME=/home/op"}
	env, err := ghEnv(base)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(env, base) {
		t.Fatalf("env = %v, want the base unchanged %v", env, base)
	}
}

func TestGHEnvFailsOnEmptyCredentialFile(t *testing.T) {
	empty := ""
	pointCredential(t, &empty)
	if _, err := ghEnv(nil); err == nil {
		t.Fatal("empty credential file: want an error, got nil")
	}
}
