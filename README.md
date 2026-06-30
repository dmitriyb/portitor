# portitor

A self-hosted **git gateway** that sits between an (untrusted) agent and your real
GitHub upstream. It is the *hard* enforcement boundary: it verifies the **result**
of a push — not the commands that produced it — and is the only component that
holds a GitHub credential.

```
agent ──ssh──▶ portitor ──┬─ git gate (pre-receive): signed? role? branch? ancestry? content rules
  (no creds)              ├─ forward (post-receive): mirror accepted feature branches upstream
                          ├─ auto-PR: open a PR for each forwarded branch
                          └─ action API (portitor pr): role-gated comment/review/merge/close/fetch
                                                       (the ONLY GitHub credential lives here)
```

Guiding principle: **identity is a credential, not a label.** Each commit is signed
by a per-role key; portitor maps the signer *fingerprint* to a role and enforces
per-role rules. A container holding only one role's key cannot act as another role.

## How it works

- **Bare repo per upstream** under `/srv/git/<name>.git`, owned by the `git` user.
- The agent clones/pushes over SSH; the key is installed with a forced command
  (`command="portitor shell <fingerprint>"`) so it can do **only** gated git +
  the role-checked `portitor pr` API — nothing else.
- `pre-receive` runs the gate and rejects the whole push atomically with a complete
  `remote:` report. `post-receive` forwards accepted feature branches upstream
  (with portitor's credential, never the agent's) and opens a PR.
- The agent never holds a GitHub token; all GitHub writes go through `portitor pr`.

## Subcommands

| command | run by | purpose |
|---|---|---|
| `portitor pre-receive` / `post-receive` | the bare repo's git hooks | the gate / forward + auto-PR |
| `portitor init-repo --bare … --config … --upstream …` | operator | create a gated bare repo + mirror |
| `portitor add-repo --repo <name> --upstream <url>` | operator | init-repo using the registry conventions |
| `portitor shell <fingerprint>` | sshd forced command | dispatch a connection to git-pack or the `pr` API |
| `portitor pr <action> --repo <name> --pr <n>` | the agent (via SSH) | one role-validated GitHub action |
| `portitor validate-config [--config <path>]` | operator / entrypoint | fail fast if a repo config is missing/malformed (run at container boot) |

---

## Configuration

Configuration is a **per-repo JSON file** — there is no global config and nothing
is passed at gate time. You write one JSON per repo and *associate* it once with
`init-repo --config` (or `add-repo`, which uses the registry path by default).

### Schema

```jsonc
{
  // The protected branch. Pushes/deletes to it are rejected (use a feature branch + PR).
  // If omitted, derived from the bare repo's HEAD symref.
  "default_branch": "main",

  // Path (inside the portitor container) to an OpenSSH allowed_signers file listing
  // the commit signers portitor trusts. REQUIRED for any commit to be accepted —
  // if empty, every commit is treated as untrusted and rejected.
  "allowed_signers": "/etc/portitor/allowed_signers",

  // The git remote (configured on the bare by init-repo) that accepted feature
  // branches are forwarded to. Default "upstream".
  "upstream_remote": "upstream",

  // owner/name slug for the GitHub action API + auto-PR. If omitted, portitor tries
  // to derive it from the upstream remote URL.
  "upstream_slug": "youruser/yourrepo",

  // Optional. When true, a feature branch whose tip is not a descendant of the
  // current default branch is rejected ("stale-base") — forces a fresh rebase.
  // Off by default.
  "require_up_to_date_with_default": false,

  // Signer key FINGERPRINT (git %GF, "SHA256:...") -> role name. The role is
  // unforgeable: it follows the key, not a label in the commit/skill.
  "roles": {
    "SHA256:aaaa…": "implementer",
    "SHA256:bbbb…": "reviewer",
    "SHA256:cccc…": "merger"
  },

  // Content rules: gate WHAT a role may change. For each introduced commit, if its
  // diff to path_glob ADDS a line matching added_regex, the signer's role must be
  // in allowed_roles — else the push is rejected with violation `name`.
  "role_rules": [
    {
      "name": "bead-close-reviewer-only",
      "path_glob": ".beads/issues.jsonl",
      "added_regex": "\"status\"\\s*:\\s*\"closed\"",
      "allowed_roles": ["reviewer", "owner"]
    }
  ]
}
```

> Build this file with a tool (e.g. `jq`), **not** a shell heredoc — a heredoc
> mangles the regex backslashes in `added_regex` and produces invalid JSON.

### `allowed_signers` file

Standard OpenSSH format, one signer per line (`<principal> namespaces="git" <key>`):

```
dca-implementer namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…
dca-reviewer    namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…
dca-merger      namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…
```

The principal label is cosmetic for portitor — the **role comes from the fingerprint**
via `roles` above; `allowed_signers` only establishes that the signature is trusted.
(The `merger` key never signs commits; listing it is harmless.)

### Roles

Roles are arbitrary strings you choose; the built-in action-API policy is below.
It mirrors `RoleActions` in `internal/action/policy.go` — the single source of truth
(`action.RoleCan`); keep this table in sync if you add an action.

| action | allowed roles |
|---|---|
| `fetch`, `comment` | any known role (implementer / fixer / reviewer / merger / owner) |
| `review` (verdict) | `reviewer`, `owner` |
| `merge`, `close` | `merger`, `owner` |

`owner` is your own (touch-required) override identity. `merger` is a dedicated,
**commit-less** landing identity — provision it only when you want merges via
portitor; omit it and merges are simply unavailable through portitor.

Gate-side, `role_rules` enforce role→content (e.g. only `reviewer`/`owner` may add
a `"status":"closed"` line to the beads file). These are generic — portitor knows
nothing of beads/spex; the rule above is just config.

---

## Config examples (every case)

### 1. Minimal (gate only, no role rules, single role)

```json
{
  "default_branch": "main",
  "allowed_signers": "/etc/portitor/allowed_signers",
  "upstream_slug": "youruser/yourrepo",
  "roles": { "SHA256:aaaa…": "implementer" },
  "role_rules": []
}
```

Accepts signed feature-branch pushes from the implementer key, rejects pushes to
`main` and unsigned/untrusted commits, forwards + auto-PRs accepted branches.

### 2. Full (all roles + content rule + ancestry)

```json
{
  "default_branch": "main",
  "allowed_signers": "/etc/portitor/allowed_signers",
  "upstream_remote": "upstream",
  "upstream_slug": "youruser/yourrepo",
  "require_up_to_date_with_default": true,
  "roles": {
    "SHA256:aaaa…": "implementer",
    "SHA256:bbbb…": "reviewer",
    "SHA256:cccc…": "merger"
  },
  "role_rules": [
    { "name": "bead-close-reviewer-only", "path_glob": ".beads/issues.jsonl",
      "added_regex": "\"status\"\\s*:\\s*\"closed\"", "allowed_roles": ["reviewer", "owner"] }
  ]
}
```

### 3. A second, generic content rule (e.g. protect a path)

```json
{ "name": "ci-config-owner-only", "path_glob": ".github/workflows/*",
  "added_regex": ".*", "allowed_roles": ["owner"] }
```

Any added line under `.github/workflows/` must be owner-signed.

### 4. Multi-repo registry layout

One portitor mediates many repos. Put one config per repo under the registry dir
(default `/etc/portitor/repos.d/`); `add-repo` and `portitor pr --repo <name>`
resolve `<name>.json` there, and the bare's hook points at the same file:

```
/etc/portitor/
├── allowed_signers
└── repos.d/
    ├── repo-a.json      # upstream_slug: youruser/repo-a, its own roles/rules
    └── repo-b.json      # upstream_slug: youruser/repo-b
```

The `roles` map may repeat the same keys across repos (or differ per repo).

### Environment overrides

Only **operational** fields can be overridden by env — never the gate-integrity
fields. `default_branch` and `allowed_signers` come *solely* from the config file, so
a stray/injected env var can't weaken the gate. Overridable: `PORTITOR_CONFIG` (the
config path the hooks load), `PORTITOR_UPSTREAM_REMOTE`, `PORTITOR_UPSTREAM_SLUG`,
`PORTITOR_REPOS_DIR` (registry, default `/etc/portitor/repos.d`), `PORTITOR_REPO_ROOT`
(bares, default `/srv/git`).

---

## Integration with the dca agent

The dca/dce agents (in the dotfiles repo) reach portitor as the **only** git remote
they can talk to; the contract:

- **Clone/push:** `ssh://git@portitor/srv/git/<repo>.git`. `<repo>` is validated
  (`[A-Za-z0-9._-]`) on both the git and `pr` paths — no traversal.
- **Auth:** one per-role SSH key per agent, supplied to the container via
  `AGENT_AUTHORIZED_KEY` (one public key per line). Each is installed with the forced
  command `command="PORTITOR_CONFIG=… portitor shell <fp>",restrict,no-touch-required`
  — so the key can only run gated git-pack + `portitor pr`.
- **Config mount:** the per-repo configs live in `PORTITOR_CONFIG_DIR` (mounted
  read-only at `/etc/portitor`; registry at `/etc/portitor/repos.d/<repo>.json`).
- **GitHub:** portitor holds the `GH_TOKEN`; on an accepted push it forwards the
  feature branch upstream and auto-opens the PR, printing `PR #<n> <url>` back to the
  agent. The agent never sees the token. It fetches PR state with
  `portitor pr fetch --repo <repo> --pr <n>` over the same SSH channel.
- **Boot check:** the entrypoint runs `portitor validate-config` over every repo
  config and refuses to start if any is invalid.

## Deployment

portitor runs as a container holding the GitHub PAT + the role map; the agent runs
elsewhere with no credential. Two ways to bring it up:

- **Declarative (recommended):** the dotfiles repo's `docker-claude --portitor up`
  wraps a `docker-compose.yml` (portitor + the agent egress proxy + the internal
  network), builds the image, reads the PAT from your keychain, and mounts the
  config dir + the persistent `/srv/git` volume. `--portitor down|status|restart`
  manage it.
- **Standalone:** `deploy/run.sh` (a keychain-PAT launcher) + `deploy/DEPLOY.md`
  (a full runbook) here in this repo.

Provision a repo once (survives restarts via the volume):

```bash
# place the repo config first, then:
docker exec -u git portitor portitor add-repo \
  --repo yourrepo --upstream https://github.com/youruser/yourrepo.git
```

(`-u git` so gh's git credential helper, configured for the git user, is used.)

## License

Apache-2.0 (see `LICENSE`).
