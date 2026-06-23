// Command portitor is the git-gateway binary. Git hooks invoke it by subcommand,
// e.g. a pre-receive hook that runs `portitor pre-receive`.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dmitriyb/portitor/internal/gate"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: portitor <subcommand>   (subcommands: pre-receive)")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "pre-receive":
		os.Exit(preReceive(os.Stdin, os.Stderr))
	default:
		fmt.Fprintf(os.Stderr, "portitor: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

// preReceive reads ref updates from r, runs the gate, and reports. Returns the
// process exit code: 0 accepts the push, non-zero rejects it.
func preReceive(r io.Reader, w io.Writer) int {
	updates, err := parseUpdates(r)
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	cfg := gate.Config{
		DefaultBranch:  os.Getenv("PORTITOR_DEFAULT_BRANCH"),
		AllowedSigners: os.Getenv("PORTITOR_ALLOWED_SIGNERS"),
	}
	repoDir := os.Getenv("GIT_DIR")
	if repoDir == "" {
		repoDir = "."
	}
	vs, err := gate.Check(repoDir, updates, cfg)
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	if len(vs) == 0 {
		return 0
	}
	// Reject atomically, reporting every violation so it can be fixed in one pass.
	fmt.Fprintln(w, "portitor: push rejected")
	for _, v := range vs {
		fmt.Fprintf(w, "  [%s] %s: %s\n", v.Rule, v.Ref, v.Detail)
	}
	return 1
}

// parseUpdates reads pre-receive stdin: one "<old> <new> <ref>" per line.
func parseUpdates(r io.Reader) ([]gate.RefUpdate, error) {
	var us []gate.RefUpdate
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("malformed pre-receive line: %q", line)
		}
		us = append(us, gate.RefUpdate{OldSHA: f[0], NewSHA: f[1], Ref: f[2]})
	}
	return us, sc.Err()
}
