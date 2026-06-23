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
git -C <repo> -c gpg.ssh.allowedSignersFile=<allowed_signers> verify-commit <sha>
```

A non-zero exit (no signature, bad signature, or signer not in `allowed_signers`) is a violation.

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

## Boundaries

The gate is generic: it knows nothing of beads/spex. Role mapping (signer fingerprint → role),
forwarding (`post-receive`), ancestry, and content rules (e.g. `.beads/issues.jsonl` transitions)
build on this core in later slices; domain rules arrive as external config, not built-in.
