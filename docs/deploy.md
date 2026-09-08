# Deploying portitor

Two different things get built or installed differently — keep them separate:

- **The `portitor` CLI/binary** is a **release artifact**: install it from the GitHub Releases page, verified, per [`install.md`](install.md) — it is the same binary the container image runs internally, plus it is useful standalone on an operator's own machine for `add-role`, `validate-config`, and `reconcile` against a mounted or copied registry.
- **The container image** (gate + egress, built from the repository `Dockerfile`) is **not** a release artifact and is **not distributed**: the operator builds it locally, from the tagged source, with `docker build -t portitor .`, which keeps the image's provenance identical to "whatever is in this checkout" rather than adding a second signed-artifact surface to maintain.

## 1. Registry: per-repo config + role keys

Write the per-repo config into the registry before bringing up the container — one file per repo at `<config-dir>/repos.d/<name>.json`, plus an `allowed_signers` file at `<config-dir>/allowed_signers`.
See `configuration.md` for the full schema and an example.

## 2. Build and run the container

```bash
docker build -t portitor .

deploy/run.sh --config-dir ./portitor-config \
  --keys ./implementer.pub,./reviewer.pub,./merger.pub
```

`deploy/run.sh` reads the GitHub PAT from your keychain and mounts the config dir read-only at `/etc/portitor` plus a persistent `/srv/git`.
The entrypoint runs `validate-config` over every registry config at boot and refuses to start if any is invalid.
Upgrade ordering follows from that: when a new release adds a config field (e.g. `allowed_committer_emails`), upgrade the binary/image **before** seeding the field — an older binary's strict decode refuses the whole config, taking the gate down at boot.

## 3. Provision a repo and bind roles

```bash
docker exec -u git portitor portitor add-repo \
  --repo myrepo --upstream https://github.com/you/myrepo.git

docker exec -u git portitor portitor add-role \
  --repo myrepo --role implementer --fingerprint SHA256:… --pub ./implementer.pub
```

`add-repo` expects the repo's config to already exist at `repos.d/<name>.json` (step 1).
`add-role` is safer than hand-editing the `roles` map — see `configuration.md`.

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
