package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/dmitriyb/portitor/internal/mcpwire"
)

// spliceMCP connects the SSH connection's stdio to the separately running
// portitor-mcp mediator. This process interprets none of the MCP traffic —
// privilege separation is the point (spec/mcp/arch_mcp.md).
func spliceMCP(fp string) int {
	return spliceMCPTo(fp, os.Getenv("PORTITOR_MCP_TARGET"), os.Stdin, os.Stdout, os.Stderr)
}

// spliceMCPTo dials the deploy-configured target, asserts the SSH-verified
// caller fingerprint in the one-line header, then copies bytes both ways
// until either side ends (client EOF half-closes toward the mediator; the
// mediator's EOF ends the splice).
//
// The git-side binary tolerates a missing mediator: an unset or undialable
// target refuses cleanly, and nothing else is affected.
func spliceMCPTo(fp, target string, in io.Reader, out, errw io.Writer) int {
	if target == "" {
		fmt.Fprintln(errw, "portitor: mcp mediation is not configured on this deployment (PORTITOR_MCP_TARGET is unset)")
		return 1
	}
	network, addr, err := mcpwire.ParseTarget(target)
	if err != nil {
		fmt.Fprintf(errw, "portitor: mcp target: %v\n", err)
		return 1
	}
	conn, err := net.Dial(network, addr)
	if err != nil {
		fmt.Fprintf(errw, "portitor: mcp mediator unavailable: %v\n", err)
		return 1
	}
	defer conn.Close()
	hdr, err := json.Marshal(mcpwire.Header{Fingerprint: fp})
	if err != nil {
		fmt.Fprintf(errw, "portitor: mcp header: %v\n", err)
		return 1
	}
	if _, err := conn.Write(append(hdr, '\n')); err != nil {
		fmt.Fprintf(errw, "portitor: mcp header: %v\n", err)
		return 1
	}
	go func() {
		_, _ = io.Copy(conn, in)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	if _, err := io.Copy(out, conn); err != nil {
		fmt.Fprintf(errw, "portitor: mcp splice: %v\n", err)
		return 1
	}
	return 0
}
