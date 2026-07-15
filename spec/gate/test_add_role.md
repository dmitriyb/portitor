# `add-role` test scenarios

Unit/integration tests over a temp `repos.d` (via `PORTITOR_REPOS_DIR`), ephemeral SSH keys, and a
seeded `<name>.json` + `allowed_signers`. Each scenario runs `add-role`, then re-loads the config
(and reads `allowed_signers`) and asserts the resulting state. No network, no YubiKey.

## Fingerprint / role validation

1. **malformed fingerprint rejected** — `--fingerprint deadbeef` (no `SHA256:` prefix / wrong length)
   → exit 2, config untouched.
2. **empty or invalid role rejected** — `--role ""` or `--role "bad name"` (whitespace/`/`) → exit 2,
   config untouched.
3. **missing config rejected** — `--repo nope` with no `repos.d/nope.json` → exit 1, no file created.
4. **repo name is path-guarded** — `--repo ../escape` → exit 2 (a `ValidName` failure is a usage
   error), nothing written outside `repos.d`.

## Role-map mutation

5. **add a new binding** — a fresh fingerprint → `roles[fp] == role`; every other config field
   unchanged; exit 0.
6. **overwrite an existing binding** — same fingerprint, different role → `roles[fp]` becomes the new
   role; exit 0.
7. **idempotent no-op** — same fingerprint + same role as already present → exit 0, config bytes
   unchanged, reported `unchanged`.

## `allowed_signers` handling (`--pub`)

8. **signing role appends its key** — `--role implementer --pub impl.pub` whose fingerprint matches
   `--fingerprint` → a `implementer namespaces="git" <keytype> <keydata>` line is appended; exit 0.
9. **dedup on re-run** — repeating scenario 8 does not add a second line for the same key blob; exit 0.
10. **fingerprint/pub mismatch refused** — `--fingerprint` of key A but `--pub` is key B → exit 1,
    neither the config nor `allowed_signers` is modified.
11. **identity-only role with `--pub` refused** — `--role merger --pub merger.pub` → exit 1;
    `allowed_signers` is **not** touched (a landing-only key must never gain signing trust). The
    fingerprint→role binding is likewise not written (the command fails before writing).
12. **`--pub` omitted leaves allowed_signers alone** — `--role implementer` with no `--pub` → the role
    binding is written but `allowed_signers` is byte-for-byte unchanged.
15. **first append creates a missing allowed_signers file** — signing role with a valid `--pub` when the
    config's `allowed_signers` path does not yet exist → the file is created (mode `0644`) with the one
    appended line; exit 0.
16. **unreadable/malformed `--pub` file** — `--pub` points at a nonexistent or non-key file (so the
    `ssh-keygen -lf` fingerprint computation fails) → exit 1; neither the config nor `allowed_signers`
    is modified.
17. **overwrite onto identity-only role with a trusted key is refused** — a fingerprint already present
    in `allowed_signers` (bound to a signing role) is re-bound with `--role merger` → exit 1; the config
    (`roles`) and `allowed_signers` are both left unchanged.

## Durability & validation

13. **atomic write** — after a successful `add-role`, the config parses cleanly (no partial JSON); a
    reader concurrent with the rename sees either the old or the new complete file.
14. **post-write validation surfaces problems** — seeding a config whose `allowed_signers` path is
    unreadable and then running `add-role` → the command exits non-zero and prints the
    `config.Validate` problem(s).
