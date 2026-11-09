# Deploy

`portitor deploy --config <file>` builds the gate image with Nix and replaces the gate
container, so that afterwards exactly one verified gate runs for the deploy config. It is
idempotent: an unchanged config rebuilds nothing and converges to the same state. The Go
front-end is the cli module's `DeployCommand`; this leaf owns the config, the image and the
sequence. Host-side it needs `docker` and `nix` on `PATH`; the gate's own runtime
dependencies do not change.

## Deploy config

A JSON file, strictly decoded (unknown fields refused), operator-owned and host-local: it holds
names and paths, never a secret. Relative paths in path fields resolve against the file's
directory; `gate_bin` must be absolute; `token_command` is an argv run as is.

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Container name, also the image repository suffix; `^[a-z0-9][a-z0-9_.-]{0,62}$` |
| `volume` | yes | Named volume for the mirrors, mounted at `/srv/git` |
| `host_key_volume` | yes | Named volume for the SSH host key, mounted at `/var/lib/portitor/hostkey` |
| `networks` | no | `[{name, aliases}]`: existing Docker networks to attach, each with its aliases |
| `ports` | no | `[{host_ip, host_port}]`: publishes container port 22; `host_port` `0` lets Docker pick |
| `config_dir` | yes | The gate's config dir (`repos.d/`, `allowed_signers`, anything they reference) |
| `authorized_keys` | yes | Paths of the role `.pub` files; their `<type> <key>` lines become `AGENT_AUTHORIZED_KEY` |
| `token_command` | yes | Argv whose stdout is the GitHub PAT; `argv[0]` is a bare name looked up on `PATH` or absolute |
| `host_key_file` | yes | Pin file: one `ssh-ed25519 <base64>` line |
| `start_timeout` | no | Go duration, default `2m`: bound on the new gate becoming ready |
| `stop_timeout` | no | Go duration, default `30s`: grace given to the previous gate when it is stopped |
| `image` | no | `{packages, overlay, pin}`: extra tools for the operator's policy commands |
| `gate_bin` | no | Replaces the `portitor-gate` beside the executable (the test harnesses use it) |

`image.packages` are attribute paths in the pinned nixpkgs. `image.overlay` is one Nix file,
staged alone beside the rendered expression and applied to nixpkgs; a relative import inside it
fails the build. `image.pin` is `{rev, sha256}` and replaces the nixpkgs pin compiled into
portitor. Volume and network names follow Docker's `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`.

## Image

`dockerTools.buildLayeredImage` over the pinned nixpkgs, for `<arch>-linux` where `<arch>` is
the host's (`amd64` → `x86_64`, `arm64` → `aarch64`). On macOS this needs a Linux builder.
Tagged `portitor-gate/<name>:<hash>`, where `<hash>` is the SHA-256 of the rendered expression's
bytes and of every staged input: the gate binary, the entrypoint, the overlay, and every config
dir file's relative path, normalized mode and bytes. Each input is read once; the bytes hashed
are the bytes staged. The build is skipped when the tag already exists.

The fixed part, which portitor owns:

- **Runtime:** bash (also `/bin/sh`), coreutils, gawk, openssh, git, gh, tini, util-linux
  (`setpriv`), cacert, with `/etc/ssl/certs/ca-certificates.crt` linking the bundle so TLS clients
  need no env var.
- **Identity:** `/etc/passwd`, `/etc/group`, `/etc/shadow` with `root`, `git` (uid and gid
  1000, home `/home/git`, shadow `*` so key login works) and the locked `sshd` privsep user.
- **sshd:** `/etc/ssh/sshd_config` — public keys only, no root login, no PAM,
  `AuthorizedKeysFile /home/git/.ssh/authorized_keys`, `HostKey
  /var/lib/portitor/hostkey/ssh_host_ed25519_key`.
- **Directories:** `/srv/git` and `/home/git` owned by `git`; `/run/sshd`, `/var/empty`;
  `/tmp` (`1777`); `/run/secrets` and `/var/lib/portitor/hostkey` (root, `0700`).
- **portitor:** the gate binary at `/usr/local/bin/portitor`; the entrypoint (embedded in
  portitor) at `/usr/local/bin/portitor-entrypoint`; `/usr/sbin/sshd`, `/usr/sbin/tini` and
  `/sbin/nologin` links.
- **Config:** the config dir at `/etc/portitor` as the top layer, symlinks followed, `.lock` and
  dot-prefixed entries left out, root-owned, files `0644`, directories and executables `0755`.
- **Image config:** entrypoint `tini -- portitor-entrypoint`, `PATH` only, no declared volumes.

The operator's `image.packages` and the overlay's packages are added to the contents. The config
dir enters the Nix store, which is world-readable on the build host and enters any Nix cache; it
holds no secret by construction.

## Sequence

Every child `docker` and `nix` runs with `GH_TOKEN` and `GITHUB_TOKEN` removed from its
environment.

1. **Lock.** An exclusive flock on `deploy-<name>.lock` under `$XDG_RUNTIME_DIR/portitor`
   (else the OS temp dir), held to the end, so two deploys of one gate never interleave.
2. **Preflight.** Decode the config. Read `portitor-gate`'s ELF header (linux, the host's
   arch) and its build info (`main.version`); a version other than the running binary's refuses.
   The daemon's architecture must equal the host's. Every listed network must exist. The
   container named `name`, when present, must carry the label `portitor.deploy=<name>`; a foreign
   one refuses. Labelled leftovers of an interrupted deploy are resolved here: an unstarted
   `<name>-next-*` is removed; a `<name>-prev-*` with no container named `name` is renamed back
   and started.
3. **Build** the image (above).
4. **Validate.** For every `repos.d/*.json`: `portitor validate-config` as `git` inside the
   built image, `--network none`.
5. **Host key.** A throwaway container of the built image reads
   `ssh_host_ed25519_key.pub` from the host-key volume. Refuse when the volume holds a key
   different from the pin, or holds none while a pin exists. Rotation is explicit: remove the
   volume and the pin file.
6. **PAT.** Run `token_command`; strip one trailing newline; an error, an empty result or an
   embedded newline refuses.
7. **Create** `<name>-next-<random>` from the tag: the label, restart `unless-stopped`, the
   default bridge, both volumes, `AGENT_AUTHORIZED_KEY`, the ports; then connect each listed
   network with its aliases.
8. **Deliver the PAT.** A tar holding `gh_token` (`0400`) on stdin to
   `docker cp - <next>:/run/secrets/`; copy it back with `docker cp <next>:/run/secrets/gh_token -`
   and compare bytes. No host file holds it; inside, it lives in the container's writable layer on
   the daemon's disk until the container is removed.
9. **Swap.** Stop the previous gate with `stop_timeout`, rename it `<name>-prev-<random>`, rename
   the new one to `name`, start it.
10. **Wait** until `ssh-keyscan -t ed25519 127.0.0.1` inside the new gate answers, bounded by
    `start_timeout`. A container that exits or restarts ends the wait at once, with its logs.
11. **Verify** by reading state back:
    - `docker inspect`: running; the mounts are exactly the two volumes; no `GH_TOKEN` in the
      env;
    - the token file is owned by `git`, mode `0400`;
    - the scanned host key equals the volume's key and the pin, when one exists;
    - for each repo config with `upstream_slug`, `gh api repos/<slug>` as `git` inside the gate
      succeeds. A deploy whose repos have no upstream makes no GitHub call.
12. **Commit.** Remove the previous gate; write the pin when none exists.

A failure in steps 1–8 leaves the previous gate untouched and removes the new container. A
failure in steps 9–11 removes the new container, renames the previous gate back and starts it.
Volumes are never removed. Every failure exits non-zero and names the step.

## Migration

A gate created by earlier tooling carries no label, so step 2 refuses it; remove it once by hand
(`docker rm -f <name>`). Its mirror volume can be reused by naming it in `volume`.
