package check

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// PredicateUnmetError means the command ran and exited non-zero: a merge-gate
// command predicate (merge_gate.checks) that did not hold — a named,
// fail-closed refusal, distinct from a failure to run the contract at all
// (see RunPredicate).
type PredicateUnmetError struct {
	ExitCode int
	Excerpt  string // the command's stderr (else stdout) head
}

func (e *PredicateUnmetError) Error() string {
	return fmt.Sprintf("predicate command exited %d: %s", e.ExitCode, e.Excerpt)
}

// RunPredicate runs command (an explicit argv, config trust-root material —
// no shell, ever) with extraArgs appended, under the SAME hermetic execution
// contract as Records: the internal-check-exec re-exec trampoline (rlimit,
// minimal PATH/HOME-only environment), a deadline, and bounded output. Unlike
// Records it is a pass/fail predicate, not a record extractor: there is no
// stdin content and no output parsing, only the exit code is consulted, and
// it runs in workdir directly (the bare repo dir for merge_gate checks — a
// predicate may need to inspect repo state — rather than Records' private
// throwaway directory, since there is no pushed content to contain).
//
// Exit 0 => nil (met). Nonzero => *PredicateUnmetError (unmet, named in the
// caller's refusal). Any other failure (not spawnable, deadline exceeded,
// output cap exceeded) is a plain operational error — fail-closed, the caller
// must refuse the merge either way.
func RunPredicate(command []string, workdir string, extraArgs ...string) error {
	if len(command) == 0 {
		return fmt.Errorf("check: no command configured")
	}
	if workdir == "" {
		return fmt.Errorf("check: no workdir configured")
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("check: locate own binary for trampoline: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	full := make([]string, 0, len(command)+len(extraArgs))
	full = append(full, command...)
	full = append(full, extraArgs...)
	args := append([]string{"internal-check-exec", workdir}, full...)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.WaitDelay = 5 * time.Second
	stdout := &limitedBuffer{max: maxOutput, what: "stdout"}
	stderr := &limitedBuffer{max: maxStderr, what: "stderr"}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("check: command timed out after %s", timeout)
	}
	if stdout.overflowed {
		return fmt.Errorf("check: command output exceeds %d bytes", maxOutput)
	}
	errText := strings.TrimSpace(stderr.buf.String())
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) && !strings.HasPrefix(errText, TrampolineSentinel) {
			// The configured command itself ran and said no: an unmet predicate.
			excerpt := errText
			if excerpt == "" {
				excerpt = strings.TrimSpace(stdout.buf.String())
			}
			return &PredicateUnmetError{ExitCode: ee.ExitCode(), Excerpt: head(excerpt, 300)}
		}
		return fmt.Errorf("check: run command: %w: %s", runErr, head(errText, 300))
	}
	return nil
}
