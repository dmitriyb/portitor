# Root and Universal-Help Tests

Tests for `RootCommand`, `MachineEntrypointDispatch`, and the exit-code contract. The cobra tree is built in-process with `newRootCommand()`, driven with `SetArgs`, and its streams captured with `SetOut`/`SetErr`.

## Scenarios

### 1. Universal help → stdout, exit 0

**Input**: `--help`, `-h`, and `help`.
**Expected**: `Execute()` returns nil (exit 0); stdout contains `Usage:` and lists the registered subcommands (`pre-receive`, `add-role`, `shell`, …); stderr is empty. (`TestHelpUniversal`.)

### 2. Subcommand help → stdout, exit 0

**Input**: `add-role --help`.
**Expected**: exit 0; stdout contains the command's usage and its flags (`--repo`, `--fingerprint`, `--pub`); stderr empty. (`TestSubcommandHelp`.)

### 3. Unknown subcommand is a usage error (exit 2)

**Input**: an unregistered command name.
**Expected**: `Execute()` returns a non-nil error that is **not** an `*exitError`, so `main` maps it to exit 2 (stderr carries cobra's `unknown command …`). (`TestUnknownCommandIsUsageError`.)

### 4. Grouped help

**Input**: `--help`.
**Expected**: subcommands appear under the `Gate (git hooks):`, `Provisioning (operator):`, and `Action channel (over SSH):` headings; `internal-check-exec` does **not** appear (it is `Hidden`); no `completion` command is listed (default disabled).

### 5. pre-receive gate contract (real binary)

**Input**: build the binary; install it as a bare repo's `pre-receive` hook; drive it with real `git push`.
**Expected**: a push to the default branch and an unsigned-commit push are declined (non-zero exit, reason on the `remote:` lines); a signed feature push is accepted (exit 0). A malformed hook line on stdin exits 1. (`TestEndToEndRealPush`; the gate-path regression suite.)

### 6. shell forced-command routing

**Input**: `shell <fp>` with `SSH_ORIGINAL_COMMAND` set to each of: unset (interactive), a disallowed command, a git-pack command against a disallowed path, and `portitor pr …`.
**Expected**: interactive → exit 1; disallowed → exit 1; git-pack disallowed path → exit 1; `pr` route reaches the cobra `pr` command with the injected fingerprint (e.g. an unknown action → exit 2). Missing `<fp>` → exit 2. Routing helpers (`classify`, `shellSplit`) are unit-tested without a network. (`TestClassify`, `TestShellSplit`.)

### 7. internal-check-exec trampoline

**Input**: `internal-check-exec <workdir> <cmd> [--flags]` via the built binary, and the `internal/check` re-exec path.
**Expected**: routes to the hidden subcommand, chdir + exec the command; trampolined `--flags` are passed through verbatim (not parsed by cobra); it is absent from help; bad args print the trampoline's own sentinel and exit 2.

## Edge Cases

- **Empty argv**: bare `portitor` prints help and exits 0 (does not panic).
- **git's argless pre-receive**: not rejected by cobra's arg validation (`ArbitraryArgs`); stdin is read and the verdict is returned.
