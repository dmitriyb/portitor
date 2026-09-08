# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

portitor is a self-hosted git gateway between an untrusted agent and an upstream forge. A `pre-receive` hook verifies the result of a push (signed by a trusted key, branch namespace only, never the default branch, role-gated content rules); a `post-receive` hook forwards accepted branches upstream with portitor's own credential and opens the PR; `portitor pr` is a role-gated action API; `portitor-mcp` mediates MCP tool calls over the same SSH channel.

portitor is **mechanism, not policy**: identity is a signer-key fingerprint mapped to a role by config, and every domain name (roles, protected paths, record fields, the record-extraction command) arrives as configuration. Its only runtime dependency is git.

## Module Hierarchy

| Module | Purpose | Depends On |
|--------|---------|------------|
| cli | Cobra root command, version, universal help, subcommand registration for every entrypoint | — |
| gate | Push verification in `pre-receive`: signature and role checks, content rules, forwarding, reconcile, config loading, audit | cli |
| action | GitHub mediation: auto-PR on forward, the `portitor shell` dispatcher, the role-gated `pr` action API with re-derived merge preconditions | cli, gate |
| mcp | `portitor-mcp`: shape-validated MCP forwarding, pinned tool allowlist, per-role tool access, upstream credentials held only by the mediator | cli, action |
| delivery | Release pipeline: signed binaries from a tag, `install.sh`, `portitor upgrade`; the container image is a local build | cli |

Go packages live under `internal/` (`gate`, `rules`, `check`, `config`, `git`, `action`, `audit`, `mcpconfig`, `mcpschema`, `mcpwire`) with the two binaries in `cmd/portitor` and `cmd/portitor-mcp`.

## Spec Convention (spexmachina)

The spec is a typed graph managed by [spexmachina](https://github.com/dmitriyb/spexmachina):

- `spec/<module>/module.json`: module requirements, components, impl and test sections
- `spec/<module>/{arch,impl,flow,test}_*.md`: content leaves referenced from `module.json`
- `spec/proposals/YYYY-MM-DD-*.md`: every change starts with a proposal; `spec/reviews/` holds review records

Validate with `spex validate`; a change session must end with `spex diff --json` reporting `errors: []`.

## Technical Constraints

- **Go standard library first**: the only external dependency is `cobra` (see `go.mod`)
- **Results, not commands**: the gate inspects the objects a push lands, never the command that produced them; verdicts are hermetic (ambient git config masked, `allowed_signers` pinned from config)
- **Fail closed**: an internal error rejects the push; a strict-decode config error refuses to boot
- **Config is the single source of gate-integrity fields**: `default_branch`, `allowed_signers`, roles and rules come only from `repos.d/<repo>.json`; env overrides touch operational fields only
- **One credential holder**: the agent never holds a GitHub token or an MCP credential; `gh` arguments are built by portitor, never forwarded

## Build & Test

- Build: `go build ./...`
- Test: `go test ./...`
- Vet: `go vet ./...`

## Git Conventions

- Default branch is `main` (never `master`)
- Always `git fetch origin` before creating a new branch
- Always branch from `origin/main`, not from the current branch
- Commits must be SSH-signed; never bypass signing

## Organizational Constraints

- **Spec traceability**: all code must trace back to spec requirements; tests verify requirements, not just coverage
- **Three enforcement tiers, named as such**: result verification (gate), request mediation with re-derived authority (`pr`), shape-validated forwarding (MCP); never document a surface as a stronger tier than it delivers
- **Mechanism/policy split**: any PR that teaches portitor a domain word (a role name, a path, a record field, a tracker) is wrong by construction; find the config seam instead
