# Registering roles (`add-role`)

`portitor add-role` binds a signer-key **fingerprint** to a **role** inside an already-provisioned
repo config. It is the counterpart to the role map that the gate reads (`Config.Roles`, fingerprint →
role) and to the `allowed_signers` file the gate verifies against: `init-repo`/`add-repo` create the
bare repo and place its config, and `add-role` fills in *who may act as what* — one fingerprint at a
time, re-runnably.

```
portitor add-role --repo <name> --role <role> --fingerprint SHA256:… [--pub <file>]
```

It never creates keys and never touches private key material (matching the whole-system invariant:
portitor knows only public keys + fingerprints). It edits an **existing** `repos.d/<name>.json`; it
does not provision a repo — run `init-repo`/`add-repo` first.

## Inputs

| flag | required | meaning |
|---|---|---|
| `--repo <name>` | yes | selects `<ReposDir>/<name>.json` (`config.ReposDir()`, honoring `PORTITOR_REPOS_DIR`). `ValidName`-guarded (`[A-Za-z0-9._-]`, not `.`/`..`) so it is a single path component and can never traverse out of the registry dir. The config must already exist — a missing file is a loud error. |
| `--role <role>` | yes | the role name to bind. Non-empty, charset `[A-Za-z0-9._-]` (a label, no whitespace or path separators). Free-form (`implementer`, `reviewer`, `merger`, `owner`, `implementer_work`, …); portitor does not enumerate the legal set. |
| `--fingerprint SHA256:…` | yes | the signer-key fingerprint as git reports it via `%GF`. Must be `SHA256:` followed by 43 chars of unpadded base64; anything else is rejected up front. This is the cross-system join key — the only thing shared between the key's owner and portitor. |
| `--pub <file>` | no | path to the OpenSSH **public** key (`<keytype> <keydata> [comment]`) whose fingerprint is `--fingerprint`. Present only when this role's key should also be trusted to sign commits (see below). |

## Effect on `repos.d/<name>.json`

`add-role` sets `roles[<fingerprint>] = <role>`:

- **add** when the fingerprint is not yet mapped;
- **overwrite** when it maps to a different role (reassigning the same key to a different role);
- **no-op** when it already maps to exactly this role — reported as `unchanged`, exit 0 (idempotent).

Every other field of the config (`default_branch`, `allowed_signers`, `upstream_*`, `role_rules`, …)
has its **value preserved** — `add-role` only edits the `roles` map. The rewrite round-trips every
untouched field through `json.RawMessage`, so field values are byte-identical; the file itself is
re-emitted with `json.MarshalIndent`, so top-level key order and whitespace may change on an actual
mutation (an idempotent no-op re-writes nothing at all).

**Overwrite onto an identity-only role is guarded.** Because `add-role`'s `allowed_signers` mutation is
purely additive (it only ever adds/dedups, never removes — see below), an overwrite that rebinds a
fingerprint *currently trusted in `allowed_signers`* to the identity-only `merger` role would leave that
key both role=`merger` **and** trusted to sign — collapsing the isolation the `--pub` refusal for
`merger` exists to protect. So an overwrite whose new role is identity-only (`merger`) is **refused with
a loud error (exit 1) when the fingerprint's key is present in `allowed_signers`**; the config is left
untouched. (Reconciling `allowed_signers` — removing the stale signer line — is out of band, so
`add-role` refuses rather than silently leaving a trusted-yet-identity-only key.) Rebinding a
fingerprint that is *not* in `allowed_signers` to `merger` is an ordinary overwrite.

## `allowed_signers` handling (`--pub`)

A role is either a **signing role** or **identity-only**. The classification is a **closed denylist**,
not an enumeration: **identity-only roles are exactly `{merger}`; every other role name is a signing
role.** This is the only rule consistent with free-form/split role names (`implementer_work`,
`reviewer_ci`, …) — the check is a membership test against `{merger}`, never an allowlist of named
signing roles.

> **Operator note — the identity-only role must be named exactly `merger`.** This security property
> is bound to a single exact string in *two* places: the `add-role` `--pub` refusal here, and the
> action policy that grants merge/close power (`spec/action`, which likewise keys off exact `merger`
> — and `owner`). A variant spelling (`merger_ci`, `Merger`, `merge`) is treated as an **ordinary
> signing role** by `add-role` (so `--pub` would grant it commit-signing trust) — but it also has **no
> landing power anywhere**, because the action policy only recognizes the exact string `merger`. So a
> mis-spelled merge role is inert rather than dangerous; still, to get both the landing authority and
> the `--pub` refusal, name the role exactly `merger`.

Identity-only roles authorize landing over the action channel and **must never be able
to sign commits** — `merger`'s dedicated key exists only to merge/close (see `spec/action`). Signing
roles (everything that is not `merger`) sign commits and therefore must appear in `allowed_signers` or
the gate rejects their commits as untrusted (`%G? == U`).

Behavior when `--pub` is given:

- **signing role** → portitor reads the pub file, **verifies its fingerprint equals `--fingerprint`**
  (computed with `ssh-keygen -lf`, delegating the crypto to ssh — mirroring the gate's "delegate to
  git" stance). On mismatch it errors and writes nothing (this blocks registering one fingerprint but
  appending a different key). On match it appends one line to the config's `allowed_signers` file:

  ```
  <role> namespaces="git" <keytype> <keydata>
  ```

  The principal label is **cosmetic** — portitor re-derives the role from the fingerprint at gate
  time — so it is set to the role name purely for readability. The append is **deduped**: if a line
  carrying the same key blob is already present, nothing is added (idempotent).

  If the config's `allowed_signers` file does **not yet exist** (the operator may manage it out of
  band and not have created it), the first append **creates it** at the config-declared path with mode
  `0644` (its content is public keys), including any missing parent directory; the append then proceeds
  through the same temp-then-rename discipline. A missing file is therefore normal, not an error.

- **identity-only role** (e.g. `merger`) → a **loud error**. Adding such a key to `allowed_signers`
  would grant a landing-only credential the power to sign commits, collapsing the role isolation the
  system depends on. The operator must not pass `--pub` for these roles.

When `--pub` is **omitted**, `allowed_signers` is left untouched (the operator manages it out of band,
e.g. via `deploy/DEPLOY.md`). Note the fingerprint→role binding alone suffices for role *attribution*;
but a signing role whose key is not yet in `allowed_signers` will have its commits rejected as
untrusted until the key is added there.

## Durability & validation

- The config is written **atomically**: marshal to a temp file in the same directory, then
  `os.Rename` over the target (atomic on one filesystem). A concurrent reader — or the gate itself —
  never observes a half-written file, and a crash mid-write leaves the previous config intact. The
  original file mode is preserved.
- The `allowed_signers` update is performed safely as well (read → dedup → append via the same
  temp-then-rename discipline), so it too is crash-safe and idempotent.
- When both a role rebind **and** an `allowed_signers` append are due, the roles write commits first
  and the signer append follows. These two files are not updated as one transaction: if the append
  fails (e.g. an unwritable signers file), the roles binding is already persisted and is **not** rolled
  back. This is deliberately **fail-closed** — a signing role whose key is not yet in `allowed_signers`
  has its commits rejected by the gate as untrusted (`%G? == U`), never wrongly accepted — so the
  partial state is safety-benign, and re-running `add-role` (idempotently) completes the append.
- **After** the write, portitor re-loads the config and runs `config.Validate`; any problem is printed
  to stderr and the command exits non-zero, so a broken config surfaces here rather than silently
  making the gate distrust every later push. Because `add-role`'s mutation is purely additive (it only
  adds/rebinds a role and optionally adds a signer), it cannot turn a valid config invalid — the
  post-write check mainly surfaces a pre-existing latent problem. Note this check runs **after** the
  atomic write commits: a non-zero exit here means the on-disk config was already mutated (additively)
  before the latent problem was surfaced, not that the write was rolled back.

## Exit codes

- `0` — applied, or an idempotent no-op re-run.
- `2` — usage error (missing/malformed flags: bad fingerprint syntax, empty/invalid role, missing
  `--repo`, or a **present-but-invalid `--repo` value** that fails `ValidName`, e.g. `../escape` — a bad
  flag value is a usage error).
- `1` — operational error (config not found; unreadable or malformed `--pub` file, or an `ssh-keygen`
  invocation failure while computing the pub's fingerprint; `--pub` fingerprint mismatch; `--pub` for an
  identity-only role; an overwrite onto an identity-only role whose key is already in `allowed_signers`;
  write failure; or post-write validation failure). For every **pre-write** rejection — bad `--pub`,
  fingerprint mismatch, `--pub`/overwrite identity-only guards, and config-not-found — the config is
  left untouched (nothing is written before these checks pass). The **post-write** cases — an
  `allowed_signers` append that fails after the roles write, and post-write validation failure — exit 1
  with the (additive) roles mutation already committed to disk; see *Durability & validation* for why
  that partial state is fail-closed rather than rolled back.

## Boundaries

`add-role` only edits the per-repo config (`roles` + optionally `allowed_signers`). It does **not**
touch the gate logic, the `shell` forced-command dispatch, or the `pr` action code — those read this
config but are unchanged by it.
