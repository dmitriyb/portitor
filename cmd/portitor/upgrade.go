package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// The canonical installer lives at the repo root (install.sh) — the release
// workflow uploads it and the README documents it. //go:embed cannot traverse
// "..", so a byte-identical copy is kept here and embedded. `go generate
// ./cmd/portitor` refreshes it from the canonical file; TestUpgradeEmbeddedMatchesReleased
// (upgrade_test.go) fails the build if the two ever diverge — that identity is
// the whole security argument: the embedded script is, for a given release,
// the same audited, signed installer a user runs by hand, so there is nothing
// to fetch and nothing to substitute.
//
//go:generate cp ../../install.sh install.sh
//go:embed install.sh
var installScript []byte

// upgradeOptions carries the `upgrade` flags through to the embedded installer.
type upgradeOptions struct {
	check    bool   // --check / --dry-run: report current vs latest, change nothing
	force    bool   // --force: allow a downgrade / skip confirmation
	rollback bool   // --rollback: restore the pre-upgrade binary from <path>.bak
	pinned   string // --version vX.Y.Z: pin the target release (empty = latest)
}

// upgradeRun updates the standalone operator binary in place by running the
// embedded, already-signed install.sh in its upgrade mode against the path of
// the currently-running binary. It is intentionally thin: the resolve /
// download / SSHSIG-verify / safe self-replace logic lives once, in the script.
//
// It does NOT touch the container image, which is a separate artifact rebuilt
// from the Dockerfile (see the command help and docs/deploy.md).
func upgradeRun(o upgradeOptions, stdout, stderr io.Writer) int {
	// Resolve the exact on-disk path of the running binary — the file the
	// script must replace. EvalSymlinks so a symlinked launcher (e.g.
	// /usr/local/bin/portitor -> …) resolves to the real binary, which is what
	// the move-aside + rename must target.
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "upgrade: cannot resolve the running binary path: %v\n", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	} else {
		fmt.Fprintf(stderr, "upgrade: cannot resolve symlinks for %s: %v\n", exe, err)
		return 1
	}

	// Write the embedded installer to a private temp file and run `sh` on it.
	// CreateTemp is 0600 (owner-only), which is all `sh <file>` needs; the file
	// is removed on return regardless of outcome.
	f, err := os.CreateTemp("", "portitor-upgrade-*.sh")
	if err != nil {
		fmt.Fprintf(stderr, "upgrade: cannot create the installer temp file: %v\n", err)
		return 1
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(installScript); err != nil {
		f.Close()
		fmt.Fprintf(stderr, "upgrade: cannot write the installer temp file: %v\n", err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(stderr, "upgrade: cannot finalize the installer temp file: %v\n", err)
		return 1
	}

	args := []string{tmp, "--upgrade", "--target", exe}
	if o.check {
		args = append(args, "--check")
	}
	if o.force {
		args = append(args, "--force")
	}
	if o.rollback {
		args = append(args, "--rollback")
	}
	// Pass our compiled-in version as the authoritative "current" so the
	// downgrade guard does not depend on re-executing the (about-to-be-replaced)
	// binary. A locally built binary carries the "dev" sentinel, which is not a
	// real version to compare — omit it and let the script probe the target.
	if version != "" && version != "dev" {
		args = append(args, "--current", version)
	}

	cmd := exec.Command("sh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// The pinned release travels via the script's existing VERSION contract.
	cmd.Env = os.Environ()
	if o.pinned != "" {
		cmd.Env = append(cmd.Env, "VERSION="+o.pinned)
	}

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(stderr, "upgrade: %v\n", err)
		return 1
	}
	return 0
}
