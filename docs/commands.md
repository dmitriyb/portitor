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
| `portitor shell <fingerprint>` | sshd forced command | dispatch to git-pack or the `pr` API |
| `portitor pr <action> --repo <name> --pr <n>` | the agent (via SSH) | one role-validated GitHub action |
| `portitor version` / `--version` / `-v` | anyone | print version, commit, and build date |

Every command above ships in the single released binary (see the README's install section) — the same binary the container image runs.
`add-role`, `validate-config`, and `reconcile` are also useful run standalone from an operator's own machine, outside the container, against a mounted or copied registry.

Prefer `add-role` over hand-editing the `roles` map: it validates the fingerprint, upserts atomically under a lock, optionally trusts a signing key in `allowed_signers` (deduped), and re-validates — so a fat-fingered key or a half-written file can't quietly weaken the gate.

## The `pr` action API

`portitor pr <fetch|comment|review|merge|close> --repo <name> --pr <n>` runs one action with portitor's credential after checking the caller's role against `action_roles` (default-deny).
Bodies are read from stdin so multi-line markdown survives transport.

`merge` additionally **re-derives its preconditions from GitHub + the local repo** (never the request) and refuses with the full unmet list: approval (`reviewDecision == APPROVED`), a `CLEAN` merge state (covers behind-base / conflicts / blocked), every `required_checks` entry green, and separation of duties — the requesting key must not have signed any commit the PR introduces (the same check guards `review --event approve`).
The final `gh pr merge` is the atomic gate; enable GitHub branch protection as defense in depth.

`owner` is your own (touch-required) override identity.
A landing role (e.g. `merger`) is a dedicated, **commit-less** identity; provision it only when you want merges via portitor — omit it (or grant nobody `merge`) and merges are unavailable through portitor.

The full mediation model — the `portitor shell` dispatch table, auto-open-PR on forward, merge-precondition re-derivation, and the audit trail — is specified in `spec/action/arch_action.md`.

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
