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
| anything else | reject | refused |

So a single key grants **gated git push/clone + the narrow action API and nothing else** — no
interactive shell, no arbitrary commands.

## Auto-open PR (post-receive)

After the gate accepts a push and `Forward` mirrors a feature branch upstream, portitor opens the PR
itself — the agent never asks. Head = the gated branch, base = the default branch, **title** from the
branch's tip commit subject, **body** from the branch's commit messages (`git log <default>..<branch>`).
It is **idempotent**: if an open PR already exists for the branch (a self-correction re-push), the
existing number is returned. The receipt (`PR #N <url>`) is printed back over the push.

## The `pr` action API

`portitor pr <action> --pr N` (bodies read from stdin so multi-line markdown survives transport):

| action | allowed roles | notes |
|---|---|---|
| `fetch` | any known role | returns PR JSON (reviews/comments/commits) — the read-side for review/fix bundles |
| `comment` | any known role | top-level PR comment |
| `review` | reviewer, owner | verdict: approve / request-changes / comment |
| `merge` | **merger**, owner | re-derives `reviewDecision == APPROVED` first; refuses otherwise |
| `close` | **merger**, owner | close without merge |

`implementer`/`fixer` can comment and fetch but **cannot** review, merge, or close — the landing
authority is isolated.

### The `merger` role

Merge/close are the most dangerous capabilities, so they require a **dedicated `merger` identity**: a
separate SSH key whose fingerprint maps to `merger` in `Config.Roles`. That role **never commits** —
the key exists only to authorize landing over the action channel. It is **optional**: if no merger
key is provisioned, merge/close are simply unavailable through portitor (a human lands out-of-band).

## Boundaries

`internal/action` constructs all `gh` arguments and is the only place portitor shells to `gh`; a
swappable `Runner` keeps it unit-testable. Provisioning the agent/merger keys into portitor's
`authorized_keys` with the `command="portitor shell <fp>"` wrapper is a deployment concern (the
portitor container entrypoint). Richer review payloads (inline review comments) extend the `pr review`
action without changing this model.
