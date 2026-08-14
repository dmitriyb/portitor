# MCP mediation — test scenarios

Unit/fuzz tests over the pure evaluators, protocol tests over in-process pipes (a fake upstream
behind the injected transport), and supervision tests via the self-exec fake upstream. No
network, no real MCP servers.

## Dispatch (cmd/portitor)

1. **`portitor mcp` classifies as mcp** — exactly two tokens → kind `mcp`, empty rest, nil err.
2. **trailing tokens rejected** — `portitor mcp x`, `portitor mcp --flag` → reject with error.
3. **closed table intact** — every pre-existing classify case unchanged; `FuzzClassify` gains:
   kind `mcp` ⇒ nil error and empty rest.
4. **unset target refuses cleanly** — no `PORTITOR_MCP_TARGET` → exit 1, one-line diagnostic,
   nothing written to stdout.
5. **target parse** — `unix:/x` → (`unix`, `/x`); `tcp:h:1` → (`tcp`, `h:1`); anything else
   (empty path/host, unknown scheme, no colon) → error.
6. **splice carries the header first** — on connect the first line the mediator side reads is
   `{"fingerprint":"<fp>"}`; subsequent bytes pass through verbatim both ways; client EOF
   half-closes toward the mediator; mediator EOF ends the splice with exit 0.

## Config (internal/mcpconfig)

7. **strict decode discipline** — unknown top-level key, duplicate key (any object), uppercase
   schema key, trailing content, wrong/missing `format_version` → each refused with its
   distinct error; `roles`/`servers`/`tools`/`tool_roles` keys are exempt from the lowercase
   rule but not from duplicate refusal.
8. **validate refusals** — non-fingerprint roles key; empty role; empty server argv / empty argv
   element / empty env name; tool with unknown server / empty `upstream_name` / non-compiling
   `params`; `tool_roles` naming an unknown tool; malformed or non-positive `limits` values.
   Each produces a named problem; a valid config produces none.
9. **default-deny views** — `Granted` is false for a tool absent from `tool_roles`, listed with
   no roles, or for a role not listed; `ListTools(role)` returns exactly the granted tools,
   sorted, with synthesized `inputSchema`.

## Schema matcher (internal/mcpschema)

10. **grammar compile refusals** — unknown field-spec key, bad `type`, empty `enum`,
    `enum`+`const` together, enum/const value of the wrong type → compile problems.
11. **accept/refuse matrix** — required present/missing; unknown field refused; each type
    accepted only for its JSON kind; `integer` refuses `1.5`, `1e2`, `"1"` and accepts `-3`;
    enum/const equality by literal for numbers, by value for strings/booleans.
12. **non-object arguments refused** — array, string, number, null (explicit) each refuse;
    absent arguments ⇒ `{}` (valid iff no required fields). A misspelled field-spec key in a
    config's `params.fields` entry refuses the whole config at parse.
13. **canonicalization** — keys sorted; numeric literals preserved verbatim (e.g. a large
    integer survives without float mangling); output re-decodes to exactly the accepted fields
    (fuzzed invariant).
14. **advertisement pinning** — missing `upstream_name` in the advertisement; advertised
    `required` naming a field our spec has optional or absent; advertised `properties` missing
    a configured field; advertised primitive type conflicting with the spec (`integer` vs
    advertised `"number"` is compatible); unreadable slice (non-object `inputSchema`,
    non-array `required`) → each refuses; a matching advertisement passes.

## Protocol loop (cmd/portitor-mcp over pipes)

15. **unknown fingerprint: no protocol** — header with an unmapped fingerprint → connection
    closes with no MCP bytes written, one audit deny. A connection that never sends a header is
    dropped at the `handshake_timeout` header deadline (audited), never pinned.
16. **subset served** — `initialize` → pinned version + `tools` capability; `ping` → `{}`;
    `notifications/initialized` ignored; unknown request → `-32601` (and an audit deny);
    unknown notification dropped, session continues.
17. **tools/list is the config's view** — exactly the caller's granted tools, sorted, even
    while the upstream advertises more/other tools; no upstream is spawned to answer it.
18. **tools/call happy path** — valid call spawns the upstream on first use, forwards the
    canonical bytes (the fake upstream asserts key order and the `upstream_name`), relays the
    result under the caller's original id, audits allow.
19. **tools/call refusals** — unknown tool; tool not granted to the role; unknown `params` key
    (`_meta` included); duplicate key in the `params` envelope; shape violation (each an error
    response with a distinct reason class + audit deny). No upstream is spawned for a refused
    call.
20. **pinning refusal is sticky** — a fake upstream advertising a drifted schema → every call
    to that tool refuses for the session; a second tool of the same server with a matching
    advertisement still works.
21. **server-initiated flows contained** — while a call is in flight the fake upstream emits a
    notification (dropped) and a request (answered `-32601`, never surfaced to the caller);
    the call still completes.
22. **bounds enforced** — an oversized client frame is a fatal session error; an oversized
    upstream response fails the call as an operational error; a handshake or call exceeding
    its timeout fails the call (audited error), not the mediator.
23. **upstream death** — a child that exits mid-session marks the server failed; later calls
    to its tools return an operational error without a respawn.

## Supervision (self-exec fake upstream)

24. **env allowlist** — the child sees `PATH`, `HOME`, and exactly the configured names (the
    fake upstream echoes its environment); an unset configured name refuses `serve` at boot.
25. **reap at session end** — after the client disconnects, the child is gone (TERM honored;
    a TERM-ignoring child is KILLed after the grace).
26. **argv, no shell** — a command containing shell metacharacters is exec'd verbatim (the
    fake upstream reports its argv), never interpreted.

## Audit

27. **every decision lands** — session deny, call allow/deny/error, refused method: one JSONL
    line each, kind `mcp`, with fingerprint/role/action/verdict/reason; an unwritable audit
    path never changes a verdict (reported on stderr).
