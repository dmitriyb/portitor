# Gate test scenarios

Integration tests over real git with ephemeral SSH keys (no YubiKey, no real credentials): a
throwaway bare "server" repo, a work repo that signs with a generated key, and an `allowed_signers`
listing that key. Each scenario constructs commits, then calls `Check` and asserts the violation
rules.

## Scenarios

1. **signed feature update accepted** — a feature branch update (`old..new`) whose new commit is
   signed by the allowed key, targeting a non-default ref → zero violations.

2. **push to default branch rejected** — the same signed update but targeting `refs/heads/main` (the
   derived default) → exactly one violation, rule `no-push-to-default`.

3. **unsigned commit rejected** — a commit made with `--no-gpg-sign` on a feature branch → one
   violation, rule `unsigned-or-untrusted-commit`.

4. **untrusted signer rejected** — a commit signed by a key *not* in `allowed_signers` → one
   violation, rule `unsigned-or-untrusted-commit`.

5. **branch creation path** — `old` is the zero SHA; new commits are enumerated with
   `rev-list <new> --not --all`. To mimic pre-receive's pre-update state in a test, the branch is
   pushed and its ref deleted on the bare side (objects remain, ref gone); a signed new branch →
   zero violations.

## End-to-end (real push)

Beyond the unit tests, the `portitor` binary is installed as an actual `pre-receive` hook on a bare
repo and exercised with `git push`:

- signed feature → `[new branch]` accepted;
- push to `main` → `pre-receive hook declined`, with `remote: … [no-push-to-default] …`;
- unsigned feature → declined, with `remote: … [unsigned-or-untrusted-commit] …`.

This confirms stdin parsing, `GIT_DIR` handling, exit-code → push rejection, and that the rejection
reasons surface to the pusher.
