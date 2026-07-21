# Commands

| command | run by | purpose |
|---|---|---|
| `portitor pre-receive` / `post-receive` | the bare's git hooks | the gate / forward + auto-PR |
| `portitor init-repo --bare … [--config …] [--upstream …]` | operator | create a gated bare repo (+ mirror) |
| `portitor add-repo --repo <name> --upstream <url>` | operator | init-repo via the registry conventions |
| `portitor add-role --repo <name> --role <r> --fingerprint SHA256:… [--pub <file>]` | operator | bind a fingerprint→role (+ trust its key) |
| `portitor upgrade-repo --repo <name>` | operator | re-bake hook shims to the current version |
| `portitor reconcile --repo <name>` | operator | re-forward accepted branches after a forward failure |
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
