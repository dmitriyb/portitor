// Command portitor is the git-gateway binary. Git hooks invoke it by subcommand:
// `pre-receive` (the gate), `post-receive` (forward accepted refs + auto-open a
// PR). `init-repo` provisions a bare repo. `shell` is the SSH forced command
// that dispatches a connection to either the git pack commands or the role-gated
// `pr` action API; `pr` performs a single validated GitHub action.
//
// Per-repo configuration is loaded from the JSON file named by PORTITOR_CONFIG
// (set in the hook shim / authorized_keys command), with env overrides.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dmitriyb/portitor/internal/action"
	"github.com/dmitriyb/portitor/internal/audit"
	"github.com/dmitriyb/portitor/internal/config"
	"github.com/dmitriyb/portitor/internal/gate"
	"github.com/dmitriyb/portitor/internal/git"
	"github.com/dmitriyb/portitor/internal/rules"
)

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
	case "shell":
		os.Exit(shellCommand(os.Args[2:]))
	case "add-repo":
		os.Exit(addRepo(os.Args[2:]))
	case "add-role":
		os.Exit(addRole(os.Args[2:]))
	case "validate-config":
		os.Exit(validateConfig(os.Args[2:]))
	case "pr":
		os.Exit(prCommand(os.Getenv("PORTITOR_FINGERPRINT"), os.Args[2:]))
	case "internal-check-exec":
		// Not part of the CLI surface: the rlimit trampoline internal/check
		// re-execs through. The SSH shell dispatcher cannot route here.
		os.Exit(internalCheckExec(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "portitor: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: portitor <pre-receive|post-receive|init-repo|add-repo|add-role|validate-config|shell|pr>")
}

// validateConfig checks a repo config up front (at container boot / by an operator)
// so a missing or malformed config fails LOUDLY here instead of silently rejecting
// every push later (an empty config makes the gate distrust all commits). Exit 0 =
// valid, non-zero = problems printed to stderr.
func validateConfig(args []string) int {
	fs := flag.NewFlagSet("validate-config", flag.ContinueOnError)
	cfgPath := fs.String("config", os.Getenv("PORTITOR_CONFIG"), "repo config JSON (default: $PORTITOR_CONFIG)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "validate-config: no config path (set $PORTITOR_CONFIG or pass --config)")
		return 2
	}
	s, err := config.LoadFile(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate-config: %v\n", err)
		return 1
	}
	problems := config.Validate(s)
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "validate-config: %s is INVALID:\n", *cfgPath)
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
		return 1
	}
	fmt.Printf("validate-config: %s OK (%d role(s)%s)\n", *cfgPath, len(s.Roles), contentSummary(s.Content))
	return 0
}

// contentSummary renders a short content-rules count for the OK line.
func contentSummary(cr *rules.ContentRules) string {
	if cr == nil {
		return ", no content rules"
	}
	structural := 0
	if cr.Structural != nil {
		structural = len(cr.Structural.Rules)
	}
	semantic := 0
	if cr.Semantic != nil {
		for _, f := range cr.Semantic.Files {
			semantic += len(f.Rules)
		}
	}
	return fmt.Sprintf(", %d structural + %d semantic rule(s)", structural, semantic)
}

func repoDir() string {
	if d := os.Getenv("GIT_DIR"); d != "" {
		return d
	}
	return "."
}

// preReceive runs the gate; exit 0 accepts the push, non-zero rejects it.
// Every decision (accept, reject, operational error — malformed stdin
// included) lands in the audit trail; an audit write failure never changes the
// verdict — it is reported loudly. The one inherently unauditable failure is a
// config that cannot be loaded (no audit path is known then).
func preReceive(r io.Reader, w io.Writer) int {
	s, err := config.Load()
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	updates, err := parseUpdates(r)
	if err != nil {
		auditGate(w, s, nil, nil, err)
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	vs, err := gate.Check(repoDir(), updates, s.Config)
	auditGate(w, s, updates, vs, err)
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

// auditGate records one gate decision. The pusher fingerprint comes from the
// environment the shell dispatcher exports; direct hook invocations without it
// simply omit the attribution.
func auditGate(w io.Writer, s config.Settings, updates []gate.RefUpdate, vs []gate.Violation, checkErr error) {
	refs := make([]string, 0, len(updates))
	for _, u := range updates {
		refs = append(refs, u.Ref)
	}
	fp := os.Getenv("PORTITOR_FINGERPRINT")
	e := audit.Event{
		Kind:        "gate",
		Repo:        repoName(),
		Fingerprint: fp,
		Role:        s.Roles[fp],
		Refs:        refs,
	}
	switch {
	case checkErr != nil:
		e.Verdict = "error"
		e.Reason = checkErr.Error()
	case len(vs) > 0:
		e.Verdict = "reject"
		parts := make([]string, 0, len(vs))
		for _, v := range vs {
			parts = append(parts, fmt.Sprintf("[%s] %s: %s", v.Rule, v.Ref, v.Detail))
		}
		e.Reason = strings.Join(parts, "; ")
	default:
		e.Verdict = "accept"
	}
	if err := audit.Append(s.AuditLog, e); err != nil {
		fmt.Fprintf(w, "portitor: audit: %v\n", err)
	}
}

// repoName derives the repo's name from the receiving repo dir (best-effort,
// audit attribution only).
func repoName() string {
	dir := repoDir()
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return strings.TrimSuffix(filepath.Base(dir), ".git")
}

// postReceive forwards accepted feature refs upstream, then auto-opens a PR for
// each (idempotent) and prints the receipt.
func postReceive(r io.Reader, w io.Writer) int {
	updates, err := parseUpdates(r)
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	s, err := config.Load()
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	def := s.DefaultBranch
	results, err := gate.Forward(repoDir(), updates, gate.ForwardConfig{
		UpstreamRemote: s.UpstreamRemote,
		DefaultBranch:  def,
	})
	fp := os.Getenv("PORTITOR_FINGERPRINT")
	auditEvent := func(w io.Writer, e audit.Event) {
		e.Repo = repoName()
		e.Fingerprint = fp
		if aerr := audit.Append(s.AuditLog, e); aerr != nil {
			fmt.Fprintf(w, "portitor: audit: %v\n", aerr)
		}
	}
	if err != nil {
		auditEvent(w, audit.Event{Kind: "forward", Verdict: "error", Reason: err.Error()})
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	rc := 0
	gh := ghClient(s)
	for _, res := range results {
		if res.Err != nil {
			fmt.Fprintf(w, "portitor: forward %s -> upstream FAILED: %v\n", res.Ref, res.Err)
			auditEvent(w, audit.Event{Kind: "forward", Refs: []string{res.Ref}, Verdict: "error", Reason: res.Err.Error()})
			rc = 1
			continue
		}
		fmt.Fprintf(w, "portitor: forwarded %s -> upstream\n", res.Ref)
		auditEvent(w, audit.Event{Kind: "forward", Refs: []string{res.Ref}, Verdict: "allow"})
		n, url, err := autoOpenPR(repoDir(), def, res.Ref, gh)
		ev := audit.Event{Kind: "auto-pr", Refs: []string{res.Ref}, PR: n}
		if err != nil {
			fmt.Fprintf(w, "portitor: PR for %s: %v\n", res.Ref, err)
			ev.Verdict = "error"
			ev.Reason = err.Error()
		} else {
			if n > 0 {
				fmt.Fprintf(w, "portitor: PR #%d %s\n", n, url)
			}
			ev.Verdict = "allow"
		}
		auditEvent(w, ev)
	}
	return rc
}

// autoOpenPR opens a PR for a forwarded feature ref (title from the tip commit,
// body from the branch's commit messages). Idempotent: if a PR already exists
// for the branch (e.g. a self-correction re-push), it returns that number.
func autoOpenPR(dir, def, ref string, gh action.GH) (int, string, error) {
	if gh.Repo == "" {
		return 0, "", errors.New("no upstream slug configured (upstream_slug)")
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if def == "" {
		d, err := defaultBranchName(dir)
		if err != nil {
			return 0, "", err
		}
		def = d
	}
	if n, err := gh.OpenPRNumber(branch); err != nil {
		return 0, "", err
	} else if n > 0 {
		return n, fmt.Sprintf("(existing PR #%d)", n), nil
	}
	title, err := git.Output(dir, "log", "-1", "--format=%s", branch)
	if err != nil {
		return 0, "", err
	}
	body, err := git.Output(dir, "log", "--reverse", "--format=%B", def+".."+branch)
	if err != nil {
		return 0, "", err
	}
	n, url, err := gh.OpenPR(branch, def, strings.TrimSpace(title), strings.TrimSpace(body))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists") {
		// Check-then-act race against live GitHub: another forward created the
		// PR between our query and the create. That is the idempotent-success
		// case, not an error — re-derive the number.
		if n2, err2 := gh.OpenPRNumber(branch); err2 == nil && n2 > 0 {
			return n2, fmt.Sprintf("(existing PR #%d)", n2), nil
		}
	}
	return n, url, err
}

// ---- pr action handler (role-validated) ----
// The action verbs are a closed mechanism set (action.Verbs); WHO may perform
// each is the per-repo config's action_roles map, default-deny. Every decision
// is appended to the audit trail.

func prCommand(fp string, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "portitor pr: action required (comment|review|merge|close|fetch)")
		return 2
	}
	act := args[0]
	fs := flag.NewFlagSet("pr", flag.ContinueOnError)
	pr := fs.Int("pr", 0, "PR number")
	event := fs.String("event", "", "review event: approve|request-changes|comment")
	repo := fs.String("repo", "", "repository name (selects the per-repo config)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if !action.KnownVerb(act) {
		fmt.Fprintf(os.Stderr, "portitor pr: unknown action %q\n", act)
		return 2
	}
	if *repo == "" {
		fmt.Fprintln(os.Stderr, "portitor pr: --repo <name> required")
		return 2
	}
	if *pr <= 0 {
		fmt.Fprintln(os.Stderr, "portitor pr: --pr <number> required")
		return 2
	}
	// Per-repo config: the role map + upstream slug come from THIS repo's config,
	// so one portitor mediates many repos. The role is re-derived from the signer
	// fingerprint against that repo's roles (a credential, not a passed-in label).
	s, err := config.Resolve(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "portitor pr: %v\n", err)
		return 1
	}
	role := s.Roles[fp]

	auditDecision := func(verdict, reason string) {
		e := audit.Event{Kind: "action", Repo: *repo, Fingerprint: fp, Role: role,
			Action: act, PR: *pr, Verdict: verdict, Reason: reason}
		if aerr := audit.Append(s.AuditLog, e); aerr != nil {
			fmt.Fprintf(os.Stderr, "portitor pr: audit: %v\n", aerr)
		}
	}
	deny := func(reason string) int {
		auditDecision("deny", reason)
		fmt.Fprintf(os.Stderr, "portitor pr: %s\n", reason)
		return 1
	}
	fail := func(err error) int {
		auditDecision("error", err.Error())
		fmt.Fprintf(os.Stderr, "portitor pr: %v\n", err)
		return 1
	}

	if role == "" {
		return deny(fmt.Sprintf("key has no role for repo %q", *repo))
	}
	if !action.RoleCan(s.ActionRoles, role, act) {
		return deny(fmt.Sprintf("role %q may not %q (action_roles is default-deny)", role, act))
	}
	gh := ghClient(s)
	if gh.Repo == "" {
		return fail(fmt.Errorf("no upstream slug configured for repo %q", *repo))
	}
	switch act {
	case "fetch":
		out, err := gh.Fetch(*pr)
		if err != nil {
			return fail(err)
		}
		fmt.Print(out)
	case "comment":
		if err := gh.Comment(*pr, readBody()); err != nil {
			return fail(err)
		}
	case "review":
		if *event == "approve" {
			// Separation of duties: an approval must not come from a key that
			// signed the PR's own commits.
			signed, err := requesterSignedPR(s, *repo, fp, *pr, gh)
			if err != nil {
				return fail(fmt.Errorf("separation-of-duties check: %w", err))
			}
			if signed {
				return deny(fmt.Sprintf("separation of duties: key %s signed commits this PR introduces; it may not approve", fp))
			}
		}
		if err := gh.Review(*pr, *event, readBody()); err != nil {
			return fail(err)
		}
	case "merge":
		st, err := gh.FetchMergeState(*pr)
		if err != nil {
			return fail(err)
		}
		unmet := action.UnmetMergePreconditions(st, s.RequiredChecks)
		signed, err := requesterSignedHead(s, *repo, fp, st.HeadRefName)
		if err != nil {
			return fail(fmt.Errorf("separation-of-duties check: %w", err))
		}
		if signed {
			unmet = append(unmet, fmt.Sprintf("separation of duties: key %s signed commits this PR introduces", fp))
		}
		if len(unmet) > 0 {
			return deny(fmt.Sprintf("PR #%d does not meet the merge preconditions: %s", *pr, strings.Join(unmet, "; ")))
		}
		// The final gh merge is the atomic gate: GitHub re-checks, so a state
		// change since the query fails there (TOCTOU closed GitHub-side).
		if err := gh.Merge(*pr); err != nil {
			return fail(err)
		}
	case "close":
		if err := gh.ClosePR(*pr); err != nil {
			return fail(err)
		}
	}
	auditDecision("allow", "")
	return 0
}

// requesterSignedPR resolves the PR's head ref, then defers to
// requesterSignedHead.
func requesterSignedPR(s config.Settings, repo, fp string, pr int, gh action.GH) (bool, error) {
	st, err := gh.FetchMergeState(pr)
	if err != nil {
		return false, err
	}
	return requesterSignedHead(s, repo, fp, st.HeadRefName)
}

// requesterSignedHead reports whether the requesting key signed any commit the
// PR introduces (default..head), verified against the LOCAL gated repo with
// the gate's own hermetic verification. Fail-closed: a head ref portitor does
// not have locally, or any verification failure, is an error the caller
// refuses on.
func requesterSignedHead(s config.Settings, repo, fp, headRef string) (bool, error) {
	if headRef == "" {
		return false, errors.New("PR head ref is empty")
	}
	tip := "refs/heads/" + headRef
	if !gate.ValidRef(tip) {
		return false, fmt.Errorf("implausible PR head ref %q", headRef)
	}
	bare := filepath.Join(config.ReposRoot(), repo+".git")
	def := s.DefaultBranch
	if def == "" {
		d, err := defaultBranchName(bare)
		if err != nil {
			return false, err
		}
		def = d
	}
	fps, err := gate.SignerFingerprints(bare, "refs/heads/"+def, tip, s.AllowedSigners)
	if err != nil {
		return false, err
	}
	return fps[fp], nil
}

// readBody reads the action body (comment/review text) from stdin, so multi-line
// markdown survives transport intact (never squeezed through SSH command args).
func readBody() string {
	b, _ := io.ReadAll(os.Stdin)
	return strings.TrimRight(string(b), "\n")
}

// ---- shell: the SSH forced command ----

// shellCommand is the forced command on an agent's authorized_keys entry
// (`command="portitor shell <fingerprint>"`). It dispatches the SSH connection
// to either the git pack commands (which run the pre/post-receive gate) or the
// role-gated pr action API — and rejects everything else, so one key grants
// gated git + the narrow action API and nothing more.
func shellCommand(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "portitor shell: fingerprint required")
		return 2
	}
	fp := args[0]
	orig := os.Getenv("SSH_ORIGINAL_COMMAND")
	kind, rest, err := classify(orig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "portitor: %v\n", err)
		return 1
	}
	switch kind {
	case "git":
		// rest = [git-receive-pack|git-upload-pack, <repo-path>]
		if !allowedRepoPath(rest[1]) {
			fmt.Fprintln(os.Stderr, "portitor: repository not allowed")
			return 1
		}
		cmd := exec.Command(rest[0], rest[1])
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		// The hooks inherit the caller's key fingerprint so the audit trail
		// can attribute the push to the connecting identity.
		cmd.Env = append(os.Environ(), "PORTITOR_FINGERPRINT="+fp)
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			fmt.Fprintf(os.Stderr, "portitor: %v\n", err)
			return 1
		}
		return 0
	case "pr":
		// Pass the fingerprint through; prCommand re-derives the role from the
		// target repo's config (per-repo role map).
		return prCommand(fp, rest) // rest = pr args after "portitor pr"
	default:
		fmt.Fprintln(os.Stderr, "portitor: command not allowed")
		return 1
	}
}

// classify decides what an SSH_ORIGINAL_COMMAND is allowed to be. Pure (no I/O)
// so the security-critical routing is unit-testable.
//   - git-receive-pack/upload-pack '<path>'  -> ("git", [cmd, path])
//   - portitor pr <action> ...               -> ("pr", [action, ...])
//   - anything else                          -> ("reject", nil)
func classify(orig string) (string, []string, error) {
	if strings.TrimSpace(orig) == "" {
		return "reject", nil, errors.New("interactive shell not permitted")
	}
	toks, err := shellSplit(orig)
	if err != nil {
		return "reject", nil, err
	}
	if len(toks) == 0 {
		return "reject", nil, errors.New("empty command")
	}
	switch toks[0] {
	// A closed table: exactly the two pack commands. git-upload-archive is
	// deliberately absent — no supported flow needs archives.
	case "git-receive-pack", "git-upload-pack":
		if len(toks) != 2 {
			return "reject", nil, errors.New("malformed git command")
		}
		return "git", []string{toks[0], toks[1]}, nil
	case "portitor":
		if len(toks) >= 2 && toks[1] == "pr" {
			return "pr", toks[2:], nil
		}
		return "reject", nil, errors.New("only `portitor pr` is allowed")
	}
	return "reject", nil, fmt.Errorf("command %q not allowed", toks[0])
}

// allowedRepoPath confines pack commands to the repo root (default /srv/git),
// requiring a *.git path with no traversal.
func allowedRepoPath(p string) bool {
	root := os.Getenv("PORTITOR_REPO_ROOT")
	if root == "" {
		root = "/srv/git"
	}
	clean := filepath.Clean(p)
	if strings.Contains(p, "..") || !strings.HasSuffix(clean, ".git") {
		return false
	}
	rel, err := filepath.Rel(root, clean)
	return err == nil && !strings.HasPrefix(rel, "..")
}

// shellSplit is a minimal POSIX-ish tokenizer (handles single/double quotes),
// enough for git's `git-receive-pack '<path>'` and our own pr args.
func shellSplit(s string) ([]string, error) {
	var toks []string
	var cur strings.Builder
	inTok := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inTok = true
		case r == ' ' || r == '\t':
			if inTok {
				toks = append(toks, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inTok {
		toks = append(toks, cur.String())
	}
	return toks, nil
}

// ghClient builds the action client for the configured upstream slug, deriving
// it from the upstream remote URL if not set explicitly.
func ghClient(s config.Settings) action.GH {
	slug := s.UpstreamSlug
	if slug == "" {
		remote := upstreamRemote(s)
		if git.ValidRemoteName(remote) {
			if url, err := git.Output(repoDir(), "remote", "get-url", remote); err == nil {
				slug = deriveSlug(strings.TrimSpace(url))
			}
		}
	}
	return action.GH{Repo: slug}
}

func upstreamRemote(s config.Settings) string {
	if s.UpstreamRemote != "" {
		return s.UpstreamRemote
	}
	return "upstream"
}

// deriveSlug extracts owner/name from a github remote URL (ssh or https forms).
func deriveSlug(url string) string {
	url = strings.TrimSuffix(url, ".git")
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:] // drop scheme -> host/owner/name
	}
	// scp-like user@host:owner/name -> drop the "user@host:" prefix
	if i := strings.Index(url, ":"); i >= 0 && !strings.Contains(url[:i], "/") {
		url = url[i+1:]
	}
	parts := strings.Split(url, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return ""
}

// ---- init-repo ----

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

	if err := git.Run("", "init", "--bare", "--initial-branch="+*def, *bare); err != nil {
		fmt.Fprintf(os.Stderr, "init-repo: %v\n", err)
		return 1
	}

	if *upstream != "" {
		_ = git.Run(*bare, "remote", "add", "upstream", *upstream)
		if err := git.Run(*bare, "fetch", "upstream"); err != nil {
			fmt.Fprintf(os.Stderr, "init-repo: warning: fetch upstream: %v\n", err)
		}
		_ = git.Run(*bare, "update-ref", "refs/heads/"+*def, "refs/remotes/upstream/"+*def)
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

// addRepo provisions a managed repo using the registry conventions: bare at
// <repos-root>/<name>.git and per-repo config at <repos-dir>/<name>.json (placed
// there first). It is init-repo with the paths derived from the repo name.
func addRepo(args []string) int {
	fs := flag.NewFlagSet("add-repo", flag.ContinueOnError)
	name := fs.String("repo", "", "repository name (required)")
	def := fs.String("default", "main", "default branch")
	upstream := fs.String("upstream", "", "upstream URL to mirror and forward to")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "add-repo: --repo is required")
		return 2
	}
	cfg := filepath.Join(config.ReposDir(), *name+".json")
	if _, err := os.Stat(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "add-repo: place the repo config at %s first\n", cfg)
		return 1
	}
	bare := filepath.Join(config.ReposRoot(), *name+".git")
	return initRepo([]string{"--bare", bare, "--default", *def, "--upstream", *upstream, "--config", cfg})
}

func upstreamNote(u string) string {
	if u == "" {
		return ""
	}
	return ", upstream=" + u
}

// ---- git helpers ----

func defaultBranchName(dir string) (string, error) {
	out, err := git.Output(dir, "symbolic-ref", "--short", "HEAD")
	return strings.TrimSpace(out), err
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

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
		u := gate.RefUpdate{OldSHA: f[0], NewSHA: f[1], Ref: f[2]}
		// Fail-closed shape check before anything reaches git argv: SHAs must be
		// 40-hex-or-zero, the ref refs/-prefixed with no control bytes.
		if err := u.Validate(); err != nil {
			return nil, fmt.Errorf("malformed hook line %q: %w", line, err)
		}
		us = append(us, u)
	}
	return us, sc.Err()
}
