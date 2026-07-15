# Gate (pre-receive)

The verification core: the checks a git `pre-receive` runs against an incoming push. It inspects
the *result* (the objects and refs being landed), never a command, so enforcement holds regardless
of how the agent produced the push (git, plumbing, libgit2, REST).

## Responsibilities

- Parse ref updates (`<old> <new> <ref>`) from `pre-receive` stdin.
- Determine the protected default branch (config, else the receiving repo's HEAD).
- For each update, evaluate the policy and collect **all** violations.
- Reject the whole push atomically (non-zero exit) with a complete, actionable `remote:` report;
  accept (exit 0) only when there are zero violations.
- Delegate cryptographic verification to git (`git verify-commit` + `allowed_signers`).

## Model

```
type RefUpdate struct { OldSHA, NewSHA, Ref string }   // one stdin line
type Config    struct { DefaultBranch, AllowedSigners string }
type Violation struct { Ref, Rule, Detail string }     // Rule = stable id; Detail = human-facing

Check(repoDir string, updates []RefUpdate, cfg Config) ([]Violation, error)
```

`Check` is a pure function of its inputs (it only shells out to `git` for facts), which makes the
gate testable in isolation with ephemeral keys and a throwaway bare repo.

## Rules (initial)

### no-push-to-default

If an update targets `refs/heads/<default>` it is rejected — whether a push or a deletion. The
default branch is `Config.DefaultBranch`, or derived from `git symbolic-ref --short HEAD` on the
receiving repo.

### unsigned-or-untrusted-commit

Every commit an update *introduces* must carry a good signature from a signer listed in
`allowed_signers`. New commits are enumerated with `git rev-list <old>..<new>` (or
`git rev-list <new> --not --all` for a branch creation, so pre-existing history isn't re-checked).
Each is verified with:

```
git -C <repo> -c gpg.ssh.allowedSignersFile=<allowed_signers> show -s --format=%G?%n%GF%n%GS <sha>
```

`%G?` must be `G` (good signature from a trusted/allowed signer); anything else — `N` (none),
`B` (bad), `U` (good but signer not in `allowed_signers`), `E` (error) — is a violation. The same
call yields `%GF` (the signer key fingerprint, used for role mapping below) and `%GS` (the matched
principal).

## Atomicity & reporting

`pre-receive` runs once per push over git's object quarantine: if the hook exits non-zero, *nothing*
is migrated. portitor therefore collects every violation across all refs/commits and prints them
together, e.g.:

```
remote: portitor: push rejected
remote:   [no-push-to-default] refs/heads/main: push to the default branch "main" is not allowed — use a feature branch and open a PR
remote:   [unsigned-or-untrusted-commit] refs/heads/feature: commit a1b2c3d4e5f6 is not signed by an allowed signer (no valid signature)
```

The agent reads these and fixes everything in one correction pass.

## Role-gated content rules

Identity is a **credential, not a label**: each commit's signer key fingerprint (`%GF`) maps to a
**role** via `Config.Roles` (fingerprint → role). A container holding only one role's key cannot
sign as another role, so the role is unforgeable.

A `RoleRule` then gates *content* by role, generically (no beads/spex knowledge built in):

```
RoleRule{ Name, PathGlob, AddedRegex, AllowedRoles }
```

For each introduced commit, if its diff to `PathGlob` adds a line matching `AddedRegex`, the
signer's role must be in `AllowedRoles` — else a violation named `Name`. Example config (a beads
rule supplied externally, not hardcoded): *"only reviewer/owner may close a bead"* →
`{PathGlob: ".beads/issues.jsonl", AddedRegex: "\"status\"\\s*:\\s*\"closed\"", AllowedRoles: ["reviewer","owner"]}`.
An untrusted commit (`%G? != G`) is rejected before any role rule (its role can't be trusted).

## Ancestry / fresh base

Opt-in via `Config.RequireUpToDateWithDefault`. For a non-default ref update, the new tip must be a
descendant of the current default branch (`git merge-base --is-ancestor <default> <new>`); otherwise
a `stale-base` violation forces a rebase before the work can land. The default branch is exempt, and
the check is skipped entirely when the flag is unset.

## Forwarding (`post-receive`)

After `pre-receive` accepts a push, `gate.Forward` (run by `portitor post-receive`) pushes each
accepted, **non-default, non-deletion** ref to the configured upstream remote (`Config.UpstreamRemote`,
default `upstream`) using credentials held only by the proxy — the agent never sees an upstream
credential. The default branch is never forwarded (it is PR/owner territory and `pre-receive` rejects
pushes to it). Each ref's outcome is reported; a failed forward yields a non-zero exit.

## Provisioning (`init-repo`) and deployment

`portitor init-repo --bare <path> [--default <branch>] [--upstream <url>] [--config <json>]` creates
the gated bare repo, optionally adds and fetches the upstream remote and seeds the default branch
from it (so agents clone the default from the proxy with no upstream credential), and installs the
`pre-receive`/`post-receive` hook shims (each exports `PORTITOR_CONFIG` and execs the matching
subcommand). The proxy ships as **its own container** (multi-stage `Dockerfile` → minimal Alpine,
key-only git-over-SSH as user `git`, repos on the `/srv/git` volume; `deploy/entrypoint.sh` runs
sshd under tini) — separate from the agent image, which holds no upstream credential.

Once a repo is provisioned, `portitor add-role` fills in its role map re-runnably — binding a signer
fingerprint to a role (and optionally trusting a signing role's public key in `allowed_signers`)
without re-provisioning. See `arch_add_role.md`.

## Boundaries

The gate is generic: it knows nothing of beads/spex; domain rules (role rules, the role map) are
external config. GitHub actions (PR open/comment/review/merge/close + read-side fetch) are handled
by the **action** module over the same SSH channel — NOT via intent artifacts, and never as a `gh`
passthrough (see `spec/action`).
