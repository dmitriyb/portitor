# Cobra Setup and Registration

How the root command is constructed and how subcommands are registered, mirroring spexmachina's `cli/root.go` + `cmd/spex/*.go` split.

## Package Layout

- `cli/root.go` (package `cli`) — exports `NewRootCmd() *cobra.Command`, and the group-ID constants `GroupGate`, `GroupProvisioning`, `GroupAction`.
- `cmd/portitor/commands.go` (package `main`) — `newRootCommand()` (assembles the tree, sets version metadata, adds every subcommand), one `newXxxCmd()` constructor per subcommand, the `*exitError` plumbing (`exitErr`, `execSub`), and the retained `func([]string) int` entry points.
- `cmd/portitor/version.go` — the `main.version/commit/date` variables and `newVersionCmd()`.
- The per-command logic (`preReceive`, `initRepoRun`, `addRoleRun`, `prRun`, `shellCommand`, …) stays in `main.go` / `addrole.go`.

## Root Command Construction

```go
func NewRootCmd() *cobra.Command {
    root := &cobra.Command{
        Use:   "portitor",
        Short: "A git gateway that verifies the result of a push and mediates GitHub actions.",
        Long:  "portitor is a git gateway: ...",
        SilenceUsage:  true,
        SilenceErrors: true,
    }
    root.CompletionOptions.DisableDefaultCmd = true
    root.AddGroup(
        &cobra.Group{ID: GroupGate, Title: "Gate (git hooks):"},
        &cobra.Group{ID: GroupProvisioning, Title: "Provisioning (operator):"},
        &cobra.Group{ID: GroupAction, Title: "Action channel (over SSH):"},
    )
    return root
}
```

### SilenceUsage and SilenceErrors

Both are `true` so cobra prints neither usage nor the error text on a failed run. Errors are handled explicitly: `main` checks `Execute()`'s return and maps it to an exit code (see flow_command_dispatch.md). This is what keeps a gate error or a rejected push from dumping command usage.

## Command Constructor Pattern

Each `newXxxCmd()` binds its flags to local variables and calls the logic function from `RunE`, wrapping the integer result:

```go
func newAddRoleCmd() *cobra.Command {
    var repo, role, fp, pub string
    cmd := &cobra.Command{
        Use: "add-role", Short: "...", GroupID: cli.GroupProvisioning, Args: cobra.NoArgs,
        RunE: func(cmd *cobra.Command, args []string) error {
            return exitErr(addRoleRun(repo, role, fp, pub))
        },
    }
    cmd.Flags().StringVar(&repo, "repo", "", "...")
    // ... role, fingerprint, pub ...
    return cmd
}
```

Explicit validation stays in the logic function (e.g. a missing `--repo` or a bad fingerprint returns 2) rather than `MarkFlagRequired`, so the exact messages and exit codes are preserved. The logic functions were converted from `func([]string) int` (which parsed with the `flag` package) to typed parameters; `add-role`'s long body binds the parameters back to pointers so the read-decide-write sequence reads byte-identically.

## Retained entry points

`initRepo`, `upgradeRepo`, `addRole`, `validateConfig` are kept as `func([]string) int` shims that run their cobra subcommand in isolation via `execSub` and return the exit code, so the behavior-locked operator unit tests keep driving the same signatures. `execSub` also backs the SSH shell dispatcher's `pr` route.

## Dependency: cobra

Add `github.com/spf13/cobra` (version matching spexmachina) to `go.mod`; it pulls in `github.com/spf13/pflag` and `github.com/inconshreveable/mousetrap` as indirects. This is a compile-time dependency confined to `cli/` and `cmd/portitor/` — the gate/rules/check modules do not import it, and the running binary still needs only git on the host.

## Migration Path

The hand-rolled `switch os.Args[1] { case "pre-receive": ...; default: usage(); os.Exit(2) }` in `main.go`, plus the hand-written `usage()`/`usageTo()`, are replaced by `newRootCommand()` + `Execute()`. The migration is mechanical: each `case` becomes a `newXxxCmd()` whose `RunE` calls the existing handler; `usage()`/`usageTo()` are deleted (cobra generates help); and the top-level `os.Exit` codes are preserved by the `*exitError` mapping.
