# Content protection (L1): structural + semantic rules

Replaces the retired textual `RoleRule` (a regex over added diff lines). The old design compared
*text* that a downstream consumer would later *parse* — two readers that could disagree, and a
scan that binary diffs, oversized lines, or `+`-prefix artifacts could silently disable. The
redesign enforces on the objects themselves, through two rule families sharing one model:

- **Structural rules** gate *file-level operations* (add / modify / delete / rename) by path and
  role, computed natively from git name-status over the full diff (rename-aware).
- **Semantic rules** gate *record-level transitions* inside a protected structured file. Portitor
  never parses the file's format itself: an **operator-configured check command** extracts the
  records (the same seam as a data-source command — any script, any tool wrapper, any service
  client can fill it, interchangeably), and portitor computes transitions generically over the
  extracted record deltas.

Portitor is mechanism, not policy: rules, paths, roles, field names, gated values, and the check
command itself live in per-repo config; portitor has **no built-in knowledge of any domain
format or third-party tool**. The mechanism's only external dependency is git (the substrate,
including signature verification) — everything domain-shaped is config. Roles are opaque
strings; the signer's role comes from its key fingerprint (`Config.Roles`), never from a label
in the content.

## Config schema (versioned)

```jsonc
"content_rules": {
  // REQUIRED. The rule-schema version. This binary understands exactly version 1.
  // Any other value (missing, 0, 2, …) is a config error and the gate refuses to
  // run — it never gates with a partially understood ruleset. Independent of the
  // application version; bumped only when this schema changes incompatibly.
  // (Adding a new matcher/operation VALUE is additive and does NOT bump it — an
  // older binary rejects unknown values fail-closed, see "Strict validation".)
  "version": 1,

  "structural": {
    "rules": [
      {
        "name": "registry-ops-elevated-only",     // stable id, surfaced as the violation rule
        "paths": ["registry/**"],                 // non-empty; glob (see "Path globs")
        "operations": ["delete", "rename"],       // non-empty subset of add|modify|delete|rename
        "roles": {"not_in": ["reviewer", "owner"]}, // optional predicate; exactly one of in/not_in
        "effect": "deny"                          // allow | deny
      }
    ],
    "defaults": [
      // Per-path default, consulted only when no rule matched. First match wins.
      {"paths": [".github/workflows/**"], "effect": "deny"}
    ]
  },

  "semantic": {
    "files": [
      {
        "path": "registry/records.jsonl",  // exact repo path of the protected file
        "check": {
          // The operator-configured record extractor. Explicit argv — never a
          // shell string, and pushed content NEVER appears in it. Part of the
          // operator trust root, same class as allowed_signers (the config
          // already decides the gate's trust anchors).
          "command": ["/usr/local/bin/records-list", "--json"],
          // Optional: relative path (inside the private working directory)
          // where the side's content is materialized before the command runs.
          // Absent => the content is piped to the command's stdin.
          "input_file": "records.jsonl",
          // Optional: dotted JSON path to the record array in the command's
          // stdout. Absent => the stdout IS the array.
          "records_path": "records",
          // Optional: the field that keys a record. Default "id".
          "id_field": "id"
        },
        "rules": [
          {
            "name": "record-approval-requires-reviewer",
            "match": {"type": "field", "field": "stage", "to": "approved"},
            "roles": {"not_in": ["reviewer", "owner"]},
            "effect": "deny"
          }
        ],
        "default": "allow"   // allow | deny — applied per change unit when no rule matched
      }
    ]
  }
}
```

## Strict validation (fail-closed compile)

Both families and each subsection are optional (`content_rules` absent = no content rules), but
whatever is present is validated strictly at load time; **any problem below is a config error
and the gate refuses to run** (rejecting every push, loudly) rather than skip what it cannot
understand:

- `version` missing or ≠ 1;
- structural: empty `paths`, a malformed glob, empty `operations`, an unknown operation, a
  missing/unknown `effect`, a `roles` predicate with both or neither of `in`/`not_in`;
- semantic: a file entry with an empty `path`, a duplicate `path` across entries, a missing
  `check`, a `check.command` that is empty or contains an empty argv element, an `input_file`
  that is absolute, non-clean, or escapes the working directory (`..`), an empty `records_path`
  segment, an empty `id_field`, a `default` other than `allow`/`deny` (absent = allow), and —
  because an under-specified matcher must never silently widen (the retired `added_regex:""`
  match-everything failure) — **incomplete matchers are errors**: a `field` matcher requires
  `field` plus at least one of `from`/`to`/`changed` (`changed` must be literally `true` and
  cannot combine with `from`/`to`); a `record` matcher requires `change` ∈ {added, removed}; a
  `labels` matcher requires exactly one of `contains`/`added`, a non-empty string;
- any unknown matcher `type` or unknown key inside a matcher (the additive-extension guard: a
  config using future vocabulary makes this binary refuse to gate, never ignore the rule);
- a duplicate rule `name` — names share **one namespace across both families**, so a violation's
  `Rule` id is unambiguous;
- a rule with no `name`.

**Retirement sentinel:** the old `role_rules` key is retired. A config still carrying it fails
validation and the gate refuses to run with a migration message — the old rules are never
silently dropped.

## Shared rule model and precedence (fixed, not configurable)

Every **decision unit** — a structural change event, or a semantic change unit (both defined
below) — is evaluated for the commit's signer role as:

1. Walk `rules` in listed order. The **first** rule whose matcher fires for this unit **and**
   whose `roles` predicate accepts the role decides: its `effect`. A rule whose matcher fires
   but whose `roles` predicate rejects the role does **not** decide — evaluation continues.
   A rule with no `roles` field accepts every role.
2. If no rule decided: the first matching per-path `default` (structural) / the file's `default`
   (semantic) decides.
3. Otherwise: **allow** (the global default — content protection is opt-in per path; the L2
   action layer is the opposite, default-deny).

`deny` produces a violation named by the deciding rule (or `structural-default` /
`semantic-default`, with the decided path/operation or record/field in the detail). Violations
aggregate across the whole push (atomic reject, complete report). An unmapped signer has the
empty role `""`: it never satisfies an `in` predicate and satisfies any `not_in` predicate that
does not list `""` — so restriction rules bind unknown signers by construction.

The decision unit is deliberately **fine-grained** (one file operation; one record's addition or
removal; one named field's change). An `allow` therefore authorizes exactly the unit its matcher
fired on — it can never launder an unrelated change that happens to sit in the same record or
commit. This is the explicit answer to the restrict-scope pitfall: **the scope of every rule is
the unit, and the semantic protection surface is the named-field set** (below).

The role predicate is how one primitive expresses both canonical forms:

- *gate* — "transition T requires role ∈ S": `{match: T, roles: {not_in: S}, effect: deny}`
  with the file default `allow`.
- *restrict* — "role R may only perform transitions ∈ S": file default `deny`, allow-rules for S
  with `roles: {in: [R]}`. A `deny` default is role-blind, so every other role needs its own
  allow — the per-field catch-all `{"type":"field","field":F,"changed":true}` with
  `roles: {in: [everyone-else]}` is the idiom (see the README example).

## Path globs

Patterns are slash-separated and matched against full repo-relative paths: `*` and character
classes match within one path segment (Go `path.Match` per segment), `**` as a whole segment
spans any number of segments (including zero — `a/**/b` matches `a/b`). A pattern with a
`dir/` prefix matches only paths **strictly under** `dir`: `"registry/**"` does not match the
path `registry` itself (to also gate the directory entry being replaced by a blob/symlink at
that exact name, list the literal path `registry` too). Malformed patterns are config errors at
compile time.

## Structural evaluation

Source of truth: `git diff-tree -r -M -C -z --name-status` over the **full** commit diff — never
a pathspec-filtered diff (pathspec filtering breaks rename detection). Per introduced commit:

- non-merge: diff against the (first) parent; a root commit diffs against the empty tree.
- merge: diff against **each** parent; a path counts as introduced by the merge only if it
  differs from **every** parent (git's combined-diff file set — a clean merge introduces
  nothing); the entry evaluated is the first-parent one (the branch's own line of development).

Status letters map to operations: `A`→add, `M`→modify, `T` (typechange)→modify, `C` (copy)→add
at the target path, `D`→delete, `R`→rename. **Any other letter is an error** (fail-closed
against future git status classes), and any git failure rejects the push.

A rename is deliberately double-visible so it cannot be an evasion of add/delete protection: it
yields two evaluation events — one at the old path with effective operations `{delete, rename}`,
one at the new path with `{add, rename}`. A rule matches an event if its `operations` intersect
the event's set and a `paths` glob matches that event's path. (So "deny delete of X" also fires
when X is renamed away, and "deny add under Y" fires when a file is renamed into Y; a rule with
operation `rename` addresses either end.) **Note for rule authors:** a rename event is never
`modify` — a rule protecting a path's content must list `add` (and typically `rename`), not
`modify` alone, or new content can arrive at the path via rename without tripping it.

## Semantic evaluation (check-command-delegated)

The protected file is parsed by whatever tooling the operator configures — the consumer's own
tooling fills the seam; portitor knows only the generic contract below. This kills
checker/consumer disagreement at the root: the reader that decides what the file *means* is the
one the operator points at, never a second parser inside portitor.

**Trigger.** For each introduced commit and each configured file entry: compare the blob ids of
`parent:path` and `commit:path` (first parent; empty side for root commits or an absent path).
Equal blobs (or both absent) → nothing to evaluate. A non-blob object at the configured path (a
tree — i.e. the name committed as a directory — or a gitlink/submodule) is an operational error
rejecting the push. For merge commits the file is evaluated only if its blob differs from
**every** parent (else its content came from an already-authorized parent), with the delta
computed against the first parent. Attribution is **per commit**: each commit's signer must
authorize the transitions that commit introduces.

**Record extraction (the check-command contract).** Per side of the delta:

- An absent path is the empty record set (the command is not run). Any present blob — including
  an empty one — goes to the command as **data**: materialized at `input_file` inside a private
  throwaway working directory, or piped to stdin when `input_file` is absent. Content never
  appears in argv and there is never a shell.
- The command runs with the working directory as cwd and must write JSON to stdout. At
  `records_path` (dotted keys; absent = the whole output) portitor expects an **array of
  objects**; each object's `id_field` value must be a non-empty string, unique in the array.
  The result is the **full field map** per record — matchers can name any field the extractor
  emits.
- Exit status **0** with a conforming output means the side parsed. A **non-zero exit** means
  the check command rejected the content: portitor surfaces a violation
  (`semantic-check-failed`, detail from the command's stderr — else stdout — excerpt)
  attributed to the commit, on *either* side of the delta, and the push is rejected. Bonus
  invariant for free: a commit can never land content its own check command rejects.
- A failure to *run* the contract — command not spawnable, deadline exceeded, output cap
  exceeded, stdout that does not parse as the declared shape, duplicate/missing ids — is an
  **operational error**: the push is rejected with the error, distinct from a content verdict.
  Fail-closed holds regardless of what fills the seam: there is no outcome in which a check
  problem lets content through.

**Change units and the named-field set.** Record comparison is by **named field only** — never
whole-record — because an extractor may emit derived or bookkeeping fields (timestamps,
counters, cache levels) that would make every edit look "modified". Concretely, the
*named-field set* of a file entry is: every field named by its `field` matchers, plus `labels`
if any `labels` matcher is present. The old/new record sets decompose into **change units**:

- *record-added(r)* — r's id exists only on the new side;
- *record-removed(r)* — r's id exists only on the old side;
- *field-change(r, F)* — r exists on both sides and `old[F] != new[F]`, generated **only for F
  in the named-field set** (an absent field is the distinct value "missing").

Fields outside the named-field set are outside the semantic protection surface by construction
— an edit to an unnamed field generates no unit when no rule names it, so a restrict config
(`default: deny`) does not block it, and derived-field noise cannot deny every edit.
Deliberately, there is no `record modified` matcher and no whole-record comparison.

**Matcher vocabulary (v1 — minimal by design; extension is additive).** Each matcher fires on
specific unit kinds; on any other unit kind it never fires:

| match | on field-change(r, F) | on record-added(r) | on record-removed(r) |
|---|---|---|---|
| `{"type":"field","field":F,"to":V}` | fires iff `new[F] == V` (and `old[F] != V`, which holds since the unit exists) | fires iff `new[F] == V` — a record *born at* V trips arrival gates | never |
| `{"type":"field","field":F,"from":V}` | fires iff `old[F] == V` | never | never |
| `{"type":"field","field":F,"from":V1,"to":V2}` | fires iff `old[F] == V1` and `new[F] == V2` | never | never |
| `{"type":"field","field":F,"changed":true}` | always (the unit exists because F changed) | fires iff F is present on r — the per-field catch-all; also closes the delete-and-re-add evasion of a field gate | never |
| `{"type":"record","change":"added"}` | never | always | never |
| `{"type":"record","change":"removed"}` | never | never | always |
| `{"type":"labels","contains":L}` | fires iff the new record's `labels` contains L (any F — a record-scoped condition) | fires iff the new record's `labels` contains L | never |
| `{"type":"labels","added":L}` | fires on the `labels` unit iff L ∈ new labels ∖ old labels | fires iff L ∈ the new record's `labels` | never |

Record **removal** is decided only by `record {change: removed}` rules (and the default): field
matchers never fire on removals. To gate deletion of records in a given state, use a
`record removed` rule — and note the structural family independently gates deleting or renaming
the *file*. `from`/`to` values are JSON values compared structurally (string, number, bool,
null).

**Isolation.** The check command is subprocess-first with strict containment, preserving the
repo-wide explicit-argv discipline:

- the config-fixed argv, `exec.CommandContext` with a 30s deadline;
- an address-space rlimit (512 MiB) applied via a re-exec trampoline (`portitor
  internal-check-exec` sets `RLIMIT_AS` on itself, then `exec`s the configured command —
  child-only, portitor's own limits untouched), with a minimal environment (PATH, HOME);
- input bounded first (`git cat-file -s`, cap 20 MiB per side) and stdout bounded by a hard cap
  (64 MiB) that errors when exceeded;
- a private throwaway working directory per invocation, removed afterwards.

A heavier sandbox (namespace/container isolation) is deliberately backlog; these bounds are the
accepted containment for now. The `command` itself is operator trust-root material — the config
already defines the gate's trust anchors (`allowed_signers`, `roles`), and the check command is
of the same class. What fills the seam is a deployment decision recorded in the deploy repo's
config, not in portitor.

## Evaluation order in the gate

Per introduced commit, after the signature check (an untrusted commit is rejected before any
content rule — its role can't be trusted): structural evaluation, then semantic evaluation for
each configured file. All violations aggregate; the push is atomic.

## Retired findings

This design supersedes the textual-scan cluster verbatim (see the 2026-07-18 review, Group B):
consumer/checker disagreement (L-CONF1), missing delete/rename/whole-file protection (L-CONF2),
binary/`-diff` suppression (PORT-2), `+`-prefix regex artifacts (PORT-5), the 4 MiB scanner
overflow (L-4MiB), and empty-regex match-everything (PORT-11). The per-commit signature loop and
per-commit attribution are retained unchanged.
