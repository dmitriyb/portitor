# Change Proposal: A container deploy that shares no host path

## Context

`deploy/run.sh` starts the gate with the operator's config directory bind-mounted
read-only at `/etc/portitor` and the GitHub PAT passed as `-e GH_TOKEN`. Two problems
follow:

- **userns-remap breaks the config mount.** With `"userns-remap"` in the daemon
  configuration, container root and `git` are subordinate host uids, not the operator.
  A config file the operator's tooling wrote `0600` (any `mktemp`-then-`mv` write, and
  `portitor add-role` keeps an existing file's mode) is unreadable inside the container,
  so the gate cannot load its own `repos.d/<repo>.json`. Without remap the mount only
  works because container root bypasses file modes.
- **The PAT is visible.** An `-e` value sits in `docker inspect` for the life of the
  container and on the docker CLI's argv while it starts — the one credential portitor
  exists to keep away from everything else.

The surrounding tooling has already moved: faber v0.8.0 shares no host path with a box,
and the operator stack that runs portitor beside faber bakes the gate config into a
per-instance image. portitor's own deploy path should hold the same property.

## Proposed change

- **Config is image content.** `deploy/run.sh` builds a derived image: `FROM <image>`
  plus a `COPY` of the whole config dir (symlinks followed; `.lock` and dot-prefixed
  entries left out) — root-owned, files `0644`, directories and executables `0755`. It
  uses the image by the ID the build prints, never a shared tag, so concurrent runs cannot
  start each other's config, and a digest-pinned `--image` keeps working. Before any
  container exists — and before the running one is removed — it runs `portitor
  validate-config` for every repo config **as user `git` inside that image**: the check
  the entrypoint makes at boot, which here also proves that `git` can read each repo
  config and that its `allowed_signers` path exists (not that every path a config names
  is readable). `run.sh` unsets `GH_TOKEN` after reading it, so no docker CLI it starts
  inherits the PAT.
- **The PAT is a file delivered into the container, never an env var or a layer.**
  `run.sh` creates the container, copies the token to `/run/secrets/gh_token` with
  `docker cp` (root-owned), then starts it; a failed copy or start removes the container.
  The entrypoint reads `/run/secrets/gh_token` and persists the credential into `git`'s gh
  config. The file now wins over a `GH_TOKEN` env value (before, a set env value won); the
  env stays as the inspect-visible legacy fallback for existing deployments.
- **The image creates `/run/secrets`** (root, `0700`) so the copy has a target.
- **Tests.** A docker-less test runs `deploy/run.sh` itself against a fake `docker` and
  asserts the call sequence: no host path mounted, the PAT in no argv, validation as `git`
  before create, create from the built ID, copy before start. The container-level
  acceptance harness delivers config and PAT the same way and writes its config `0600`, so
  it only passes if the bake normalizes modes.
- **Docs.** Roles are bound in the host config dir before `run.sh` (`add-role` with the
  standalone CLI), never inside a running container.

The in-container paths (`/etc/portitor`, `/run/secrets/gh_token`, `/srv/git`) keep their
meaning; no Go code, config field or CLI changes. One operator-visible behaviour changes:
a host-side config edit no longer reaches a running gate by itself — it takes a re-run of
`deploy/run.sh`, which recreates the container: open SSH sessions drop, the gate's SSH host
key is regenerated (it always was on a recreate; now every config change is one), and extra
networks must be reattached. This is a patch-level change.

## Impact expectation

- **gate**: the `Container` component description in `module.json` and its paragraph in
  `arch_pre_receive.md` (config baked into a derived image; PAT as a copied-in file; no
  host path).
- **action**: the deployment paragraph in `arch_action.md` (the credential arrives at
  `/run/secrets/gh_token`, `GH_TOKEN` as fallback).
- **deploy**: `Dockerfile` (`/run/secrets`, header example), `deploy/run.sh`,
  `deploy/entrypoint.sh` (the file wins over `GH_TOKEN`), `deploy/DEPLOY.md`, `docs/deploy.md`,
  `README.md`.
- **tests**: a new docker-less `cmd/portitor` test of `deploy/run.sh`, described in
  `spec/gate/test_pre_receive.md`;
  `cmd/portitor/gateaccept_harness_test.go` (acceptance tag) builds the config image and
  copies the PAT in instead of `-v …:/etc/portitor:ro` and `-e GH_TOKEN`.
