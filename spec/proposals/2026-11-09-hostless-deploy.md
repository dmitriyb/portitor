# Change Proposal: `portitor deploy` — the gate as a Nix image that shares no host path

## Context

portitor has no maintained way to run the gate. `deploy/run.sh` on `main` bind-mounts the
config dir and passes the PAT as `-e GH_TOKEN`: under Docker's `userns-remap` the mounted
config is unreadable, and the PAT sits in `docker inspect`. Operators therefore run their own
scripts around their own Nix images, and those copies drift from portitor.

Three more problems surface on the way:

- With a token, the entrypoint runs `gh auth login`, so a gate cannot boot without GitHub.
- The real-GitHub acceptance suite reads its PAT from the macOS keychain and skips everywhere
  else.
- `init-repo` creates the bare repo before it bakes the hooks, and `portitor shell` does not
  check for hooks, so a failed provisioning can leave a repo that accepts every push.

## Decisions

1. **`portitor deploy --config <file>`** builds the gate image and replaces the gate container.
   It is idempotent. The container shares no host path: its only mounts are two named volumes,
   one for the mirrors and one for the SSH host key.
2. **The image is built with Nix**, over a nixpkgs pin compiled into portitor, overridable per
   deploy.
   - portitor fixes everything the gate itself needs.
   - The operator declares only extra tools for their policy's commands, as faber's
     `{packages, overlay, pin}`.
   - The config dir is baked in as the top layer.
3. **One binary, installed as a pair.** `install.sh` installs the linux build for the host's
   arch beside `portitor` as `portitor-gate`, and `deploy` always bakes that file. `upgrade`
   and `--rollback` treat the two as one unit. `deploy` refuses a pair whose versions differ.
4. **The deploy config is a host-local JSON file without secrets.** The PAT comes from
   `token_command`, an argv whose stdout is the PAT.
5. **The PAT reaches the gate only as a file**, `/run/secrets/gh_token`, copied into the
   created container. It never appears in an env var, an argv, an image layer or a host file.
   `deploy` verifies that the PAT was delivered and that GitHub accepts it. Tests can opt out
   of the GitHub check.
6. **The gate does not log in.** portitor passes the token per call to:
   - `gh`;
   - network `git`, via a `portitor git-credential` helper;
   - merge predicates.

   Content checks get no token. A gate without the token file refuses to boot. Operator-side
   commands with no such file keep today's behaviour.
7. **The host key persists in its volume.** `deploy` writes it to a pin file and refuses when
   the key no longer matches.
8. **Provisioning is atomic**, and `portitor shell` refuses a repo that is not fully
   provisioned. At boot the gate provisions every configured repo that is not provisioned
   yet. A repo without an upstream is served without the refresh-before-serve.
9. **A failed deploy leaves the previous gate running.** The old container is removed only
   after the new one has been verified.
10. **Every deploy step is verified by reading back the state it changed**, not by an exit
    code.
11. **Tests:**
    - **Unit**, against fake `docker` and `nix`: every run, every OS.
    - **Docker**, against a real daemon with `userns-remap`: CI on every pull request, no
      secrets.
    - **Acceptance**, against real GitHub: CI only, on pull requests and on `main`. A missing
      secret fails the run in CI.

    The acceptance PAT reaches the test step's env only. It is limited to Contents, Pull
    requests, Actions and Commit statuses on one private disposable repo, with no Workflows
    permission: it can
    run the workflows the repo already has, never add one, and a test proves GitHub refuses
    it. The repo's seed, workflow included, is prepared once by the operator. No acceptance
    scenario is skipped in CI. This replaces "user-run only, never CI" from
    `2026-08-05-gate-acceptance-suite.md`.

    This assumes people push portitor's branches. Once agents do, a policy that freezes
    `.github/**` for them (as spexmachina's does) is required first.
12. **CI installs Nix** with a pinned action and a pinned Nix version, and caches the store.
13. **Removed:** `deploy/run.sh`, the Alpine `Dockerfile`, `deploy/DEPLOY.md` and the `run.sh`
    test. Pre-1.0, so this is a minor release.

Out of scope: pruning old images, an MCP target in the deploy config, and the operator-side
scripts this replaces.

## Impact expectation

- **cli**: the `deploy` and `git-credential` commands; `upgrade` for the pair.
- **delivery**: a new `Deploy` component (`arch_deploy.md`, `test_deploy.md`) holding the
  deploy sequence, the config fields and the image contents; `install.sh` for the pair; CI.
- **gate**: the `Container` component; atomic `init-repo`; `shell`'s refusal; the check
  trampoline's env for predicates.
- **action**: the per-call credential.
- **CLAUDE.md**: delivery now depends on gate; the host side of `deploy` needs `docker` and
  `nix`.
- **docs**: `docs/deploy.md`, `docs/install.md`, `README.md`.
