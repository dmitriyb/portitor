# Change Proposal: Committer-email allowlist in the push gate

## Context

The gate's signature check is purely key-based: `%G? == G` against
`allowed_signers`, fingerprint → role. The committer *email* is never
inspected, so a push whose commits carry an arbitrary email (a box-side
misconfiguration, or a synthetic placeholder) lands cleanly — and only later
surfaces as a problem on the upstream forge, where signature verification is
tied to a registered account email (GitHub shows such commits as Unverified,
`reason: no_user`). The gate is the repo's policy chokepoint; an email policy
belongs here, not in every client.

## Proposed change

A new optional per-repo config field `allowed_committer_emails` (list of
strings). When non-empty, every commit a push introduces must have a committer
email from the list; a commit that doesn't is a violation with rule id
`committer-email-not-allowed`, collected alongside the other violations
(independent of the signature verdict, so one correction pass fixes both).
Empty or absent ⇒ the check is skipped (existing deployments unaffected).
The field is additive: no `format_version` bump — the strict decode already
makes an older binary reject a config that carries it, which is the intended
fail-closed behavior on mixed versions.

Mechanism, not policy: portitor validates list entries only for non-emptiness
and never interprets addresses; which emails verify on which forge is the
operator's business.

## Impact expectation

- **gate**: `Config` gains `allowed_committer_emails`; `commitSig` also
  returns `%ce`; a new per-commit rule in `Check`. Spec leaves
  `arch_pre_receive.md`, `test_pre_receive.md`, `module.json` (seed
  requirement), `arch_config.md` (validate-config list).
- **config**: `topLevelKeys` + `Validate` (entries non-empty).
- No CLI change: the field is seeded by the operator's provisioning tooling
  (e.g. jq), like the other policy fields.
