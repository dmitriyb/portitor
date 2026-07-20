package main

import (
	"fmt"
	"io"
)

// version, commit, and date are stamped by GoReleaser via -ldflags at build
// time (main.version=..., main.commit=..., main.date=...); a locally built
// binary keeps these defaults.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// printVersion prints the version, source commit, and build date.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "portitor %s (commit %s, built %s)\n", version, commit, date)
}
