# Command Dispatch and Exit Codes

How input flows from argv through cobra to the correct subcommand handler, and how each handler's result becomes the process exit code.

## Flow

```
argv: portitor add-role --repo r --fingerprint SHA256:… --role reviewer
                │
                ▼
        ┌────────────────┐
        │  RootCommand   │
        │  Execute()     │
        │ 1. match sub   │
        │    "add-role"  │
        │ 2. parse flags │
        └───────┬────────┘
                │
                ▼
        ┌────────────────┐        ┌─────────────────────────────┐
        │ add-role RunE  │──────▶ │ addRoleRun(repo,role,fp,pub) │
        │ reads flag vars│        │ validates, writes, returns   │
        │ exitErr(code)  │◀────── │ 0 / 1 / 2                    │
        └───────┬────────┘        └─────────────────────────────┘
                │  error (nil | *exitError)
                ▼
        ┌────────────────┐
        │     main()     │  nil → exit 0
        │ map err→code   │  *exitError → its code
        └────────────────┘  other (cobra usage error) → exit 2
```

## Key Behaviors

1. **No args**: bare `portitor` prints help to stdout and exits 0 (cobra convention). This is the one intentional change from the hand-rolled dispatcher, which exited 2 to stderr; no machine entrypoint depends on bare invocation.
2. **Unknown subcommand**: `portitor bogus` → cobra returns `unknown command "bogus" for "portitor"`; `main` prints it to stderr and exits 2.
3. **Universal help**: `--help`/`-h`/`help` at the root, and `<cmd> --help` / `help <cmd>` at any subcommand, print usage to stdout and exit 0.
4. **Version**: `version`, `--version`, `-v` print the version line to stdout and exit 0.
5. **Machine entrypoints**: git pipes stdin to `pre-receive`/`post-receive` and reads the verdict from the exit code; sshd invokes `shell <fp>`; `internal/check` re-execs `internal-check-exec`. cobra neither rejects their argv nor touches their stdin (see arch_machine_entrypoints.md).

## Exit-code contract

| Outcome | Exit | Source |
|---------|------|--------|
| success / accept | 0 | logic returns 0 → `exitErr(0)` = nil |
| push rejected, operational error | 1 | logic returns 1 → `*exitError{1}` |
| command's own usage error (bad flag value, missing required flag, unknown `pr` action) | 2 | logic returns 2 → `*exitError{2}` |
| cobra usage error (unknown command/flag, wrong arg count) | 2 | non-`*exitError` → `main`'s final `os.Exit(2)` |

The `*exitError.Error()` is deliberately empty: the command already emitted its own diagnostics, so `main` uses only the code and does not double-print. This preserves the gate accept/reject codes and every code callers or tests depend on.

## Data Shapes

### argv → RootCommand

- `os.Args`: list of string (from the Go runtime).
- cobra parses into: the matched subcommand, its local flags, and positional `args []string`. The root has no persistent flags.

### RootCommand → subcommand handler (RunE signature)

- `cmd *cobra.Command` (with `InOrStdin()`, `OutOrStdout()`, `ErrOrStderr()` accessors), `args []string`.
- Returns `error`: `nil` for exit 0, `*exitError{code}` for a command exit code, or (from cobra internals) a usage error.

Any new subcommand must be registered in `newRootCommand()` via a `newXxxCmd()` constructor; a new machine entrypoint must declare the arg/flag-parsing relaxations its caller requires (`ArbitraryArgs` and/or `DisableFlagParsing`) so cobra does not reject a non-human invocation.
