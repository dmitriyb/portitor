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

- **Docker tier** — a job in `ci.yml`: `PORTITOR_REQUIRE_USERNS=1 go test -tags docker ./...`.
  No secrets. Its scenarios are in `test_deploy.md`.
- **Acceptance tier** — `.github/workflows/acceptance.yml`: `go test -tags acceptance ./...`
  against one disposable GitHub repo. The step gets `PORTITOR_ACCEPT_PAT` (a repository secret)
  and `PORTITOR_ACCEPT_REPO` (a repository variable) in its env and nothing else does. The PAT is
  fine-grained, limited to Contents and Pull requests on that one private repo, with Actions
  disabled there, so a leak reaches only a repo every scenario resets anyway. The workflow's
  concurrency group is the single global `acceptance` with `cancel-in-progress: false`: every run
  resets the same repo, so two runs must never overlap and a cancelled run would leave its PRs
  and branches behind.

The acceptance tier assumes people push portitor's branches. Once agents push them, a policy
that denies them any change under `.github/**` (as spexmachina's gate policy does) must be in
place first; otherwise an agent-written workflow could reach other repository secrets.

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
