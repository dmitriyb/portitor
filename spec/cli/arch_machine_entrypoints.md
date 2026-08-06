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

The `git` route dispatches `git-receive-pack`/`git-upload-pack '<repo-path>'` (after `allowedRepoPath` confirms the path stays under the configured repo root) to the matching git subcommand against the bare mirror; pre/post-receive hooks apply the gate on the `git-receive-pack` (push) side. For **`git-upload-pack` only** — serving a clone/fetch read — the dispatcher first force-updates the mirror's default branch from `upstream` before invoking it: `git fetch <upstream_remote> +<def>:refs/heads/<def>` (the `+` forces the mirror's default to upstream's tip — a rewind on an upstream non-fast-forward, not merely a fast-forward), the **default branch only**, no `--prune`, no other refs — feature branches live only in the mirror (arrived via gated pushes) and are never touched. This exists because a merge (`pr merge`, see spec/action/arch_action.md) lands on GitHub via the `gh` API, not on the mirror — without this refresh the mirror's default branch is written once at `init-repo`/`add-repo` time and then frozen, so a served clone would never see a landed merge.

Portitor is a **stateless CLI** — one process per SSH connection, no shared memory across concurrent clones — so the refresh is serialized across concurrent clones by a **per-repo flock** (`<bare>/portitor-refresh.lock`, `LOCK_EX`) rather than any in-memory coordination: one refresh runs at a time, waiters queue, and a waiter that acquires the lock after the holder already advanced the ref gets a cheap no-op fetch. The whole flock-acquire-plus-fetch is bounded by the repo's **`serve_refresh_timeout`** config field (default 30s when absent — see spec/gate/arch_config.md; the existing `NetworkTimeout` of 5m is far too long for this path, since a blip would otherwise hang a clone for 5 minutes). An empty `default_branch` is guarded **before** the refspec is built, so a misconfigured repo fails with a clear diagnostic rather than issuing the malformed refspec `+:refs/heads/`. An upstream that legitimately lacks the default branch yet (a fresh, empty upstream — the same case `init-repo` tolerates at provisioning) is treated as nothing-to-refresh: the dispatcher serves the mirror's current state rather than failing the clone. On any other fetch error, or on the flock/fetch timeout, the dispatcher writes a wrapped diagnostic to stderr and returns 1 — the clone fails loudly, the mirror is never served stale. `git-receive-pack` (push) is unchanged: no refresh runs before a push, and the pre-receive/post-receive gate logic is untouched.

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
