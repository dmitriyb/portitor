# Version Command Tests

Tests for `VersionCommand` and the `--version`/`-v` flag. The tree is built in-process with `newRootCommand()` after overriding the `version`/`commit`/`date` package variables (restored via `t.Cleanup`).

## Scenarios

### 1. All three forms are byte-identical

**Input**: `version`, `--version`, `-v` (with `version=v0.1.0`, `commit=abc1234`, `date=2026-07-22`).
**Expected**: each prints exactly `portitor v0.1.0 (commit abc1234, built 2026-07-22)\n` to stdout and exits 0; stderr empty. (`TestVersionForms`.)

### 2. Dev defaults

**Input**: `version` built without ldflags.
**Expected**: exit 0; output is `portitor dev (commit none, built unknown)`.

### 3. Version --help

**Input**: `version --help`.
**Expected**: exit 0; stdout contains usage for the `version` subcommand.

### 4. Exits cleanly

**Input**: `version`.
**Expected**: exit 0, no output to stderr.

## Edge Cases

- **Extra arguments**: `version foo` — `version` sets `Args: cobra.NoArgs`, so an extra positional is a cobra usage error → exit 2.
- **Provenance link**: the injected values match the release tag/commit/date, so a shipped binary's own `portitor version` is an unfalsifiable link back to its provenance.
