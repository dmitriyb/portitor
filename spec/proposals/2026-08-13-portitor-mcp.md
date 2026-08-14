# Change Proposal: portitor-mcp — shape-validated MCP mediation over the same SSH channel

## Context

The agent box has no route to the network except SSH to portitor; portitor is
the only component holding a GitHub credential. MCP breaks that invariant from
the outside: every MCP server an agent needs today means a token inside the
agent container plus a per-host egress hole — exactly the credential sprawl
portitor exists to prevent. The realistic alternative to mediation is not "no
MCP"; it is tokens in the agent overlay and holes in the egress lock.

The goal is to extend the existing invariant — *the only upstream credential
lives behind the gate* — from GitHub to every MCP upstream, over the same
single SSH key the agent already holds, without adding a second identity or a
second network path.

## Design decisions (settled)

- **The guarantee tier is named, and it is not the `pr` API's.** portitor has
  two enforcement tiers today: result verification (the git gate — a verdict is
  a function of verified objects) and request mediation with re-derived
  authority (the `pr` API — the verb is tied to state the gate checks itself).
  MCP calls are imperative JSON-RPC; there is no artifact to verify and no
  managed state to tie a tool call to. This surface is a third, weaker tier:
  **shape-validated forwarding** — only these tools, only these argument
  shapes, only this role, audited, credentials never leave the mediator. That
  is the egress allowlist's guarantee class lifted from host granularity to
  tool-call granularity, and it must be documented as exactly that — never
  presented as gate-grade or `pr`-grade authority.
- **Shapes, not semantics.** "Read-only" is not a protocol property;
  `annotations.readOnlyHint` is declared by the upstream server and is
  untrusted. portitor validates the operator's per-tool predicates and claims
  nothing about what a tool *means* — consistent with the existing doctrine
  that portitor is generic mechanism and ships no domain names. The operator
  who writes the predicates carries the burden of knowing the tool's JSON
  shape; that maintenance *is* the security review, made explicit.
- **Allowlist schema, strict decode, canonical forward.** A denylist predicate
  ("must not contain field X") is bypassable by anything the matcher did not
  anticipate. Params must match the declared schema exactly: unknown fields
  refused, strict decoding, and the request re-serialized canonically so the
  bytes reaching the upstream are the bytes the predicate saw — never a second
  parse with different tolerances. Everything not explicitly allowed is
  refused.
- **The tool list is pinned in config.** The exposed tool set and each tool's
  expected schema live in portitor's config; the upstream's advertisement is
  checked against it, and a mismatch refuses the tool rather than forwarding
  it. Upstream schema drift therefore fails closed — a visible refusal fixed
  in config, not a silent hole. `tools/list_changed` and all other
  server-initiated flows (sampling, elicitation) are not forwarded.
- **Same channel, third route.** `classify()` gains one row: `portitor mcp`.
  The table stays closed; identity stays the SSH key fingerprint mapped to a
  role, as everywhere else. No new key, no new endpoint.
- **Splice, not exec — privilege separation.** On an `mcp` connection the
  dispatcher does not exec the MCP code; it splices the connection's stdio to
  a separately running `portitor-mcp` process. The mediator parses
  attacker-influenced JSON and supervises third-party server processes — it
  never holds the GitHub credential, and the git side never holds MCP upstream
  tokens. Different process, different user/container, different credential
  set.
- **Separate binary, same module, coupled release.** `cmd/portitor-mcp` is a
  second build target sharing the internal packages. One tag ships both
  artifacts at one version through the existing pipeline and signing key:
  the shared config schema makes lockstep the correct semantics — one version
  number states "this pair was tested together". Per-component cadences are a
  later problem, if ever.
- **Protocol subset, hand-rolled.** The mediator implements the server side of
  the MCP stdio transport for exactly `initialize`, `tools/list` (served from
  config, pinned), `tools/call`, and `ping`; every other method is refused.
  The subset is small enough to own, and owning it preserves the "only
  runtime dependency is git" property rather than adopting an SDK whose full
  surface we would then have to audit and refuse piecemeal.
- **Upstreams are session-scoped stdio children.** v1 upstream servers are
  operator-configured argv commands (no shell, ever) spawned by `portitor-mcp`
  per session, with credentials injected from the mediator's own environment
  or files. Lifetime is the SSH connection — the existing process-per-
  connection model, unchanged. HTTP transports and OAuth token management are
  explicitly out of scope for v1.
- **MCP config is its own file in its own trust domain.** The surface is not
  per-repo, and the mediator is a separate privilege domain, so its config
  does not live in `repos.d/`. `portitor-mcp` reads one strictly-decoded
  config (servers, tools, roles, tool→role grants) and validates it at boot
  with the same fail-closed posture as the gate.
- **Egress stays infrastructure.** The `--internal` network and the agent-side
  proxy are declarative deploy config, owned outside this repo. No Go grows
  on that leg.

## Proposed change

### Dispatch: the third route

`portitor shell` classifies `portitor mcp` and splices the connection's stdio
to the `portitor-mcp` process (unix socket or a `dca-net` port — a deploy
choice, not a protocol one), exporting the caller's fingerprint alongside. The
closed-table property and its fuzz invariants extend to the new row.

### `portitor-mcp`: the mediator

A second binary. Per connection it:

1. Resolves the caller's role from its own config (fingerprint → role);
   unknown fingerprints are refused before any protocol exchange.
2. Speaks the MCP stdio subset; `tools/list` returns exactly the tools the
   caller's role is granted — the config's view, never an upstream's.
3. On `tools/call`: default-deny role check, strict schema validation of
   `params` (unknown fields refused), canonical re-serialization, forward to
   the owning upstream, relay the result. Every call is audited (role, tool,
   verdict, reason) through the existing audit trail mechanism.
4. Spawns the owning upstream server on first use in the session (argv from
   config, credentials from the mediator's environment), reaps it at session
   end.

### Config schema: the mediator's file

```jsonc
{
  "roles": { "SHA256:…": "implementer" },      // fingerprint → role, as in repos.d
  "servers": {
    "tracker": {
      "command": ["tracker-mcp-server"],       // argv, no shell
      "env": ["TRACKER_TOKEN"]                 // names passed through from the mediator's env
    }
  },
  "tools": {
    "tracker.search": {
      "server": "tracker",
      "upstream_name": "search",               // the name the upstream advertises
      "params": { /* allowlist schema: fields, types, enums; unknown = refuse */ }
    }
  },
  "tool_roles": {                              // default-deny, the action_roles shape
    "tracker.search": ["implementer", "reviewer"]
  }
}
```

The `params` matcher vocabulary is deliberately minimal (required/optional
fields, primitive types, enum/const constraints, no unknown fields — ever);
its exact grammar is specified in the arch spec, not here. A tool absent from
`tool_roles`, or listed with no roles, is refused for everyone.

## Impact expectation

- **cmd/portitor**: `classify()` gains the `mcp` row plus the splice client;
  `FuzzClassify` invariants extended. No other git-side change.
- **cmd/portitor-mcp** (new): protocol loop, schema matcher, upstream
  supervisor — new `internal/mcp*` packages, reusing `internal/audit` and the
  strict-decode config patterns. Pure evaluators (classification, schema
  verdicts, list assembly) unit- and fuzz-tested beside the I/O.
- **release**: second `builds:` id in `.goreleaser.yaml`;
  `generate-manifest.sh`, `manifest.json`, and `install.sh` learn the second
  artifact. Same tag, same key, same version.
- **deploy**: a second container/user on `dca-net` for the mediator, with its
  own outbound egress for upstream APIs and its own credential mounts; the
  git-side container is unchanged and keeps sole custody of the GitHub
  credential.
- **spec/docs**: new `spec/mcp/` module (arch, impl, test seeds);
  `docs/architecture.md` gains the tier statement; `docs/commands.md`,
  `docs/configuration.md`, `docs/deploy.md` gain the mediator.
- **Compatibility**: additive (minor bump). The git-side binary tolerates the
  absence of a mediator (the `mcp` route refuses cleanly when the splice
  target is absent); rollout stays binary-first, then config.

## Out of scope

- Validating MCP semantics, or trusting `readOnlyHint` / any upstream-declared
  annotation.
- Forwarding server-initiated MCP flows: sampling, elicitation,
  `tools/list_changed`.
- HTTP / streamable-HTTP upstream transports and OAuth token storage or
  refresh (v1 is stdio-only).
- Generic passthrough without a pinned tool list and per-tool schemas.
- Deciding *when* to call a tool — caller-side policy, as with re-runs.
- Any change to the egress networking layer.
