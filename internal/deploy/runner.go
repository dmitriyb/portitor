package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runner runs one docker or nix command and returns its stdout. An error
// carries the command's stderr.
type Runner interface {
	Run(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error)
}

// scrubbedVars never reach a docker or nix child: the PAT travels only as the
// copied-in file.
var scrubbedVars = []string{"GH_TOKEN", "GITHUB_TOKEN"}

func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, s := range scrubbedVars {
			if k == s {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// ExecRunner runs the real binaries.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = scrubEnv(os.Environ())
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, firstArgs(args), err, tail(errb.String(), 4096))
	}
	return out.Bytes(), nil
}

// TokenCommand runs the operator's token_command with the operator's own
// environment, its stderr passed through, and returns its stdout.
func TokenCommand(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("token_command %s: %w", argv[0], err)
	}
	return out, nil
}

func firstArgs(args []string) string {
	if len(args) > 2 {
		args = args[:2]
	}
	return strings.Join(args, " ")
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
