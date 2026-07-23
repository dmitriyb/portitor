package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestHelpUniversal pins the point of the cobra migration: `portitor --help`,
// `portitor -h`, and `portitor help` all print usage to stdout and exit 0
// (Execute returns nil), with nothing on stderr.
func TestHelpUniversal(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := newRootCommand()
			var out, errb strings.Builder
			root.SetOut(&out)
			root.SetErr(&errb)
			root.SetArgs(args)

			if err := root.Execute(); err != nil {
				t.Fatalf("Execute(%v) returned error (want nil / exit 0): %v", args, err)
			}
			if errb.Len() != 0 {
				t.Errorf("help wrote to stderr (want stdout only): %q", errb.String())
			}
			got := out.String()
			for _, want := range []string{"portitor", "Usage:", "pre-receive", "add-role", "shell"} {
				if !strings.Contains(got, want) {
					t.Errorf("help missing %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestSubcommandHelp confirms help is universal at every level: `portitor
// <cmd> --help` prints that command's usage (including its flags) to stdout and
// exits 0.
func TestSubcommandHelp(t *testing.T) {
	root := newRootCommand()
	var out, errb strings.Builder
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"add-role", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("add-role --help returned error: %v", err)
	}
	if errb.Len() != 0 {
		t.Errorf("add-role --help wrote to stderr: %q", errb.String())
	}
	got := out.String()
	for _, want := range []string{"add-role", "--repo", "--fingerprint", "--pub"} {
		if !strings.Contains(got, want) {
			t.Errorf("add-role help missing %q:\n%s", want, got)
		}
	}
}

// TestVersionForms preserves the released version contract: `version`,
// `--version`, and `-v` all print the same "portitor <v> (commit <c>, built
// <d>)" line to stdout and exit 0.
func TestVersionForms(t *testing.T) {
	origV, origC, origD := version, commit, date
	t.Cleanup(func() { version, commit, date = origV, origC, origD })
	version, commit, date = "v0.1.0", "abc1234", "2026-07-22"
	want := "portitor v0.1.0 (commit abc1234, built 2026-07-22)\n"

	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := newRootCommand()
			var out, errb strings.Builder
			root.SetOut(&out)
			root.SetErr(&errb)
			root.SetArgs(args)

			if err := root.Execute(); err != nil {
				t.Fatalf("Execute(%v): %v", args, err)
			}
			if got := out.String(); got != want {
				t.Errorf("Execute(%v) stdout = %q, want %q", args, got, want)
			}
			if errb.Len() != 0 {
				t.Errorf("Execute(%v) stderr = %q, want empty", args, errb.String())
			}
		})
	}
}

// TestUnknownCommandIsUsageError locks the usage-error exit-code decision: an
// unknown subcommand is a cobra usage error (not a command-supplied *exitError),
// which main maps to exit 2 — the hand-rolled dispatcher's code.
func TestUnknownCommandIsUsageError(t *testing.T) {
	root := newRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"definitely-not-a-command"})

	err := root.Execute()
	if err == nil {
		t.Fatal("an unknown subcommand must error")
	}
	var ee *exitError
	if errors.As(err, &ee) {
		t.Fatalf("unknown subcommand must be a cobra usage error (→ exit 2), got *exitError code %d", ee.code)
	}
}
