# Configuration

A **per-repo JSON file in the registry** (`/etc/portitor/repos.d/<repo>.json`) is the single canonical config identity: the gate hooks, `add-role`, and `portitor pr` all read the same file.
`init-repo`/`add-repo` default to it (`init-repo --config` remains for deliberate exceptions).
There is no global config and nothing is passed at gate time.

For the formal rules governing how this file is found, versioned, and strictly decoded, see `spec/gate/arch_config.md`.
For the full content-rules schema, matcher vocabulary, and evaluation semantics, see `spec/gate/arch_content_rules.md`.
This document is the practical reference: the schema by example, `allowed_signers`, and the multi-repo registry layout.

## Schema

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

Only **operational** fields honor env overrides (`PORTITOR_UPSTREAM_REMOTE`, `PORTITOR_UPSTREAM_SLUG`, `PORTITOR_REPOS_DIR`, `PORTITOR_REPO_ROOT`, `PORTITOR_CONFIG` for the hook-baked path).
Gate-integrity fields (`default_branch`, `allowed_signers`, roles, rules) come solely from the file, so a stray env var can't weaken the gate.

## `allowed_signers`

Standard OpenSSH format, one signer per line (`<principal> namespaces="git" <key>`):

```
implementer namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…
reviewer    namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…
```

The principal label is cosmetic — the role comes from the fingerprint via `roles`.
**Keys of `identity_only_roles` (the landing identity) must NOT be listed**: a landing-only key that can also sign commits collapses the isolation, so `add-role` refuses to trust such a key.

This is the same OpenSSH `allowed_signers` format used for the release-artifact signature verification in the README's install section, but for a different namespace: commit signatures use `namespaces="git"`, release artifacts use `-n file`.
The two are deliberately domain-separated — a key trusted for one is never implicitly trusted for the other unless the same `allowed_signers` line lists both namespaces.

## Content rules

Two rule families under `content_rules`, both role×change gates:

- **Structural** — file operations (`add`/`modify`/`delete`/`rename`) × path glob × role, over the full rename-aware diff; a rename is double-visible, so it can't evade add/delete protection.
- **Semantic** — record-level transitions inside a protected structured file; portitor never parses the format itself — **your check command** (any script / tool wrapper) extracts records as generic `{id, fields}`, portitor computes the delta and applies field/record/label transition rules, content reaches the command only as data (a materialized file or stdin), never argv, never a shell, and a non-zero exit is a rejection, so a commit can never land content the command itself rejects.

Both share one model: first matching rule (whose role predicate accepts) decides; else the per-path/file default; else allow.
The full matcher vocabulary and semantics are in `spec/gate/arch_content_rules.md`.

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

To **restrict** a role to specific transitions, set `"default": "deny"` and add `allow` rules for what each role may do (e.g. implementer may only move `stage` from `draft` to `review`); fields no rule names are outside the protection surface.
See `spec/gate/arch_content_rules.md` for the restrict pattern in full.

## Multi-repo registry

One portitor mediates many repos — one config per repo under the registry dir:

```
/etc/portitor/
├── allowed_signers
└── repos.d/
    ├── repo-a.json
    └── repo-b.json
```

`add-repo` and `portitor pr --repo <name>` resolve `<name>.json` there; the bare's hook points at the same file.
Role keys may repeat across repos or differ.

Prefer `add-role` over hand-editing the `roles` map: it validates the fingerprint, upserts atomically under a lock, optionally trusts a signing key in `allowed_signers` (deduped), and re-validates — so a fat-fingered key or a half-written file can't quietly weaken the gate.
See `spec/gate/arch_add_role.md` for the full `add-role` behavior.
