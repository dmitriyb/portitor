# Change Proposal: Configurable merge method (squash | merge | rebase)

## Context

portitor's `merge` is hardcoded to `gh pr merge --squash` (`internal/action/
action.go`, `GH.Merge`). The merge strategy is policy, not mechanism: it decides
what the default branch's history looks like — one squashed commit per PR, a
merge commit preserving the branch's commits, or the branch's commits rebased
linearly. It is the one merge behavior that is NOT user-declared, unlike
`merge_gate.review`, `merge_gate.checks`, `required_checks`, and `content_rules`.

The choice is load-bearing beyond aesthetics: a squash lands a single new commit
authored by the merging account (GitHub-signed, web-flow "Verified"), collapsing
the branch's individual commits — including any **role-key-signed** commits.
`merge` or `rebase` instead preserve those commits on the default branch with
their signatures intact. So the method also decides whether the default branch
carries role-signed history, which some deployments require.

## Proposed change

Add `merge_gate.merge_method`: `"squash"` (default) | `"merge"` | `"rebase"`.

- `GH.Merge` takes the method and maps it to the corresponding `gh pr merge`
  flag (`--squash` / `--merge` / `--rebase`); `--match-head-commit <head>` (the
  head-pin TOCTOU close) is unchanged and applies to every method.
- Enum-validated in `Validate` exactly like `merge_gate.review`; an empty or
  absent field defaults to `"squash"`, so every existing config is
  byte-identical to today — a pure addition.
- The repo must *allow* the chosen method: GitHub rejects `--merge` when merge
  commits are disabled on the repo, `--rebase` when rebase merging is disabled,
  etc. The gate forwards that error verbatim (the same fail-loud contract as
  every other `gh` action) rather than second-guessing the repo's settings.

`merge_gate.merge_method` lives in `merge_gate` because it is a merge behavior,
beside the review source and the check predicates.

## Impact expectation

- **action**: `GH.Merge(pr, headSHA)` gains a method parameter and maps it to
  the `gh pr merge` flag; `MergeGateConfig` gains a `MergeMethod` field with a
  `MergeMethodOrDefault()` accessor (empty → "squash"), mirroring
  `ReviewSource()`.
- **config**: `Validate` accepts `merge_gate.merge_method` from
  `"" | "squash" | "merge" | "rebase"` and rejects anything else.
- **cmd/gate**: the `merge` command passes the effective method to `GH.Merge`.
- **tests**: fake-`gh` unit coverage asserts the correct `--squash|--merge|
  --rebase` flag per method and the default; a config test covers the enum +
  default.
- **spec/docs**: `arch_action.md` (the merge preconditions/landing section) and
  `arch_config.md` (the `merge_gate` validation list).
- **Compatibility**: additive; absent `merge_method` → `squash`, no behavior
  change. Strict decode already tolerates unknown-to-older-binaries keys per the
  upgrade-binary-first rule (a new key makes an older binary refuse the config,
  the established contract).
