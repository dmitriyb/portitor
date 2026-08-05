# Change Proposal: Transparent approve — retire the internal review verdict; approval is native GitHub or git content

## Context

Merge-gate-v2 (2026-08-04) introduced an *internal* review verdict: `review`
posts a COMMENT toward GitHub and records the real verdict in the gate-owned
`reviews_log` (keyed by PR + head), which `merge_gate.review: internal` then
consults. It also kept a hardcoded separation-of-duties check (the requester
must not have signed any commit the PR introduces), guarding both `approve`
and `merge`. Both existed to make review work for the single-account
deployments portitor targets, where GitHub refuses self-approval (HTTP 422).

Two problems have surfaced.

**1. A gate-proxy holding authoritative state is the wrong architecture.**
`reviews_log` is not a cache of a GitHub fact — it *is* the approval (the
GitHub side is a cosmetic COMMENT). So the one durable record of "approved"
lives in a jsonl the proxy must never lose or corrupt, and every `approve` is
a dual-write (append the log, post to GitHub) with no atomicity: a crash
between them, or a lost file, and the truth is gone or inconsistent. A gate
that forwards actions should keep its truth in the systems it forwards to —
GitHub, or the git history — not inside itself.

**2. The hardcoded separation-of-duties collides with a configured rule.**
Under the beads workflow the reviewer closes the bead by committing
`.beads/issues.jsonl` (status → closed) — a reviewer-signed commit, and
push-time `content_rules` already gate that to reviewer-only. The reviewer
then submits `approve`, and the hardcoded check refuses it: "key … signed
commits this PR introduces; it may not approve." Two portitor behaviors —
*reviewer signs the bead-close* and *a signer may not approve* — directly
contradict. The separation-of-duties rule is also the one gate rule that is
NOT user-declared: every other precondition flows through `action_roles`,
`content_rules`, `merge_gate`, but this one is baked into the command handler.

The root observation: `approve` is the only verb that is not a transparent
GitHub action. `merge`, `close`, `resolve`, `reply`, `comment` each map 1:1 to
a real GitHub action portitor merely forwards. `approve` is a fiction — a
COMMENT plus internal bookkeeping — invented solely to fake native approval
inside a single account. Every legitimate approval signal is representable
without it: on GitHub (a native review, for separated accounts) or in git (a
signed commit, for single-account) — both already sources of truth, and both
already consultable by the merge gate.

## Proposed change

### 1. `approve` becomes a transparent GitHub passthrough

`review --event <approve|request-changes|comment>` submits the corresponding
**real** GitHub review event, not a forced COMMENT. `--inline` is unchanged in
shape (stdin `{body, comments:[{path,line,body}]}`), now carried on that one
native review. If the forge refuses the event — e.g. HTTP 422 on a
self-approval — the gate fails loudly and forwards the error verbatim; a
single account trying to approve its own PR is a topology/config problem, not
one portitor papers over with invented state. (`comment` is always
self-account-safe, so inline COMMENT threads — the fix-loop feedback channel —
work in every deployment; only `approve`/`request-changes` require a distinct
reviewer account.)

### 2. Retire the internal review verdict (`reviews_log`)

Remove `reviews_log` and the `internal` merge-gate review source. `merge_gate.review`
becomes `github | none` (default **`none`**). Approval truth lives where it
belongs:

- **`github`** — GitHub's native `reviewDecision == APPROVED` (separated-account
  deployments). GitHub enforces required approvals, refuses self-approval, and
  dismisses stale approvals on push — natively, no gate state.
- **git content, via `merge_gate.checks`** — the predicate seam merge-gate-v2
  already ships. A single-account deployment expresses the reviewer's verdict
  as a signed commit (the bead-close), gated reviewer-only by the existing
  push-time `content_rules`, and a `checks` predicate (e.g. `bead-closed`)
  reads it at the current head. Because the push-time rule already guarantees
  only the reviewer key may sign a bead-close, a *closed* bead is
  reviewer-attested by construction — the predicate need only check the state,
  not re-verify the signer.

Default `none` (not `internal`) because portitor must not invent a gate: an
absent `merge_gate` means only the mandatory `mergeStateStatus == CLEAN` and
`required_checks` apply, and a deployment that wants a review precondition
declares `github` or a `checks` predicate.

### 3. Remove the hardcoded separation-of-duties

Delete the check from both the `approve` and `merge` paths. Integrity moves to
the source of truth: for separated accounts GitHub enforces reviewer ≠ author
natively; for single-account git-content, push-time `content_rules` already
guarantee only the reviewer key may sign a bead-close, so approval authorship
is attested in git. No rule remains hardcoded in the command handler — every
precondition is user-declared.

### 4. Re-derive gate-thread identity from GitHub

Auto-resolve on approve and `resolve --gate-threads` currently read the gate's
own thread ids from `reviews_log`. With the log gone, identify them by
**author** — the review threads created by the gate's own PAT account — from
the same `reviewThreads` GraphQL data `fetch` already returns. Human-authored
threads are still never auto-resolved.

### 5. Two supported worlds, documented; no invented state

- **Separated accounts (native).** Reviewer is a distinct GitHub account →
  `review --event approve` posts a real APPROVE; `merge_gate.review: github` +
  branch protection gate the merge. portitor stores nothing.
- **Single account (git content).** No native approve is possible → the
  reviewer's durable acts are the reviewer-signed bead-close and resolving its
  threads; `merge_gate.review: none` + `merge_gate.checks: [bead-closed]` gate
  the merge. portitor stores nothing.
- A single-account deployment that configures `review: github` (unsatisfiable)
  or keeps `approve` in its workflow deadlocks — loudly, at the gate, on its
  own misconfiguration. That is preferable to hidden internal state that
  rescues a fundamentally author == reviewer situation.

The mandatory `mergeStateStatus == CLEAN` precondition (merge-gate-v2) stays:
portitor remains a strict superset of GitHub's branch-protection rules, never a
bypass. This proposal supersedes the internal-verdict half of
`2026-08-04-merge-gate-v2.md`; the configurable-preconditions and thread-aware
API halves stand.

## Impact expectation

- **action**: `Review`/`ReviewInline` submit the caller's real event
  (approve/request-changes/comment) instead of forcing COMMENT; delete the
  `reviews_log` write/lookup and the internal-approval path; derive gate-thread
  ids by author; drop the separation-of-duties helper (`requesterSignedPR`/
  `requesterSignedHead`) call sites.
- **cmd/gate**: remove the two hardcoded separation-of-duties checks (the
  `review approve` path and the `merge` precondition); `merge_gate.review` enum
  shrinks to `github|none`, default `none`.
- **config**: `merge_gate.review` enum + default; retire the `reviews_log` key;
  strict decode refuses a config still carrying `reviews_log` or
  `review: internal` (the established upgrade-binary-first rule).
- **cli**: verb surface unchanged; `--inline` now rides a real event;
  `--gate-threads` is author-derived.
- **spec/docs**: `arch_action.md`, `arch_config.md`, the merge-gate leaves;
  mark the internal-verdict section of the merge-gate-v2 leaves superseded.
- **tests**: fake-gh unit updates (real event submitted; sod removed;
  author-derived gate threads); the gated real-gh suite gains a
  self-approve-422-fails-loudly case and a single-account bead-closed-predicate
  merge pass.
- **Compatibility**: removing `internal`/`reviews_log` is a breaking config
  change; strict decode makes a mismatched binary refuse a stale config
  (upgrade-binary-first). The default flips from `internal` to `none`, so a
  gate that relied on the internal verdict must now declare `review: github` or
  a `checks` predicate — the behavior change is the point.
