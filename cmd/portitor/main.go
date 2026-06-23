// Command portitor is the git-gateway binary. Git hooks invoke it by subcommand:
// a pre-receive hook runs `portitor pre-receive` (the gate), a post-receive hook
// runs `portitor post-receive` (forwarding accepted refs to the real upstream).
// `portitor init-repo` provisions a bare repo wired with those hooks.
//
// Per-repo configuration is loaded from the JSON file named by PORTITOR_CONFIG
// (set in the hook shim), with a few env overrides for operational fields.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dmitriyb/portitor/internal/gate"
)

// settings is portitor's per-repo configuration: the gate checks plus forwarding.
type settings struct {
	gate.Config
	UpstreamRemote string `json:"upstream_remote"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "pre-receive":
		os.Exit(preReceive(os.Stdin, os.Stderr))
	case "post-receive":
		os.Exit(postReceive(os.Stdin, os.Stderr))
	case "init-repo":
		os.Exit(initRepo(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "portitor: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: portitor <pre-receive|post-receive|init-repo>")
}

// loadSettings reads PORTITOR_CONFIG (JSON) if set, then applies env overrides for
// the operational fields a hook shim commonly sets per repo.
func loadSettings() (settings, error) {
	var s settings
	if path := os.Getenv("PORTITOR_CONFIG"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return s, fmt.Errorf("read PORTITOR_CONFIG: %w", err)
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return s, fmt.Errorf("parse PORTITOR_CONFIG %s: %w", path, err)
		}
	}
	if v := os.Getenv("PORTITOR_DEFAULT_BRANCH"); v != "" {
		s.DefaultBranch = v
	}
	if v := os.Getenv("PORTITOR_ALLOWED_SIGNERS"); v != "" {
		s.AllowedSigners = v
	}
	if v := os.Getenv("PORTITOR_UPSTREAM_REMOTE"); v != "" {
		s.UpstreamRemote = v
	}
	return s, nil
}

func repoDir() string {
	if d := os.Getenv("GIT_DIR"); d != "" {
		return d
	}
	return "."
}

// preReceive runs the gate; exit 0 accepts the push, non-zero rejects it.
func preReceive(r io.Reader, w io.Writer) int {
	updates, err := parseUpdates(r)
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	s, err := loadSettings()
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	vs, err := gate.Check(repoDir(), updates, s.Config)
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	if len(vs) == 0 {
		return 0
	}
	fmt.Fprintln(w, "portitor: push rejected")
	for _, v := range vs {
		fmt.Fprintf(w, "  [%s] %s: %s\n", v.Rule, v.Ref, v.Detail)
	}
	return 1
}

// postReceive forwards accepted feature refs to the real upstream.
func postReceive(r io.Reader, w io.Writer) int {
	updates, err := parseUpdates(r)
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	s, err := loadSettings()
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	results, err := gate.Forward(repoDir(), updates, gate.ForwardConfig{
		UpstreamRemote: s.UpstreamRemote,
		DefaultBranch:  s.DefaultBranch,
	})
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	rc := 0
	for _, res := range results {
		if res.Err != nil {
			fmt.Fprintf(w, "portitor: forward %s -> upstream FAILED: %v\n", res.Ref, res.Err)
			rc = 1
		} else {
			fmt.Fprintf(w, "portitor: forwarded %s -> upstream\n", res.Ref)
		}
	}
	return rc
}

// initRepo provisions a bare repo wired with portitor's pre/post-receive hooks.
func initRepo(args []string) int {
	fs := flag.NewFlagSet("init-repo", flag.ContinueOnError)
	bare := fs.String("bare", "", "path to the bare repo to create (required)")
	def := fs.String("default", "main", "default branch")
	upstream := fs.String("upstream", "", "upstream URL to mirror and forward to (optional)")
	configPath := fs.String("config", "", "portitor config JSON for this repo (default <bare>/portitor.json)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *bare == "" {
		fmt.Fprintln(os.Stderr, "init-repo: --bare is required")
		return 2
	}
	cfg := *configPath
	if cfg == "" {
		cfg = filepath.Join(*bare, "portitor.json")
	}

	if err := runGit("", "init", "--bare", "--initial-branch="+*def, *bare); err != nil {
		fmt.Fprintf(os.Stderr, "init-repo: %v\n", err)
		return 1
	}

	if *upstream != "" {
		// Mirror the upstream so agents clone the default branch from portitor and
		// never need an upstream credential. Tolerate an already-added remote.
		_ = runGit(*bare, "remote", "add", "upstream", *upstream)
		if err := runGit(*bare, "fetch", "upstream"); err != nil {
			fmt.Fprintf(os.Stderr, "init-repo: warning: fetch upstream: %v\n", err)
		}
		// Seed the default branch from upstream so clones work (ignore if absent).
		_ = runGit(*bare, "update-ref", "refs/heads/"+*def, "refs/remotes/upstream/"+*def)
	}

	for _, hook := range []string{"pre-receive", "post-receive"} {
		shim := fmt.Sprintf("#!/bin/sh\nexport PORTITOR_CONFIG=%s\nexec \"${PORTITOR_BIN:-portitor}\" %s\n", shellQuote(cfg), hook)
		p := filepath.Join(*bare, "hooks", hook)
		if err := os.WriteFile(p, []byte(shim), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "init-repo: write %s: %v\n", hook, err)
			return 1
		}
	}

	fmt.Printf("portitor: initialized bare repo %s (default=%s, config=%s%s)\n",
		*bare, *def, cfg, upstreamNote(*upstream))
	return 0
}

func upstreamNote(u string) string {
	if u == "" {
		return ""
	}
	return ", upstream=" + u
}

func runGit(dir string, args ...string) error {
	a := args
	if dir != "" {
		a = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", a...)
	var errb strings.Builder
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// shellQuote single-quotes s for safe embedding in a /bin/sh script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseUpdates reads hook stdin: one "<old> <new> <ref>" per line.
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
			return nil, fmt.Errorf("malformed hook line: %q", line)
		}
		us = append(us, gate.RefUpdate{OldSHA: f[0], NewSHA: f[1], Ref: f[2]})
	}
	return us, sc.Err()
}
