// Package git is the single wrapper around the git CLI used across portitor (the
// gate, the forwarder, and the command layer). Centralizing it gives one place for
// stdout/stderr capture and error formatting (and a future hook for logging).
package git

import (
	"fmt"
	"os/exec"
	"strings"
)

// Output runs `git [-C dir] <args...>` and returns stdout. On failure the error
// wraps the captured stderr. A blank dir runs git in the current working directory.
func Output(dir string, args ...string) (string, error) {
	a := args
	if dir != "" {
		a = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", a...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Run is Output with stdout discarded — for git commands run only for their effect.
func Run(dir string, args ...string) error {
	_, err := Output(dir, args...)
	return err
}
