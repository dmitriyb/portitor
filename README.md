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
| `portitor upgrade-repo --repo <name>` | operator | re-bake a repo's hook shims to the current version |
| `portitor reconcile --repo <name>` | operator | re-forward accepted branches upstream after a forward failure |
| `portitor shell <fingerprint>` | sshd forced command | dispatch a connection to git-pack or the `pr` API |
| `portitor pr <action> --repo <name> --pr <n>` | the agent (via SSH) | one role-validated GitHub action |
| `portitor validate-config [--config <path>]` | operator / entrypoint | fail fast if a repo config is missing/malformed (run at container boot) |

---

## Configuration

Configuration is a **per-repo JSON file** in the registry
(`/etc/portitor/repos.d/<repo>.json`) — the **single canonical config identity**:
the gate hooks, `add-role`, and `portitor pr` all read the same file. `init-repo`
and `add-repo` default to it (`init-repo --config` remains for deliberate
exceptions; `add-role` warns if a repo's baked hook path diverges from the
registry file it edits). There is no global config and nothing is passed at gate
time.

### Schema

```jsonc
{
  // REQUIRED. The on-disk format version; this binary operates only with
  // exactly the version it understands (currently 1). Missing/lower/higher
  // refuses to run — never gate with a partially understood config. Unknown
  // keys, duplicate keys, and differently-cased keys are rejected outright.
  "format_version": 1,

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

  // Action policy: which roles may invoke each `portitor pr` verb. The verbs are
  // a closed mechanism set; WHO may run each is yours to define — DEFAULT-DENY:
  // an unlisted action (or an absent map) is refused for everyone.
  "action_roles": {
    "fetch":   ["implementer", "fixer", "reviewer", "merger", "owner"],
    "comment": ["implementer", "fixer", "reviewer", "merger", "owner"],
    "review":  ["reviewer", "owner"],
    "merge":   ["merger", "owner"],
    "close":   ["merger", "owner"]
  },

  // Roles whose keys are landing-only and must never gain commit-signing trust.
  // add-role refuses --pub for them and refuses rebinding an already-trusted
  // key to them. Absent = every role is a signing role.
  "identity_only_roles": ["merger"],

  // Check names that must be successful before a merge (matched against the
  // PR's statusCheckRollup). Empty/absent = advisory (repos without CI yet).
  "required_checks": ["ci/test"],

  // Optional. Append-only JSONL decision trail (gate accept/reject, action
  // allow/deny, auto-PR outcomes). Unset disables it; a write failure never
  // changes a verdict (reported loudly instead).
  "audit_log": "/srv/git/audit/yourrepo.jsonl",

  // Content rules: gate WHAT a role may change (see spec/gate/arch_content_rules.md).
  // Two families under one versioned schema — "version" must be exactly 1 for this
  // binary, and anything the binary does not fully understand refuses to gate.
  "content_rules": {
    "version": 1,

    // Structural: file-level operations (add|modify|delete|rename) × path glob × role.
    // First matching rule decides; else the first matching per-path default; else allow.
    "structural": {
      "rules": [
        { "name": "registry-ops-reviewer-only",
          "paths": ["registry/**"], "operations": ["delete", "rename"],
          "roles": {"not_in": ["reviewer", "owner"]}, "effect": "deny" }
      ],
      "defaults": []
    },

    // Semantic: record transitions inside a protected structured file. portitor never
    // parses the format itself — YOUR check command (any script/tool wrapper) extracts
    // the records; portitor evaluates generic field-transition rules over the delta.
    "semantic": {
      "files": [
        { "path": "registry/records.jsonl",
          "check": {
            "command": ["/usr/local/bin/records-list", "--json"],  // explicit argv, no shell
            "input_file": "records.jsonl",   // content materialized here (omit => stdin)
            "records_path": "records",       // dotted path to the array (omit => output IS the array)
            "id_field": "id"                 // record key (default "id")
          },
          "rules": [
            { "name": "record-approval-reviewer-only",
              "match": {"type": "field", "field": "stage", "to": "approved"},
              "roles": {"not_in": ["reviewer", "owner"]}, "effect": "deny" }
          ],
          "default": "allow" }
      ]
    }
  }
}
```

### `allowed_signers` file

Standard OpenSSH format, one signer per line (`<principal> namespaces="git" <key>`):

```
dca-implementer namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…
dca-reviewer    namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…
```

The principal label is cosmetic for portitor — the **role comes from the fingerprint**
via `roles` above; `allowed_signers` only establishes that the signature is trusted.
**Keys of `identity_only_roles` (e.g. the landing identity) must NOT be listed**:
a landing-only credential that can also sign commits collapses the role isolation,
so `add-role` refuses to trust such keys and refuses to rebind an already-trusted
key to an identity-only role.

### Registering roles with `add-role` (don't hand-edit the roles map)

Prefer `portitor add-role` over editing the `roles` map by hand: it validates the
fingerprint shape, upserts the binding atomically, optionally trusts a signing
role's public key in `allowed_signers` (fingerprint-checked, deduped), re-validates
the whole config, and serializes concurrent runs under a lock — so a fat-fingered
key or a half-written file can't quietly weaken the gate.

```bash
# Bind a signing role and trust its key for commit signatures in one step:
portitor add-role --repo myrepo --role implementer \
  --fingerprint SHA256:aaaa… --pub ./implementer.pub

# Bind the landing identity (identity-only: NO --pub, never trusted to sign):
portitor add-role --repo myrepo --role merger --fingerprint SHA256:cccc…
```

It edits the registry file (`<repos.d>/<name>.json`) and warns if the repo's baked
hook reads a different config. Re-running with the same arguments is an idempotent
no-op. See `spec/gate/arch_add_role.md` for the full contract.

### Roles

Roles are arbitrary strings you choose; portitor ships **no role names**. The
action-API policy is your config's `action_roles` map (default-deny — see the
schema above); the table there is the *recommended* layout:

| action | recommended roles |
|---|---|
| `fetch`, `comment` | every working role |
| `review` (verdict) | `reviewer`, `owner` |
| `merge`, `close` | `merger`, `owner` |

`owner` is your own (touch-required) override identity. `merger` is a dedicated,
**commit-less** landing identity — provision it only when you want merges via
portitor; omit it (or grant nobody `merge`) and merges are simply unavailable
through portitor.

`merge` additionally re-derives its preconditions from GitHub + the local repo
and refuses with the full unmet list: approval (`reviewDecision == APPROVED`),
a `CLEAN` merge state (covers behind-base / conflicts / blocked), every
`required_checks` entry green, and separation of duties (the requesting key
must not have signed any commit the PR introduces — the same check guards
`review --event approve`). The final `gh pr merge` remains the atomic gate;
enable GitHub branch protection as defense in depth.

Gate-side, `content_rules` enforce role→content (e.g. only `reviewer`/`owner` may
move a record's `stage` to `approved`, and only they may delete/rename files under
a protected directory). These are generic mechanism — every domain name (paths,
fields, values, the record-extraction command) is config; portitor ships none.

---

## Config examples (every case)

### 1. Minimal (gate only, no content rules, single role)

```json
{
  "format_version": 1,
  "default_branch": "main",
  "allowed_signers": "/etc/portitor/allowed_signers",
  "upstream_slug": "youruser/yourrepo",
  "roles": { "SHA256:aaaa…": "implementer" }
}
```

Accepts signed feature-branch pushes from the implementer key, rejects pushes to
`main` and unsigned/untrusted commits, forwards + auto-PRs accepted branches.

### 2. Full (all roles + content rules + ancestry)

```json
{
  "format_version": 1,
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
  "identity_only_roles": ["merger"],
  "action_roles": {
    "fetch":   ["implementer", "reviewer", "merger", "owner"],
    "comment": ["implementer", "reviewer", "merger", "owner"],
    "review":  ["reviewer", "owner"],
    "merge":   ["merger", "owner"],
    "close":   ["merger", "owner"]
  },
  "content_rules": {
    "version": 1,
    "structural": {
      "rules": [
        { "name": "registry-ops-reviewer-only",
          "paths": ["registry/**"], "operations": ["delete", "rename"],
          "roles": {"not_in": ["reviewer", "owner"]}, "effect": "deny" }
      ]
    },
    "semantic": {
      "files": [
        { "path": "registry/records.jsonl",
          "check": { "command": ["/usr/local/bin/records-list", "--json"],
                     "input_file": "records.jsonl", "records_path": "records" },
          "rules": [
            { "name": "record-approval-reviewer-only",
              "match": {"type": "field", "field": "stage", "to": "approved"},
              "roles": {"not_in": ["reviewer", "owner"]}, "effect": "deny" }
          ],
          "default": "allow" }
      ]
    }
  }
}
```

### 3. Protect a path structurally (owner-only CI config)

A `structural.rules` entry (a fragment — it goes inside `content_rules` next to
`"version": 1` as in example 2):

```json
{ "name": "ci-config-owner-only",
  "paths": [".github/workflows/**"],
  "operations": ["add", "modify", "delete", "rename"],
  "roles": {"not_in": ["owner"]}, "effect": "deny" }
```

Any file operation under `.github/workflows/` must be owner-signed — including
deletes and renames into or out of the directory (a rename is double-visible, so
it can't evade add/delete protection).

### 4. Restrict a role to specific transitions (semantic, default-deny)

The *restrict* form: the file default denies, allows carve out what each role may
do per named field. Here the implementer may only move `stage` from `draft` to
`review`; reviewer/owner may change `stage` freely; nobody else touches it.
Fields no rule names (e.g. `title`) are outside the protection surface — edits to
them are not gated:

```json
{ "path": "registry/records.jsonl",
  "check": { "command": ["/usr/local/bin/records-list", "--json"],
             "input_file": "records.jsonl", "records_path": "records" },
  "rules": [
    { "name": "no-born-or-moved-approved",
      "match": {"type": "field", "field": "stage", "to": "approved"},
      "roles": {"not_in": ["reviewer", "owner"]}, "effect": "deny" },
    { "name": "impl-may-submit",
      "match": {"type": "field", "field": "stage", "from": "draft", "to": "review"},
      "roles": {"in": ["implementer"]}, "effect": "allow" },
    { "name": "reviewer-owns-stage",
      "match": {"type": "field", "field": "stage", "changed": true},
      "roles": {"in": ["reviewer", "owner"]}, "effect": "allow" },
    { "name": "records-may-be-added",
      "match": {"type": "record", "change": "added"},
      "effect": "allow" }
  ],
  "default": "deny" }
```

(The first rule is the *gate* form riding in front of the restrict rules: a record
arriving at `approved` — by transition **or** born there on addition — is denied
for everyone but reviewer/owner, before the broader allows can decide.)

### 5. Multi-repo registry layout

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
