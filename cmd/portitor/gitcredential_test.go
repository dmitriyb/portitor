package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/credential"
)

func pointCredentialFile(t *testing.T, content *string) {
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

func runGitCredential(t *testing.T, op, req string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := gitCredential([]string{op}, strings.NewReader(req), &out, &errb)
	return code, out.String(), errb.String()
}

func TestGitCredentialAnswersGitHubFromFile(t *testing.T) {
	tok := "ghp_gate\n"
	pointCredentialFile(t, &tok)
	code, out, _ := runGitCredential(t, "get", "protocol=https\nhost=github.com\npath=o/r.git\n\n")
	if code != 0 || out != "username=x-access-token\npassword=ghp_gate\n" {
		t.Fatalf("got code %d, out %q", code, out)
	}
}

func TestGitCredentialIgnoresOtherHosts(t *testing.T) {
	tok := "ghp_gate"
	pointCredentialFile(t, &tok)
	for _, req := range []string{"protocol=https\nhost=example.com\n", "protocol=http\nhost=github.com\n", "protocol=https\nhost=github.com.evil\n"} {
		code, out, _ := runGitCredential(t, "get", req)
		if code != 0 || out != "" {
			t.Fatalf("request %q: got code %d, out %q; want 0 and no answer", req, code, out)
		}
	}
}

func TestGitCredentialFailsWithoutUsableFile(t *testing.T) {
	empty := ""
	for name, c := range map[string]*string{"absent": nil, "empty": &empty} {
		t.Run(name, func(t *testing.T) {
			pointCredentialFile(t, c)
			code, out, errs := runGitCredential(t, "get", "protocol=https\nhost=github.com\n")
			if code != 1 || out != "" || errs == "" {
				t.Fatalf("got code %d, out %q, stderr %q; want 1, no answer, a diagnostic", code, out, errs)
			}
		})
	}
}

func TestGitCredentialStoreAndEraseAreNoOps(t *testing.T) {
	pointCredentialFile(t, nil)
	for _, op := range []string{"store", "erase"} {
		if code, out, _ := runGitCredential(t, op, "protocol=https\nhost=github.com\nusername=x\npassword=y\n"); code != 0 || out != "" {
			t.Fatalf("%s: got code %d, out %q", op, code, out)
		}
	}
}

func TestGitCredentialUsage(t *testing.T) {
	if code, _, _ := runGitCredential(t, "frobnicate", ""); code != 2 {
		t.Fatalf("unknown op: code %d, want 2", code)
	}
	var out, errb bytes.Buffer
	if code := gitCredential(nil, strings.NewReader(""), &out, &errb); code != 2 {
		t.Fatalf("no op: code %d, want 2", code)
	}
}
