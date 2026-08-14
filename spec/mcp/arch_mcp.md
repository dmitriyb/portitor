# MCP mediation (`portitor-mcp`)

portitor extends its one invariant — *the only upstream credential lives behind the gate* — from
GitHub to every MCP upstream: agents reach MCP tools over the **same SSH channel** and the **same
key** they already hold, and no MCP token ever enters the agent container. The realistic
alternative is not "no MCP"; it is tokens in the agent overlay and per-host holes in the egress
lock — exactly the sprawl portitor exists to prevent.

## The guarantee tier (weakest of three — say so)

portitor now has three enforcement tiers, and this surface is the weakest. Never present it as
gate-grade or `pr`-grade authority:

1. **Result verification** (the git gate): a verdict is a function of verified objects.
2. **Request mediation with re-derived authority** (the `pr` API): the verb is tied to managed
   state the gate checks itself.
3. **Shape-validated forwarding** (this surface): only these tools, only these argument shapes,
   only this role, audited, credentials never leave the mediator. MCP calls are imperative
   JSON-RPC — there is no artifact to verify and no managed state to tie a call to. This is the
   egress allowlist's guarantee class lifted from host granularity to tool-call granularity, and
   nothing more.

**Shapes, not semantics.** "Read-only" is not a protocol property; `annotations.readOnlyHint` is
declared by the upstream server and is untrusted. portitor validates the operator's per-tool
predicates and claims nothing about what a tool *means* — consistent with the doctrine that
portitor is generic mechanism and ships no domain names. The operator who writes the predicates
carries the burden of knowing the tool's JSON shape; that maintenance *is* the security review,
made explicit.

## Same channel, third route

`classify()` (the pure SSH forced-command router) gains one row; the table stays **closed**:

| original command | route | effect |
|---|---|---|
| `git-receive-pack '<path>'` / `git-upload-pack '<path>'` | `git` | exec the real pack command (the gate runs) |
| `portitor pr <action> …` | `pr` | the role-gated action API |
| `portitor mcp` — **exactly two tokens, no arguments** | `mcp` | splice the connection's stdio to the mediator |
| anything else | reject | refused |

`portitor mcp` with trailing tokens is rejected: the MCP conversation runs over stdio, so there
is nothing an argument could legitimately carry, and the narrowest surface wins. Identity stays
the SSH key fingerprint, as everywhere else — no new key, no new endpoint.

### Splice, not exec — privilege separation

On an `mcp` connection the dispatcher does **not** exec MCP code in the git-side process; it
splices the connection's stdio to a separately running `portitor-mcp` process. The mediator
parses attacker-influenced JSON and supervises third-party server processes — it must never hold
the GitHub credential, and the git side must never hold MCP upstream tokens. Different process,
different user/container, different credential set.

- The splice target is named by `PORTITOR_MCP_TARGET` in the git-side environment:
  `unix:<path>` (a unix socket) or `tcp:<host>:<port>` (a `dca-net` port) — a deploy choice, not
  a protocol one.
- An unset target, or a target that cannot be dialed, **refuses cleanly** (exit 1, a one-line
  diagnostic): the git-side binary tolerates the absence of a mediator.
- After connecting, the dispatcher writes one header line — a single strict-decoded JSON object
  `{"fingerprint":"SHA256:…"}` terminated by `\n` — then copies bytes both ways until EOF
  (client EOF half-closes the write side so the mediator sees end-of-session). The dispatcher
  asserts the fingerprint because it verified the SSH key; **the socket's reachability is the
  trust boundary** (filesystem permissions for `unix:`, network isolation for `tcp:`) — whoever
  can connect can assert any fingerprint, so the target must be reachable only by the git-side
  dispatcher.

## `portitor-mcp`: the mediator

A second binary in the same module, released in lockstep (one tag, one version, same signing
key). Subcommands: `serve` (the daemon: `--config <path>` or `$PORTITOR_MCP_CONFIG`, `--listen
unix:<path>|tcp:<host>:<port>` or `$PORTITOR_MCP_LISTEN`), `validate-config`, and `version`.

At boot, `serve` loads and validates the config with the same fail-closed posture as the gate
(refuse to start on any problem), and additionally refuses to start if any configured server
`env` name is unset in the mediator's own environment — a missing credential must fail loudly at
boot, not at first tool call.

Per connection (one session per SSH connection):

1. Read the header line — deadline-bounded by `limits.handshake_timeout`, so a connection that
   never sends its header cannot pin a mediator goroutine pre-admission — and resolve the
   caller's role from the config's `roles` (fingerprint → role). An unknown fingerprint (or a
   malformed header) is refused **before any protocol exchange**: the connection is closed
   after an audit record, no MCP handshake happens, and the splice surfaces it to the caller as
   an immediate EOF (the refusal's record lives in the mediator's audit trail). An admitted
   session may then idle for the SSH connection's lifetime; session-count bounds are
   deploy-level (sshd `MaxSessions`, container process limits), not mediator config.
2. Speak the MCP stdio server subset (below). Requests are handled **sequentially** in arrival
   order — one in flight at a time per session; correctness first, pipelining never.
3. On `tools/call`: default-deny role check → strict schema validation → canonical
   re-serialization → forward to the owning upstream → relay the result. Every call is audited.
4. Spawn the owning upstream server on first use in the session; reap all of the session's
   children at session end.

### Protocol subset (hand-rolled, closed)

The mediator implements the server side of the MCP stdio transport (newline-delimited JSON-RPC
2.0) for **exactly** this method table; the subset is small enough to own, and owning it
preserves the "only runtime dependency is git" property (no SDK whose full surface would then
need auditing and piecemeal refusal):

| method | kind | handling |
|---|---|---|
| `initialize` | request | answered from config: the mediator's supported protocol version, `capabilities: {"tools": {}}`, its own `serverInfo`. Client params are not forwarded anywhere, so they are read leniently — shape validation guards the forwarded surface, not this local reply. |
| `notifications/initialized` | notification | accepted, ignored |
| `ping` | request | `{}` |
| `tools/list` | request | answered **from config, pinned**: exactly the tools the caller's role is granted (`tool_roles`), each with an `inputSchema` synthesized from its `params` allowlist (`additionalProperties: false`). Never an upstream's advertisement. Deterministic order (sorted by tool name). |
| `tools/call` | request | the mediation path (below) |
| any other request | request | JSON-RPC error `-32601` (method not found), audited as a deny |
| any other notification | notification | dropped (JSON-RPC forbids replying to a notification) |

Server-initiated MCP flows are **not forwarded** in either direction: an upstream's request
(sampling, elicitation) is answered by the mediator with a `-32601` error and never reaches the
agent; an upstream's notification (`tools/list_changed` included) is dropped. The pinned config
is the only source of the tool list, so a list-changed signal has nothing to update.

A frame exceeding the configured message cap, or a byte stream that cannot be framed, is a fatal
session error (audited): framing is the outermost trust boundary and does not degrade.

### `tools/call` mediation

1. `params` must be a JSON object with **exactly** the keys `name` (required) and `arguments`
   (optional); unknown keys — `_meta` included — are refused, and duplicate keys at any depth
   refuse too (the silent-last-wins shadowing class is refused everywhere an attacker-authored
   object is decoded, envelope and arguments alike). Absent `arguments` is the empty object.
2. `name` must be a configured tool; the caller's role must be granted it in `tool_roles`
   (default-deny: a tool absent from `tool_roles`, or listed with no roles, is refused for
   everyone).
3. `arguments` is validated against the tool's `params` allowlist schema (grammar below):
   strict decode, unknown fields refused, everything not explicitly allowed refused.
4. The accepted arguments are **re-serialized canonically** (object keys sorted, numeric
   literals preserved verbatim) and a fresh `tools/call` request is constructed by the mediator
   — `{"name": <upstream_name>, "arguments": <canonical>}` with a mediator-assigned id — so the
   bytes reaching the upstream are the bytes the predicate saw: never a second parse with
   different tolerances, and never the caller's raw bytes.
5. The upstream's response (result or error) is relayed to the caller under the caller's
   original id, bounded by the upstream message cap and the call timeout.
6. The decision is audited (below) whatever the verdict.

A denial (role, shape, pinning) is a JSON-RPC **error** response naming the reason class;
refusals are visible, never silent.

### Pinned tool list and advertisement drift (fail closed)

The exposed tool set and each tool's expected shape live **only** in the mediator's config. When
an upstream is first spawned in a session, the mediator performs the MCP client handshake
(`initialize`, `notifications/initialized`) and calls `tools/list` once, then checks each
configured tool of that server against the advertisement:

- the upstream must advertise a tool named `upstream_name` — else the tool is refused;
- every name in the advertised `inputSchema.required` must be a **required** field of the
  configured `params` (an upstream that grew a new required field would reject every canonical
  call — surface it as a pinning refusal, not as upstream noise);
- when the advertised `inputSchema` declares `properties`, every configured `params` field must
  appear among them (an upstream that dropped or renamed a field the operator still allows is
  drift — refuse);
- when an advertised property declares a primitive `"type"` string, it must match the configured
  field's type (`integer` accepts an advertised `number`).

The check reads only this bounded slice of JSON Schema — nothing else of the advertisement is
interpreted, and an advertisement too malformed to read this slice (a non-object `inputSchema`,
a non-array `required`) refuses the tool. A tool refused by pinning stays refused for the whole
session: `tools/call` on it returns a pinning refusal (audited). Upstream schema drift is
therefore a **visible refusal fixed in config**, not a silent hole. `tools/list` to the caller
is unaffected (it is the config's view by definition).

### Upstream supervision (session-scoped stdio children)

v1 upstream servers are operator-configured **argv commands — no shell, ever** — spawned by
`portitor-mcp` per session on first use, speaking MCP over stdio. HTTP transports and OAuth
token management are explicitly out of scope for v1.

- **Environment**: the child receives a minimal environment — `PATH` and `HOME` from the
  mediator's own environment, plus exactly the names listed in the server's `env`, with values
  from the mediator's environment (or its credential mounts). Credentials are injected here and
  **never appear in config, argv, or any protocol message**.
- **Lifetime**: the SSH connection — the existing process-per-connection model, unchanged. At
  session end every child is terminated (SIGTERM, a bounded grace, then SIGKILL) and reaped.
- A child that exits, or whose handshake times out, marks its server failed for the session;
  calls to its tools return an operational error (audited). No respawn within a session —
  restart semantics are the next session's.

### Config: its own file, its own trust domain

The surface is not per-repo, and the mediator is a separate privilege domain, so its config does
not live in `repos.d/`. `portitor-mcp` reads **one** strictly-decoded JSON file with the same
discipline as the gate config (byte-exact top-level keys, duplicate-key refusal, lowercase
schema keys with data maps exempt, `DisallowUnknownFields`, trailing-content refusal, its own
`format_version` guard — supported version 1):

```jsonc
{
  "format_version": 1,
  "roles": { "SHA256:…": "implementer" },       // fingerprint → role, as in repos.d
  "servers": {
    "tracker": {
      "command": ["tracker-mcp-server"],        // argv, no shell
      "env": ["TRACKER_TOKEN"]                  // names passed through from the mediator's env
    }
  },
  "tools": {
    "tracker.search": {
      "server": "tracker",
      "upstream_name": "search",                // the name the upstream advertises
      "description": "Search tracker issues",   // optional; shown in tools/list
      "params": {                               // allowlist schema (grammar below)
        "fields": {
          "query": { "type": "string", "required": true },
          "limit": { "type": "integer" },
          "order": { "type": "string", "enum": ["asc", "desc"] }
        }
      }
    }
  },
  "tool_roles": {                               // default-deny, the action_roles shape
    "tracker.search": ["implementer", "reviewer"]
  },
  "audit_log": "/var/log/portitor-mcp/audit.jsonl",  // optional, as in repos.d configs
  "limits": {                                   // optional; mechanism bounds, defaults below
    "message_bytes": 1048576,                   // client→mediator frame cap (default 1 MiB)
    "upstream_message_bytes": 8388608,          // upstream→mediator frame cap (default 8 MiB)
    "handshake_timeout": "30s",                 // upstream spawn+initialize+tools/list bound
    "call_timeout": "120s"                      // per forwarded tools/call bound
  }
}
```

`validate-config` (and `serve` at boot) refuse, fail-closed: an unsupported `format_version`; a
non-fingerprint `roles` key or empty role; a server with an empty argv, an empty argv element,
or an empty `env` name; a tool referencing an unknown server, with an empty `upstream_name`, or
with a `params` block that does not compile under the grammar; a `tool_roles` key naming an
unknown tool; a malformed or non-positive `limits` duration or byte count.

### The `params` matcher grammar (deliberately minimal, exact)

`params.fields` maps each allowed argument name to a field spec:

- `type` (required): one of `string | integer | number | boolean`. That is the whole type
  vocabulary — **no objects, no arrays** in v1: a nested shape the operator cannot fully pin is
  a shape the mediator must not forward.
- `required` (optional, default false).
- `enum` (optional): a non-empty array of values of the field's type; the argument must equal
  one of them.
- `const` (optional): a single value of the field's type; the argument must equal it. `enum` and
  `const` are mutually exclusive.

Validation of a call's `arguments`:

- must be a JSON object (absent ⇒ `{}`); anything else refuses;
- **unknown fields refuse — ever**; every `required` field must be present;
- `string` ⇒ a JSON string; `boolean` ⇒ a JSON boolean; `number` ⇒ a JSON number; `integer` ⇒ a
  JSON number whose literal is an optionally-signed run of digits (no fraction, no exponent) —
  the check runs on the **literal**, so no float round-trip can smuggle a non-integer;
- `enum`/`const` compare decoded values of the declared type (numbers compare by literal).

Canonical re-serialization emits the accepted fields with keys sorted and numeric literals
preserved verbatim; strings are re-encoded by the mediator's JSON encoder. Everything the
upstream receives passed this validation — there is no other path.

## Audit

Every session admission and every call decision appends one fsync'd JSON line to the config's
`audit_log` through the same `internal/audit` trail as the gate: kind `mcp`, the caller's
fingerprint and role, the tool (or refused method) in the `action` field, verdict
(`allow|deny|error`), and reason. As everywhere else: an unset `audit_log` disables the trail,
and a write failure never changes a verdict — it is loudly reported instead.

## Release and compatibility

`cmd/portitor-mcp` is a second build target sharing the internal packages. One tag ships both
artifacts at one version through the existing pipeline and signing key: the shared config
discipline makes lockstep the correct semantics — one version number states "this pair was
tested together". The change is additive (minor bump); the git-side binary tolerates a missing
mediator (the `mcp` route refuses cleanly), so rollout stays binary-first, then config.

## Boundaries and non-goals

- No validation of MCP semantics; no trust in `readOnlyHint` or any upstream-declared
  annotation.
- No forwarding of server-initiated flows (sampling, elicitation, `tools/list_changed`).
- No HTTP / streamable-HTTP upstream transports, no OAuth storage or refresh (v1 is stdio-only).
- No generic passthrough without a pinned tool list and per-tool schemas.
- Deciding *when* to call a tool is caller-side policy, as with re-runs.
- The `--internal` network and the agent-side proxy are declarative deploy config, owned outside
  this repo. No Go grows on that leg.
