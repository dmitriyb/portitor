# Self-update (install.sh upgrade mode, embedded into the binary)

`portitor upgrade` reuses the signed `install.sh` rather than reimplementing the update in Go: the same script that a first-time user downloads-and-verifies-then-runs is embedded into the binary and driven in an "upgrade mode" against the running binary's own path. One implementation, one copy of the release-signing key (in `install.sh`), and — because the script travels inside the already-trusted, signed binary instead of being fetched — nothing to substitute: there is no fetch-and-verify-the-script step, because the embedded script is, for a given release, byte-identical to the standalone `install.sh`.

The CLI front-end (`portitor upgrade`) is specified by the cli module's UpgradeCommand; this component specifies the script behaviour it drives.

> NOTE: the other delivery seed docs (`arch_release.md`, `module.json`) predate two shipped changes and describe them stale — release signing is SSHSIG (`ssh-keygen -Y sign`, `.sig`), not minisign; and `install.sh` (plus `install.sh.sig`) is now published to each release and documented in the README. This self-update component builds on the SSHSIG + published-`install.sh` reality; reconciling the older minisign/"no install script" wording is a separate cleanup, deliberately not folded into the upgrade change.

## Modes

`install.sh` with no flags is first-install mode, unchanged: resolve → download → verify → `install -m` into `INSTALL_DIR` (with the existing sudo fallback). Upgrade mode is opt-in via `--upgrade --target <path>` and changes two things: it targets the exact path of the currently-running binary (passed by the Go side, resolved via `os.Executable` + `filepath.EvalSymlinks`) instead of `INSTALL_DIR`, and it replaces via move-aside + rename instead of `install`.

The resolve / download / `ssh-keygen -Y verify` (against the embedded `SIGNING_PUBKEY`, `-n file` namespace) logic is shared and untouched between the two modes; upgrade mode adds decisions around it, not a second verifier. Fail-closed is preserved end to end: a failed download or a signature that does not verify exits non-zero having changed nothing on disk.

## Safe self-replace

Overwriting a running binary in place hits `ETXTBSY` on Linux; a `rename(2)` over it is safe — the running process keeps its inode, and the next invocation gets the new file. So upgrade mode never writes over the target: it stages the verified new binary in the target's own directory (same filesystem, so the final swap is an atomic rename), then `mv`s the running binary aside to `<target>.bak` and `mv`s the new one into place. It never uses `install`/`cp` over the running file. The directory-writability requirement of rename is what the RequireRoot probe checks.

## Downgrade guard

A signature proves authenticity, not freshness: a channel attacker who can steer a download could serve a genuine-but-older signed release. Upgrade mode therefore compares the target version against the current version (`--current` from the Go side, else probed from `<target> version`) with a numeric major.minor.patch comparison (`ver_cmp`): equal → "already up to date", exit 0; older → refuse unless `--force`; newer → proceed. `--check` / `--dry-run` reports this comparison and changes nothing (no download, no replace).

## Rollback

The move-aside already leaves `<target>.bak`; `--rollback` restores it with an atomic rename and exits. On any failure during the replace itself, the backup is restored before exiting non-zero, so the on-disk binary is never left missing or half-updated.

## RequireRoot, no auto-sudo

First-install mode may retry with sudo; upgrade mode must not. Silently re-running a self-replace as root is a surprise, so upgrade mode instead probes the target directory's writability and, if it cannot write, prints a clear "re-run with elevated privileges" message and stops — it never self-invokes sudo.

## Embedded == released (the identity invariant)

`//go:embed` cannot traverse `..`, so the repo-root canonical `install.sh` cannot be embedded directly from `cmd/portitor`. The canonical file stays at the repo root — the release workflow uploads it (`release.yml`: `cp install.sh dist/install.sh`) and the README documents it — and a byte-identical copy is kept at `cmd/portitor/install.sh`, refreshed by `//go:generate cp ../../install.sh install.sh`. A Go test (`TestUpgradeEmbeddedInstallMatchesCanonical`) fails if the two diverge. That byte-identity is the whole security argument: it is what lets `upgrade` skip fetching-and-verifying the script, because the embedded copy is provably the same audited, signed installer a user runs by hand.

## Test-only origin hooks

So the fake-origin harness can drive the real script against a throwaway release, `install.sh` reads `PORTITOR_API_BASE` / `PORTITOR_DL_BASE`, defaulting to the production GitHub endpoints. These are not trust-sensitive: whatever origin is used, the downloaded archive must still verify against `SIGNING_PUBKEY`, so redirecting the origin can never install an unsigned or tampered binary. The trust anchor itself is deliberately NOT env-overridable — the harness bakes a throwaway test key into its own temp copy of the script, exactly as a real release bakes the real key, so the shipped script's anchor is never weakened.
