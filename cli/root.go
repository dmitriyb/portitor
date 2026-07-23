// Package cli constructs portitor's cobra command tree. It mirrors
// spexmachina's cli/root.go + cmd/spex/*.go split: NewRootCmd builds the
// top-level command here, and cmd/portitor wires the subcommands onto it.
package cli

import "github.com/spf13/cobra"

// Command group IDs, used to organize the subcommands under headed sections in
// the root help output (mirroring the hand-rolled usage text's grouping).
const (
	GroupGate         = "gate"
	GroupProvisioning = "provisioning"
	GroupAction       = "action"
)

// NewRootCmd constructs the top-level portitor command. It is not runnable on
// its own; the caller (cmd/portitor/main.go) sets the version metadata, adds
// the subcommands, and calls Execute. SilenceUsage/SilenceErrors keep cobra
// from dumping usage or the error text on a failed run — main owns error
// reporting and the exit-code mapping, so a rejected push never prints command
// usage.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "portitor",
		Short: "A git gateway that verifies the result of a push and mediates GitHub actions.",
		Long: `portitor is a git gateway: it verifies the result of a push (the pre-receive
gate), forwards accepted feature branches upstream and auto-opens PRs (the
post-receive hook), and mediates a narrow, role-validated set of GitHub actions
over SSH (the pr action API). Per-repo configuration is loaded from the JSON
file named by PORTITOR_CONFIG.

See README.md and spec/ for the full model.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Do not add cobra's default `completion` command — it is not part of
	// portitor's command surface. The `help` command is kept: it is exactly the
	// universal help the toolchain wants (`portitor help <cmd>`).
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddGroup(
		&cobra.Group{ID: GroupGate, Title: "Gate (git hooks):"},
		&cobra.Group{ID: GroupProvisioning, Title: "Provisioning (operator):"},
		&cobra.Group{ID: GroupAction, Title: "Action channel (over SSH):"},
	)
	return root
}
