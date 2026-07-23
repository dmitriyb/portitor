# Delivery test scenarios

Unlike the gate and action modules, most of this module's behavior lives in CI-only YAML rather
than Go functions `go test` can exercise directly. Verification is therefore split between what
CI enforces on every run and what was proven locally, once, against the real tool chain.

## What CI enforces (regression coverage)

1. **PR fast gate** — `go build ./...`, `go vet ./...`, `go test ./...` must all pass before a PR
   can land; this is the existing repo-wide test suite, not delivery-specific tests. A break here
   blocks merge, not just the eventual release.
2. **Push-to-main race gate** — the same suite plus `go test -race ./...`; a data race introduced
   by any change (not just delivery code) surfaces within one push of merge, not at release time.
3. **Nightly fuzz** — every `func Fuzz*` discovered in the tree runs for a bounded `-fuzztime`;
   a crashing input uploads as a `fuzz-failures` artifact (see `arch_ci.md`). This is regression
   coverage for the parsing/matching functions named in `spec/reviews/2026-07-18.md` §5, not for
   the delivery pipeline itself.
4. **Release-job test gate** — `.github/workflows/release.yml` runs `go test ./...` before
   invoking GoReleaser, so a release build never ships from a red tree even if CI's own gate was
   somehow bypassed for the tagged commit.
5. **Self-update proofs (ride `go test ./...`)** — two delivery-specific checks run under the same suite the fast gate already enforces, so they gate merge like everything else:
   - **Embedded == released byte-identity** — `TestUpgradeEmbeddedInstallMatchesCanonical` fails if `cmd/portitor/install.sh` (the embedded copy) ever diverges from the canonical repo-root `install.sh`. This is the invariant the whole "nothing to substitute" security argument rests on; a drift is caught at merge, not at release.
   - **Fake-origin upgrade harness** — `TestUpgradeFakeOriginHarness` runs `scripts/install_upgrade_test.sh`, which drives the real `install.sh` end to end against a throwaway release origin (served over `file://` via the `PORTITOR_API_BASE`/`PORTITOR_DL_BASE` origin hooks) and a throwaway signing key baked into a temp copy of the script. It exercises resolve → download → `ssh-keygen -Y verify` → self-replace of a running binary, and the paths that matter most: the fail-closed path (a tampered artifact must leave the target untouched and exit non-zero), `--rollback`, the downgrade guard (± `--force`), `--check`, `--version` pinning, first-install, and RequireRoot (no auto-sudo). `sh -n` proves only that the script parses; the harness proves the logic. It self-skips only when a tool it needs is absent from the environment.

## What was proven locally (one-time, this module's local proof)

Run as `goreleaser release --snapshot --clean` against an untagged tree, with a throwaway minisign
keypair (`minisign -G -W`, generated solely for this proof — never committed, never used for a
real release):

1. **Cross-arch build succeeds** — all four `goos`×`goarch` combinations (`linux_amd64`,
   `linux_arm64`, `darwin_amd64`, `darwin_arm64`) compile from `./cmd/portitor` with
   `CGO_ENABLED=0` and produce a `portitor_<version>_<os>_<arch>.tar.gz` archive containing the
   binary, `README.md`, and `LICENSE`.
2. **ldflags stamping is correct** — the `linux_arm64` archive was extracted and its binary run
   directly (the dev host is linux/arm64, so this target alone is natively executable here);
   `portitor version` printed the snapshot version, the real commit SHA, and the build timestamp
   GoReleaser injected — confirming `main.version`/`main.commit`/`main.date` in
   `cmd/portitor/version.go` receive exactly what `-ldflags -X` sends them.
3. **Signing round-trips** — `minisign -Vm <archive> -P <pubkey>` verified against the snapshot's
   own public key for a sampled archive; "Signature and comment signature verified" with the
   expected trusted comment (`portitor <tag>`).
4. **Checksums are internally consistent** — `sha256sum -c checksums.txt` against all four
   archives in `dist/`, run from within `dist/`, reported `OK` for all four.
5. **Manifest generation matches reality** — `scripts/generate-manifest.sh dist <ref> ok` was run
   against the real `dist/artifacts.json`/`metadata.json` this snapshot produced (not a
   hand-constructed fixture). Every `sha256`/`size_bytes` pair in the resulting `manifest.json`
   was cross-checked against `checksums.txt` and byte-verified against the archives directly — an
   exact match for all four targets.
6. **Fuzz-target discovery matches the tree** — the `grep`/`awk` discovery loop `fuzz.yml` uses
   was run standalone against the working tree and found all nine `func Fuzz*` targets across
   `cmd/portitor`, `internal/rules`, and `internal/config`; one target (`FuzzShellSplit`) was then
   run through the exact `go test -run='^$' -fuzz=… -fuzztime=…` invocation the workflow uses, for
   a short bounded duration, to confirm the command form itself (not just the discovery logic) is
   correct.

Item 2 is a *manual* substitute for the release workflow's own "Verify binary version matches tag"
step pattern used in the beads_rust template this pipeline was adapted from — portitor's release
workflow does not include an equivalent automated step, because GoReleaser's snapshot mode has no
tag to compare against and a real tagged release's `{{.Version}}` is derived directly from the tag
by GoReleaser itself (there is no separate version file, e.g. no `Cargo.toml`, that could drift
from it the way beads_rust's could).
