# Gate (pre-receive)

The verification core: the checks a git `pre-receive` runs against an incoming push. It inspects
the *result* (the objects and refs being landed), never a command, so enforcement holds regardless
of how the agent produced the push (git, plumbing, libgit2, REST).

## Responsibilities

- Parse ref updates (`<old> <new> <ref>`) from `pre-receive` stdin, **validating shape**: each
  line is `<old> <new> <ref>` where each SHA is exactly 40 hex chars or the all-zero id, and the
  ref is `refs/`-prefixed with no control bytes. A malformed line rejects the whole push (an
  input the gate cannot fully understand is never partially enforced).
- Determine the protected default branch (config, else the receiving repo's HEAD).
- For each update, evaluate the policy and collect **all** violations.
- Reject the whole push atomically (non-zero exit) with a complete, actionable `remote:` report;
  accept (exit 0) only when there are zero violations.
- Delegate cryptographic verification to git (`git verify-commit` + `allowed_signers`).

## Deterministic verification environment

The gate's verdict must be a function of the push, the repo, and the config — never of ambient
machine state. Three environment invariants hold for every git subprocess the gate runs:

- **Replace-object substitution is disabled** (`-c core.useReplaceRefs=false`, applied by the
  shared git wrapper to *every* portitor git call, gate and forwarder alike). Without this, a
  `refs/replace/*` ref landed in the repo would make every later `git show` / `rev-list` silently
  verify a *substituted* object in place of the pushed one.
- **Fact-gathering calls are hermetic**: the gate's git subprocesses run with the global and
  system git config masked out (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_SYSTEM=/dev/null`),
  so an ambient `~/.gitconfig` (e.g. one written by `gh auth setup-git`) can never contribute a
  trust root, a `gpg.ssh.program`, or any other verification input. The receiving repo's own
  config (operator territory) remains honored. Forwarding's `git push` is deliberately **not**
  config-masked — it legitimately needs the proxy's credential helpers — but it still runs with
  replace-objects disabled.
- **The trust root is pinned unconditionally**: `-c gpg.ssh.allowedSignersFile=<path>` is passed
  on every verification call *even when the configured path is empty*. An empty value makes git
  report every signature as untrusted (`%G? == U`, verified empirically), so an unconfigured
  `allowed_signers` deterministically rejects — it never falls back to an ambient
  `allowedSignersFile` from machine config.

Every subprocess (git, gh, ssh-keygen) runs under a deadline. A hung subprocess is an
operational error, and the gate's error direction is rejection — a push is never accepted
because a check could not complete.

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

### ref-namespace

Only branch refs are accepted: every update's ref must be `refs/heads/<name>` with a non-empty
name. Any other namespace — `refs/tags/*`, `refs/notes/*`, and especially `refs/replace/*`
(whose objects would substitute for others in later git reads) — is a violation named
`ref-namespace`, for creates, updates, and deletions alike. A ref the gate refuses gets no
further evaluation (there is nothing meaningful to sign- or role-check on a refused namespace);
the push as a whole is rejected. This allowlist is deliberately mechanism-level: portitor
mediates *branch landing*, and no supported flow pushes anything but branches.

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

run hermetically (see *Deterministic verification environment*): replace-objects disabled,
global/system config masked, and the `allowedSignersFile` pin passed **unconditionally** — an
empty configured path yields `%G? == U` for every commit, so a missing trust root fails closed.

`%G?` must be `G` (good signature from a trusted/allowed signer); anything else — `N` (none),
`B` (bad), `U` (good but signer not in `allowed_signers`), `E` (error) — is a violation. The same
call yields `%GF` (the signer key fingerprint, used for role mapping below) and `%GS` (the matched
principal). A *failure of the verification subprocess itself* (git exits non-zero, or the deadline
expires) is distinguished from a signature verdict: it surfaces as an operational error that
rejects the push, not as a synthetic "unsigned" violation.

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

Content protection is two rule families evaluated per introduced commit, after the signature
check (an untrusted commit is rejected before any content rule — its role can't be trusted):

- **structural rules** — file-level operations (add/modify/delete/rename) × path glob × role,
  computed from git name-status over the full, rename-aware commit diff;
- **semantic rules** — record-level transitions inside a protected structured file, extracted
  by an **operator-configured check command** (portitor never parses the format itself; any
  script, tool wrapper, or service client fills that seam interchangeably) and evaluated
  generically over record deltas.

Both share one `match/effect/default` model with a fixed precedence (first-match, then per-path
default, then global allow) and a **versioned rule schema** that fails closed on anything this
binary does not fully understand. The full design — schema, glob semantics, matcher vocabulary,
merge handling, the check-command contract and its isolation bounds — is specified in
`arch_content_rules.md`. Example (domain policy supplied externally, never hardcoded): *"moving
a record's `stage` field to `approved` requires an elevated role"* → a semantic rule matching
`stage → approved` with `roles {not_in: [reviewer, owner]} → deny`. The mechanism's only
external dependency is git; every domain name above comes from config.

The former textual `RoleRule` (regex over added diff lines) is **retired**; a config still
carrying `role_rules` fails validation loudly rather than having its rules silently dropped.

## Ancestry / fresh base

Opt-in via `Config.RequireUpToDateWithDefault`. For a non-default ref update, the new tip must be a
descendant of the current default branch (`git merge-base --is-ancestor <default> <new>`); otherwise
a `stale-base` violation forces a rebase before the work can land. The default branch is exempt, and
the check is skipped entirely when the flag is unset.

## Forwarding (`post-receive`)

After `pre-receive` accepts a push, `gate.Forward` (run by `portitor post-receive`) pushes each
accepted, **non-default, non-deletion** ref to the configured upstream remote (`Config.UpstreamRemote`,
default `upstream`) using credentials held only by the proxy — the agent never sees an upstream
credential. Only `refs/heads/*` refs are ever forwarded (the gate refuses other namespaces; the
forwarder independently skips them as defense in depth). The remote name is validated before use
(non-empty, no leading `-`, no whitespace/control bytes) so a malformed configured value can never
be read as a git option, and the refspec is built only from a validated 40-hex SHA and a
`refs/heads/`-prefixed ref. The default branch is never forwarded (it is PR/owner territory and
`pre-receive` rejects pushes to it).

**Every update yields a reported outcome — nothing is silently dropped.** Each result carries a
status:

- `forwarded` — pushed to upstream.
- `already-upstream` — the push was rejected but the upstream branch already **contains** the new
  tip (a later, containing push forwarded first — out-of-order forwarding). This is **success**:
  the content the update carried is upstream. Containment is proven locally (the remote tip is an
  ancestor-or-equal of the new tip, using the remote tip object that a received push left in the
  repo); if it cannot be proven, the failure stands (fail-closed).
- `skipped-default` — the ref is (now) the default branch. Reported explicitly so a default-branch
  change **between** pre- and post-receive cannot silently drop an accepted ref (the earlier
  fail-open where Forward exited 0 with no output).
- `skipped-non-branch` / `skipped-deletion` — a non-`refs/heads` ref or a deletion.
- `failed` — the push failed and upstream does not contain the tip; post-receive exits non-zero
  and points the operator at `portitor reconcile`.

## Recovery (`reconcile`)

An upstream-forward failure cannot be recovered by a re-push: pre-receive accepts an already-present
tip with zero new commits, so post-receive's forward never re-fires. `portitor reconcile --repo <name>`
re-forwards every local non-default branch that upstream does not already contain (idempotent — a
branch already upstream is a no-op) and re-attempts the auto-open PR for each. It reads the same
per-repo config and uses the same ancestor-aware forward.

## Provisioning (`init-repo`) and deployment

`portitor init-repo --bare <path> [--default <branch>] [--upstream <url>] [--config <json>]` creates
the gated bare repo, optionally adds and fetches the upstream remote and seeds the default branch
from it (so agents clone the default from the proxy with no upstream credential), and installs the
`pre-receive`/`post-receive` hook shims (each exports `PORTITOR_CONFIG` and execs the matching
subcommand). Without `--config` the path defaults to the **registry** —
`<ReposDir>/<name>.json`, the single canonical per-repo config identity every consumer reads
(see `arch_config.md`).

Provisioning is fail-loud, not best-effort: a remote-add / fetch / seed error aborts (never bakes a
gate that cannot forward); the default branch is seeded only when upstream actually has it. The hook
shims are written **atomically** (temp-then-rename) so a partial write can never leave an executable
stub that exits 0 and accepts every push, and each shim carries a **version marker**
(`# portitor-hook-version: N`) — a frozen compatibility surface. If the baked config is present it
must load and validate here (a known-bad config fails at provisioning, not silently at every later
push); an absent config is a loud warning (a bootstrap may place it next).

`portitor upgrade-repo --repo <name>` (or `--bare <path>`) re-bakes the hook shims to the current
version idempotently, reading the config path from the existing shim — the re-provisioning path
when the CLI's shim contract evolves. The hook subcommand names (`pre-receive`, `post-receive`) are
a frozen compatibility surface: renaming one would strand every already-provisioned repo. The proxy ships as **its own container** (multi-stage `Dockerfile` → minimal Alpine,
key-only git-over-SSH as user `git`, repos on the `/srv/git` volume; `deploy/entrypoint.sh` runs
sshd under tini) — separate from the agent image, which holds no upstream credential.

Once a repo is provisioned, `portitor add-role` fills in its role map re-runnably — binding a signer
fingerprint to a role (and optionally trusting a signing role's public key in `allowed_signers`)
without re-provisioning. See `arch_add_role.md`.

## Boundaries

The gate is generic: it knows nothing of any domain format or third-party tool; domain rules
(content rules, the role map, the record-extraction command) are external config, and the
mechanism's only external dependency is git. GitHub actions (PR open/comment/review/merge/close + read-side fetch) are handled
by the **action** module over the same SSH channel — NOT via intent artifacts, and never as a `gh`
passthrough (see `spec/action`).
