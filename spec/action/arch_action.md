# Action (GitHub-action mediation)

portitor is more than a git proxy: it is the **only** component with a GitHub credential, and it
mediates every GitHub action the agent might want. The agent holds no `gh` and no token; it reaches
GitHub solely through portitor over the **same SSH channel** it uses for git.

The governing rule mirrors the gate's: **never trust the request — re-derive authority from verified
state.** There is no `gh` passthrough. Each action is one of:

- **performed by portitor as a consequence of verified git state** (auto-open PR on forward), or
- **a narrow, role-validated operation** whose target portitor can tie to managed work (the `pr` API).

## One key, two surfaces (`portitor shell`)

An agent's key is installed with a forced command:

```
command="portitor shell <fingerprint>",restrict ssh-ed25519 AAAA… agent-key
```

`portitor shell` reads `$SSH_ORIGINAL_COMMAND` and `classify()`s it (a pure function):

| original command | route | effect |
|---|---|---|
| `git-receive-pack '<path>'` / `git-upload-pack '<path>'` | `git` | exec the real pack command (so pre/post-receive = the gate runs); `<path>` confined to the repo root, `*.git`, no `..` |
| `portitor pr <action> …` | `pr` | run the role-gated action API; role = `Config.Roles[fingerprint]` |
| anything else — including `git-upload-archive` | reject | refused |

This table is **closed**: exactly the two pack commands and `portitor pr`. `git-upload-archive`
is deliberately rejected (no supported flow needs archives; the narrowest surface wins). So a
single key grants **gated git push/clone + the narrow action API and nothing else** — no
interactive shell, no arbitrary commands. The dispatcher exports the caller's key fingerprint to
the pack subprocess environment, so the hooks can attribute the push in the audit trail.

## Auto-open PR (post-receive)

After the gate accepts a push and `Forward` mirrors a feature branch upstream, portitor opens the PR
itself — the agent never asks. Head = the gated branch, base = the default branch, **title** from the
branch's tip commit subject, **body** from the branch's commit messages (`git log <default>..<branch>`).
It is **idempotent**: if an open PR already exists for the branch (a self-correction re-push), the
existing number is returned — including under the check-then-act race (two forwards racing: if the
create call fails because the PR already exists, portitor re-queries and returns the existing number
as success rather than surfacing a spurious error). The receipt (`PR #N <url>`) is printed back over
the push.

## The `pr` action API

`portitor pr <action> --pr N` (bodies read from stdin so multi-line markdown survives transport).

**The action verbs are a closed mechanism set** — `fetch | comment | review | reply | resolve | merge | close` —
but **who may perform each is per-repo config, default-deny**: `action_roles` maps each verb to
the roles allowed to invoke it, and a verb not listed (or listed with no roles, or an absent
`action_roles` altogether) is refused for everyone. Every action is privileged, so the default is
the opposite of L1 content (which defaults to allow outside protected paths). Roles are free-form
opaque strings, consistent with the rest of the system; `validate-config` rejects an
`action_roles` key that is not one of the closed verbs.

```jsonc
"action_roles": {
  "fetch":   ["implementer", "fixer", "reviewer", "merger", "owner"],
  "comment": ["implementer", "fixer", "reviewer", "merger", "owner"],
  "review":  ["reviewer", "owner"],
  "reply":   ["implementer", "fixer", "owner"],
  "resolve": ["reviewer", "owner"],
  "merge":   ["merger", "owner"],
  "close":   ["merger", "owner"]
}
```

The table above is the **recommended** deployment policy (landing authority isolated in a
dedicated identity, thread resolution isolated in the reviewing identity so a fixer can never
grade its own answers), not a built-in: portitor ships no role names.

### Review threads (`fetch` + `reply` + `resolve`)

`fetch` includes the PR's review threads — id, resolved-state, path/line, and the comment chain —
via one GraphQL query (`reviewThreads`) merged into the existing JSON, so a fix step sees inline
feedback deterministically and addressed-state is data, not inference. `reply --pr N --thread
<id>` answers INTO a thread (body on stdin; GraphQL `addPullRequestReviewThreadReply`).
`resolve --pr N --thread <id>` resolves one (`resolveReviewThread`); `resolve --pr N
--gate-threads` resolves every unresolved thread the gate's own reviews created — the gate's own
identified by author (the gate's PAT account), so human-authored threads are never auto-resolved
by the gate. Thread enumeration reads the first
100 threads per query (no pagination in this pass): beyond that, resolution may take another
round — merge stays safe regardless, gated by the mandatory `CLEAN` check.

### review is a transparent GitHub passthrough

`review --event <approve|request-changes|comment>` submits the caller's **real** GitHub review
event and holds no gate state — portitor forwards the action, it does not record a verdict. With
`--inline`, stdin is a JSON document `{"body": <md>, "comments": [{"path", "line", "body"}, ...]}`
carried on that one native review so an agent raises real inline threads; without it, stdin is the
plain markdown body. If the forge refuses the event — GitHub returns HTTP 422 when the PAT account
approves its own PR — the gate fails loudly and forwards the error verbatim; a single account
approving its own PR is a topology/config problem, not one portitor papers over with invented
state. (`comment` is always self-account-safe, so the inline-thread feedback channel works in every
deployment; only `approve`/`request-changes` require a distinct reviewer account.) `review --event
approve` additionally auto-resolves the gate's own unresolved threads on that PR — the gate's own
identified by author (the gate's PAT account), not from any stored record.

The approval verdict therefore lives in a source of truth, never in portitor: on GitHub (a native
review, read by `merge_gate.review: github`) or in git (a signed commit — e.g. a reviewer-signed
bead-close — read by a `merge_gate.checks` predicate). A gate-proxy holding the authoritative
"approved" bit in its own `reviews_log` was a dual-write with no atomicity (a crash between the
append and the GitHub post, or a lost file, and the only record of approval is gone); it is
retired (see 2026-08-05-transparent-approve).

## Merge preconditions (re-derived, never trusted; review source configurable)

`merge` re-derives every precondition from authoritative GitHub state in one query
(`mergeStateStatus`, `statusCheckRollup`, `headRefName`, plus `reviewDecision` when configured)
plus the local repo, and refuses with the full list of unmet conditions:

- **review** — per the config's `merge_gate.review`, one of `github | none` (default `none` when
  the block or field is absent):
  `github`: GitHub's native `reviewDecision == "APPROVED"` — separated-account deployments, where
  GitHub itself enforces required approvals, refuses self-approval, and dismisses stale approvals
  on push. `none`: no review precondition here; the review gate, if any, is expressed as a
  `merge_gate.checks` predicate over git content (below). Default `none` because portitor invents
  no gate: an absent `merge_gate` requires only `CLEAN` + `required_checks`, and a deployment that
  wants a review precondition declares `github` or a `checks` predicate. (The retired `internal`
  source recorded a gate-owned verdict to fake native approval inside a single account; that state
  is gone — single-account deployments express the reviewer's verdict as a signed commit read by a
  `checks` predicate. See 2026-08-05-transparent-approve.)
- `mergeStateStatus == CLEAN` — **mandatory and non-configurable**; one field covers
  up-to-date-with-base (`BEHIND`), conflict-free (`DIRTY`), and blocked (`BLOCKED`, which
  inherits every GitHub branch rule including required conversation resolution). Portitor is a
  strict superset of GitHub's own rules, never a bypass.
- **required checks green** — the config's `required_checks` list; each named check must appear
  in `statusCheckRollup` with a successful conclusion. An empty list makes checks advisory
  (deliberate: repos without CI yet).
- **command predicates** — `merge_gate.checks`: named commands under the same hermetic execution
  contract as content-rule check commands (explicit argv from config, no shell, deadline,
  bounded output), run in the bare repo dir with the PR number and head SHA appended as the
  final two argv elements. Exit 0 = met; nonzero = unmet, named in the refusal; failure to RUN
  is an operational error (fail-closed). This is the seam for repo-policy predicates (e.g. a
  beads-state check) without portitor learning any domain word.
Separation of duties is **not** a portitor-enforced precondition — the hardcoded rule ("the
requesting key must not have signed any commit the PR introduces", formerly guarding both
`review --event approve` and `merge`) is removed. Reviewer-≠-author integrity lives in the source
of truth instead: GitHub's own branch protection for separated accounts, and, for single-account
git-content, the push-time `content_rules` that already gate a bead-close to the reviewer key (so a
closed bead is reviewer-attested by construction). See 2026-08-05-transparent-approve.

**Enforcement is hybrid, and the merge is head-pinned:** portitor re-derives for a clear verdict
and an actionable error, and the final `gh pr merge --match-head-commit <head>` lands exactly the
head the preconditions — the git-content predicates in particular — were evaluated against:
GitHub's atomic re-check covers only GitHub-enforced rules, so without the pin a push racing the
merge window could land a head a `merge_gate.checks` predicate never evaluated. A moved head fails
the merge (TOCTOU closed for both rule sources). Operators should additionally enable GitHub branch
protection (required checks + require-up-to-date) as defense in depth.

## Audit trail

Every L1 gate decision (accept/reject/operational error, malformed hook stdin included), every
forward outcome, every L2 action decision (allow/deny/error, with the reason), and every
auto-open outcome appends one JSON line to the config's `audit_log` path (fsync'd; file created
0600, missing parent directories 0700). Event kinds: `gate`, `forward`, `action`, `auto-pr`.
Events carry time, kind, repo, the caller's key fingerprint + role where known (the shell
dispatcher exports the fingerprint to the hook environment for push attribution), action/PR/
refs, verdict, and reason. The one inherently unauditable failure is a config that cannot be
loaded — no audit path is known then. An unset `audit_log` disables the trail (operator choice);
a **write failure never changes a verdict** — it is loudly reported to stderr instead, so an
audit-disk problem cannot block landing work (the trade-off is deliberate and visible).

### The landing identity

Merge/close are the most dangerous capabilities, so the recommended policy gives them to a
**dedicated landing identity**: a separate SSH key whose fingerprint maps to a landing-only role
(conventionally `merger`) in `Config.Roles`, granted `merge`/`close` in `action_roles`. That role
**never commits** — the key exists only to authorize landing over the action channel. It is
**optional**: with no such key provisioned (or no role granted `merge` in `action_roles`),
merge/close are simply unavailable through portitor (a human lands out-of-band).

## Boundaries

`internal/action` constructs all `gh` arguments and is the only place portitor shells to `gh`; a
swappable `Runner` keeps it unit-testable. Deployment wiring lives in `deploy/entrypoint.sh`: it
installs each agent/role key into `authorized_keys` with the `command="portitor shell <fp>"` forced
wrapper (`restrict`ed), and gives portitor its GitHub credential from `GH_TOKEN` via `gh auth login`
+ `gh auth setup-git` — one PAT serving both `gh pr` and `git push upstream`. Richer review payloads
(inline review comments) extend the `pr review` action without changing this model.
