# Deploying portitor

Two different things get built or installed differently — keep them separate:

- **The `portitor` CLI/binary** is a **release artifact**: install it from the GitHub Releases page, verified, per [`install.md`](install.md) — it is the same binary the container image runs internally, plus it is useful standalone on an operator's own machine for `add-role`, `validate-config`, and `reconcile` against a mounted or copied registry.
- **The container image** (gate + egress, built from the repository `Dockerfile`) is **not** a release artifact and is **not distributed**: the operator builds it locally, from the tagged source, with `docker build -t portitor .`, which keeps the image's provenance identical to "whatever is in this checkout" rather than adding a second signed-artifact surface to maintain.

## 1. Registry: per-repo config + role keys

Write the per-repo config into the registry before bringing up the container — one file per repo at `<config-dir>/repos.d/<name>.json`, plus an `allowed_signers` file at `<config-dir>/allowed_signers`.
See `configuration.md` for the full schema and an example.

Bind the roles here too, on the host, with the standalone CLI — the container reads its config from its image, so roles bound inside a running container would vanish at the next `deploy/run.sh`.
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

A config left pointing at a host path fails `deploy/run.sh`'s validation before any container exists.

`add-role` is safer than hand-editing the `roles` map — see `configuration.md`.

## 2. Build and run the container

```bash
docker build -t portitor .

deploy/run.sh --config-dir ./portitor-config \
  --keys ./implementer.pub,./reviewer.pub,./merger.pub
```

`deploy/run.sh` reads the GitHub PAT from your keychain, bakes the whole config dir (minus `.lock` and dot-prefixed entries) into a derived image at `/etc/portitor`, root-owned and world-readable, and runs `portitor validate-config` for every repo config as user `git` inside that image. Only then does it create the container from the image (by ID), copy the PAT to `/run/secrets/gh_token` inside, and start it with a persistent `/srv/git` volume.
No host path is mounted and the PAT is never an env var or an image layer, so nothing the gate reads depends on host file ownership — the property a Docker daemon with userns-remap needs.
The validation proves that `git` can read every repo config and that its `allowed_signers` path exists; it does not open the other paths a config names (rule and check commands).
The whole config dir becomes world-readable inside the image and stays in docker's build cache, so keep secrets out of it.
The PAT lives in the container's writable layer (`/run/secrets/gh_token`, and `git`'s gh login), as it always did; `docker commit`/`export` of the container would capture it.
A config change no longer reaches a running gate by itself: re-run `deploy/run.sh`, which bakes a new image and recreates the container — open SSH sessions drop, the gate's SSH host key is regenerated (re-pin it wherever agents keep `known_hosts`), and a container attached to extra networks must be reattached. Superseded config images are left untagged; `docker image prune` removes them.
The entrypoint runs `validate-config` over every registry config at boot and refuses to start if any is invalid.
Upgrade ordering follows from that: when a new release adds a config field (e.g. `allowed_committer_emails`), upgrade the binary/image **before** seeding the field — an older binary's strict decode refuses the whole config. Through `deploy/run.sh` that refusal comes at validation, before the running container is replaced; a container started any other way fails at boot.

## 3. Provision a repo

```bash
docker exec -u git portitor portitor add-repo \
  --repo myrepo --upstream https://github.com/you/myrepo.git
```

`add-repo` expects the repo's config to already exist at `repos.d/<name>.json` (step 1, baked by `deploy/run.sh`).

## 4. Point the agent at portitor

The agent clones and pushes over SSH (`ssh://git@portitor/srv/git/myrepo.git`); its key is installed with a forced command so it can do **only** gated git plus the role-checked `portitor pr` API.
On an accepted push, portitor forwards the branch upstream with its own credential and opens the PR, printing `PR #<n> <url>` back.
See `commands.md` for the full command/action reference and `architecture.md` for how the gate decides.

## 5. The MCP mediator (optional, its own container)

`portitor-mcp` runs as a **second container/user** beside the git-side one — different process, different credential set: the mediator holds the MCP upstream tokens (its own env/credential mounts) and its own outbound egress for upstream APIs; the git-side container is unchanged and keeps sole custody of the GitHub credential, with no MCP token and no new egress.

- Give the mediator its config file (see `configuration.md`) and start `portitor-mcp serve --config <path> --listen unix:<path>|tcp:<host>:<port>`; boot fails loudly on an invalid config or a configured-but-unset credential env name.
- Point the git-side container at it with `PORTITOR_MCP_TARGET` (same `unix:`/`tcp:` form). The splice target's reachability is the trust boundary for the asserted caller fingerprint — confine it (socket permissions / network isolation) to the git-side dispatcher.
- With no `PORTITOR_MCP_TARGET`, `portitor mcp` refuses cleanly and everything else is unaffected — roll out binary-first, then config.
- The `--internal` network and the agent-side egress lock are declarative deploy config owned outside this repo, unchanged by the mediator.

## Live end-to-end proof

`../deploy/DEPLOY.md` is a full runbook for a real run against the dca agent (dotfiles repo) and a real GitHub sandbox repo: role keys, a concrete config, container bring-up, mirroring, and a live agent push through to an opened PR.
Use it to validate a deployment end-to-end, or as a worked example of everything above with real values filled in.
