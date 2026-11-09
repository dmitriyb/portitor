package credential

import (
	"os"
	"path/filepath"
	"testing"
)

func withFile(t *testing.T, content *string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh_token")
	if content != nil {
		if err := os.WriteFile(p, []byte(*content), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	old := Path
	Path = p
	t.Cleanup(func() { Path = old })
}

func str(s string) *string { return &s }

func TestReadAbsentIsNotAnError(t *testing.T) {
	withFile(t, nil)
	tok, present, err := Read()
	if err != nil || present || tok != "" {
		t.Fatalf("absent file: got (%q, %v, %v), want (\"\", false, nil)", tok, present, err)
	}
}

func TestReadStripsOneTrailingNewline(t *testing.T) {
	withFile(t, str("ghp_abc\n"))
	tok, present, err := Read()
	if err != nil || !present || tok != "ghp_abc" {
		t.Fatalf("got (%q, %v, %v), want (\"ghp_abc\", true, nil)", tok, present, err)
	}
}

func TestReadRefusesEmptyAndMultiLine(t *testing.T) {
	for name, c := range map[string]string{"empty": "", "newline only": "\n", "two lines": "a\nb", "cr": "a\rb"} {
		t.Run(name, func(t *testing.T) {
			withFile(t, str(c))
			if _, present, err := Read(); err == nil || !present {
				t.Fatalf("content %q: got present=%v err=%v, want present and an error", c, present, err)
			}
		})
	}
}
