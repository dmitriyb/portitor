# RootCommand

The top-level `portitor` cobra command. Every other subcommand is a child of this command. Mirrors spexmachina's `cli/root.go`.

## Responsibilities

- Define the root `cobra.Command` with the binary name `portitor`, a short description, and long usage text (constructed by `cli.NewRootCmd()` in the `cli/` package).
- Set `SilenceUsage` and `SilenceErrors` so cobra prints neither usage nor the error text on a failed run — `main` owns error reporting and the exit-code mapping, so a rejected push or a gate error never prints command usage.
- Register the command groups (`gate`, `provisioning`, `action`) that head the help output, reproducing the structure of the old hand-rolled `usage()` text.
- Disable cobra's default `completion` command (`CompletionOptions.DisableDefaultCmd = true`) so the command **set** does not grow. The `help` command is kept — it is exactly the universal help the toolchain wants (`portitor help <cmd>`).
- Leave version metadata and subcommand registration to the caller: `NewRootCmd()` returns a bare, non-runnable root; `cmd/portitor`'s `newRootCommand()` sets `Version`/`SetVersionTemplate` and calls `AddCommand(...)`.

## Subcommand Registration Pattern

Each subcommand is a `newXxxCmd() *cobra.Command` constructor in `cmd/portitor` (mirroring spex's per-file `cmd/spex/*.go` split). A constructor defines the command's flags and a `RunE` that reads them and calls the command's logic function, wrapping the integer result as an error via `exitErr`. Wiring happens in `newRootCommand()`:

```
root := cli.NewRootCmd()
root.Version = fmt.Sprintf("%s (commit %s, built %s)", version, commit, date)
root.SetVersionTemplate("portitor {{.Version}}\n")
root.AddCommand(
    newPreReceiveCmd(), newPostReceiveCmd(),
    newInitRepoCmd(), newAddRepoCmd(), newUpgradeRepoCmd(),
    newReconcileCmd(), newAddRoleCmd(), newValidateConfigCmd(),
    newShellCmd(), newPrCmd(func() string { return os.Getenv("PORTITOR_FINGERPRINT") }),
    newVersionCmd(), newInternalCheckExecCmd(),
)
```

The `pr` command takes an actor-fingerprint function so the CLI path reads `$PORTITOR_FINGERPRINT` while the SSH shell dispatcher injects the connection fingerprint — the `pr` grammar lives in exactly one place (see MachineEntrypointDispatch).

## Command Groups

| Group ID | Title | Members |
|----------|-------|---------|
| `gate` | Gate (git hooks): | `pre-receive`, `post-receive` |
| `provisioning` | Provisioning (operator): | `init-repo`, `add-repo`, `upgrade-repo`, `reconcile`, `add-role`, `validate-config` |
| `action` | Action channel (over SSH): | `shell`, `pr` |

`version` and `help` are ungrouped (they appear under cobra's "Additional Commands"). `internal-check-exec` is `Hidden` and never appears in help.

## Design Rationale

cobra is the de facto standard for Go CLIs and is already how the sibling tool `spex` is built. It provides declarative subcommand registration, auto-generated help at every level, POSIX flag parsing via pflag, and command grouping with no custom code — removing the drift that a hand-maintained `usage()` block invites. This is an intentional exception to the "standard library first" stance, isolated to the CLI layer; the gate, rules, and check modules do not import cobra, so their only-runtime-dependency-is-git property is unaffected.
