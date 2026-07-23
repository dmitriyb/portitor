package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// version, commit, and date are stamped by GoReleaser via -ldflags at build
// time (main.version=..., main.commit=..., main.date=...); a locally built
// binary keeps these defaults.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// printVersion prints the version, source commit, and build date. It backs both
// the `version` subcommand and the root's --version/-v flag (see newRootCommand).
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "portitor %s (commit %s, built %s)\n", version, commit, date)
}

// newVersionCmd is the `version` subcommand; `--version`/`-v` are cobra's
// built-in version flag, wired to the same line in newRootCommand.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			printVersion(cmd.OutOrStdout())
			return nil
		},
	}
}
