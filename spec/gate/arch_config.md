# Per-repo configuration: identity, versioning, decode discipline

The per-repo JSON config is portitor's trust root: it names the allowed signers, the role map,
the content rules, the action policy, and the audit trail. This document pins three properties
of how that file is *found* and *read* — each one fail-closed, because a config that is
misresolved, partially understood, or silently mis-keyed is a weakened control, not a smaller
one.

## One canonical identity: the registry

Every consumer of a repo's config reads **the same file**: `<ReposDir>/<name>.json`
(`config.ReposDir()`, default `/etc/portitor/repos.d`, `PORTITOR_REPOS_DIR`-aware).

- `add-repo` has always used the registry. `init-repo` without `--config` now defaults to the
  registry path too, deriving `<name>` from the bare path's basename (a basename that fails
  `ValidName` is a usage error asking for an explicit `--config` — never a silently different
  location). The previous default (`<bare>/portitor.json`) is retired: it made the gate enforce
  one file while `add-role`/`pr` edited another, so role grants made through the supported tool
  never reached the push gate.
- The hook shims bake the config path at provisioning time. `add-role` cross-checks: after
  editing `<ReposDir>/<name>.json` it reads the target repo's baked `pre-receive` shim and
  **warns** when the shim's `PORTITOR_CONFIG` differs from the file it just edited (a repo
  provisioned before this convention, or with an explicit divergent `--config`). A warning, not
  an error — the operator may run a deliberate split — but the divergence is never silent.
- `--config` remains available for deliberate exceptions; whoever uses it owns keeping the
  consumers aligned.

- The hook consumers refuse to run at all when `PORTITOR_CONFIG` is unset (its absence means a
  broken or bypassed provisioning; a gate running with a zero config would not be uniformly
  fail-closed — a deletion-only push introduces no commits to distrust).
- `readInto` rejects trailing content after the config object (a concatenated or merge-damaged
  file must never have half of itself silently dropped).

## Format version (fail-closed, both directions)

The config carries a **`format_version`** stamp. This binary understands exactly version 1.

- `format_version` missing, lower, or higher than the supported version makes every consumer
  **refuse to operate** at load time — the gate rejects every push (loudly), `add-role`/`pr`/
  `validate-config` refuse. A newer-format config on an older binary must never gate with the
  subset it understands: silently dropping an unknown rule type is a weakened security control.
- The schema version is **independent of the application version** — bumped only when the
  on-disk format actually changes incompatibly, so the guard trips only on a real format change.
  (Purely additive fields do not bump it; the strict decode below rejects unknown keys on an
  older binary anyway, which is the additive-change guard.)
- Migrations / backward-compat readers are deliberately out of scope: across a bump the operator
  re-provisions (`init-repo`/`add-repo` + `add-role` are re-runnable). There is no silent
  auto-migration.

## Decode discipline (strict, token-level)

`readInto` applies three layers before a config is used, all fail-closed:

1. **Token-level key check** over the raw JSON, before any decode:
   - the top level must be an object whose keys are exactly the known set (byte-exact — `Roles`
     is not `roles`; Go's case-insensitive field matching must never resurrect a revoked binding
     from a stale differently-cased key);
   - within **every** object, duplicate keys are errors (JSON's silent last-wins is how a stale
     binding shadows a live one);
   - within every *schema* object, keys must be lowercase (the whole schema is lowercase
     snake_case, so a differently-cased key is at best a typo and at worst a shadow — reject).
     The two **data maps** — the values of `roles` (fingerprint keys, inherently mixed-case) and
     `action_roles` — are exempt from the lowercase rule; they keep the duplicate check.
2. **Strict decode**: `DisallowUnknownFields` — an unknown key anywhere is an error (the retired
   `role_rules` key remains *known* so its dedicated migration message fires instead of a
   generic unknown-key error).
3. **Version check**: `format_version == 1` exactly, per the section above.

`validate-config` (and `add-role`'s post-write check) additionally verify content: gate-integrity
fields present, `allowed_signers` readable, the roles map non-empty with **fingerprint-shaped
keys** (`SHA256:` + 43 base64 chars — the shape git reports via `%GF`; anything else can never
match a real signer, so it is dead weight that hides a typo'd revocation or grant) and non-empty
role values, `action_roles` verbs from the closed set, and `content_rules` compiling cleanly.

## Boundaries

Environment overrides remain operational-only (`PORTITOR_UPSTREAM_REMOTE`, `PORTITOR_UPSTREAM_SLUG`,
`PORTITOR_REPOS_DIR`, `PORTITOR_REPO_ROOT`, `PORTITOR_CONFIG` as the hook-baked path); no
gate-integrity field has an env override. The config file itself is operator trust-root material,
read as one atomically-renamed snapshot.
