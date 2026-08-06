package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dmitriyb/portitor/internal/config"
	"github.com/dmitriyb/portitor/internal/git"
)

// mirrorRefreshLockFile is the per-repo flock's filename, colocated inside
// the bare mirror directory it serializes.
const mirrorRefreshLockFile = "portitor-refresh.lock"

// refreshDefaultBranch force-updates the bare mirror's default branch from
// upstream before serving a git-upload-pack (clone/fetch) read, so a served
// clone reflects merges — which land on GitHub via `pr merge`
// (internal/action.GH.Merge), never on the mirror itself. Default branch
// ONLY: no --prune, no other refs — feature branches live only in the mirror
// (arrived via gated pushes) and must never be touched. The `+` refspec
// force-updates the mirror's ref to upstream's tip (a rewind on an upstream
// non-fast-forward), not merely a fast-forward.
//
// Portitor is a stateless CLI — one process per SSH connection, no shared
// memory across concurrent clones — so serializing the refresh across them is
// an on-disk per-repo flock (mirrorRefreshLockFile), bounded by the repo's
// serve_refresh_timeout (config.Settings.ServeRefreshTimeoutOrDefault,
// default 30s: git.NetworkTimeout's 5m is far too long to hold a clone
// hostage to a single blip on this path). One refresh runs at a time;
// waiters queue; a waiter that acquires the lock after the holder already
// advanced the ref gets a cheap no-op fetch (objects already present, ref
// already at tip).
func refreshDefaultBranch(bare string) error {
	name := strings.TrimSuffix(filepath.Base(bare), ".git")
	s, err := config.Resolve(name)
	if err != nil {
		return fmt.Errorf("load config for %s: %w", name, err)
	}
	def := s.DefaultBranch
	if def == "" {
		// Guard BEFORE building the refspec: config.Resolve does not itself
		// validate default_branch, so an empty value here would otherwise
		// reach git as the malformed refspec "+:refs/heads/".
		return fmt.Errorf("refresh %s: empty default_branch in config", name)
	}
	remote := s.UpstreamRemote
	if remote == "" {
		remote = "upstream"
	}
	timeout := s.ServeRefreshTimeoutOrDefault()

	start := time.Now()
	release, err := acquireFlockTimeout(filepath.Join(bare, mirrorRefreshLockFile), timeout)
	if err != nil {
		return fmt.Errorf("refresh %s: %w", name, err)
	}
	defer release()

	// The timeout bounds the whole flock-acquire-plus-fetch, not each half
	// independently — a slow lock wait must eat into the fetch's budget too.
	remaining := timeout - time.Since(start)
	if remaining <= 0 {
		return fmt.Errorf("refresh %s: serve_refresh_timeout (%s) exhausted acquiring the refresh lock", name, timeout)
	}

	refspec := "+" + def + ":refs/heads/" + def
	if err := git.OutputNetworkRunTimeout(bare, remaining, "fetch", remote, refspec); err != nil {
		// A fresh/empty upstream legitimately lacks the default branch yet —
		// the same tolerance initRepoRun applies at provisioning (main.go's
		// hasDefault check). Nothing to refresh: serve the mirror as-is
		// rather than fail the clone.
		if strings.Contains(err.Error(), "couldn't find remote ref "+def) {
			return nil
		}
		return fmt.Errorf("fetch %s %s: %w", remote, def, err)
	}
	return nil
}

// acquireFlockTimeout takes an exclusive advisory flock on path (created if
// absent, 0600), bounded by timeout. flock(2) has no built-in timeout, so the
// blocking acquire races in a goroutine against a timer: on timeout this
// returns immediately with an error, WITHOUT stopping the goroutine — it is
// left blocked on LOCK_EX (and f stays open) until the current holder
// releases, at which point the acquire silently succeeds into a value
// nothing reads and f is never closed here (reclaimed only when the process
// exits, or whenever the GC finalizer happens to close the now-unreferenced
// *os.File). That leak is safe ONLY because this codepath's caller returns 1
// on this error, so the whole portitor process — one per SSH connection —
// exits within microseconds; the leaked fd and goroutine die with it. Reusing
// this helper from a long-lived process would leak a real fd and goroutine
// per timeout and must not be done without adding cancellation.
func acquireFlockTimeout(path string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	acquired := make(chan error, 1)
	go func() { acquired <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }()
	select {
	case err := <-acquired:
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		return func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("lock %s: timed out after %s waiting for a concurrent refresh to finish", path, timeout)
	}
}
