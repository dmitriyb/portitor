package main

import "github.com/dmitriyb/portitor/internal/check"

// internalCheckExec is the `internal-check-exec` subcommand: the rlimit
// re-exec trampoline behind internal/check. Not part of the CLI surface (not
// in usage; the SSH shell dispatcher cannot route to it). The body lives in
// internal/check so test binaries can intercept the re-exec in TestMain.
func internalCheckExec(args []string) int {
	return check.TrampolineMain(args)
}
