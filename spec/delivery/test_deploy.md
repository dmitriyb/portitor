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
- Boot rejection: a container created from the deployed image without the credential file,
  and one given a repo config that does not validate, each exit non-zero with the entrypoint's
  refusal and never start sshd. They are created from the image directly, because `deploy`'s
  own validation would refuse the broken config before any container exists.
- In CI the job runs the daemon with `userns-remap`; when `PORTITOR_REQUIRE_USERNS=1` and
  `docker info` reports no user namespace, the test fails instead of skipping.

## Acceptance (`go test -tags acceptance -run GateAccept ./cmd/portitor/`; CI only)

The real-GitHub suite (`gate_accept_test.go`) stands its gate up through the same builder. Its
PAT and repo slug come from `PORTITOR_ACCEPT_PAT` and `PORTITOR_ACCEPT_REPO`. When `CI` is set
and either is missing, the suite fails; outside CI it skips. Every scenario resets the repo to
the operator-prepared seed (`testdata/acceptance-seed`, tag `testdata/acceptance-seed.tag`) and
first verifies the tag holds exactly that tree. No scenario is skipped:

- the single-account merge model: an approval recorded as repo content by the reviewer
  role, enforced at push, gating the merge, landed by a merger who signs nothing;
- mirror refresh on serve: a clone through the gate reflects upstream's advanced default branch;
- the Actions proxy: `pr checks`, `pr logs` and a capped, role-gated `pr rerun` on the seed's
  pass+fail workflow, confirmed against GitHub;
- review passthrough: a comment review lands, GitHub's refusal of a self-approval is forwarded
  verbatim;
- gate threads: a thread raised through the gate is resolved by `resolve --gate-threads`;
- the PAT bound: GitHub refuses the acceptance PAT's push of a new workflow.

Not covered here: that `resolve --gate-threads` leaves a thread by another author alone needs a
second GitHub account (`GateAuthoredThreads` is unit-tested); the head-pin race on merge is not
reproducible against GitHub deterministically (`--match-head-commit` is unit-tested in
`internal/action`).
