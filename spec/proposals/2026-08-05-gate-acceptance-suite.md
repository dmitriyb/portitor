# Change Proposal: Container-level gate acceptance suite (user-executed, opt-in)

## Context

portitor has two test tiers today: fast unit tests over pure functions with a
stubbed `gh` runner, and the opt-in real-`gh` suite (`realgh_test.go`,
2026-08-04-merge-gate-v2 §4) that drives the `internal/action.GH` adapter
against a disposable repo. Neither exercises the gate *through its own front
door*: the sshd forced-command dispatch, config validation at container **boot**,
push-time content-rule enforcement over `git-receive-pack`, and the `portitor
pr` action API as invoked over SSH. The recently landed transparent-approve
change (2026-08-05) is exactly the kind of behavior that only a
gating-through-the-container test can prove complete — that a release needs no
further patch — because its surface is the boot-time config decode, the wire
protocol, and real GitHub state, not a function signature.

## Proposed change

A third, **container-level acceptance suite**: it builds the gate image from the
current tree, stands the gate up, and drives real git + `portitor pr` flows
*through* it against the disposable repo, asserting the gate's decisions.

### Execution model — user-run only, never CI (v1)

Explicitly not automation. Like `realgh_test.go` it is opt-in and self-skips,
and v1 adds no CI wiring of any kind:

- Gated behind a build tag (`//go:build acceptance`) **and** the existing
  `testdata/realgh.local.json` (disposable repo slug + PAT keychain service)
  **and** a docker availability probe; any missing → `t.Skip`. It can never run
  under `go test ./...` or in CI.
- Run explicitly: `go test -tags acceptance -run GateAccept ./...` (a doc line
  in the suite header, mirroring the real-`gh` header).
- The PAT is read only from the keychain via `security` (never the process
  env, never committed), and scrubbed from any failure output (reuse
  `redactPAT`).

### Harness

- **Build** the gate image from the working tree (`docker build` of the repo
  `Dockerfile`, tagged uniquely per run) — so the suite tests THIS code, not a
  release.
- **Ephemeral role keys.** Generate throwaway `ed25519` keypairs for
  implementer / reviewer / merger per run (NOT the operator's YubiKey keys — a
  test harness must be hardware-free and repeatable). The gate authorizes and
  verifies purely by fingerprint, so ephemeral keys are indistinguishable to it;
  the harness signs test commits with them (`git -c user.signingkey=…
  commit -S`).
- **Stand up** the gate container (role pubkeys as `AGENT_AUTHORIZED_KEY`, the
  scenario's per-repo config mounted at `/etc/portitor`, a scratch repos
  volume, a mapped SSH port, the PAT delivered as the deploy path delivers it),
  `add-repo` the disposable slug, and pin its host key for the client.
- **Drive** as each role over SSH: `git push` to exercise pre/post-receive, and
  `portitor pr …` to exercise the forced-command action API; assert both the
  gate's stdout/exit and the resulting GitHub state (via the PAT).
- **Reset + teardown always**: reset the disposable repo to its seed tag before
  each scenario (the faber-e2e reset shape), and remove the container + volumes
  + temp keys on exit, even on failure.

### Scenario matrix (v1)

1. **Transparent approve** — `review --event comment` posts a real COMMENT
   review; `review --event approve` from the PAT's own account returns 422 and
   the gate forwards the error verbatim (fails loudly, no gate state written).
2. **Config boot rejection** — a per-repo config carrying `reviews_log` or
   `merge_gate.review: "internal"` makes the gate refuse at boot
   (`validate-config` / entrypoint), never serving — the strict-decode /
   upgrade-binary-first contract.
3. **Single-account merge model** (the headline case that dead-ended before) —
   `merge_gate.review: none` + `merge_gate.checks: [bead-closed]` +
   `content_rules` bead-close = reviewer-only: push a feature branch; `merge`
   is **refused** while the bead is open; a **non-reviewer** bead-close is
   **rejected at push**; a **reviewer-signed** bead-close is accepted; `merge`
   then **succeeds** and lands on the disposable repo.
4. **Separation-of-duties is gone** — the reviewer having signed the bead-close
   no longer blocks anything, and the merger (who signed nothing) merges; the
   flow that previously failed with the sod refusal now completes.
5. **Gate-thread auto-resolve by author** — after a gate inline review and a
   human reply thread, `resolve --gate-threads` resolves only the
   gate-authored threads and leaves the human thread unresolved.
6. **Head-pinning** — `merge` is refused when the head advanced since the
   precondition evaluation (`--match-head-commit` TOCTOU).

### Deferred (documented, not silently skipped)

- **Native approve success** (`merge_gate.review: github` with a real APPROVE):
  needs a **second** GitHub account to cast a non-self approval; v1 asserts the
  self-approve **refusal** only and logs the native-success case as requiring a
  second account. This is a harness-credential limitation, not a gate gap.

## Impact expectation

- **tests**: a new `//go:build acceptance` suite (its own file, likely
  `cmd/portitor/gate_accept_test.go` plus a small docker/ssh harness helper),
  reusing `realgh.local.json` gating + `redactPAT`; no production code changes.
- **spec/docs**: this proposal; a `spec/gate/test_*.md` leaf enumerating the
  scenario matrix and the user-run/opt-in contract; the real-`gh` harness note
  in the merge-gate-v2 leaf gains a pointer to the container tier.
- **CI**: none in v1 (deliberate). A later proposal may wire a gated job once
  the suite is proven by hand.
- **No production behavior change**: this is test infrastructure only; the gate
  binary and config contract are untouched.
