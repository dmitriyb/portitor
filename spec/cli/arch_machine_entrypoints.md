# MachineEntrypointDispatch

How cobra wraps portitor's load-bearing, non-human entrypoints without breaking their invocation, stdin, or exit-code contracts — the part of the migration with the least slack, since git, sshd, and the rlimit trampoline invoke these directly.

## The exit-code plumbing

A subcommand's logic returns an `int` exit code. Its `RunE` wraps that as an error via `exitErr(code)`:

- `exitErr(0)` returns `nil` (success).
- `exitErr(n != 0)` returns a `*exitError{code: n}` whose `Error()` is the empty string — the command has already written its own diagnostics, so the dispatch uses only the code.

`main` inspects the `Execute()` result:

```go
if err := root.Execute(); err != nil {
    var ee *exitError
    if errors.As(err, &ee) {
        os.Exit(ee.code)   // the command ran → its own 0/1/2 passes through
    }
    fmt.Fprintln(os.Stderr, err)
    os.Exit(2)             // ONLY cobra usage errors reach here
}
```

So exit 2 is exactly the usage-error bucket the hand-rolled dispatcher used (unknown subcommand, unknown/malformed flag, wrong arg count), while a rejected push (1), an operational error (1), and a command's own validation error (2) all pass through unchanged.

## pre-receive / post-receive

git invokes `portitor pre-receive` (and `post-receive`) with **no arguments** and pipes the ref updates on **stdin**; the exit code is the accept/reject verdict. Each command sets `Args: cobra.ArbitraryArgs` so cobra never rejects git's invocation regardless of arg count, and its `RunE` reads `cmd.InOrStdin()` and returns the verdict:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return exitErr(preReceive(cmd.InOrStdin(), cmd.ErrOrStderr()))
}
```

Rejection reasons and audit-write failures go to `cmd.ErrOrStderr()` (git relays them to the pusher as `remote:` lines), exactly as before. cobra does not touch stdin.

## shell &lt;fingerprint&gt;

`shell` is the SSH forced command (`command="portitor shell <fingerprint>"`); the connecting client's request arrives on `SSH_ORIGINAL_COMMAND`, not argv. argv is operator-controlled and a SHA256 fingerprint never looks like a flag, so `Args: cobra.ArbitraryArgs` with default flag parsing is safe, and `shellCommand(args)` does its own arg check and internal routing untouched. Universal help still works (`portitor shell --help`).

The `pr` route does **not** re-implement the `pr` grammar: it re-executes the same cobra `pr` command with the connection fingerprint injected as the actor identity —

```go
case "pr":
    return execSub(newPrCmd(func() string { return fp }), rest)
```

— so the CLI path (`$PORTITOR_FINGERPRINT`) and the SSH path (connection fingerprint) share one definition of the action grammar, flags, and validation.

## internal-check-exec (hidden trampoline)

`internal/check` re-execs `portitor internal-check-exec <workdir> <argv...>` into itself to apply `RLIMIT_AS` to the operator-configured record-extractor command. It is not part of the CLI surface (the SSH shell dispatcher cannot route here), so it is a `Hidden` subcommand — present for the re-exec to resolve, absent from help. Two properties are load-bearing:

- `DisableFlagParsing: true` — `<argv...>` is the operator's command and may carry arbitrary flags (`--version`, `-x`, …) that cobra must pass through verbatim, not parse.
- The `RunE` calls `os.Exit(internalCheckExec(args))` **directly**. The trampoline execs away on success; on failure it returns an exit code under a freshly-lowered `RLIMIT_AS`, so no allocation may unwind back through cobra — `os.Exit` from the handler mirrors the hand-rolled `main`'s direct exit.
