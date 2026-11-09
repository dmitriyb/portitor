# Deploying portitor

Two different things get installed or built differently — keep them separate:

- **The binaries** are **release artifacts**: install them from the GitHub Releases page, verified, per [`install.md`](install.md). `install.sh` installs `portitor` and, beside it, `portitor-gate` — the linux build for the host's arch, the same program — which `portitor deploy` bakes into the gate image. `portitor` is also useful standalone on an operator's machine for `add-role`, `validate-config`, and `reconcile`.
- **The gate image** is **not** a release artifact: `portitor deploy` builds it on the operator's host with Nix (`docker` and `nix` on `PATH`; on macOS, a Linux builder, as faber's builds need), from portitor's own fixed definition plus any tools the deploy config declares.

## 1. Registry: per-repo config + role keys

Write the per-repo config into the registry before bringing up the container — one file per repo at `<config-dir>/repos.d/<name>.json`, plus an `allowed_signers` file at `<config-dir>/allowed_signers`.
See `configuration.md` for the full schema and an example.

Bind the roles here too, on the host, with the standalone CLI — the gate reads its config from its image, so roles bound inside a running gate would vanish at the next `portitor deploy`.
`add-role` writes `allowed_signers` at the path the config names, and at rest that is the in-container `/etc/portitor/allowed_signers`; point it at the host file for the calls and back afterwards (needs `jq` and the standalone CLI; the `trap` restores the path even when a call fails):

```bash
(
  set -euo pipefail
  dir=$PWD/portitor-config cfg=$PWD/portitor-config/repos.d/myrepo.json
  orig=$(jq -r .allowed_signers "$cfg")   # the in-container path, restored as is
  case "$orig" in /etc/portitor/*) ;; *) echo "allowed_signers is not under /etc/portitor: $orig" >&2; exit 1 ;; esac
  signers() { jq --arg p "$1" '.allowed_signers=$p' "$cfg" >"$cfg.new" && mv "$cfg.new" "$cfg"; }
  trap 'signers "$orig"' EXIT
  signers "$dir/${orig#/etc/portitor/}"
  PORTITOR_REPOS_DIR=$dir/repos.d portitor add-role \
    --repo myrepo --role implementer --fingerprint SHA256:… --pub ./implementer.pub
)
```

A config left pointing at a host path fails `portitor deploy`'s validation before any container changes.

`add-role` is safer than hand-editing the `roles` map — see `configuration.md`.

## 2. Deploy the gate

A deploy config, host-local and without secrets (all fields: `spec/delivery/arch_deploy.md`):

```json
{
  "name": "portitor",
  "volume": "portitor-repos",
  "host_key_volume": "portitor-hostkey",
  "networks": [{"name": "agents-net", "aliases": ["portitor"]}],
  "config_dir": "portitor-config",
  "authorized_keys": ["keys/implementer.pub", "keys/reviewer.pub", "keys/merger.pub"],
  "token_command": ["security", "find-generic-password", "-s", "portitor", "-w"],
  "host_key_file": "portitor.hostkey",
  "image": {"packages": [], "overlay": "tools.nix"}
}
```

```bash
portitor deploy --config deploy.json
```

`token_command` is any argv that prints the GitHub PAT — `secret-tool lookup …`, `systemd-creds decrypt --user …`, `security find-generic-password …`; portitor never learns where you keep it. `image.packages` and `image.overlay` add the tools your own policy's check commands call (nixpkgs attribute names; an overlay for anything outside nixpkgs); portitor's own runtime is fixed.

`portitor deploy` builds the image (skipped when its content hash already exists), validates every repo config as user `git` inside it, checks the host key against the pin, reads the PAT, creates the new gate under a temporary name, copies the PAT into it as `/run/secrets/gh_token` and reads it back, then stops the previous gate, starts the new one, waits for its sshd and verifies the result by reading it back: running, exactly the mirror and host-key volumes mounted, no `GH_TOKEN` env, the token file `git`-owned `0400`, the served host key equal to the pin, and GitHub accepting the PAT for every repo with an `upstream_slug`. Only then is the previous gate removed. Any failure before the swap leaves the previous gate untouched; any failure after it restores and restarts the previous gate. Volumes are never removed.

No host path is mounted and the PAT is never an env var, an argv or an image layer, so nothing the gate reads depends on host file ownership — the property a Docker daemon with userns-remap needs. The PAT does live in the container's writable layer on the daemon's disk; `docker commit`/`export` of the container would capture it. The config dir becomes image content and enters the Nix store, world-readable on the build host — keep secrets out of it.

A config change reaches the gate by re-running `portitor deploy`, which replaces the container: open SSH sessions drop, and the listed networks and aliases are reattached by the deploy itself. The SSH host key lives in its own volume and survives every deploy; the first deploy writes it to `host_key_file`, and a later deploy refuses when the volume's key differs from that pin, or the volume is empty while a pin exists. To rotate deliberately, remove both the volume and the pin file.

The entrypoint validates every registry config at boot and refuses to start if any is invalid. Upgrade ordering follows from that: when a new release adds a config field (e.g. `allowed_committer_emails`), upgrade the binaries **before** seeding the field — an older binary's strict decode refuses the whole config, and `portitor deploy` refuses at validation, before the running gate is touched.

A gate container created by earlier tooling carries no deploy label, so `portitor deploy` refuses to replace it: remove it once by hand (`docker rm -f <name>`); its mirror volume can be reused by naming it in `volume`.

## 3. Provisioning

The gate provisions each repo at boot from its config: for every `repos.d/<name>.json` whose repo is not provisioned yet, the entrypoint runs `portitor add-repo --ensure`, with the config's `default_branch` and, when `upstream_slug` is set, `https://github.com/<slug>.git` as the upstream. Provisioning is atomic, and `portitor shell` refuses a repo that is not fully provisioned, so a failed first boot can never leave a repo that accepts every push. An existing repo that is not provisioned is refused, never overwritten: inspect it and move it aside.

## 4. Point the agent at portitor

The agent clones and pushes over SSH (`ssh://git@portitor/srv/git/myrepo.git`); its key is installed with a forced command so it can do **only** gated git plus the role-checked `portitor pr` API.
Pin the gate's host key on the agent side from `host_key_file`.
On an accepted push, portitor forwards the branch upstream with its own credential and opens the PR, printing `PR #<n> <url>` back.
See `commands.md` for the full command/action reference and `architecture.md` for how the gate decides.

## 5. The MCP mediator (optional, its own container)

`portitor-mcp` runs as a **second container/user** beside the git-side one — different process, different credential set: the mediator holds the MCP upstream tokens (its own env/credential mounts) and its own outbound egress for upstream APIs; the git-side container is unchanged and keeps sole custody of the GitHub credential, with no MCP token and no new egress.

- Give the mediator its config file (see `configuration.md`) and start `portitor-mcp serve --config <path> --listen unix:<path>|tcp:<host>:<port>`; boot fails loudly on an invalid config or a configured-but-unset credential env name.
- Point the git-side container at it with `PORTITOR_MCP_TARGET` (same `unix:`/`tcp:` form). `portitor deploy` has no field for it yet. The splice target's reachability is the trust boundary for the asserted caller fingerprint — confine it (socket permissions / network isolation) to the git-side dispatcher.
- With no `PORTITOR_MCP_TARGET`, `portitor mcp` refuses cleanly and everything else is unaffected — roll out binary-first, then config.
- The `--internal` network and the agent-side egress lock are declarative deploy config owned outside this repo, unchanged by the mediator.

## Testing a deployment

The docker tier (`go test -tags docker ./cmd/portitor/`) deploys a gate from the working tree against a real daemon and drives a signed push and a fetch through it, with no GitHub; the acceptance tier runs the real-GitHub suite in CI. See `spec/delivery/test_deploy.md`.
