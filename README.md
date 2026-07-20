# portitor

A self-hosted **git gateway** between an untrusted agent and your real GitHub
upstream. It is the *hard* enforcement boundary: it verifies the **result** of a
push — not the commands that produced it — and is the only component that holds a
GitHub credential.

```
agent ──ssh──▶ portitor ──┬─ git gate (pre-receive): signed? role? branch? content rules
  (no creds)              ├─ forward (post-receive): mirror accepted feature branches upstream
                          ├─ auto-PR: open a PR for each forwarded branch
                          └─ action API (portitor pr): role-gated comment/review/merge/close/fetch
                                                       (the ONLY GitHub credential lives here)
```

**Guiding principle — identity is a credential, not a label.** Each commit is
signed by a per-role key; portitor maps the signer *fingerprint* to a role and
enforces per-role rules. A container holding only one role's key cannot act as
another role. portitor is generic mechanism: every domain name (roles, paths,
fields, values, the record-extraction command) is config; portitor ships none.
Its only dependency is git.

---

## Quickstart

portitor runs as a container holding the GitHub PAT + the role map; the agent
runs elsewhere with no credential.

```bash
# 1. Write the per-repo config into the registry (see Configuration below).
#    One file per repo: <config-dir>/repos.d/<name>.json (+ allowed_signers).

# 2. Bring up the container (deploy/run.sh reads the PAT from your keychain and
#    mounts the config dir read-only at /etc/portitor + a persistent /srv/git).
deploy/run.sh --config-dir ./portitor-config \
  --keys ./implementer.pub,./reviewer.pub,./merger.pub

# 3. Provision the repo (config must already be at repos.d/<name>.json).
docker exec -u git portitor portitor add-repo \
  --repo myrepo --upstream https://github.com/you/myrepo.git

# 4. Bind role keys (or write the roles map by hand — add-role is safer).
docker exec -u git portitor portitor add-role \
  --repo myrepo --role implementer --fingerprint SHA256:… --pub ./implementer.pub
```

The agent then clones and pushes over SSH (`ssh://git@portitor/srv/git/myrepo.git`);
its key is installed with a forced command so it can do **only** gated git + the
role-checked `portitor pr` API. On an accepted push, portitor forwards the branch
upstream with its own credential and opens the PR, printing `PR #<n> <url>` back.

`deploy/DEPLOY.md` is a full end-to-end runbook; the entrypoint runs
`validate-config` over every registry config at boot and refuses to start if any
is invalid.

---

## Releases

The `portitor` binary (the same binary the container image runs, useful
standalone for `add-role`/`validate-config`/`reconcile` from an operator's
machine) is published on the [GitHub Releases page][releases] for
linux/darwin, amd64/arm64. The container itself is **not** a release
artifact — build it locally with `docker build -t portitor .`.

[releases]: https://github.com/dmitriyb/portitor/releases

Each release publishes, per platform: a `.tar.gz` archive, a `.sha256`
checksum, and a `.minisig` Ed25519 signature, plus a consolidated
`checksums.txt` and a machine-readable `manifest.json` (schema, target,
sha256, and size per artifact — for tooling that wants to pin a checksum
without scraping the release page).

**Verify a checksum:**

```bash
sha256sum -c portitor_<version>_<os>_<arch>.tar.gz.sha256
```

**Verify the signature** ([minisign][minisign], Ed25519):

```bash
minisign -Vm portitor_<version>_<os>_<arch>.tar.gz \
  -P RWT5i/aTmI+e2Fi0U0na8qcxEYbMsMYd6JBYbhSDOtxDqF+5orMVUFZO
```

[minisign]: https://jedisct1.github.io/minisign/

Releases also carry a [SLSA build provenance attestation][slsa], verifiable with:

```bash
gh attestation verify portitor_<version>_<os>_<arch>.tar.gz --owner dmitriyb
```

[slsa]: https://slsa.dev/

---

## Configuration

A **per-repo JSON file in the registry** (`/etc/portitor/repos.d/<repo>.json`) is
the **single canonical config identity**: the gate hooks, `add-role`, and
`portitor pr` all read the same file. `init-repo`/`add-repo` default to it
(`init-repo --config` remains for deliberate exceptions). There is no global
config and nothing is passed at gate time.

### Schema

```jsonc
{
  // REQUIRED. On-disk format version; this binary operates only with the exact
  // version it understands (1). Missing/lower/higher refuses to run — never
  // gate a partially understood config. Unknown, duplicate, or differently-cased
  // keys are rejected outright.
  "format_version": 1,

  // Protected branch. Pushes/deletes to it are rejected (use a feature branch +
  // PR). If omitted, derived from the bare repo's HEAD symref.
  "default_branch": "main",

  // Path (in the container) to an OpenSSH allowed_signers file listing the
  // commit signers portitor trusts. REQUIRED — if empty, every commit is
  // untrusted and rejected.
  "allowed_signers": "/etc/portitor/allowed_signers",

  // Signer FINGERPRINT (git %GF, "SHA256:…") -> role. The role follows the key,
  // not a label in the commit — so it is unforgeable.
  "roles": {
    "SHA256:aaaa…": "implementer",
    "SHA256:bbbb…": "reviewer",
    "SHA256:cccc…": "merger"
  },

  // Which roles may invoke each `portitor pr` verb. Verbs are a closed set; WHO
  // may run each is yours to define — DEFAULT-DENY (an unlisted action, or an
  // absent map, is refused for everyone).
  "action_roles": {
    "fetch":   ["implementer", "reviewer", "merger", "owner"],
    "comment": ["implementer", "reviewer", "merger", "owner"],
    "review":  ["reviewer", "owner"],
    "merge":   ["merger", "owner"],
    "close":   ["merger", "owner"]
  },

  // Roles whose keys are landing-only and must never gain commit-signing trust.
  // add-role refuses --pub for them and refuses rebinding an already-trusted key
  // to them. Absent = every role is a signing role.
  "identity_only_roles": ["merger"],

  // Optional. Checks that must be green before a merge (matched against the PR's
  // statusCheckRollup). Empty/absent = advisory.
  "required_checks": ["ci/test"],

  // Optional. The git remote (added by init-repo) accepted branches forward to.
  "upstream_remote": "upstream",
  // owner/name for gh; else derived from the upstream remote URL.
  "upstream_slug": "you/myrepo",
  // Optional. Reject a feature branch whose tip is not a descendant of the
  // current default ("stale-base"). Off by default.
  "require_up_to_date_with_default": false,

  // Optional. Append-only JSONL decision trail (gate/forward/action/auto-pr).
  // Unset disables it; a write failure never changes a verdict (reported loudly).
  "audit_log": "/srv/git/audit/myrepo.jsonl",

  // Content rules — gate WHAT a role may change (see below).
  "content_rules": { "version": 1, "structural": { "rules": [] }, "semantic": { "files": [] } }
}
```

Only **operational** fields honor env overrides (`PORTITOR_UPSTREAM_REMOTE`,
`PORTITOR_UPSTREAM_SLUG`, `PORTITOR_REPOS_DIR`, `PORTITOR_REPO_ROOT`,
`PORTITOR_CONFIG` for the hook-baked path). Gate-integrity fields
(`default_branch`, `allowed_signers`, roles, rules) come solely from the file, so
a stray env var can't weaken the gate.

### `allowed_signers`

Standard OpenSSH format, one signer per line (`<principal> namespaces="git" <key>`):

```
implementer namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…
reviewer    namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…
```

The principal label is cosmetic — the role comes from the fingerprint via
`roles`. **Keys of `identity_only_roles` (the landing identity) must NOT be
listed**: a landing-only key that can also sign commits collapses the isolation,
so `add-role` refuses to trust such a key.

### Content rules

Two rule families under `content_rules`, both role×change gates:

- **Structural** — file operations (`add`/`modify`/`delete`/`rename`) × path glob
  × role, over the full rename-aware diff. A rename is double-visible, so it can't
  evade add/delete protection.
- **Semantic** — record-level transitions inside a protected structured file.
  portitor never parses the format itself: **your check command** (any script /
  tool wrapper) extracts records as generic `{id, fields}`; portitor computes the
  delta and applies field/record/label transition rules. Content reaches the
  command only as data (a materialized file or stdin), never argv, never a shell.
  A non-zero exit is a rejection — so a commit can never land content the command
  itself rejects.

Both share one model: first matching rule (whose role predicate accepts) decides;
else the per-path/file default; else allow. The full matcher vocabulary and
semantics are in `spec/gate/arch_content_rules.md`.

```jsonc
"content_rules": {
  "version": 1,
  "structural": {
    "rules": [
      // Only reviewer/owner may delete or rename anything under registry/.
      { "name": "registry-ops-reviewer-only",
        "paths": ["registry/**"], "operations": ["delete", "rename"],
        "roles": {"not_in": ["reviewer", "owner"]}, "effect": "deny" }
    ]
  },
  "semantic": {
    "files": [
      { "path": "registry/records.jsonl",
        "check": {
          "command": ["/usr/local/bin/records-list", "--json"], // explicit argv
          "input_file": "records.jsonl",  // content materialized here (omit => stdin)
          "records_path": "records",       // dotted path to the array (omit => output IS the array)
          "id_field": "id"                 // record key (default "id")
        },
        "rules": [
          // Only reviewer/owner may move a record to stage=approved (by transition
          // OR born there on addition).
          { "name": "approval-reviewer-only",
            "match": {"type": "field", "field": "stage", "to": "approved"},
            "roles": {"not_in": ["reviewer", "owner"]}, "effect": "deny" }
        ],
        "default": "allow" }
    ]
  }
}
```

To **restrict** a role to specific transitions, set `"default": "deny"` and add
`allow` rules for what each role may do (e.g. implementer may only move `stage`
from `draft` to `review`); fields no rule names are outside the protection
surface. See `arch_content_rules.md` for the restrict pattern in full.

### Multi-repo registry

One portitor mediates many repos — one config per repo under the registry dir:

```
/etc/portitor/
├── allowed_signers
└── repos.d/
    ├── repo-a.json
    └── repo-b.json
```

`add-repo` and `portitor pr --repo <name>` resolve `<name>.json` there; the bare's
hook points at the same file. Role keys may repeat across repos or differ.

---

## Commands

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

Prefer `add-role` over hand-editing the `roles` map: it validates the fingerprint,
upserts atomically under a lock, optionally trusts a signing key in
`allowed_signers` (deduped), and re-validates — so a fat-fingered key or a
half-written file can't quietly weaken the gate.

### The `pr` action API

`portitor pr <fetch|comment|review|merge|close> --repo <name> --pr <n>` runs one
action with portitor's credential after checking the caller's role against
`action_roles` (default-deny). Bodies are read from stdin so multi-line markdown
survives transport.

`merge` additionally **re-derives its preconditions from GitHub + the local repo**
(never the request) and refuses with the full unmet list: approval
(`reviewDecision == APPROVED`), a `CLEAN` merge state (covers behind-base /
conflicts / blocked), every `required_checks` entry green, and separation of
duties — the requesting key must not have signed any commit the PR introduces
(the same check guards `review --event approve`). The final `gh pr merge` is the
atomic gate; enable GitHub branch protection as defense in depth.

`owner` is your own (touch-required) override identity. A landing role (e.g.
`merger`) is a dedicated, **commit-less** identity; provision it only when you
want merges via portitor — omit it (or grant nobody `merge`) and merges are
unavailable through portitor.

---

## How it works

The gate (`pre-receive`) inspects the objects being landed, delegating the crypto
to git and deciding fail-closed — any internal error rejects the whole push:

- **Deterministic verification.** Every portitor git call disables replace-object
  substitution; the gate's fact-gathering runs hermetically (ambient git config
  masked) and pins the `allowed_signers` trust root unconditionally, so a verdict
  is a function of the push, the repo, and the config — never machine state.
- **Signed by an allowed signer.** Each introduced commit must carry `%G? == G`;
  its `%GF` fingerprint maps to a role.
- **Branch namespace only.** Only `refs/heads/<name>` is accepted; the default
  branch is never a push target; other namespaces (tags, notes, `refs/replace/*`)
  are refused.
- **Content rules** gate file operations and record transitions by role (above).
- **Forwarding** (`post-receive`) mirrors accepted feature branches upstream and
  auto-opens the PR, reporting every ref's outcome (an out-of-order forward whose
  tip upstream already contains is success). `reconcile` recovers a failed one.
- **Audit** appends a durable, fsync'd decision trail (never altering a verdict).

The mechanism is pure and unit-/fuzz-tested; the authoritative spec is under
`spec/**` (`spec/gate/`, `spec/action/`).

---

## Integration with the dca agent

The dca/dce agents (dotfiles repo) reach portitor as their **only** git remote:

- **Clone/push:** `ssh://git@portitor/srv/git/<repo>.git`. `<repo>` is validated
  (`[A-Za-z0-9._-]`, no traversal) on both the git and `pr` paths.
- **Auth:** one per-role SSH key per agent, supplied via `AGENT_AUTHORIZED_KEY`
  (one public key per line), installed with the `portitor shell <fp>` forced
  command (`restrict`, `no-touch-required` for resident FIDO2 keys).
- **Config mount:** the registry mounted read-only at `/etc/portitor`.
- **GitHub:** portitor holds the `GH_TOKEN`; the agent never sees it and fetches
  PR state with `portitor pr fetch --repo <repo> --pr <n>`.

Bring-up: the dotfiles `docker-claude --portitor up` (compose wrapper) or the
standalone `deploy/run.sh` + `deploy/DEPLOY.md` here.

## License

Apache-2.0 (see `LICENSE`).
