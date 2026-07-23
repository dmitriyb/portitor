# How it works

The gate (`pre-receive`) inspects the objects being landed, delegating the crypto to git and deciding fail-closed — any internal error rejects the whole push:

- **Deterministic verification.** Every portitor git call disables replace-object substitution; the gate's fact-gathering runs hermetically (ambient git config masked) and pins the `allowed_signers` trust root unconditionally, so a verdict is a function of the push, the repo, and the config — never machine state.
- **Signed by an allowed signer.** Each introduced commit must carry `%G? == G`; its `%GF` fingerprint maps to a role.
- **Branch namespace only.** Only `refs/heads/<name>` is accepted; the default branch is never a push target; other namespaces (tags, notes, `refs/replace/*`) are refused.
- **Content rules** gate file operations and record transitions by role (see `configuration.md`).
- **Forwarding** (`post-receive`) mirrors accepted feature branches upstream and auto-opens the PR, reporting every ref's outcome (an out-of-order forward whose tip upstream already contains is success); `reconcile` recovers a failed one.
- **Audit** appends a durable, fsync'd decision trail (never altering a verdict).

The mechanism is pure and unit-/fuzz-tested.
This page is a summary; the authoritative, requirement-level specification is under `spec/**`:

- `spec/gate/arch_pre_receive.md` — the gate rules, deterministic verification environment, forwarding, and recovery in full.
- `spec/gate/arch_config.md` — how the per-repo config is found, versioned, and strictly decoded.
- `spec/gate/arch_content_rules.md` — the structural/semantic content-rules schema and matcher vocabulary.
- `spec/gate/arch_add_role.md` — how `add-role` binds a fingerprint to a role.
- `spec/action/arch_action.md` — the `portitor shell` dispatch, the `pr` action API, and merge-precondition re-derivation.

## Guiding principle

Identity is a credential, not a label.
Each commit is signed by a per-role key; portitor maps the signer *fingerprint* to a role and enforces per-role rules.
A container holding only one role's key cannot act as another role.
portitor is generic mechanism: every domain name (roles, paths, fields, values, the record-extraction command) is config; portitor ships none.
Its only runtime dependency is git.
