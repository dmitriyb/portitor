# Installing portitor

One binary is published on the [GitHub Releases page][releases]: `portitor`
(linux/darwin, amd64/arm64). It is the same binary the container image runs,
and it is useful standalone on an operator's machine for `add-role`,
`validate-config` and `reconcile`. The container image (gate + egress) is
**not** a release artifact: build it from this repository's `Dockerfile` with
`docker build -t portitor .`, see [`deploy.md`](deploy.md). Every release
archive is signed with SSHSIG (`ssh-keygen -Y sign`), verifiable with the
`ssh-keygen` that ships with OpenSSH — no extra tool to install just to verify.

[releases]: https://github.com/dmitriyb/portitor/releases

## Why not `curl | sh`

A piped script executes as it streams and cannot verify itself before it
runs. Verification therefore has to wrap the download from outside the
stream: download the script, verify the script, then run it. That is the
whole reason the primary path is three commands rather than one pipe.

## Primary: verified install script

**bash / zsh:**

```bash
curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh     -o install.sh \
&& curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh.sig -o install.sh.sig \
&& ssh-keygen -Y verify -f <(printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n') \
     -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh \
&& sh install.sh \
&& rm -f install.sh install.sh.sig
```

**fish:**

```fish
curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh -o install.sh
and curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh.sig -o install.sh.sig
and ssh-keygen -Y verify -f (printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' | psub) -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
and sh install.sh
and rm -f install.sh install.sh.sig
```

**plain `sh`** (no `<(…)` process substitution): write the allowed-signers
line to a file first, then verify against it.

```sh
printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
```

The block verifies **the script itself** against the public key below and
only then runs it. `install.sh` resolves the latest release, detects your
OS/arch, downloads the matching `portitor` archive and its signature, and
verifies the **binary** with the same key (embedded in the script, trusted
because the script was just verified) before installing it.

The script is POSIX `sh`. Set `VERSION=v0.1.0` before `sh install.sh` to
install a specific release, and `INSTALL_DIR=DIR` to choose the install
directory (default `/usr/local/bin`).

## Maximal: verify the binary archive directly

No install script — download the archive for your platform from the
[Releases page][releases], then verify it by any one of:

```bash
# SSHSIG, against the same pinned key as above
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file \
  -s portitor_<version>_<os>_<arch>.tar.gz.sig < portitor_<version>_<os>_<arch>.tar.gz

# SLSA provenance via Sigstore/Rekor — identity-anchored, no key to manage
gh attestation verify portitor_<version>_<os>_<arch>.tar.gz --repo dmitriyb/portitor

# Go users: the Go module checksum database
go install github.com/dmitriyb/portitor/cmd/portitor@<tag>
```

Each release also carries a consolidated `checksums.txt`, one `.sha256` per
archive, and a machine-readable `manifest.json` (schema, target, sha256, size
per artifact).

## What each channel protects, and what it doesn't

- **Primary** verifies both the install script and the binary it fetches,
  end to end: `download → verify → run`, never a piped script.
- **Maximal** gives the strongest per-artifact check for a single file, with
  no script in between.
- The trust anchor in both cases is the public key **copied from this page**.
  That defeats tampering in transit; the residual risk is a look-alike copy
  of this repository, closed by using the known repository URL and by pinning
  the key **once** — copy it a single time, then verify every future release
  against that pinned copy.
- Signatures and attestations give **authenticity, not freshness**: an
  attacker who can intercept a download could still steer a *first install*
  to a genuine-but-older release. For *updates* this is closed: `portitor upgrade`
  is forward-only and hard-refuses a resolved latest older than what is
  installed.

## Public key

```
dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing
```

The same key serves all three verification paths above, and the same line
is published by portitor's sibling tools, [faber](https://github.com/dmitriyb/faber)
and [spexmachina](https://github.com/dmitriyb/spexmachina), so one pinned
copy serves all three. It can be cross-checked against GitHub's own copy at
`https://api.github.com/users/dmitriyb/ssh_signing_keys` once it is added
under Settings → SSH and GPG keys → Signing keys — useful if this page itself
is suspected of being tampered with in a fork or mirror.

## Upgrading

An installed binary updates itself with `portitor upgrade`, which embeds the
same signed `install.sh` and runs it against the running binary's own path:
the same resolve → download → SSHSIG-verify, then a safe in-place swap
(move-aside plus `rename(2)`, never a write over the running file), keeping
the previous binary as `<path>.bak`. `upgrade` maintains the standalone
binary only; the container image is rebuilt from the `Dockerfile`, as above.

Upgrade is **forward-only**: it hard-refuses, non-overridably, a resolved
latest that is *older* than the installed version — a signature proves
authenticity, not freshness, so a latest that moved backward is treated as a
rollback anomaly. `--check` reports the comparison without changing anything
(a warning, not a refusal, when latest is older than installed),
`--version vX.Y.Z` installs an exact release in any direction (the deliberate
path to an older release), and `--rollback` restores the backup. See
[`commands.md`](commands.md) for the flag reference.
