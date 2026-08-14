# MCP mediation — implementation notes

## Package layout

- `internal/mcpwire` — the JSON-RPC 2.0 / MCP stdio framing and routing, pure:
  - `Message` (jsonrpc, id as `json.RawMessage`, method, params/result/error raw) with
    `Parse` (rejects a non-`"2.0"` `jsonrpc`) and response constructors (`NewResult`,
    `NewError`).
  - Newline-delimited framing: `ReadFrame(r *bufio.Reader, max int)` (a frame longer than
    `max` is an error, not a truncation) and `WriteFrame` (marshal + `\n`).
  - `Route(Message) RouteKind` — the closed method table (`initialize`, `notifications/
    initialized`, `ping`, `tools/list`, `tools/call`, unknown-request, unknown-notification,
    invalid). Pure; the security-critical dispatch is unit- and fuzz-tested like `classify`.
- `internal/mcpschema` — the allowlist matcher, pure:
  - `Spec` / `Field` decoded from the config's `params` block; `Compile` returns the
    grammar-violation list (empty = valid), mirroring `rules.Compile`.
  - `Validate(spec, rawArguments) (canonical []byte, err)` — strict decode via
    `json.Decoder.UseNumber` so integerness and enum comparison run on the numeric **literal**;
    canonical output has sorted keys (Go's `encoding/json` map marshaling) with `json.Number`
    preserving literals verbatim.
  - `SynthesizeInputSchema(spec)` — the `tools/list` JSON Schema (`type: object`,
    `properties`, `required` sorted, `additionalProperties: false`).
  - `CheckAdvertised(spec, inputSchema json.RawMessage) error` — the bounded pinning check of
    `arch_mcp.md` (required-subset, property-presence, primitive-type agreement).
- `internal/mcpconfig` — the mediator's config: `Settings`, `Parse` with the full raw-key
  discipline copied from `internal/config` (top-level keys byte-exact; `roles`, `servers`,
  `tools`, `tool_roles` are data maps exempt from the lowercase rule; duplicate keys refused
  everywhere; `DisallowUnknownFields`; trailing-content refusal; `format_version` guard),
  `LoadFile`, `Validate`, and the pure views the session loop consumes: `RoleFor(fingerprint)`,
  `Granted(role, tool) bool` (default-deny), `ListTools(role)` (sorted descriptors). It
  deliberately does **not** import `internal/config` (the git-side package pulls the gate; the
  fingerprint regexp is duplicated with a pointer comment).
- `cmd/portitor-mcp` — the I/O beside the pure evaluators: cobra root (`serve`,
  `validate-config`, `version` — the same exit-code plumbing as `cmd/portitor`), the listener,
  the per-connection session loop, and the upstream supervisor.

`cmd/portitor` gains only the `mcp` classify row and the splice client (`spliceMCP`, backed by
the testable `spliceMCPTo`). The git side links exactly one mediator-shared package —
`internal/mcpwire`, for the splice-target grammar (`ParseTarget`) and the header type it must
emit — and never parses or interprets MCP traffic; the protocol loop, matcher, and supervisor
packages stay out of the git-side binary.

## Session loop (cmd/portitor-mcp)

- Header: one line, capped at 4096 bytes, strict-decoded `{"fingerprint": "…"}` (unknown keys
  refused). Unknown fingerprint ⇒ audit deny, close. No MCP bytes are written before this
  admission decision.
- Requests are handled sequentially; the caller's `id` is held while the forwarded request uses
  a fresh mediator-assigned numeric id, and the relay rewrites the id back on the response.
- While waiting for an upstream response the loop discards upstream notifications and answers
  upstream **requests** with `-32601` (server-initiated flows are not forwarded); only the
  response matching the forwarded id completes the call, bounded by `limits.call_timeout`.
- Upstream child: spawned on first call to one of its tools; handshake = `initialize` →
  `notifications/initialized` → `tools/list`, bounded by `limits.handshake_timeout`; the
  advertisement map (name → `inputSchema`) is cached per session and each configured tool's
  pinning verdict is computed once. Environment: `PATH`, `HOME`, plus the configured `env`
  names only. Session end: SIGTERM, 2s grace, SIGKILL, `Wait` — no orphans.
- `serve` refuses to boot when any configured server's `env` name is unset in the mediator's
  environment (fail loud at boot, not at first call).

## Testing hooks

- The upstream client speaks over an injected `io.ReadWriteCloser`, so protocol tests run
  against an in-process fake upstream over pipes — no subprocess, no network.
- Process supervision (spawn, env allowlist, reap) is tested by re-execing the test binary as a
  fake upstream (`TestMain` intercepts an env flag), the same self-exec pattern
  `internal/check`'s trampoline tests use.
- Pure evaluators are fuzzed beside their unit tests: `FuzzRoute` (never panics; closed table),
  `FuzzValidate` (never panics; a nil error implies the canonical bytes re-decode to exactly
  the accepted field set, every field satisfying its spec), `FuzzParseConfig` (never panics; a
  nil error implies `Validate` runs without panic), and the git-side `FuzzClassify` extended
  with the `mcp` row invariants.
