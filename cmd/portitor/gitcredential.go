package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/dmitriyb/portitor/internal/credential"
)

// gitCredential is the git credential helper the gate configures for
// https://github.com (`portitor git-credential <get|store|erase>`): it answers
// `get` from the gate's credential file, so network git authenticates without
// any stored login. Any other host gets no answer and git falls through.
func gitCredential(args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(errw, "portitor git-credential: usage: git-credential <get|store|erase>")
		return 2
	}
	req := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			req[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(errw, "portitor git-credential: read request: %v\n", err)
		return 1
	}
	switch args[0] {
	case "store", "erase":
		return 0
	case "get":
	default:
		fmt.Fprintf(errw, "portitor git-credential: unknown operation %q\n", args[0])
		return 2
	}
	if req["protocol"] != "https" || req["host"] != "github.com" {
		return 0
	}
	tok, present, err := credential.Read()
	if err != nil {
		fmt.Fprintf(errw, "portitor git-credential: %v\n", err)
		return 1
	}
	if !present {
		fmt.Fprintf(errw, "portitor git-credential: no credential file at %s\n", credential.Path)
		return 1
	}
	fmt.Fprintf(out, "username=x-access-token\npassword=%s\n", tok)
	return 0
}
