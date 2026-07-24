package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/dmitriyb/portitor/cli"
	"github.com/spf13/cobra"
)

// ---- exit-code plumbing ----

// exitError carries a process exit code out of a RunE. Its message is empty on
// purpose: the command has already written its own diagnostics, so the dispatch
// uses only the code. An Execute error that is NOT an *exitError is a cobra
// usage error (unknown subcommand, bad flag, wrong arg count) and maps to exit
// 2 — the hand-rolled dispatcher's usage-error code.
type exitError struct{ code int }

func (e *exitError) Error() string { return "" }
func (e *exitError) ExitCode() int { return e.code }

// exitErr wraps a command's integer exit code as a RunE error (nil for 0).
func exitErr(code int) error {
	if code == 0 {
		return nil
	}
	return &exitError{code: code}
}

// execSub runs one cobra command in isolation (as its own root) and returns the
// process exit code, mirroring the top-level dispatch in main: a command's own
// code passes through; any other (cobra usage) error is exit 2. It backs both
// the SSH forced-command `pr` route and the CLI unit-test entry points.
func execSub(cmd *cobra.Command, args []string) int {
	cmd.SetArgs(args)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	fmt.Fprintln(cmd.ErrOrStderr(), err)
	return 2
}

// ---- root assembly ----

// newRootCommand builds the full portitor command tree: the cobra root, its
// version metadata (backing --version/-v), and every subcommand, grouped as the
// hand-rolled usage text grouped them.
func newRootCommand() *cobra.Command {
	root := cli.NewRootCmd()
	root.Version = fmt.Sprintf("%s (commit %s, built %s)", version, commit, date)
	root.SetVersionTemplate("portitor {{.Version}}\n")
	root.AddCommand(
		newPreReceiveCmd(),
		newPostReceiveCmd(),
		newInitRepoCmd(),
		newAddRepoCmd(),
		newUpgradeRepoCmd(),
		newReconcileCmd(),
		newAddRoleCmd(),
		newValidateConfigCmd(),
		newUpgradeCmd(),
		newShellCmd(),
		newPrCmd(func() string { return os.Getenv("PORTITOR_FINGERPRINT") }),
		newVersionCmd(),
		newInternalCheckExecCmd(),
	)
	return root
}

// ---- gate (git hooks) ----

func newPreReceiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "pre-receive",
		Short:   "Run the gate over an incoming push (accept/reject)",
		Long:    "The pre-receive git hook. git invokes it with no arguments and pipes the ref\nupdates on stdin; the exit code is the accept (0) / reject (non-zero) verdict.",
		GroupID: cli.GroupGate,
		// git passes no arguments and pipes stdin — accept any args so cobra never
		// rejects the invocation, and read the verdict from stdin.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(preReceive(cmd.InOrStdin(), cmd.ErrOrStderr()))
		},
	}
}

func newPostReceiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "post-receive",
		Short:   "Forward accepted feature branches upstream + auto-open PRs",
		Long:    "The post-receive git hook. git invokes it with no arguments and pipes the ref\nupdates on stdin; it forwards accepted feature refs upstream and auto-opens a PR\nfor each.",
		GroupID: cli.GroupGate,
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(postReceive(cmd.InOrStdin(), cmd.ErrOrStderr()))
		},
	}
}

// ---- provisioning (operator) ----

func newInitRepoCmd() *cobra.Command {
	var bare, def, upstream, configPath string
	cmd := &cobra.Command{
		Use:     "init-repo",
		Short:   "Create a gated bare repo (+ optional upstream mirror)",
		GroupID: cli.GroupProvisioning,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(initRepoRun(bare, def, upstream, configPath))
		},
	}
	cmd.Flags().StringVar(&bare, "bare", "", "path to the bare repo to create (required)")
	cmd.Flags().StringVar(&def, "default", "main", "default branch")
	cmd.Flags().StringVar(&upstream, "upstream", "", "upstream URL to mirror and forward to (optional)")
	cmd.Flags().StringVar(&configPath, "config", "", "portitor config JSON for this repo (default: the registry, <repos.d>/<name>.json)")
	return cmd
}

func newAddRepoCmd() *cobra.Command {
	var name, def, upstream string
	cmd := &cobra.Command{
		Use:     "add-repo",
		Short:   "init-repo via the registry conventions",
		GroupID: cli.GroupProvisioning,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(addRepoRun(name, def, upstream))
		},
	}
	cmd.Flags().StringVar(&name, "repo", "", "repository name (required)")
	cmd.Flags().StringVar(&def, "default", "main", "default branch")
	cmd.Flags().StringVar(&upstream, "upstream", "", "upstream URL to mirror and forward to")
	return cmd
}

func newUpgradeRepoCmd() *cobra.Command {
	var repo, bareFlag string
	cmd := &cobra.Command{
		Use:     "upgrade-repo",
		Short:   "Re-bake hook shims to the current version",
		GroupID: cli.GroupProvisioning,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(upgradeRepoRun(repo, bareFlag))
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repository name (uses the registry paths)")
	cmd.Flags().StringVar(&bareFlag, "bare", "", "explicit bare repo path (overrides --repo)")
	return cmd
}

func newReconcileCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:     "reconcile",
		Short:   "Re-forward accepted branches after a forward failure",
		GroupID: cli.GroupProvisioning,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(reconcileRun(repo))
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repository name (selects the per-repo config)")
	return cmd
}

func newAddRoleCmd() *cobra.Command {
	var repo, role, fp, pub string
	cmd := &cobra.Command{
		Use:     "add-role",
		Short:   "Bind a fingerprint→role (+ optionally trust its signing key)",
		GroupID: cli.GroupProvisioning,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(addRoleRun(repo, role, fp, pub))
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repository name (selects repos.d/<name>.json)")
	cmd.Flags().StringVar(&role, "role", "", "role to bind the fingerprint to")
	cmd.Flags().StringVar(&fp, "fingerprint", "", "signer key fingerprint (SHA256:...)")
	cmd.Flags().StringVar(&pub, "pub", "", "OpenSSH public key file to trust for a signing role (optional)")
	return cmd
}

func newValidateConfigCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:     "validate-config",
		Short:   "Fail fast on a missing/invalid config",
		GroupID: cli.GroupProvisioning,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(validateConfigRun(cfgPath))
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", os.Getenv("PORTITOR_CONFIG"), "repo config JSON (default: $PORTITOR_CONFIG)")
	return cmd
}

func newUpgradeCmd() *cobra.Command {
	var o upgradeOptions
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update the installed portitor binary forward to the latest signed release",
		Long: `Update the standalone portitor binary in place to the latest signed release.
Upgrade is forward-only: it resolves the latest release and moves toward it.

It reuses the same signed install.sh the README documents — embedded in this
binary, run against the path of the currently-running binary — so the update is
resolved, downloaded, and SSHSIG-verified by exactly the audited installer, then
swapped in safely (move-aside + rename, never a write over the running file),
keeping the displaced binary at <path>.bak.

If the resolved latest is OLDER than the installed version, upgrade hard-refuses
and no flag overrides it — a latest that moved backward is a rollback anomaly
(a compromised origin serving an old but validly-signed release as "latest"). To
install an older release deliberately, name it with --version, which installs
that exact release in any direction with no anomaly guard.

  portitor upgrade                 upgrade forward to the latest release
  portitor upgrade --check         report availability only; change nothing
  portitor upgrade --version vX.Y.Z install that exact release, any direction
  portitor upgrade --rollback      restore the pre-upgrade binary from <path>.bak

This updates the standalone operator binary only (the one also used for
add-role/validate-config/reconcile). The container image (gate + egress) is a
separate artifact rebuilt from the Dockerfile — upgrade does not touch it; see
docs/deploy.md.`,
		GroupID: cli.GroupProvisioning,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(upgradeRun(o, cmd.OutOrStdout(), cmd.ErrOrStderr()))
		},
	}
	cmd.Flags().BoolVar(&o.check, "check", false, "report the latest release and change nothing (warns, does not refuse, when latest is older than installed)")
	cmd.Flags().BoolVar(&o.check, "dry-run", false, "alias for --check")
	cmd.Flags().StringVar(&o.pinned, "version", "", "install a specific release (vX.Y.Z), any direction, instead of the forward-only latest")
	cmd.Flags().BoolVar(&o.rollback, "rollback", false, "restore the pre-upgrade binary from the .bak left by the last upgrade")
	return cmd
}

// ---- action channel (over SSH) ----

func newShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "shell <fingerprint>",
		Short:   "SSH forced command: dispatch to git-pack or the pr API",
		Long:    "The SSH forced command (command=\"portitor shell <fingerprint>\"). It reads\nSSH_ORIGINAL_COMMAND, classifies the git command, and routes the connection to\neither the git pack commands or the role-gated pr action API — rejecting\neverything else.",
		GroupID: cli.GroupAction,
		// The forced-command argv is operator-controlled (command="portitor shell
		// <fp>"); the connecting client's request arrives on SSH_ORIGINAL_COMMAND,
		// not argv. Accept any args so cobra never second-guesses the dispatch;
		// shellCommand does its own arg check and internal routing.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(shellCommand(args))
		},
	}
}

// newPrCmd builds the `pr` command. actorFingerprint supplies the actor
// identity: the CLI reads $PORTITOR_FINGERPRINT (baked into the forced command),
// while the SSH shell dispatcher injects the connection fingerprint directly
// (see shellCommand's pr route). The grammar lives here alone.
func newPrCmd(actorFingerprint func() string) *cobra.Command {
	var prNum int
	var event, repo string
	cmd := &cobra.Command{
		Use:     "pr <comment|review|merge|close|fetch>",
		Short:   "Run one role-validated GitHub action",
		GroupID: cli.GroupAction,
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitErr(prRun(actorFingerprint(), args, prNum, event, repo,
				cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()))
		},
	}
	cmd.Flags().IntVar(&prNum, "pr", 0, "PR number")
	cmd.Flags().StringVar(&event, "event", "", "review event: approve|request-changes|comment")
	cmd.Flags().StringVar(&repo, "repo", "", "repository name (selects the per-repo config)")
	return cmd
}

// ---- hidden: rlimit trampoline ----

func newInternalCheckExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "internal-check-exec",
		Hidden: true,
		// Not part of the CLI surface: internal/check re-execs `portitor
		// internal-check-exec <workdir> <argv...>` into itself, and the SSH shell
		// dispatcher cannot route here. DisableFlagParsing because <argv...> is the
		// operator-configured command and may carry arbitrary flags cobra must not
		// parse.
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// os.Exit directly (not exitErr): after the trampoline applies RLIMIT_AS
			// to itself, unwinding allocations back through cobra could abort under
			// the fresh limit — exactly why the hand-rolled main os.Exit'd here.
			os.Exit(internalCheckExec(args))
			return nil
		},
	}
}

// ---- entry points retained for unit tests and internal reuse ----
// Each runs its cobra subcommand in isolation and returns the exit code, so the
// behavior-locked operator tests keep driving `func([]string) int`.

func initRepo(args []string) int       { return execSub(newInitRepoCmd(), args) }
func upgradeRepo(args []string) int    { return execSub(newUpgradeRepoCmd(), args) }
func addRole(args []string) int        { return execSub(newAddRoleCmd(), args) }
func validateConfig(args []string) int { return execSub(newValidateConfigCmd(), args) }
