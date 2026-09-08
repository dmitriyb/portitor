# Commands

| command | run by | purpose |
|---|---|---|
| `portitor pre-receive` / `post-receive` | the bare's git hooks | the gate / forward + auto-PR |
| `portitor init-repo --bare … [--config …] [--upstream …]` | operator | create a gated bare repo (+ mirror) |
| `portitor add-repo --repo <name> --upstream <url>` | operator | init-repo via the registry conventions |
| `portitor add-role --repo <name> --role <r> --fingerprint SHA256:… [--pub <file>]` | operator | bind a fingerprint→role (+ trust its key) |
| `portitor upgrade-repo --repo <name>` | operator | re-bake hook shims to the current version |
| `portitor reconcile --repo <name>` | operator | re-forward accepted branches after a forward failure |
| `portitor upgrade [--check] [--version vX.Y.Z] [--rollback]` | operator | update the standalone binary forward to the latest signed release |
| `portitor validate-config [--config <path>]` | operator / boot | fail fast on a missing/invalid config |
| `portitor shell <fingerprint>` | sshd forced command | dispatch to git-pack, the `pr` API, or the MCP splice |
| `portitor pr <action> --repo <name> --pr <n>` | the agent (via SSH) | one role-validated GitHub action |
| `portitor mcp` | the agent (via SSH) | splice the connection to the MCP mediator |
| `portitor version` / `--version` / `-v` | anyone | print version, commit, and build date |
| `portitor-mcp serve [--config <path>] [--listen <target>]` | the mediator container | run the MCP mediator daemon |
| `portitor-mcp validate-config [--config <path>]` | operator / boot | fail fast on a missing/invalid mediator config |
| `portitor-mcp version` / `--version` / `-v` | anyone | print version, commit, and build date |

Every `portitor` command above ships in the single released `portitor` binary (see [`install.md`](install.md)) — the same binary the container image runs. `portitor-mcp` is a second released binary from the same tag and version (one tag ships the tested pair); it runs in its own container/user with its own credentials — see `deploy.md`.
`add-role`, `validate-config`, and `reconcile` are also useful run standalone from an operator's own machine, outside the container, against a mounted or copied registry.

Prefer `add-role` over hand-editing the `roles` map: it validates the fingerprint, upserts atomically under a lock, optionally trusts a signing key in `allowed_signers` (deduped), and re-validates — so a fat-fingered key or a half-written file can't quietly weaken the gate.

## The `pr` action API

`portitor pr <fetch|comment|review|reply|resolve|describe|checks|rerun|logs|merge|close> --repo <name> --pr <n>` runs one action with portitor's credential after checking the caller's role against `action_roles` (default-deny). `review --inline` reads a JSON body raising inline threads; `reply --thread <id>` answers into a thread; `resolve --thread <id>` (or `--gate-threads` for every thread the gate's own reviews created) resolves them; `describe` overwrites the PR's body/description (modeled on `comment`, differing only in the gh call); `checks` returns the PR's per-check CI state + `run_attempt` + `mergeStateStatus` in one JSON response; `rerun` cancels/re-runs the PR head's failed or stuck workflow run(s) under the gate-enforced `checks` policy (attempt cap, stale-head refusal, all-failed switch); `logs` returns failed jobs' logs, tailed gate-side to `checks.log_tail_bytes`. No verb accepts a run id, job id, or workflow name — the gate resolves the run from the PR head.
Bodies are read from stdin so multi-line markdown survives transport.

`merge` additionally **re-derives its preconditions from authoritative GitHub state + the local repo** (never the request) and refuses with the full unmet list: the configured review source (`merge_gate.review`: `github` wants native `reviewDecision == APPROVED`; `none` — the default — carries no review precondition here, expressing one instead, if wanted, as a `merge_gate.checks` git-content predicate; the retired `internal` source and its `reviews_log` are gone — see `spec/proposals/2026-08-05-transparent-approve.md`), a `CLEAN` merge state (mandatory; covers behind-base / conflicts / blocked, and inherits every GitHub branch rule), every `required_checks` entry green, and every `merge_gate.checks` command predicate passing (argv + PR number + head SHA, hermetic). Separation of duties is **not** a portitor precondition (removed with transparent-approve — reviewer-≠-author integrity lives in GitHub branch protection or push-time `content_rules`). The merge itself lands via the configured `merge_gate.merge_method` (`squash` default | `merge` | `rebase`), head-pinned (`--match-head-commit`) to the evaluated head.
The final `gh pr merge` is the atomic gate; enable GitHub branch protection as defense in depth.

`owner` is your own (touch-required) override identity.
A landing role (e.g. `merger`) is a dedicated, **commit-less** identity; provision it only when you want merges via portitor — omit it (or grant nobody `merge`) and merges are unavailable through portitor.

The full mediation model — the `portitor shell` dispatch table, auto-open-PR on forward, merge-precondition re-derivation, and the audit trail — is specified in `spec/action/arch_action.md`.

## MCP mediation (`portitor mcp` → `portitor-mcp`)

`portitor mcp` (no arguments — anything more is rejected) is the third `classify()` route: the dispatcher splices the SSH connection's stdio to the separately running `portitor-mcp` mediator (`PORTITOR_MCP_TARGET`, `unix:<path>` or `tcp:<host>:<port>`), asserting the caller's key fingerprint in a one-line header. With no target configured, the route refuses cleanly — a deployment without a mediator loses nothing else.

The mediator speaks a closed MCP stdio subset (`initialize`, `tools/list`, `tools/call`, `ping`) and forwards a `tools/call` only after a default-deny role check and strict allowlist-schema validation of the arguments, re-serialized canonically; the tool list is pinned in the mediator's config and upstream schema drift refuses the tool rather than forwarding it. Upstream MCP servers are session-scoped stdio children of the mediator, with credentials injected from the mediator's environment — never from the agent, never through the git-side container.

**This is the weakest of portitor's three enforcement tiers** — shape-validated forwarding (only these tools, these argument shapes, this role, audited), not gate-grade or `pr`-grade authority; see `architecture.md`. The full model — dispatch, protocol subset, matcher grammar, pinning, supervision — is specified in `spec/mcp/arch_mcp.md`; the mediator's config is described in `configuration.md`.

## Upgrading the binary

`portitor upgrade` updates the installed **standalone binary** to the latest signed release, in place. Upgrade is **forward-only**: with no flag it resolves the latest release and moves toward it.
It does not reimplement the update: it embeds the same signed `install.sh` the README documents and runs it against the path of the currently-running binary, so the new release is resolved, downloaded, and SSHSIG-verified by exactly that audited installer — the embedded copy is byte-identical to the standalone `install.sh` (a `go test` check enforces that identity).
The swap is safe over a running binary: the installer moves the current binary aside and `rename(2)`s the new one into place (never a write over the running file, which would hit `ETXTBSY` on Linux), keeping the displaced binary as `<path>.bak`.

If the resolved latest is **older** than the installed version, `upgrade` hard-refuses and no flag overrides it — a latest that moved backward is a rollback anomaly (a compromised origin serving an old but validly-signed release as "latest"). A signature proves authenticity, not freshness. To install an older release deliberately, name it with `--version`, which installs that exact release in any direction with no anomaly guard.

- `--check` / `--dry-run` — report the latest release and change nothing. When the latest is older than installed it prints an anomaly **warning** but still exits 0 (its job is to report, not to gate).
- `--version vX.Y.Z` — install that exact release in **any** direction (including older than installed), instead of the forward-only latest.
- `--rollback` — restore `<path>.bak` (the binary displaced by the last upgrade).

If the target directory is not writable, `upgrade` prints a "re-run with elevated privileges" message and stops — it never silently re-invokes `sudo`.

This command maintains the **standalone operator binary** only (the one also used for `add-role`, `validate-config`, and `reconcile`).
The container image (gate + egress) is a separate artifact rebuilt from the `Dockerfile` — `upgrade` does not touch it; see `deploy.md` for the CLI-vs-image split.
