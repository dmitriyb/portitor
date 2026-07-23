# UpgradeCommand

The `portitor upgrade` subcommand: the thin Go front-end that self-updates the installed standalone binary by driving the embedded, already-signed `install.sh`. It is a CLI-layer command; the update mechanism itself (resolve → download → verify → safe self-replace, the downgrade guard, rollback, RequireRoot) lives in `install.sh` and is specified by the delivery module's SelfUpdate component (`spec/delivery/arch_self_update.md`).

## Responsibilities

- Register as a provisioning-group cobra subcommand (`newUpgradeCmd()` in `cmd/portitor/commands.go`), consistent with the other operator commands, with universal `--help`.
- Resolve the exact on-disk path of the running binary via `os.Executable()` then `filepath.EvalSymlinks` — a symlinked launcher resolves to the real file, which is what the move-aside + rename must target.
- `//go:embed` the canonical `install.sh` (from the byte-identical package copy `cmd/portitor/install.sh`), write it to a private (0600) temp file, and run `sh <file>` synchronously in upgrade mode against the resolved path. The command is intentionally thin: it reimplements none of the resolve/download/verify/replace logic.
- Report `old → new` by streaming the script's stdout/stderr through unchanged, and propagate the script's exit code as the process exit code.
- Update the standalone operator binary only. The container image (gate + egress) is a separate artifact rebuilt from the `Dockerfile`; the help text states this explicitly, so an operator is never misled into thinking `upgrade` touches the deployed image.

## Flags

| Flag | Effect |
|------|--------|
| `--check` / `--dry-run` | Report current-vs-latest and change nothing (both bind the same field). |
| `--version vX.Y.Z` | Pin a specific release instead of the latest. |
| `--rollback` | Restore the pre-upgrade binary from `<path>.bak`. |
| `--force` | Allow a move to an older version (downgrade). |

`Args: cobra.NoArgs` — the command takes no positional arguments.

## Script invocation contract

The command translates its flags into the script's upgrade-mode flags: it always passes `--upgrade --target <resolved-path>`, appends `--check` / `--force` / `--rollback` as set, and passes `--current <version>` sourced from the compiled-in `main.version` — the authoritative current version, so the downgrade guard never depends on re-executing the about-to-be-replaced binary (a `dev` build omits it and lets the script probe the target). A pinned `--version` travels to the script via its existing `VERSION` environment contract, not a new flag, reusing the untouched resolve path.

## Exit codes

The script's exit code passes through verbatim (`exec.ExitError.ExitCode()`), wrapped as the command's own `*exitError` so `main` uses only the code — matching every other subcommand. A failure to even launch the script (temp-file or `os.Executable` error) exits 1 with a diagnostic on stderr.

## Registration

`UpgradeCommand` is a child of `RootCommand`, registered via `newUpgradeCmd()` in `newRootCommand()`, in the provisioning group. It is distinct from the pre-existing `upgrade-repo` command (which re-bakes a repo's hook shims); the two share a verb prefix but nothing else.
