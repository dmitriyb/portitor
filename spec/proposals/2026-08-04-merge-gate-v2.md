# Change Proposal: Merge gate v2 — internal review verdict, configurable preconditions, review threads

## Context

The merge preconditions hardcode `reviewDecision == "APPROVED"`. Empirically
verified against a real repo (dmitriyb/merge-test, 2026-08-04): GitHub sets
`reviewDecision` **only** when branch protection requires reviews — on a repo
without that rule it is null, so the hardcoded check blocks every merge
forever; and *with* the rule, the gate's PAT is the PR author's account and
GitHub refuses self-approval (HTTP 422), so the check is equally
unsatisfiable. Either way the auto-merge chain dead-ends at a manual merge —
for the single-account deployments portitor targets, the GitHub review
machinery is structurally unusable *and* redundant: portitor already
attributes reviews cryptographically (reviewer role = key fingerprint,
`action_roles`-gated), which is stronger attribution than a GitHub username.

Separately, review threads (inline comments) are invisible to the action API:
`fetch` returns flat reviews/comments with no thread ids or resolved-state,
nothing can reply into a thread, and nothing can resolve one — although the
GraphQL API supports all three with the existing PAT (verified: reviewThreads
enumeration, addPullRequestReviewThreadReply, resolveReviewThread; and an
unresolved thread flips `mergeStateStatus` to BLOCKED when the repo requires
conversation resolution, CLEAN after resolve). The fix loop therefore cannot
deterministically answer or clear inline feedback.

## Proposed change

### 1. Internal review verdict (the review record)

`review` records its verdict in gate-owned state: one appended JSON line
(fsync'd, like the audit log) at the config's `reviews_log` path —
`{time, pr, head_sha, fingerprint, role, event, threads}` — where `head_sha`
is the PR head at review time and `threads` are the ids of review threads
this review created. Lookup is last-wins per (pr, head): a new push
invalidates the verdict by construction. Toward GitHub the review posts as a
COMMENT-type review (same-account safe); with `--inline`, stdin is a JSON
document `{"body": md, "comments": [{"path", "line", "body"}...]}` so agent
reviews can raise real inline threads (thread ids captured into the record).
`review --event approve` additionally resolves every unresolved thread the
gate's own reviews created on that PR (recorded ids only — human threads are
never auto-resolved). The existing separation-of-duties check (requester must
not have signed introduced commits) is unchanged and guards approve.

### 2. Configurable merge preconditions (`merge_gate`)

A per-repo config block replaces the hardcoded review check:

```jsonc
"merge_gate": {
  "review": "internal",          // internal (default when absent) | github | none
  "checks": [                     // optional command predicates, run in the gate
    {"name": "bead-closed", "command": ["br", "--no-db", "..."]}
  ]
}
```

- `review: internal` — a reviews_log approval for the PR's CURRENT head, by a
  role `action_roles` allows to review. `github` — the old
  `reviewDecision == APPROVED` (multi-account deployments). `none` — skip.
- `mergeStateStatus == CLEAN` stays **mandatory and non-configurable** — it
  inherits every GitHub branch-protection rule (checks, conflicts,
  conversation resolution), so portitor remains a strict superset of GitHub's
  own rules, never a bypass.
- `required_checks` stays as is.
- `checks` are command predicates under the same hermetic execution contract
  as content-rule check commands (explicit argv, no shell, deadline, bounded
  output); the gate appends two args: the PR number and the head SHA, and
  runs in the bare repo dir. Exit 0 = pass; nonzero = unmet (named); failure
  to run = operational error (fail-closed).
- Separation of duties: unchanged, still mandatory.

### 3. Thread-aware action API

- `fetch` gains `reviewThreads`: id, isResolved, path, line, and the comment
  chain (author, body) per thread — one GraphQL query merged into the
  existing JSON.
- New verb `reply --pr N --thread <id>` (body on stdin) — answer INTO a
  thread (addPullRequestReviewThreadReply).
- New verb `resolve --pr N --thread <id>` (or `--gate-threads` for all
  unresolved gate-recorded ones) — resolveReviewThread.
- The closed verb set becomes `fetch | comment | review | reply | resolve |
  merge | close`; both new verbs are `action_roles`-gated, default-deny as
  ever. Recommended policy: `reply` for the fixing role, `resolve` for the
  reviewer (self-grading structurally excluded).

### 4. Real-GitHub test harness

Unit tests keep the fake gh runner. A new opt-in e2e suite runs against a
real disposable repo: gated on a local, gitignored `testdata/realgh.local.json`
({slug, pat keychain service}); the PAT is read by exec'ing `security` —
never from the environment. Scenarios: the four research probes automated
(null reviewDecision, self-approve refusal, thread create/reply/resolve with
mergeStateStatus BLOCKED→CLEAN, squash-merge) plus the full v2 merge-gate
pass (internal verdict + threads + predicates) and precise refusal lists.

## Impact expectation

- **action**: verbs, review recording/auto-resolve, merge preconditions,
  GraphQL calls; `arch_action.md` + module seed.
- **gate/config**: `merge_gate` + `reviews_log` schema keys, validation
  (enum, argv non-empty, closed verb set), `arch_config.md`.
- **cli**: flag surface for the new verbs (`--thread`, `--inline`,
  `--gate-threads`).
- **tests**: fake-gh unit coverage + the gated real-GitHub suite.
- **Fail-closed compatibility**: both new top-level keys are additive; the
  strict decode makes an older binary refuse a config carrying them (the
  established upgrade-binary-first rule). Absent `merge_gate` defaults to
  `review: internal` — the behavior change IS the bugfix (the old default
  could never merge in the deployments portitor targets).
