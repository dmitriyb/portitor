# Version Injection

How version and build metadata are injected at build time and surfaced by `version` / `--version` / `-v`.

## ldflags Pattern

Go's `-ldflags -X` linker flag sets string variables at build time without modifying source. The variables are declared in the `main` package (`cmd/portitor/version.go`) with defaults:

```go
var (
    version = "dev"
    commit  = "none"
    date    = "unknown"
)
```

The release build injects values. In portitor this is done by GoReleaser via `.goreleaser.yaml`, whose ldflags target `main.version`/`main.commit`/`main.date`:

```
- -X main.version={{.Version}}
- -X main.commit={{.Commit}}
- -X main.date={{.Date}}
```

A local ad-hoc build uses the same keys, e.g. `go build -ldflags "-X main.version=v0.1.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%Y-%m-%d)" ./cmd/portitor/`.

## Wiring to cobra

`newRootCommand()` builds the version string from the injected variables and hands it to cobra's built-in version flag:

```go
root.Version = fmt.Sprintf("%s (commit %s, built %s)", version, commit, date)
root.SetVersionTemplate("portitor {{.Version}}\n")
```

Setting `root.Version` (non-empty) makes cobra add a `--version` flag and, since `v` is free on the root, a `-v` shorthand. The `version` subcommand prints the same line independently via `printVersion(cmd.OutOrStdout())`, so all three forms match byte-for-byte.

## Dev Builds

Built without ldflags (e.g. `go run ./cmd/portitor/`), the defaults apply: `version` is `dev`, `commit` is `none`, `date` is `unknown` — clearly distinguishing a development build from a release, whose own `portitor version` output is a direct link back to its provenance (the release proposal's motivation).
