# VersionCommand

The `portitor version` subcommand, and the equivalent `--version`/`-v` root flag. Prints the version string and build provenance.

## Responsibilities

- Print a single line: `portitor <version> (commit <commit>, built <date>)`.
- Exit 0 after printing, with nothing on stderr.
- Offer all three forms — `portitor version`, `portitor --version`, `portitor -v` — with byte-identical output.

## Output Format

```
portitor v0.1.0 (commit abc1234, built 2026-07-22)
```

One line, human-readable, unchanged from the released `v0.1.0` output. The subcommand prints it via `printVersion(cmd.OutOrStdout())`; the `--version`/`-v` flag is cobra's built-in version flag, wired to the same line by setting `root.Version` to `<version> (commit <commit>, built <date>)` and `SetVersionTemplate("portitor {{.Version}}\n")`. cobra adds `-v` as the shorthand for `--version` because no other root flag claims `v`.

## Version Variables

Three package-level variables in the `main` package (`cmd/portitor/version.go`) with default values, overridden at build time via `ldflags` (see impl_version_injection.md):

| Variable | Default | ldflags key |
|----------|---------|-------------|
| `version` | `dev` | `-X main.version=v0.1.0` |
| `commit` | `none` | `-X main.commit=abc1234` |
| `date` | `unknown` | `-X main.date=2026-07-22` |

The variables stay in `package main` because the GoReleaser `.goreleaser.yaml` ldflags target `main.version`/`main.commit`/`main.date`; `newRootCommand()` reads them when building the version string, so the root's `Version` reflects the injected values.

## Registration

`VersionCommand` is a child of `RootCommand`, registered via `newVersionCmd()` in `newRootCommand()`. It sets `Args: cobra.NoArgs` (extra positional arguments are a usage error). `--version`/`-v` are added automatically by cobra once `root.Version` is non-empty.
