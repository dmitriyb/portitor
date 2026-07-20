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
	case "upgrade-repo":
		os.Exit(upgradeRepo(os.Args[2:]))
	case "reconcile":
		os.Exit(reconcile(os.Args[2:]))
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
	case "-h", "--help", "help":
		usageTo(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "portitor: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() { usageTo(os.Stderr) }

func usageTo(w io.Writer) {
	fmt.Fprint(w, `portitor — a git gateway that verifies the result of a push and mediates GitHub actions.

usage: portitor <command> [flags]

gate (git hooks):
  pre-receive             run the gate over an incoming push (accept/reject)
  post-receive            forward accepted feature branches upstream + auto-open PRs

provisioning (operator):
  init-repo   --bare <path> [--default <b>] [--upstream <url>] [--config <json>]
  add-repo    --repo <name> [--default <b>] [--upstream <url>]
  upgrade-repo --repo <name> | --bare <path>    re-bake hook shims to the current version
  add-role    --repo <name> --role <role> --fingerprint SHA256:… [--pub <file>]
  reconcile   --repo <name>    re-forward accepted branches after a forward failure
  validate-config [--config <path>]    fail fast on a missing/invalid config

action channel (over SSH):
  shell <fingerprint>     forced command: dispatch to git-pack or the pr API
  pr <comment|review|merge|close|fetch> --repo <name> --pr <n> [--event …]

See README.md and spec/ for the full model.
`)
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
	// Config first so a malformed-stdin rejection is auditable (parity with
	// preReceive); a config that cannot load is the one unauditable failure.
	s, err := config.Load()
	if err != nil {
		fmt.Fprintf(w, "portitor: %v\n", err)
		return 1
	}
	updates, err := parseUpdates(r)
	if err != nil {
		if aerr := audit.Append(s.AuditLog, audit.Event{Kind: "forward", Repo: repoName(),
			Fingerprint: os.Getenv("PORTITOR_FINGERPRINT"), Verdict: "error", Reason: err.Error()}); aerr != nil {
			fmt.Fprintf(w, "portitor: audit: %v\n", aerr)
		}
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
		switch res.Status {
		case gate.StatusForwarded, gate.StatusAlreadyUpstream:
			if res.Status == gate.StatusAlreadyUpstream {
				fmt.Fprintf(w, "portitor: %s already on upstream (out-of-order forward)\n", res.Ref)
			} else {
				fmt.Fprintf(w, "portitor: forwarded %s -> upstream\n", res.Ref)
			}
			auditEvent(w, audit.Event{Kind: "forward", Refs: []string{res.Ref}, Verdict: "allow", Reason: string(res.Status)})
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
		case gate.StatusFailed:
			fmt.Fprintf(w, "portitor: forward %s -> upstream FAILED: %v (recover with: portitor reconcile --repo <name>)\n", res.Ref, res.Err)
			auditEvent(w, audit.Event{Kind: "forward", Refs: []string{res.Ref}, Verdict: "error", Reason: res.Err.Error()})
			rc = 1
		default: // skipped-default / skipped-non-branch / skipped-deletion
			fmt.Fprintf(w, "portitor: %s not forwarded (%s)\n", res.Ref, res.Status)
			auditEvent(w, audit.Event{Kind: "forward", Refs: []string{res.Ref}, Verdict: "skip", Reason: string(res.Status)})
		}
	}
	return rc
}

// reconcile re-forwards a repo's accepted feature branches that never reached
// upstream (an upstream-forward failure cannot be re-triggered by a re-push,
// since pre-receive accepts an already-present tip with zero new commits). It
// is idempotent — a branch already upstream is a no-op — and re-attempts the
// auto-open PR for each reconciled branch.
func reconcile(args []string) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository name (selects the per-repo config)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *repo == "" {
		fmt.Fprintln(os.Stderr, "reconcile: --repo <name> required")
		return 2
	}
	s, err := config.Resolve(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: %v\n", err)
		return 1
	}
	bare := filepath.Join(config.ReposRoot(), *repo+".git")
	def := s.DefaultBranch
	auditEvent := func(e audit.Event) {
		e.Repo = *repo
		if aerr := audit.Append(s.AuditLog, e); aerr != nil {
			fmt.Fprintf(os.Stderr, "reconcile: audit: %v\n", aerr)
		}
	}
	results, err := gate.Reconcile(bare, gate.ForwardConfig{UpstreamRemote: s.UpstreamRemote, DefaultBranch: def})
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: %v\n", err)
		return 1
	}
	gh := ghClientFor(bare, s)
	rc := 0
	for _, res := range results {
		switch res.Status {
		case gate.StatusForwarded:
			fmt.Printf("reconcile: forwarded %s -> upstream\n", res.Ref)
			auditEvent(audit.Event{Kind: "forward", Refs: []string{res.Ref}, Verdict: "allow", Reason: "reconcile"})
			n, url, err := autoOpenPR(bare, def, res.Ref, gh)
			ev := audit.Event{Kind: "auto-pr", Refs: []string{res.Ref}, PR: n}
			if err != nil {
				fmt.Fprintf(os.Stderr, "reconcile: PR for %s: %v\n", res.Ref, err)
				ev.Verdict, ev.Reason = "error", err.Error()
			} else {
				if n > 0 {
					fmt.Printf("reconcile: PR #%d %s\n", n, url)
				}
				ev.Verdict = "allow"
			}
			auditEvent(ev)
		case gate.StatusAlreadyUpstream:
			fmt.Printf("reconcile: %s already upstream\n", res.Ref)
			auditEvent(audit.Event{Kind: "forward", Refs: []string{res.Ref}, Verdict: "skip", Reason: "already-upstream"})
		case gate.StatusFailed:
			fmt.Fprintf(os.Stderr, "reconcile: %s FAILED: %v\n", res.Ref, res.Err)
			auditEvent(audit.Event{Kind: "forward", Refs: []string{res.Ref}, Verdict: "error", Reason: res.Err.Error()})
			rc = 1
		}
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
	return ghClientFor(repoDir(), s)
}

// ghClientFor builds the action client for a specific repo dir, deriving the
// slug from the upstream remote URL when not set explicitly. A derived slug is
// validated (owner/name shape) — a malformed value must never reach `gh -R`.
func ghClientFor(dir string, s config.Settings) action.GH {
	slug := s.UpstreamSlug
	if slug == "" {
		remote := upstreamRemote(s)
		if git.ValidRemoteName(remote) {
			if url, err := git.Output(dir, "remote", "get-url", remote); err == nil {
				slug = deriveSlug(strings.TrimSpace(url))
			}
		}
	}
	if slug != "" && !validSlug(slug) {
		slug = "" // an unusable slug is no slug — callers already handle Repo=="".
	}
	return action.GH{Repo: slug}
}

// validSlug reports whether s is a well-formed owner/name GitHub slug: exactly
// two non-empty path segments of safe characters, no leading '-'.
func validSlug(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.HasPrefix(p, "-") {
			return false
		}
		for _, r := range p {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
				r == '_', r == '-', r == '.':
			default:
				return false
			}
		}
	}
	return true
}

func upstreamRemote(s config.Settings) string {
	if s.UpstreamRemote != "" {
		return s.UpstreamRemote
	}
	return "upstream"
}

// deriveSlug extracts owner/name from a github remote URL (ssh or https forms).
// It returns "" for anything that does not yield a well-formed owner/name slug,
// so a malformed remote URL can never reach `gh -R` as a bogus target.
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
	if len(parts) < 2 {
		return ""
	}
	slug := parts[len(parts)-2] + "/" + parts[len(parts)-1]
	if !validSlug(slug) {
		return ""
	}
	return slug
}

// ---- init-repo ----

func initRepo(args []string) int {
	fs := flag.NewFlagSet("init-repo", flag.ContinueOnError)
	bare := fs.String("bare", "", "path to the bare repo to create (required)")
	def := fs.String("default", "main", "default branch")
	upstream := fs.String("upstream", "", "upstream URL to mirror and forward to (optional)")
	configPath := fs.String("config", "", "portitor config JSON for this repo (default: the registry, <repos.d>/<name>.json)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *bare == "" {
		fmt.Fprintln(os.Stderr, "init-repo: --bare is required")
		return 2
	}
	cfg := *configPath
	if cfg == "" {
		// The registry is the single canonical config identity: the same file
		// the gate hooks, add-role, and the pr API read. A divergent default
		// here is how role grants used to silently miss the gate.
		name := strings.TrimSuffix(filepath.Base(*bare), ".git")
		if !config.ValidName(name) {
			fmt.Fprintf(os.Stderr, "init-repo: cannot derive a registry config name from bare path %q; pass --config explicitly\n", *bare)
			return 2
		}
		cfg = filepath.Join(config.ReposDir(), name+".json")
	}

	if err := git.Run("", "init", "--bare", "--initial-branch="+*def, *bare); err != nil {
		fmt.Fprintf(os.Stderr, "init-repo: %v\n", err)
		return 1
	}

	if *upstream != "" {
		// Provisioning must fail loudly, not bake an unverified repo: a
		// swallowed remote-add / seed error leaves a gate that cannot forward.
		if err := git.Run(*bare, "remote", "add", "upstream", *upstream); err != nil {
			fmt.Fprintf(os.Stderr, "init-repo: add upstream remote: %v\n", err)
			return 1
		}
		if err := git.OutputNetworkRun(*bare, "fetch", "upstream"); err != nil {
			fmt.Fprintf(os.Stderr, "init-repo: fetch upstream: %v\n", err)
			return 1
		}
		// Seed the default branch from upstream only if upstream has it (a fresh
		// empty upstream legitimately does not).
		hasDefault, err := refExists(*bare, "refs/remotes/upstream/"+*def)
		if err != nil {
			fmt.Fprintf(os.Stderr, "init-repo: check upstream default branch: %v\n", err)
			return 1
		}
		if hasDefault {
			if err := git.Run(*bare, "update-ref", "refs/heads/"+*def, "refs/remotes/upstream/"+*def); err != nil {
				fmt.Fprintf(os.Stderr, "init-repo: seed default branch: %v\n", err)
				return 1
			}
		}
	}

	if err := bakeHooks(*bare, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "init-repo: %v\n", err)
		return 1
	}

	// Never bake a KNOWN-BAD config silently: if the config is present, it must
	// load and validate (else the gate would reject every later push with no
	// cause surfaced here). An absent config is a loud warning, not an error —
	// a bootstrap may place it next — but the gate will not work until it does.
	if _, err := os.Stat(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "init-repo: warning: config %s is not present yet — place it (add-role/add-repo) before pushing; the gate will reject until then\n", cfg)
	} else if s, err := config.LoadFile(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "init-repo: config %s: %v\n", cfg, err)
		return 1
	} else if problems := config.Validate(s); len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "init-repo: config %s is invalid:\n", cfg)
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
		return 1
	}

	fmt.Printf("portitor: initialized bare repo %s (default=%s, config=%s%s)\n",
		*bare, *def, cfg, upstreamNote(*upstream))
	return 0
}

// hookShimVersion stamps the baked hook shims. It is a frozen compatibility
// surface: bumped only when the shim contract (env vars, subcommand names) or
// the frozen subcommand names change, so upgrade-repo can detect a stale bake.
const hookShimVersion = 1

// hookMarker prefixes the version line every baked shim carries.
const hookMarker = "# portitor-hook-version: "

// bakeHooks writes the pre-receive/post-receive shims atomically (a partial
// write must never leave an executable stub that exits 0 and accepts every
// push). Each shim carries a version marker for upgrade-repo.
func bakeHooks(bare, cfg string) error {
	hooksDir := filepath.Join(bare, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("hooks dir: %w", err)
	}
	for _, hook := range []string{"pre-receive", "post-receive"} {
		// PORTITOR_BIN lets the container image point the shim at an absolute
		// binary path (deploy/entrypoint.sh); it is env-controlled but safe —
		// sshd does not forward client env by default (no AcceptEnv), so a
		// pushing agent cannot set it.
		shim := fmt.Sprintf("#!/bin/sh\n%s%d\nexport PORTITOR_CONFIG=%s\nexec \"${PORTITOR_BIN:-portitor}\" %s\n",
			hookMarker, hookShimVersion, shellQuote(cfg), hook)
		if err := atomicWrite(filepath.Join(hooksDir, hook), []byte(shim), 0o755); err != nil {
			return fmt.Errorf("write %s hook: %w", hook, err)
		}
	}
	return nil
}

// upgradeRepo re-bakes a provisioned repo's hook shims to the current version
// (idempotent), preserving the config path already baked in. It is the
// re-provisioning path when the CLI's shim contract evolves.
func upgradeRepo(args []string) int {
	fs := flag.NewFlagSet("upgrade-repo", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository name (uses the registry paths)")
	bareFlag := fs.String("bare", "", "explicit bare repo path (overrides --repo)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bare := *bareFlag
	if bare == "" {
		if !config.ValidName(*repo) {
			fmt.Fprintln(os.Stderr, "upgrade-repo: --repo <name> or --bare <path> required")
			return 2
		}
		bare = filepath.Join(config.ReposRoot(), *repo+".git")
	}
	cfg, ok := bakedHookConfig(bare)
	if !ok {
		fmt.Fprintf(os.Stderr, "upgrade-repo: %s has no baked pre-receive hook to read the config path from; re-run init-repo\n", bare)
		return 1
	}
	if err := bakeHooks(bare, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "upgrade-repo: %v\n", err)
		return 1
	}
	fmt.Printf("upgrade-repo: re-baked hooks for %s (version %d, config=%s)\n", bare, hookShimVersion, cfg)
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

// refExists reports whether ref resolves to a commit in the repo. Exit status 1
// means "absent"; any other failure (e.g. a timeout) is a real error — it must
// not silently read as "absent" and skip seeding.
func refExists(dir, ref string) (bool, error) {
	_, err := git.Output(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
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
