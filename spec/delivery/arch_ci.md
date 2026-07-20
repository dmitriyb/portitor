# CI (build/vet/test, race, fuzz)

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
