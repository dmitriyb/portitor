# portitor

A self-hosted **git gateway** between an untrusted agent and your real GitHub upstream.
It is the *hard* enforcement boundary: it verifies the **result** of a push — not the commands that produced it — and is the only component that holds a GitHub credential.

```
agent ──ssh──▶ portitor ──┬─ git gate (pre-receive): signed? role? branch? content rules
  (no creds)              ├─ forward (post-receive): mirror accepted feature branches upstream
                          ├─ auto-PR: open a PR for each forwarded branch
                          └─ action API (portitor pr): role-gated comment/review/merge/close/fetch
                                                       (the ONLY GitHub credential lives here)
```

Identity is a credential, not a label: each commit is signed by a per-role key, and portitor maps the signer *fingerprint* — never a label in the commit — to a role.
portitor is generic mechanism; every domain name (roles, paths, fields, the record-extraction command) is config it ships none of.
Its only runtime dependency is git.
See `docs/architecture.md` for the full model.

---

## Install

The `portitor` binary — the same binary the container image runs, also useful standalone for `add-role`/`validate-config`/`reconcile` from an operator's machine — is published on the [GitHub Releases page][releases] for linux/darwin, amd64/arm64.
Every release archive is signed with SSHSIG (`ssh-keygen -Y sign`), verifiable with the `ssh-keygen` that already ships with OpenSSH on essentially every machine — no extra tool to install just to verify.

[releases]: https://github.com/dmitriyb/portitor/releases

### Primary: verified install script

**bash / zsh:**

```bash
curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh     -o install.sh \
&& curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh.sig -o install.sh.sig \
&& ssh-keygen -Y verify -f <(printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n') \
     -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh \
&& sh install.sh
```

**fish:**

```fish
curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh -o install.sh
and curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh.sig -o install.sh.sig
and ssh-keygen -Y verify -f (printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' | psub) -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
and sh install.sh
```

This downloads `install.sh`, verifies **the script itself** against the public key below, and only then runs it — never `curl | sh`.
`install.sh` then resolves the latest release, detects your OS/arch, downloads the matching binary archive and its signature, and verifies the **binary** with the same key (embedded in the script, trusted because the script was just verified) before installing it.
Set `VERSION=v0.1.0` before the final `sh install.sh` to install a specific release instead of the latest.

The block above needs bash or zsh (`<(…)` process substitution).
Under a plain `sh`, write the allowed-signers line to a file first:

```sh
printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
```

### Maximal: verify the binary archive directly

No install script — download the archive for your platform from the [Releases page][releases], then verify it by any one of:

```bash
# SSHSIG, against the same pinned key as above
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file \
  -s portitor_<version>_<os>_<arch>.tar.gz.sig < portitor_<version>_<os>_<arch>.tar.gz

# SLSA provenance via Sigstore/Rekor — identity-anchored, no key to manage
gh attestation verify portitor_<version>_<os>_<arch>.tar.gz --repo dmitriyb/portitor

# Go users: the Go module checksum database
go install github.com/dmitriyb/portitor/cmd/portitor@<tag>
```

Each release also carries a consolidated `checksums.txt`, one `.sha256` per archive, and a machine-readable `manifest.json` (schema, target, sha256, size per artifact).

### What each channel protects, and what it doesn't

- **Primary** verifies both the install script and the binary it fetches, end to end — `download → verify → run`, never a piped script: a piped `curl … | sh` executes as it streams and cannot verify itself before running, so verification has to wrap the download from outside the stream, which is exactly why the primary path is not a one-liner pipe.
- **Maximal** gives you the strongest per-artifact check for a single file, with no script in between.
- The trust anchor in both cases is the public key **copied from this README** — that defeats tampering of the download in transit; the residual risk is being sent to a look-alike or phishing copy of this repository, closed by using the known repository URL and by pinning the public key **once** — copy it a single time, then verify every future release against that pinned copy rather than re-copying it from wherever you happen to land.
- Signatures and attestations give **authenticity, not freshness**: a channel attacker who can intercept your download could still steer you to a genuine-but-older, vulnerable release (a downgrade); this applies to every channel above equally, and there is no minimum-version floor enforced today — note it as a residual risk rather than a solved one.

### Public key

```
dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing
```

This is the same key across all three verification paths above (SSHSIG install script, SSHSIG archive, and the `allowed_signers` line either way).
It can also be pinned and cross-checked against GitHub's own copy at `https://api.github.com/users/dmitriyb/ssh_signing_keys`, once it is added under Settings → SSH and GPG keys → Signing keys — useful if this README itself is ever suspected of being tampered with in a fork or mirror.

The container image (gate + egress) is **not** a release artifact: it is built locally by the operator from this repository's `Dockerfile` (`docker build -t portitor .`).
See `docs/deploy.md` for the CLI-vs-image split in full.

---

## Usage sketch

```bash
docker exec -u git portitor portitor add-repo \
  --repo myrepo --upstream https://github.com/you/myrepo.git

docker exec -u git portitor portitor add-role \
  --repo myrepo --role implementer --fingerprint SHA256:… --pub ./implementer.pub
```

The agent then clones and pushes over SSH (`ssh://git@portitor/srv/git/myrepo.git`).
portitor gates the push (signed? role? content rules?), forwards an accepted branch upstream with its own credential, and opens the PR — printing `PR #<n> <url>` back over the push.

## Learn more

- [`docs/deploy.md`](docs/deploy.md) — deploying portitor: registry, container bring-up, provisioning.
- [`docs/configuration.md`](docs/configuration.md) — the per-repo config schema, `allowed_signers`, content rules, the multi-repo registry.
- [`docs/commands.md`](docs/commands.md) — the full command reference and the `pr` action API.
- [`docs/architecture.md`](docs/architecture.md) — how the gate decides, with links to the authoritative spec.
- [`deploy/DEPLOY.md`](deploy/DEPLOY.md) — a live end-to-end runbook against a real GitHub sandbox.
- `spec/**` — the authoritative, requirement-level specification (spexmachina format).

## License

Apache-2.0 (see `LICENSE`).
