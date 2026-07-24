# Upgrade Command Tests

Tests for `UpgradeCommand` (`cmd/portitor/upgrade_test.go`). The command's own logic is thin, so its Go tests cover registration, help scope, and the embedded-script identity invariant; the end-to-end update behaviour (resolve → download → verify → self-replace, downgrade guard, rollback, fail-closed) is exercised by the delivery module's fake-origin harness (`spec/delivery/test_delivery.md`), which this command drives unchanged.

## Scenarios

### 1. Embedded install.sh is byte-identical to the canonical one

**Input**: the embedded `installScript` bytes versus the repo-root `install.sh`.
**Expected**: byte-for-byte equal; otherwise the test fails with a "run `go generate ./cmd/portitor`" message. (`TestUpgradeEmbeddedMatchesReleased`.) This identity is the security argument: the script the binary runs is exactly the audited, signed installer, so there is nothing to fetch and nothing to substitute.

### 2. Help scopes to the binary, not the image

**Input**: `upgrade --help`.
**Expected**: exit 0; stdout lists the flags (`--check`, `--version`, `--rollback`), describes the upgrade as `forward-only` (there is no `--force`), and states that `upgrade` maintains the standalone binary only, with the container image rebuilt from the `Dockerfile`. (`TestUpgradeHelpScopesToBinaryNotImage`.)

### 3. Fake-origin harness rides the Go test gate

**Input**: `TestUpgradeFakeOriginHarness` runs `scripts/install_upgrade_test.sh`.
**Expected**: the harness exits 0 (all scenarios pass). Skipped only when a tool it needs (`sh`, `curl`, `ssh-keygen`, `tar`, `mktemp`, `cut`, `awk`, `sed`) is absent, so the upgrade/rollback/fail-closed proofs share the same `go test ./...` gate as the rest of the suite.

## Edge Cases

- **Extra arguments**: `upgrade foo` — `Args: cobra.NoArgs`, so an extra positional is a cobra usage error → exit 2.
- **`--dry-run` alias**: `--dry-run` and `--check` bind the same field; either sets check mode.
- **Name collision**: `upgrade` and the pre-existing `upgrade-repo` are separate commands; help and dispatch must keep them distinct.
