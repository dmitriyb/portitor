package main

import (
	"fmt"
	"io"
	"net"
	"os"

	"github.com/dmitriyb/portitor/internal/mcpconfig"
	"github.com/dmitriyb/portitor/internal/mcpwire"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	var cfgPath, listen string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP mediator daemon",
		Long: `Listen on the splice target and mediate MCP sessions: one session per
connection (the git-side dispatcher splices one SSH connection each), admitted
by the asserted caller fingerprint against the config's roles, speaking the
closed MCP subset, forwarding shape-validated tool calls to session-scoped
upstream servers.

Boot is fail-closed: an invalid config, or a configured server env name unset
in this process's environment, refuses to start.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(serveRun(cfgPath, listen, cmd.ErrOrStderr()))
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", os.Getenv("PORTITOR_MCP_CONFIG"), "mediator config JSON (default: $PORTITOR_MCP_CONFIG)")
	cmd.Flags().StringVar(&listen, "listen", os.Getenv("PORTITOR_MCP_LISTEN"), "listen target: unix:<path> or tcp:<host>:<port> (default: $PORTITOR_MCP_LISTEN)")
	return cmd
}

// serveRun boots the mediator: load + validate the config (fail-closed, the
// gate's posture), check every configured credential env name is present (a
// missing credential fails loudly at boot, not at first tool call), listen,
// and hand each accepted connection to a session.
func serveRun(cfgPath, listen string, errw io.Writer) int {
	if cfgPath == "" {
		fmt.Fprintln(errw, "portitor-mcp serve: no config path (set $PORTITOR_MCP_CONFIG or pass --config)")
		return 2
	}
	if listen == "" {
		fmt.Fprintln(errw, "portitor-mcp serve: no listen target (set $PORTITOR_MCP_LISTEN or pass --listen)")
		return 2
	}
	s, err := mcpconfig.LoadFile(cfgPath)
	if err != nil {
		fmt.Fprintf(errw, "portitor-mcp serve: %v\n", err)
		return 1
	}
	if problems := mcpconfig.Validate(s); len(problems) > 0 {
		fmt.Fprintf(errw, "portitor-mcp serve: config %s is INVALID:\n", cfgPath)
		for _, p := range problems {
			fmt.Fprintf(errw, "  - %s\n", p)
		}
		return 1
	}
	if problems := missingServerEnv(s); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(errw, "portitor-mcp serve: %s\n", p)
		}
		return 1
	}
	ln, err := listenTarget(listen)
	if err != nil {
		fmt.Fprintf(errw, "portitor-mcp serve: %v\n", err)
		return 1
	}
	defer ln.Close()
	fmt.Fprintf(errw, "portitor-mcp serve: listening on %s (%d tool(s), %d server(s))\n", listen, len(s.Tools), len(s.Servers))
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(errw, "portitor-mcp serve: accept: %v\n", err)
			return 1
		}
		go func() {
			defer conn.Close()
			runSession(s, conn, spawnServer, errw)
		}()
	}
}

// missingServerEnv lists every configured server env name unset in the
// mediator's environment (empty = all present).
func missingServerEnv(s mcpconfig.Settings) []string {
	var problems []string
	for name, srv := range s.Servers {
		for _, e := range srv.Env {
			if _, ok := os.LookupEnv(e); !ok {
				problems = append(problems, fmt.Sprintf("servers[%q]: env %q is not set in the mediator's environment (refusing to boot without the credential)", name, e))
			}
		}
	}
	return problems
}

// listenTarget listens on the shared splice-target grammar. A stale unix
// socket file left by an unclean shutdown is removed first — but only a
// socket: anything else at that path is an operator error to surface, never
// something to delete.
func listenTarget(target string) (net.Listener, error) {
	network, addr, err := mcpwire.ParseTarget(target)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if fi, serr := os.Stat(addr); serr == nil {
			if fi.Mode()&os.ModeSocket == 0 {
				return nil, fmt.Errorf("listen %s: %s exists and is not a socket", target, addr)
			}
			if rerr := os.Remove(addr); rerr != nil {
				return nil, fmt.Errorf("listen %s: remove stale socket: %w", target, rerr)
			}
		}
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", target, err)
	}
	return ln, nil
}
