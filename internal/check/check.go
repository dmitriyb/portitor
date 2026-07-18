// Package check runs the operator-configured record-extraction command for
// semantic content rules — the generic seam any script, tool wrapper, or
// service client can fill. Portitor knows only the contract (see
// spec/gate/arch_content_rules.md): content goes in as data (a materialized
// file or stdin — never argv, never a shell), records come out as JSON at a
// config-declared path, keyed by a config-declared id field. No tool identity
// exists in this package.
//
// Containment: config-fixed argv, a 30s deadline, an address-space rlimit
// applied by the `portitor internal-check-exec` re-exec trampoline, a minimal
// environment, bounded output, and a private throwaway working directory.
package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmitriyb/portitor/internal/rules"
)

const (
	// MaxInput bounds a protected file's blob size; the gate checks it before
	// reading the blob, and Records double-guards.
	MaxInput = 20 << 20
	// MemLimit is the address-space cap the trampoline applies to the check
	// command (child-only; portitor's own limits are untouched).
	MemLimit = 512 << 20
	// maxOutput bounds the command's stdout; exceeding it aborts the run.
	maxOutput = 64 << 20
	// maxStderr bounds captured stderr (diagnostics only).
	maxStderr = 64 << 10
	// timeout bounds the whole invocation.
	timeout = 30 * time.Second
)

// TrampolineSentinel prefixes every diagnostic the internal-check-exec
// trampoline emits before it execs the configured command. It lets Records
// distinguish "the contract could not be run" (operational) from "the command
// ran and rejected the content" (a content verdict).
const TrampolineSentinel = "internal-check-exec:"

// ContentRejectedError means the check command ran and exited non-zero: a
// fail-closed content verdict the gate surfaces as a push violation, distinct
// from a failure to run the contract at all.
type ContentRejectedError struct {
	ExitCode int
	Excerpt  string // the command's stderr (else stdout) head
}

func (e *ContentRejectedError) Error() string {
	return fmt.Sprintf("check command rejected the content (exit %d): %s", e.ExitCode, e.Excerpt)
}

// Records runs the configured check command over one side's content and
// returns the full field map per record, keyed by the configured id field.
// The error is *ContentRejectedError when the command exited non-zero; any
// other error is operational (not spawnable, deadline, output cap, output not
// matching the declared shape) — both directions reject the push.
func Records(def rules.CheckDef, content []byte) (map[string]rules.Record, error) {
	if len(def.Command) == 0 {
		return nil, fmt.Errorf("check: no command configured")
	}
	if len(content) > MaxInput {
		return nil, fmt.Errorf("check: input is %d bytes, cap %d", len(content), MaxInput)
	}
	dir, err := os.MkdirTemp("", "portitor-check-*")
	if err != nil {
		return nil, fmt.Errorf("check: workdir: %w", err)
	}
	defer os.RemoveAll(dir)

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("check: locate own binary for trampoline: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := append([]string{"internal-check-exec", dir}, def.Command...)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.WaitDelay = 5 * time.Second
	stdout := &limitedBuffer{max: maxOutput, what: "stdout"}
	stderr := &limitedBuffer{max: maxStderr, what: "stderr"}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if f := def.InputFile; f != "" {
		// Re-assert the compile-time guarantee before touching the filesystem.
		clean := filepath.Clean(f)
		if filepath.IsAbs(f) || clean != f || clean == "." || strings.HasPrefix(clean, "..") {
			return nil, fmt.Errorf("check: input_file %q escapes the working directory", f)
		}
		p := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, fmt.Errorf("check: materialize input: %w", err)
		}
		if err := os.WriteFile(p, content, 0o600); err != nil {
			return nil, fmt.Errorf("check: materialize input: %w", err)
		}
	} else {
		cmd.Stdin = bytes.NewReader(content)
	}

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("check: command timed out after %s", timeout)
	}
	if stdout.overflowed {
		return nil, fmt.Errorf("check: command output exceeds %d bytes", maxOutput)
	}
	errText := strings.TrimSpace(stderr.buf.String())
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) && !strings.HasPrefix(errText, TrampolineSentinel) {
			// The configured command itself ran and said no: a content verdict.
			excerpt := errText
			if excerpt == "" {
				excerpt = strings.TrimSpace(stdout.buf.String())
			}
			return nil, &ContentRejectedError{ExitCode: ee.ExitCode(), Excerpt: head(excerpt, 300)}
		}
		return nil, fmt.Errorf("check: run command: %w: %s", runErr, head(errText, 300))
	}
	return parseRecords(def, stdout.buf.Bytes())
}

// parseRecords extracts the record array at the declared records_path and keys
// each record by the declared id field. Any shape surprise is an operational
// error — the contract was not met, and fail-closed means reject.
func parseRecords(def rules.CheckDef, out []byte) (map[string]rules.Record, error) {
	var root any
	if err := json.Unmarshal(out, &root); err != nil {
		return nil, fmt.Errorf("check: command output is not JSON: %v", err)
	}
	node := root
	for _, key := range def.RecordsKeys() {
		obj, ok := node.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("check: records_path %q: %q is not reachable in the output", def.RecordsPath, key)
		}
		node, ok = obj[key]
		if !ok {
			return nil, fmt.Errorf("check: records_path %q: key %q missing from the output", def.RecordsPath, key)
		}
	}
	arr, ok := node.([]any)
	if !ok {
		return nil, fmt.Errorf("check: records_path %q does not lead to an array", def.RecordsPath)
	}
	idField := def.KeyField()
	records := make(map[string]rules.Record, len(arr))
	for i, el := range arr {
		rec, ok := el.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("check: record %d is not an object", i)
		}
		id, ok := rec[idField].(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("check: record %d: missing or non-string %q", i, idField)
		}
		if _, dup := records[id]; dup {
			return nil, fmt.Errorf("check: duplicate record id %q in output", id)
		}
		records[id] = rec
	}
	return records, nil
}

func head(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// limitedBuffer caps how much a subprocess may write; the overflow aborts the
// copy (cmd.Run returns an error) instead of buffering unboundedly.
type limitedBuffer struct {
	buf        bytes.Buffer
	max        int64
	n          int64
	what       string
	overflowed bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.n += int64(len(p))
	if b.n > b.max {
		b.overflowed = true
		return 0, fmt.Errorf("check %s exceeds %d bytes", b.what, b.max)
	}
	return b.buf.Write(p)
}
