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

**The action verbs are a closed mechanism set** — `fetch | comment | review | merge | close` —
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
  "merge":   ["merger", "owner"],
  "close":   ["merger", "owner"]
}
```

The table above is the **recommended** deployment policy (landing authority isolated in a
dedicated identity), not a built-in: portitor ships no role names.

## Merge preconditions (re-derived, never trusted)

`merge` re-derives every precondition from authoritative GitHub state in one query
(`reviewDecision`, `mergeStateStatus`, `statusCheckRollup`, `headRefName`) plus the local repo,
and refuses with the full list of unmet conditions:

- `reviewDecision == APPROVED` — at least one approval, no pending changes-requested.
- `mergeStateStatus == CLEAN` — **mandatory**; one field covers up-to-date-with-base (`BEHIND`),
  conflict-free (`DIRTY`), and blocked (`BLOCKED`). A `BEHIND` branch is not mergeable even if
  its tip is green.
- **required checks green** — the config's `required_checks` list; each named check must appear
  in `statusCheckRollup` with a successful conclusion. An empty list makes checks advisory
  (deliberate: repos without CI yet).
- **separation of duties** — the requesting key must not have signed any commit the PR
  introduces. Verified against the *local* gated repo (`rev-list default..head`, `%GF` per
  commit — the same delegated-to-git verification the gate uses); the same check guards
  `review --event approve`. This is mostly inherent (roles are distinct keys) — the explicit
  check guards misconfiguration and an owner acting in multiple roles. A head ref portitor does
  not have locally is a refusal (fail-closed).

**Enforcement is hybrid:** portitor re-derives for a clear verdict and an actionable error, but
the final `gh pr merge` is the atomic gate — GitHub re-checks, so a state change in the window
fails the merge (TOCTOU closed GitHub-side). Operators should additionally enable GitHub branch
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
