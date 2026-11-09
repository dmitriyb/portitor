package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dmitriyb/portitor/internal/git"
)

// provisionLockTimeout bounds waiting for a concurrent provisioning of the same
// repo (an operator's add-repo racing the gate's boot); it covers one full
// upstream fetch.
const provisionLockTimeout = git.NetworkTimeout

// provisioningPath is where init-repo builds a repo before renaming it into
// place. It never ends in .git, so allowedRepoPath never serves it.
func provisioningPath(bare string) string {
	return filepath.Join(filepath.Dir(bare), "."+filepath.Base(bare)+".provisioning")
}

// provisionLockPath serializes provisionings of one repo. Like the
// provisioning path it never ends in .git.
func provisionLockPath(bare string) string {
	return filepath.Join(filepath.Dir(bare), "."+filepath.Base(bare)+".lock")
}

// repoProvisioned reports whether bare is a repo the gate may serve: both hook
// shims are portitor's and no core.hooksPath (in any config scope git-receive-pack
// would read) points git elsewhere. reason names what is missing when it is not.
func repoProvisioned(bare string) (ok bool, reason string, err error) {
	for _, hook := range []string{"pre-receive", "post-receive"} {
		b, err := os.ReadFile(filepath.Join(bare, "hooks", hook))
		if errors.Is(err, os.ErrNotExist) {
			return false, hook + " hook missing", nil
		}
		if err != nil {
			return false, "", fmt.Errorf("read %s hook: %w", hook, err)
		}
		if !isPortitorShim(string(b), hook) {
			return false, hook + " hook is not a portitor shim", nil
		}
	}
	_, err = git.Output(bare, "config", "--get", "core.hooksPath")
	if err == nil {
		return false, "core.hooksPath overrides the hooks", nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return true, "", nil
	}
	return false, "", fmt.Errorf("read core.hooksPath: %w", err)
}

// isPortitorShim recognizes the shim bakeHooks writes for hook.
func isPortitorShim(content, hook string) bool {
	var marker, cfg, execLine bool
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, hookMarker):
			marker = true
		case strings.HasPrefix(line, "export PORTITOR_CONFIG="):
			cfg = true
		case strings.HasPrefix(line, "exec ") && strings.HasSuffix(line, " "+hook):
			execLine = true
		}
	}
	return marker && cfg && execLine
}

// acquireProvisionLock takes the per-repo provisioning lock, creating the
// parent directory the repo will live in.
func acquireProvisionLock(bare string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(bare), err)
	}
	return acquireFlockTimeout(provisionLockPath(bare), provisionLockTimeout)
}
