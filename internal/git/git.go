// Package git is the single wrapper around the git CLI used across portitor (the
// gate, the forwarder, and the command layer). Centralizing it gives one place for
// stdout/stderr capture, error formatting, and the hardening every portitor git
// call shares: replace-object substitution disabled, a per-call deadline, and
// (for the gate's fact-gathering) ambient-config isolation.
package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds local git subprocesses. A hung subprocess must never
// block a push indefinitely; the caller's error direction is rejection.
const DefaultTimeout = 60 * time.Second

// NetworkTimeout bounds git subprocesses that talk to a remote (push/fetch).
const NetworkTimeout = 5 * time.Minute

// Output runs `git -c core.useReplaceRefs=false [-C dir] <args...>` under
// DefaultTimeout and returns stdout. On failure the error wraps the captured
// stderr. A blank dir runs git in the current working directory.
//
// Replace-object substitution is disabled on every call: a refs/replace/* ref
// present in the repo must never make portitor read a substituted object in
// place of the one actually pushed.
func Output(dir string, args ...string) (string, error) {
	return run(dir, DefaultTimeout, false, args...)
}

// OutputNetwork is Output with NetworkTimeout — for push/fetch.
func OutputNetwork(dir string, args ...string) (string, error) {
	return run(dir, NetworkTimeout, false, args...)
}

// OutputNetworkRun is OutputNetwork with stdout discarded — for network git
// commands run only for their effect (fetch).
func OutputNetworkRun(dir string, args ...string) error {
	_, err := run(dir, NetworkTimeout, false, args...)
	return err
}

// OutputHermetic is Output with the global and system git config masked
// (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM=/dev/null) — for the gate's
// fact-gathering calls, whose verdict must be a function of the push, the repo,
// and portitor's config only, never ambient machine state (an ambient
// ~/.gitconfig could otherwise contribute a trust root or a gpg.ssh.program).
// The repo's own config (operator territory) remains honored.
func OutputHermetic(dir string, args ...string) (string, error) {
	return run(dir, DefaultTimeout, true, args...)
}

// Run is Output with stdout discarded — for git commands run only for their effect.
func Run(dir string, args ...string) error {
	_, err := Output(dir, args...)
	return err
}

func run(dir string, timeout time.Duration, hermetic bool, args ...string) (string, error) {
	a := []string{"-c", "core.useReplaceRefs=false"}
	if dir != "" {
		a = append(a, "-C", dir)
	}
	a = append(a, args...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", a...)
	if hermetic {
		// os/exec keeps the last value for a duplicated key, so these override
		// any ambient GIT_CONFIG_* in the parent environment. GIT_CONFIG_COUNT=0
		// also neutralizes GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n env-injected
		// config, so no ambient channel can contribute a verification input.
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_COUNT=0")
	}
	// A child that inherits the pipes must not extend the deadline indefinitely.
	cmd.WaitDelay = 5 * time.Second
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return out.String(), fmt.Errorf("git %s: timed out after %s", strings.Join(args, " "), timeout)
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// ValidRemoteName guards a configured remote name before it reaches git argv:
// non-empty, no leading '-' (would be read as an option), and no whitespace or
// control bytes.
func ValidRemoteName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}
