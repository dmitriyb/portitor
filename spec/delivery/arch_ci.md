# CI (build/vet/test, race, docker, acceptance, fuzz)

Three triggers, tiered by cost, each in its own workflow file so a trigger change in one never
risks the others.

## Fast gate (`pull_request`, `push` to `main`)

`.github/workflows/ci.yml` runs one job on both triggers:

```
go build ./...
go vet   ./...
go test  ./...
```

On `push` to `main` only, a fourth step adds `go test -race ./...`. `-race` roughly doubles wall
time; paying it on every PR push would slow the feedback loop for no benefit a merged commit
doesn't already get, so it runs once, after merge, not per-push-to-a-PR-branch.

Both triggers set `paths-ignore: ["**.md", "spec/**"]` — a docs-only or spec-only change never
burns a runner — and the workflow's `concurrency` group is `ci-${{ github.workflow }}-${{
github.ref }}` with `cancel-in-progress: true`, so pushing a second commit to the same PR cancels
the first run's job rather than queuing behind it.

## Docker and acceptance tiers (`pull_request`, `push` to `main`)

Both tiers deploy a real gate, so both jobs install Nix (`cachix/install-nix-action` pinned by
commit SHA, with a pinned Nix version) and cache the Nix store between runs, and both configure
the runner's Docker daemon with `"userns-remap": "default"` and restart it before the tests.

The remap step is `scripts/ci-userns-remap.sh`, which merges the setting into the runner's
`daemon.json` and fails unless the restarted daemon reports a user namespace.

- **Docker tier** — a job in `ci.yml`: `PORTITOR_REQUIRE_USERNS=1 go test -tags docker -run
  DockerTier -timeout 40m ./cmd/portitor/`. No secrets. Its scenarios are in `test_deploy.md`.
- **Acceptance tier** — `.github/workflows/acceptance.yml`: `go test -tags acceptance -run
  GateAccept -timeout 50m ./cmd/portitor/`, described below.

## The acceptance tier

The acceptance tier proves the gate against real GitHub: a gate deployed from the working tree
through the same builder `portitor deploy` uses, driven over SSH as each role, its effects read
back from GitHub. Its scenarios are listed in `test_deploy.md`; this section is the machinery
around them.

**One disposable repo.** Every scenario runs against one private GitHub repo that exists for
nothing else (`PORTITOR_ACCEPT_REPO`, a repository variable). Actions are enabled there, limited
to the owner's own actions and reusable workflows; the only workflow is the seed's.

**The seed is data in this repo.** The repo's pristine state is the tree under
`cmd/portitor/testdata/acceptance-seed` — a probe file, the approvals fixture, and the
pass+fail workflow the Actions-proxy scenario runs, triggered only on that scenario's branch so
no other scenario's PR carries checks — at the tag `cmd/portitor/testdata/acceptance-seed.tag`
names. Changing the seed means changing that tree and the tag name together.

**The operator prepares the seed; the suite never does.** The seed holds a workflow file, and
writing one needs the Workflows permission, which the suite's credential deliberately lacks. So
the seed is written with the repo owner's own credential: `scripts/prepare-acceptance-seed.sh
<clone>` makes, in a clone of the disposable repo, a parentless `main` commit whose tree is
exactly the seed, and the tag; the operator pushes both (`--force` for `main`) and deletes
superseded seed tags. It runs by hand, once per seed change, never in CI.

**Every scenario starts from the seed.** Before each scenario the suite verifies the tag exists
and its tree equals the seed tree path for path and byte for byte — a missing or stale seed
fails, naming the fix — then resets the repo to it: closes open pull requests, deletes every
branch but the default, and force-moves the default branch to the tag.

**One credential, bounded by what GitHub refuses.** `PORTITOR_ACCEPT_PAT` (a repository secret)
is a fine-grained PAT on the disposable repo only, with Contents, Pull requests and Actions
(read/write) and no Workflows permission. Contents and Pull requests are what the gate's own
credential needs to forward branches, open, review and merge pull requests; Actions is what the
Actions proxy needs to read runs and re-run jobs. A leaked PAT can therefore push to a repo
every scenario resets, and re-run the seed's own workflow — never add a workflow that runs code
of its choosing. `TestGateAccept_PATCannotWriteWorkflows` pushes a new workflow with it and fails
the moment GitHub accepts it, so that bound is checked on every run instead of assumed.

**Where the credential goes.** Only the acceptance step's env holds the PAT; the harness
hands it to the gate as the copied-in credential file and keeps it out of every docker child's
env. The job is skipped for pull requests from forks and from Dependabot, which get no secrets;
under CI a missing secret or variable fails the suite rather than skipping it.

**Runs never overlap.** The workflow's concurrency group is the single global `acceptance`
with `cancel-in-progress: false`: every run resets the same repo, so two runs must never
interleave, and a cancelled run would leave its pull requests and branches behind.

**The trust assumption.** The acceptance tier runs the code of the branch under test with the
PAT in reach. It assumes people push portitor's branches. Once agents push them, a policy that
denies them any change under `.github/**` (as spexmachina's gate policy does) must be in place
first; otherwise an agent-written workflow could reach other repository secrets.

## Nightly fuzz (`schedule`)

`.github/workflows/fuzz.yml` exists because `spec/reviews/2026-07-18.md` §5 calls for fuzzing the
gate's parsing and matching functions on a recurring cadence, beyond what `go test` runs on every
push. It runs once nightly (`17 5 * * *`, off the top of the hour) and on `workflow_dispatch` for
an ad hoc run.

**Targets are discovered, not enumerated.** The alternative — a YAML list of fuzz function names —
goes stale the moment someone adds a `func Fuzz*` and forgets to update the workflow (or, worse,
silently fuzzes nothing when a name in the review doc doesn't match the actual function name, as
already happened once: the review's `FuzzConfigLoad` is `FuzzLoadFile` in `internal/config`). The
job instead does:

```bash
grep -rl '^func Fuzz' --include='*_test.go' .        # which files define a fuzz target
grep -oE '^func (Fuzz[A-Za-z0-9_]+)' "$file"          # which function(s), per file
```

and for each `(package-dir, FuncName)` pair runs

```bash
go test -run='^$' -fuzz="^${name}\$" -fuzztime="${FUZZTIME}" "./${dir}"
```

`FUZZTIME` defaults to `3m` per target (overridable via `workflow_dispatch` input) — at the time
this module was written, nine targets exist across three packages (`cmd/portitor`:
`FuzzParseUpdates`, `FuzzClassify`, `FuzzShellSplit`, `FuzzSignerLineKeyBlob`; `internal/rules`:
`FuzzMatchGlob`, `FuzzDeltaUnits`, `FuzzCompile`, `FuzzEvaluateSemantic`; `internal/config`:
`FuzzLoadFile`), so nine sequential 3-minute runs fit comfortably inside the job's 90-minute
timeout. A target added later is picked up the following night with no workflow edit.

A failing target leaves Go's usual `testdata/fuzz/<FuzzName>/<hash>` failing-input file in the
tree; the job uploads every `**/testdata/fuzz/**` path as a `fuzz-failures` artifact on failure
(`if: failure()`) so the crashing input can be pulled down and added as a seed corpus entry
without reproducing the crash by hand first.
