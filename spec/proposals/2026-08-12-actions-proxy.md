# Change Proposal: Proxy the GitHub Actions surface (`checks` | `rerun` | `logs`)

## Context

portitor's action set is PR-domain only (`fetch | comment | review | reply |
resolve | describe | merge | close`). Nothing reaches the GitHub Actions API, so
a caller cannot observe CI state, re-run a hung workflow, or read a failed job's
log through the gate — and boxes have no route to GitHub except through the
gate. A merge attempted while checks are still running reads `mergeStateStatus
== UNSTABLE` and is (correctly) refused; the caller-side loop that should wait
for checks *before* attempting the merge has no data to wait on, and a run that
hangs cannot be recovered at all.

`gh pr view --json statusCheckRollup` already returns `status`, `startedAt`,
`completedAt`, `workflowName`, and `detailsUrl` (which embeds the run id and job
id). portitor currently discards all of it — the data is on the wire, the
`CheckRun` struct just drops it.

## Design decisions (settled)

- **The box names the PR; the gate resolves the run.** No verb accepts a run
  id, job id, or workflow name. The gate derives the workflow run(s) from the
  PR's head SHA — the same discipline `-R` already encodes for the repo. It
  matters more here: a caller-supplied run id would let a box re-run a release
  workflow, publishing artifacts entirely outside the review path.
- **No `workflow_dispatch`, ever.** Re-run replays an already-defined workflow
  on an already-pushed commit — no new code, no caller-supplied inputs, blast
  radius is compute. `workflow_dispatch` takes arbitrary inputs and is a
  code-execution primitive. It stays out of the verb set.
- **Check state lives in one verb.** `fetch` remains the PR-conversation domain
  (reviews, comments, threads). CI and merge-readiness state go in `checks`.
  Two verbs, two domains, no overlapping views to drift apart.
- **Cancellation is run-level.** The GitHub API has no per-job cancel. The
  stuck path is therefore: cancel the run, then re-run failed/cancelled jobs.
  Already-completed green jobs are not re-run — the desired outcome — but the
  mechanism is coarser than "re-run that one job"; policy is written against
  run-level semantics.
- **Enforcement belongs in the gate, not the caller.** A caller-side limit is
  advisory. The gate refuses a re-run when the attempt cap is reached or when
  the run's head SHA no longer matches the PR head — the same reasoning that
  makes `merge_gate` re-derive state rather than trust the request.
- **The gate cannot distinguish callers.** An automation prelude and an agent
  authenticate as the same role, so "automatic re-runs only for stuck jobs" is
  a client-side policy and must not be presented as a gate guarantee. What the
  gate *can* enforce is a config switch on whether a run whose jobs all
  completed `FAILURE` may be re-run at all, plus the attempt cap that bounds
  every case.

## Proposed change

Three new closed-set verbs, all `action_roles`-gated, default-deny like every
other verb, plus a `checks` config block.

### `pr checks --pr N` (read)

One JSON response: per check — `name`, `status`, `conclusion`, `startedAt`,
`completedAt`, `workflowName`, the owning run id / job id (parsed by the gate
from `detailsUrl`, so no caller ever parses URLs in shell), the run's
`run_attempt`, and the check's resolved budget (per-name or default, in
seconds); plus top-level `mergeStateStatus` and `headRefOid`.

`run_attempt` is not in `statusCheckRollup`; it comes from the run object
(`/repos/{owner}/{repo}/actions/runs/{run_id}`). It matters because callers may
be stateless: "have I already re-run this twice?" has to be a fact read from
GitHub, not a counter held by the caller.

### `pr rerun --pr N` (write)

1. Resolve the run(s) from the PR head SHA; refuse if a targeted run's head no
   longer matches the (re-derived) PR head — stale.
2. Refuse if a targeted run's `run_attempt` has reached `checks.max_attempts`.
3. Refuse if every job of a targeted run completed `FAILURE` and
   `checks.allow_rerun_failed` is false.
4. If a targeted run is in flight, cancel it and wait for the cancel to
   complete; then re-run its failed/cancelled jobs.

Runs that completed entirely green are never targets; if nothing is a target,
the verb refuses ("nothing to re-run") rather than reporting a success that did
nothing. Each refusal carries a distinct, attributable message. Returns the new
attempt number per re-run run so a caller can confirm the re-run took.

### `pr logs --pr N` (read)

Failed jobs only. Each failed job's log is **tailed and byte-capped
gate-side** from `checks.log_tail_bytes` — a caller cannot request the full
log. Actions logs reach tens of megabytes; an unbounded response would blow an
agent's context budget, and the cap has to be enforced where the box cannot
override it. This is also the natural redaction point if a workflow ever echoes
something it should not. The gate returns bytes; agents interpret them (no log
parsing, no failure classification).

### Config schema: the `checks` block

```jsonc
"checks": {
  // Per-check-name budgets: how long before a pending check counts as stuck.
  // Data for the caller (surfaced by `pr checks` as budgetSeconds) — deciding
  // WHEN to re-run stays client-side policy.
  "budgets": [ {"name": "test", "budget": "3m"} ],
  "default_budget": "5m",        // budget for names not listed (default 5m)
  "max_attempts": 3,             // gate-enforced attempt cap (default 3)
  "allow_rerun_failed": false,   // may an all-jobs-FAILED run be re-run?
  "log_tail_bytes": 65536        // per-job log tail cap (default 64 KiB)
}
```

Budgets are per check name, not one global number: on a representative repo
`build` completes in ~17s, `vet` ~27s, `test` ~97s, and a spec gate can
legitimately run for minutes — a single flat budget either declares healthy
jobs stuck or never fires. `budgets` is an array of `{name, budget}` objects
(the `merge_gate.checks` shape), so check names live in values and the
config's lowercase-schema-key rule is untouched.

## Impact expectation

- **action**: `Verbs` gains `checks`, `rerun`, `logs`. `CheckRun` widens to
  carry `status/startedAt/completedAt/workflowName/detailsUrl`. New
  `ChecksConfig` (beside `MergeGateConfig`, same nil-safe accessor pattern).
  Pure, unit-testable evaluators (report assembly, the re-run plan/refusal
  logic, the log tail) beside the gh I/O methods, mirroring
  `UnmetMergePreconditions`.
- **config**: `Settings` gains `checks`; the top-level key set gains `checks`;
  `Validate` checks budget names non-empty/unique, durations valid and
  positive, and the numeric fields non-negative. An unknown `action_roles`
  verb still fails closed.
- **cmd/gate**: three new `prRun` cases; the re-run refusals are policy
  denials (audited `deny` with the reason), gh failures stay `error`.
- **tests**: stubbed-runner unit coverage for every pure evaluator and gh call
  shape; config validation coverage; accept-level coverage for the rerun path
  in the style of the seeded gate acceptance suite.
- **spec/docs**: `arch_action.md` (the new Actions-surface section),
  `arch_config.md` (validation list), `docs/commands.md`,
  `docs/configuration.md`.
- **Compatibility**: additive (release as a **minor** bump — new verbs + new
  schema key). A repo config naming the new verbs fails validation on an older
  binary, and the gate validates every `repos.d/*.json` at boot — so the
  rollout order is fixed: release the binary/image first, then add the new
  verbs to per-repo configs (the established upgrade-binary-first rule).

## Out of scope

- `workflow_dispatch`, or any verb taking caller-supplied workflow inputs.
- Deciding *when* to re-run — that policy lives with the caller.
- Log parsing or failure classification.
