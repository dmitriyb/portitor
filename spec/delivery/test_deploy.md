# Deploy test scenarios

Three tiers. Each assertion must fail when the behaviour it names is removed.

## Unit (`go test ./...`, every OS)

`deploy` runs against fake `docker` and `nix` that record every call and its environment.

- **Order.** Lock, preflight, build, validate, host key and PAT all happen before any call that
  changes a container; the swap happens only after the PAT round-trip matches.
- **Refusals before any container change:** an unknown config field; a `name` outside the
  charset; a non-linux or wrong-arch `portitor-gate`; a build (module version, VCS revision)
  other than the running binary's; a daemon arch other than
  the host's; a missing network; a foreign container named `name`; an invalid repo config; a
  host-key volume key differing from the pin; an empty volume with a pin; a failing, empty or
  multi-line `token_command`.
- **The PAT** appears in no argv and no child environment, and no file under the host temp dir
  ever holds it. `GH_TOKEN` and `GITHUB_TOKEN` set in `deploy`'s own environment reach no
  `docker` or `nix` child.
- **Rollback.** A failure injected at each of steps 8–11 — including a token read-back mismatch,
  an extra mount, a `GH_TOKEN` in the env, a different served host key, and an interrupt during
  the wait — leaves the previous gate named `name` and started, and removes the new container;
  no call removes a volume.
- **The token tar** holds exactly `gh_token`, mode `0400`.
- **Leftovers.** A `-next-` container is removed; a lone `-prev-` one is restored; a `-prev-`
  beside the gate refuses.
- **Tag.** Changing any hashed input — a config file's bytes, its relative path or exec bit, the
  overlay, a package, the gate binary, the entrypoint, the rendered expression — changes the
  tag; an existing tag skips `nix build`.
- **Lock.** A second deploy of the same `name` blocks while the first holds the lock.
- **Credential path.** `git-credential get` answers `x-access-token` and the file's token for
  `github.com` only, nothing for another host, and exits 1 on a missing or empty file; the `gh`
  Runner sets `GH_TOKEN` only on its child; a merge predicate gets `GH_TOKEN`, a content check
  does not; with no file, neither `gh` nor the trampoline gets one.
- **Image expression.** The rendered expression contains the fixed part, the declared packages,
  the staged overlay, and no `config.Volumes`.

## Docker (`go test -tags docker -run DockerTier ./cmd/portitor/`; CI on every pull request)

A real daemon and a real `nix build`; a gate deployed from the working tree through `gate_bin`,
a dummy PAT, and a repo config without `upstream_slug`, so nothing reaches GitHub.

- `docker inspect`: running, exactly the two volumes, no `GH_TOKEN`.
- Inside: the token file is `git`-owned, `0400`, and byte-equal to the dummy PAT.
- With ephemeral role keys: a signed push to a feature branch passes `pre-receive`; a fetch
  (`git ls-remote`) through the forced command sees it, before and after a re-deploy.
- A second deploy with a changed config replaces the gate and keeps the host key.
- In CI the job runs the daemon with `userns-remap`; when `PORTITOR_REQUIRE_USERNS=1` and
  `docker info` reports no user namespace, the test fails instead of skipping.

## Acceptance (`go test -tags acceptance -run GateAccept ./cmd/portitor/`; CI only)

The real-GitHub suite (`gate_accept_test.go`) stands its gate up through the same builder. Its
PAT and repo slug come from `PORTITOR_ACCEPT_PAT` and `PORTITOR_ACCEPT_REPO`. When `CI` is set
and either is missing, the suite fails; outside CI it skips. The Actions-proxy scenario
(`RerunPath`) pushes a workflow and waits for Actions runs, which CI's PAT and repo deliberately
cannot do; it runs only with `PORTITOR_ACCEPT_ACTIONS=1`, a PAT with the Workflows permission and
Actions enabled on the disposable repo. The boot-rejection scenario is still a stub; when filled
in, it creates a container from the built image directly, because `deploy`'s own validation
would refuse first.
