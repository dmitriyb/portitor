# Release pipeline (build, sign, checksum, manifest, attest, publish)

Triggered by `push` of a `v*` tag. Two files own it: `.goreleaser.yaml` (what GoReleaser builds)
and `.github/workflows/release.yml` (the steps GoReleaser doesn't do itself). Everything downstream
of "the four archives exist in `dist/`" — per-artifact checksums, the manifest, the provenance
attestation — is explicit workflow steps, not GoReleaser plugins, so each is independently
readable and independently rerunnable.

## Build (GoReleaser)

`.goreleaser.yaml`'s `builds` entry compiles `./cmd/portitor` for the four-target matrix (`linux`/
`darwin` × `amd64`/`arm64`) with:

- `CGO_ENABLED=0` — matches the Dockerfile's static build, no libc dependency to cross-compile.
- `-trimpath` and ldflags `-s -w` — no build-machine paths or symbol table in the shipped binary.
- ldflags `-X main.version={{.Version}} -X main.commit={{.Commit}} -X main.date={{.Date}}` — stamps
  the three vars `cmd/portitor/version.go` declares (default `"dev"`/`"none"`/`"unknown"` for a
  plain `go build`), surfaced by `portitor version` / `portitor --version` / `portitor -v`. A
  downloaded binary's version output is then a direct, unfalsifiable link back to the tag, commit,
  and build time that produced it.

Each build is archived (`archives`) as `portitor_<version>_<os>_<arch>.tar.gz`, bundling the binary
with `README.md` and `LICENSE`. `checksum.name_template: checksums.txt` produces one consolidated
sha256 file over all four archives (GoReleaser's `checksum` block only ever produces this single
combined file — per-artifact `.sha256` files are a workflow step, below).

## Signing (minisign)

`.goreleaser.yaml`'s `signs` block runs, once per archive:

```
minisign -S -s "{{ .Env.MINISIGN_KEY_PATH }}" -m "${artifact}" -x "${signature}" -t "portitor {{ .Tag }}"
```

`MINISIGN_KEY_PATH` is not the secret itself — the release workflow's "Write minisign secret key"
step writes the `MINISIGN_SECRET_KEY` repository secret to `$RUNNER_TEMP/minisign.key` (`umask
077`, so the file is created `0600`) and exports the path via `GITHUB_ENV`, then a later step
(`if: always()`) removes it. The key material therefore never appears in a process argv (visible
to any other process on the runner via `/proc`) or in a template-expanded command line that GitHub
Actions might echo to a log. The public half is committed to README.md's Releases section, so a
verifier can run `minisign -Vm <archive> -P <pubkey>` entirely offline.

Signing, the sha256 checksums, and the SLSA attestation (below) are three independent ways to
trust a downloaded binary — deliberately redundant, not layered fallbacks; a verifier who trusts
only one of the three still gets a real guarantee.

## Publish (GoReleaser → GitHub Release)

`goreleaser release --clean` (invoked via `goreleaser/goreleaser-action`) creates the GitHub
release itself from the `v*` tag and uploads the four archives, `checksums.txt`, and the four
`.minisig` files. `release.prerelease: auto` in `.goreleaser.yaml` marks a tag containing a
pre-release suffix (e.g. `v0.1.0-rc.1`) as a GitHub pre-release automatically — no separate
`workflow_dispatch` input to remember to set.

## Per-artifact checksums

A consolidated `checksums.txt` is enough to verify all four archives together, but not to verify
one binary in isolation without the other three. The release workflow's "Generate per-artifact
checksums" step covers that case directly:

```bash
cd dist
for f in portitor_*.tar.gz; do sha256sum "$f" > "$f.sha256"; done
```

producing `portitor_<version>_<os>_<arch>.tar.gz.sha256` per archive, uploaded to the release
alongside `manifest.json` via `gh release upload … --clobber` (GoReleaser has already created the
release by this point; `gh release upload` adds assets to it rather than creating a second one).

## Manifest (`scripts/generate-manifest.sh`)

Invoked as `scripts/generate-manifest.sh dist "${GITHUB_REF_NAME}" ok`, from the repository root
(the script reads `dist/artifacts.json` and `dist/metadata.json`, and `artifacts.json`'s `path`
fields are relative to the root GoReleaser was invoked from).

- `dist/metadata.json` supplies `version` (tag with the leading `v` stripped), `commit`, and
  `date` directly.
- `dist/artifacts.json` is an array covering every artifact GoReleaser produced (binaries,
  archives, the metadata file itself); the script filters to `type == "Archive"` — exactly the
  four published `.tar.gz` files, not the intermediate per-target binary directories GoReleaser
  also lists.
- For each archive entry, the script does **not** trust `artifacts.json`'s own `extra.Checksum`
  field (present on modern GoReleaser, but an unverified assumption is a needless dependency on
  an internal field surviving a future GoReleaser upgrade unchanged) — it recomputes `sha256sum`
  and `stat -c%s` directly against the archive file on disk. The manifest is therefore
  self-verifying: it says what the bytes in `dist/` actually hash to, not what GoReleaser's
  internal bookkeeping claims they hash to.
- `target` is `<goos>_<goarch>` (e.g. `linux_amd64`), matching the archive's own naming
  convention.

Output shape (`schema_version` pins the shape itself, independent of `tool`'s version):

```json
{
  "schema_version": 1,
  "tool": "portitor",
  "version": "0.1.0",
  "git_sha": "…",
  "git_ref": "v0.1.0",
  "built_at": "2026-…",
  "status": "ok",
  "artifacts": [
    {
      "name": "portitor_0.1.0_linux_amd64.tar.gz",
      "target": "linux_amd64",
      "sha256": "…",
      "size_bytes": 1346559,
      "archive_format": "tar.gz"
    }
  ]
}
```

This is a **generic** shape — no portitor-specific field, no bespoke delivery-metadata schema
elsewhere in the codebase. `.goreleaser.yaml` remains the sole source of truth for *how* artifacts
are built; `manifest.json` is a derived, disposable projection a downstream consumer (e.g. a Nix
overlay pinning a `sha256` per platform) can consume programmatically instead of scraping the
release page or hand-copying a checksum out of `checksums.txt`. Re-running the script against the
same `dist/` is idempotent and side-effect-free (it only reads).

## SLSA provenance

The release job's last step, `actions/attest-build-provenance`, runs with `subject-path:
dist/portitor_*.tar.gz` (the four archives only — not the checksum/signature/manifest files, which
aren't independently executable artifacts). The workflow's `permissions: {id-token: write,
attestations: write}` are what let this step mint a Sigstore-backed attestation from the job's
OIDC identity; a verifier runs `gh attestation verify <archive> --owner <org>` and gets an
independent, GitHub-native chain of custody back to *this exact workflow run* — distinct from, and
not dependent on, the minisign signature.

## Local proof (no tag, no CI, no secrets)

Every piece above runs identically outside CI:

```bash
export MINISIGN_KEY_PATH=/path/to/a/local/minisign.key   # any key, e.g. `minisign -G -W`
goreleaser release --snapshot --clean
./scripts/generate-manifest.sh dist v0.0.0-snapshot-test ok > dist/manifest.json
```

`--snapshot` skips the "must be on a tag" / "must publish" checks GoReleaser normally enforces, so
the full build → archive → sign → checksum chain runs on an uncommitted or untagged tree. This is
precisely how this module's local proof was captured: a throwaway minisign keypair, a snapshot
release, `minisign -Vm` against the resulting signature, `sha256sum -c` against `checksums.txt`,
and `generate-manifest.sh` run against the real `dist/artifacts.json`/`metadata.json` it produced
— not a hypothetical shape guessed in advance.

## Boundaries

This module ships binaries only. The Dockerfile builds the same `CGO_ENABLED=0 go build` binary
into a minimal Alpine image, but that image is never built or pushed by this pipeline — the
operator runs `docker build -t portitor .` themselves, locally, against the tagged source (see
`README.md`'s Quickstart). No package-manager formula (Homebrew/Scoop/AUR) and no cross-repo
checksum-dispatch notification are in scope: the templates this pipeline was adapted from
(beads_viewer, beads_rust) both couple to infrastructure — a homebrew-tap repo, an ACFS
installer-notification repo — that has no portitor equivalent, and building one for a hypothetical
future consumer is exactly the premature-coupling this module's Manifest component substitutes
for: `manifest.json` gives any future consumer a stable, generic integration point without
portitor knowing who they are.
