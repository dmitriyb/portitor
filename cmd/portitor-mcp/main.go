// Command portitor-mcp is the MCP mediator: the second portitor binary,
// running as its own process/user/container with its own credential set. The
// git-side dispatcher splices an SSH connection's stdio here (`portitor mcp`),
// and the mediator speaks a closed MCP stdio subset, forwarding shape-
// validated tool calls to operator-configured upstream servers whose
// credentials never leave this process's environment. It never holds the
// GitHub credential; the git side never holds MCP upstream tokens.
//
// This is portitor's third, weakest enforcement tier — shape-validated
// forwarding, never gate-grade or pr-grade authority. See spec/mcp/arch_mcp.md.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/dmitriyb/portitor/internal/mcpconfig"
	"github.com/spf13/cobra"
)

// exitError carries a process exit code out of a RunE, exactly as in
// cmd/portitor: the command has already written its own diagnostics; a
// non-exitError Execute error is a cobra usage error and maps to exit 2.
type exitError struct{ code int }

func (e *exitError) Error() string { return "" }

// exitErr wraps a command's integer exit code as a RunE error (nil for 0).
func exitErr(code int) error {
	if code == 0 {
		return nil
	}
	return &exitError{code: code}
}

func main() {
	root := newRootCommand()
	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "portitor-mcp",
		Short: "The MCP mediator: shape-validated tool-call forwarding behind portitor's SSH channel.",
		Long: `portitor-mcp mediates MCP for agents behind portitor: the SSH dispatcher
splices a connection here, and the mediator forwards only configured tools,
only schema-valid argument shapes, only for the caller's role — audited, with
upstream credentials never leaving the mediator. Configuration is one
strictly-decoded JSON file (--config / $PORTITOR_MCP_CONFIG).

See spec/mcp/arch_mcp.md for the full model.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.Version = fmt.Sprintf("%s (commit %s, built %s)", version, commit, date)
	root.SetVersionTemplate("portitor-mcp {{.Version}}\n")
	root.AddCommand(
		newServeCmd(),
		newValidateConfigCmd(),
		newVersionCmd(),
	)
	return root
}

func newValidateConfigCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "validate-config",
		Short: "Fail fast on a missing/invalid mediator config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(validateConfigRun(cfgPath))
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", os.Getenv("PORTITOR_MCP_CONFIG"), "mediator config JSON (default: $PORTITOR_MCP_CONFIG)")
	return cmd
}

// validateConfigRun checks the mediator config up front (at container boot /
// by an operator) so a missing or malformed config fails LOUDLY here instead
// of refusing every session later. Exit 0 = valid, non-zero = problems on
// stderr — the same posture as the gate's validate-config.
func validateConfigRun(cfgPath string) int {
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "validate-config: no config path (set $PORTITOR_MCP_CONFIG or pass --config)")
		return 2
	}
	s, err := mcpconfig.LoadFile(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate-config: %v\n", err)
		return 1
	}
	problems := mcpconfig.Validate(s)
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "validate-config: %s is INVALID:\n", cfgPath)
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
		return 1
	}
	fmt.Printf("validate-config: %s OK (%d role(s), %d server(s), %d tool(s))\n",
		cfgPath, len(s.Roles), len(s.Servers), len(s.Tools))
	return 0
}
