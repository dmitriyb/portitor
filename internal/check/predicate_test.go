package check

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRunPredicateMet(t *testing.T) {
	requireSh(t)
	dir := t.TempDir()
	s := script(t, "exit 0\n")
	if err := RunPredicate([]string{s}, dir); err != nil {
		t.Fatalf("exit 0 should be met: %v", err)
	}
}

func TestRunPredicateUnmet(t *testing.T) {
	requireSh(t)
	dir := t.TempDir()
	s := script(t, "echo bead not closed >&2\nexit 3\n")
	err := RunPredicate([]string{s}, dir)
	var ue *PredicateUnmetError
	if !errors.As(err, &ue) {
		t.Fatalf("want *PredicateUnmetError, got %v", err)
	}
	if ue.ExitCode != 3 || !strings.Contains(ue.Excerpt, "bead not closed") {
		t.Fatalf("unmet = %+v", ue)
	}
}

func TestRunPredicateExtraArgs(t *testing.T) {
	requireSh(t)
	dir := t.TempDir()
	// Assert PR number + head SHA land as the final two argv elements.
	s := script(t, `if [ "$2" = "42" ] && [ "$3" = "deadbeef" ]; then exit 0; else echo "argv=$@" >&2; exit 1; fi`+"\n")
	if err := RunPredicate([]string{s, "fixed-arg"}, dir, "42", "deadbeef"); err != nil {
		t.Fatalf("extra args not appended correctly: %v", err)
	}
}

func TestRunPredicateWorkdir(t *testing.T) {
	requireSh(t)
	dir := t.TempDir()
	marker := "predicate-workdir-marker"
	if err := os.WriteFile(dir+"/"+marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := script(t, "test -f "+marker+"\n")
	// The predicate runs IN workdir directly (not a private throwaway dir),
	// unlike Records — a merge_gate check may need to see repo state.
	if err := RunPredicate([]string{s}, dir); err != nil {
		t.Fatalf("predicate should run in workdir: %v", err)
	}
}

func TestRunPredicateOperationalFailures(t *testing.T) {
	dir := t.TempDir()
	t.Run("command not found", func(t *testing.T) {
		err := RunPredicate([]string{"/nonexistent/tool-xyz"}, dir)
		if err == nil {
			t.Fatal("want an error")
		}
		var ue *PredicateUnmetError
		if errors.As(err, &ue) {
			t.Fatalf("an unrunnable command is operational, not an unmet predicate: %v", err)
		}
	})
	t.Run("no command configured", func(t *testing.T) {
		if err := RunPredicate(nil, dir); err == nil {
			t.Fatal("want an error for an empty command")
		}
	})
	t.Run("no workdir configured", func(t *testing.T) {
		if err := RunPredicate([]string{"true"}, ""); err == nil {
			t.Fatal("want an error for an empty workdir")
		}
	})
}

func TestRunPredicateMinimalEnv(t *testing.T) {
	requireSh(t)
	dir := t.TempDir()
	t.Setenv("PORTITOR_TEST_LEAK", "leaked")
	s := script(t, `if [ -n "$PORTITOR_TEST_LEAK" ]; then exit 1; else exit 0; fi`+"\n")
	if err := RunPredicate([]string{s}, dir); err != nil {
		t.Fatalf("environment leaked into the predicate command: %v", err)
	}
}

// TestRunPredicateNoShell confirms the command is exec'd directly (argv, no
// shell interpolation) — a shell metacharacter in an argument must not be
// interpreted.
func TestRunPredicateNoShell(t *testing.T) {
	if _, err := exec.LookPath("echo"); err != nil {
		t.Skip("echo not available")
	}
	dir := t.TempDir()
	// If a shell were involved, "$(echo pwned)" would expand; exec'd directly
	// it is simply printed as a literal argument and the command exits 0
	// (echo always succeeds) — so this only pins there's no crash/injection,
	// the unmet/met assertion belongs to the other tests.
	if err := RunPredicate([]string{"echo", "$(echo pwned)"}, dir); err != nil {
		t.Fatalf("plain echo should succeed: %v", err)
	}
}
