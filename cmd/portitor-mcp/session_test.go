package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmitriyb/portitor/internal/audit"
	"github.com/dmitriyb/portitor/internal/mcpconfig"
	"github.com/dmitriyb/portitor/internal/mcpwire"
)

var (
	fpImplementer = "SHA256:" + strings.Repeat("i", 43)
	fpStranger    = "SHA256:" + strings.Repeat("s", 43)
)

// testSettings parses a realistic mediator config (through the real strict
// decode) with the audit trail pointed into the test's temp dir.
func testSettings(t *testing.T, limitsJSON string) (mcpconfig.Settings, string) {
	t.Helper()
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	body := `{
  "format_version": 1,
  "roles": {"` + fpImplementer + `": "implementer"},
  "servers": {"srv": {"command": ["unused-in-pipe-tests"]}},
  "tools": {
    "t.search": {"server": "srv", "upstream_name": "search",
      "params": {"fields": {"query": {"type": "string", "required": true}, "limit": {"type": "integer"}}}},
    "t.get": {"server": "srv", "upstream_name": "get"},
    "t.drift": {"server": "srv", "upstream_name": "drift",
      "params": {"fields": {"q": {"type": "string", "required": true}}}}
  },
  "tool_roles": {"t.search": ["implementer"], "t.get": ["implementer"], "t.drift": ["implementer"]},
  "audit_log": ` + fmt.Sprintf("%q", auditPath) + `,
  "limits": ` + limitsJSON + `
}`
	s, err := mcpconfig.Parse([]byte(body))
	if err != nil {
		t.Fatalf("test config: %v", err)
	}
	if p := mcpconfig.Validate(s); len(p) > 0 {
		t.Fatalf("test config invalid: %v", p)
	}
	return s, auditPath
}

func defaultLimits() string {
	return `{"message_bytes": 4096, "call_timeout": "500ms", "handshake_timeout": "2s"}`
}

// readAudit decodes the trail lines.
func readAudit(t *testing.T, path string) []audit.Event {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var events []audit.Event
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var e audit.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		events = append(events, e)
	}
	return events
}

// fakeUpstream is an in-process MCP server behind the injected transport: it
// advertises a fixed tool set and records what reaches it.
type fakeUpstream struct {
	tools map[string]json.RawMessage // advertised name → inputSchema (nil = none)
	// onCall answers a tools/call; nil answers {"ok":true}.
	onCall func(name string, args json.RawMessage) mcpwire.Message
	// preCall frames are written after receiving a tools/call, before its
	// response (server-initiated flows).
	preCall []mcpwire.Message
	// silent, when set, never answers tools/call (timeout testing).
	silent bool
	// dieAfterCalls, when > 0, exits serve (closing the transport) after
	// answering that many tools/call requests (upstream-death testing).
	dieAfterCalls int64

	spawns    atomic.Int64
	calls     atomic.Int64
	lastName  atomic.Value // string: last upstream tool name called
	lastArgs  atomic.Value // string: last raw params bytes as received
	gotErrors atomic.Int64 // -32601 replies received back (server-initiated request containment)
}

func (fu *fakeUpstream) spawn(mcpconfig.Server) (io.ReadWriteCloser, func(), error) {
	fu.spawns.Add(1)
	client, server := net.Pipe()
	go fu.serve(server)
	return client, func() { server.Close() }, nil
}

func (fu *fakeUpstream) serve(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	respond := func(m mcpwire.Message) bool { return mcpwire.WriteFrame(conn, m) == nil }
	for {
		frame, err := mcpwire.ReadFrame(br, 1<<20)
		if err != nil {
			return
		}
		msg, err := mcpwire.Parse(frame)
		if err != nil {
			return
		}
		switch msg.Method {
		case "initialize":
			r, _ := mcpwire.NewResult(msg.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "fake", "version": "0"},
			})
			if !respond(r) {
				return
			}
		case "notifications/initialized":
		case "tools/list":
			type tool struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema,omitempty"`
			}
			var tools []tool
			for name, schema := range fu.tools {
				tools = append(tools, tool{Name: name, InputSchema: schema})
			}
			r, _ := mcpwire.NewResult(msg.ID, map[string]any{"tools": tools})
			if !respond(r) {
				return
			}
		case "tools/call":
			fu.calls.Add(1)
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			fu.lastName.Store(p.Name)
			fu.lastArgs.Store(string(p.Arguments))
			for _, m := range fu.preCall {
				if !respond(m) {
					return
				}
			}
			if fu.silent {
				continue
			}
			var reply mcpwire.Message
			if fu.onCall != nil {
				reply = fu.onCall(p.Name, p.Arguments)
			} else {
				reply, _ = mcpwire.NewResult(nil, map[string]any{"ok": true})
			}
			reply.ID = msg.ID
			if !respond(reply) {
				return
			}
			if fu.dieAfterCalls > 0 && fu.calls.Load() >= fu.dieAfterCalls {
				return
			}
		default:
			if msg.Method == "" && msg.Error != nil {
				fu.gotErrors.Add(1) // the mediator's reply to our server-initiated request
			}
		}
	}
}

// advertised returns a healthy advertisement matching testSettings.
func healthyTools() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"search": json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"limit":{"type":"number"}}}`),
		"get":    nil,
		"drift":  json.RawMessage(`{"properties":{"other":{"type":"string"}}}`), // pinned field q missing → drift
	}
}

// mcpClient drives one session end like the spliced dispatcher + agent would.
type mcpClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

// startSession runs a session against cfg/spawn and returns the client end,
// header already written for fp.
func startSession(t *testing.T, cfg mcpconfig.Settings, spawn spawnFunc, fp string) *mcpClient {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		runSession(cfg, serverConn, spawn, io.Discard)
	}()
	t.Cleanup(func() {
		clientConn.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("session did not end")
		}
	})
	c := &mcpClient{t: t, conn: clientConn, br: bufio.NewReader(clientConn)}
	hdr, _ := json.Marshal(mcpwire.Header{Fingerprint: fp})
	c.sendRaw(string(hdr))
	return c
}

func (c *mcpClient) sendRaw(line string) {
	c.t.Helper()
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write([]byte(line + "\n")); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

func (c *mcpClient) recv() (mcpwire.Message, error) {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	frame, err := mcpwire.ReadFrame(c.br, 1<<20)
	if err != nil {
		return mcpwire.Message{}, err
	}
	return mcpwire.Parse(frame)
}

func (c *mcpClient) roundTrip(line string) mcpwire.Message {
	c.t.Helper()
	c.sendRaw(line)
	m, err := c.recv()
	if err != nil {
		c.t.Fatalf("recv after %s: %v", line, err)
	}
	return m
}

// TestHeaderDeadlineEndsAnIdleConnection: a connection that never sends its
// header must not pin a mediator goroutine — the header read is bounded by
// limits.handshake_timeout and expiry ends the session with an audit record.
func TestHeaderDeadlineEndsAnIdleConnection(t *testing.T) {
	cfg, auditPath := testSettings(t, `{"handshake_timeout": "100ms"}`)
	fu := &fakeUpstream{tools: healthyTools()}
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		runSession(cfg, serverConn, fu.spawn, io.Discard)
	}()
	// Send nothing at all: the session must end on its own, well before the
	// test's patience runs out.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a headerless connection was never dropped")
	}
	events := readAudit(t, auditPath)
	if len(events) != 1 || events[0].Verdict != "deny" || !strings.Contains(events[0].Reason, "session header") {
		t.Fatalf("audit = %+v", events)
	}
}

func TestSessionUnknownFingerprintRefusedBeforeProtocol(t *testing.T) {
	cfg, auditPath := testSettings(t, defaultLimits())
	fu := &fakeUpstream{tools: healthyTools()}
	c := startSession(t, cfg, fu.spawn, fpStranger)
	// No MCP bytes are written; the connection just closes.
	if _, err := c.recv(); err == nil {
		t.Fatal("expected the connection to close with no protocol bytes")
	}
	events := readAudit(t, auditPath)
	if len(events) != 1 || events[0].Verdict != "deny" || events[0].Fingerprint != fpStranger {
		t.Fatalf("audit = %+v", events)
	}
	if fu.spawns.Load() != 0 {
		t.Fatal("no upstream may be spawned for a refused session")
	}
}

func TestSessionSubset(t *testing.T) {
	cfg, auditPath := testSettings(t, defaultLimits())
	fu := &fakeUpstream{tools: healthyTools()}
	c := startSession(t, cfg, fu.spawn, fpImplementer)

	init := c.roundTrip(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`)
	if init.Error != nil || !strings.Contains(string(init.Result), `"protocolVersion"`) || !strings.Contains(string(init.Result), `"tools"`) {
		t.Fatalf("initialize: %+v", init)
	}
	c.sendRaw(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	ping := c.roundTrip(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if ping.Error != nil || string(ping.Result) != `{}` {
		t.Fatalf("ping: %+v", ping)
	}

	// A request outside the subset: -32601, audited.
	res := c.roundTrip(`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)
	if res.Error == nil || res.Error.Code != mcpwire.CodeMethodNotFound {
		t.Fatalf("unknown request: %+v", res)
	}
	// An unknown notification is dropped, session continues.
	c.sendRaw(`{"jsonrpc":"2.0","method":"notifications/cancelled"}`)
	if again := c.roundTrip(`{"jsonrpc":"2.0","id":4,"method":"ping"}`); again.Error != nil {
		t.Fatalf("session dead after dropped notification: %+v", again)
	}
	// A garbage line answers -32700 and the session continues.
	bad := c.roundTrip(`this is not json`)
	if bad.Error == nil || bad.Error.Code != mcpwire.CodeParseError {
		t.Fatalf("garbage line: %+v", bad)
	}
	if again := c.roundTrip(`{"jsonrpc":"2.0","id":5,"method":"ping"}`); again.Error != nil {
		t.Fatalf("session dead after parse error: %+v", again)
	}

	var denied bool
	for _, e := range readAudit(t, auditPath) {
		if e.Action == "resources/list" && e.Verdict == "deny" {
			denied = true
		}
	}
	if !denied {
		t.Fatal("refused method not audited")
	}
	if fu.spawns.Load() != 0 {
		t.Fatal("the subset methods must not spawn an upstream")
	}
}

func TestToolsListIsThePinnedConfigView(t *testing.T) {
	cfg, _ := testSettings(t, defaultLimits())
	fu := &fakeUpstream{tools: healthyTools()}
	c := startSession(t, cfg, fu.spawn, fpImplementer)
	res := c.roundTrip(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if res.Error != nil {
		t.Fatalf("tools/list: %+v", res)
	}
	var body struct {
		Tools []mcpconfig.ToolDescriptor `json:"tools"`
	}
	if err := json.Unmarshal(res.Result, &body); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range body.Tools {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "t.drift,t.get,t.search" {
		t.Fatalf("tools = %v (want the config's granted view, sorted)", names)
	}
	if fu.spawns.Load() != 0 {
		t.Fatal("tools/list must be answered from config without spawning an upstream")
	}
}

func TestToolsCallHappyPath(t *testing.T) {
	cfg, auditPath := testSettings(t, defaultLimits())
	fu := &fakeUpstream{tools: healthyTools()}
	c := startSession(t, cfg, fu.spawn, fpImplementer)

	// Arguments deliberately NOT in canonical order (query before limit): the
	// fake upstream must see the canonical re-serialization, not these bytes.
	res := c.roundTrip(`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"t.search","arguments":{"query":"needle","limit":10}}}`)
	if res.Error != nil {
		t.Fatalf("call: %+v", res)
	}
	if string(res.ID) != `"call-1"` {
		t.Fatalf("result under the caller's id: got %s", res.ID)
	}
	if !strings.Contains(string(res.Result), `"ok":true`) {
		t.Fatalf("result: %s", res.Result)
	}
	if got := fu.lastName.Load(); got != "search" {
		t.Fatalf("upstream saw tool %v, want the upstream_name", got)
	}
	// Canonical bytes: keys sorted (the input had query first), literals
	// verbatim — proof the session forwards Validate's output, never the
	// caller's raw bytes.
	if got := fu.lastArgs.Load(); got != `{"limit":10,"query":"needle"}` {
		t.Fatalf("upstream saw arguments %v", got)
	}
	events := readAudit(t, auditPath)
	last := events[len(events)-1]
	if last.Action != "t.search" || last.Verdict != "allow" || last.Role != "implementer" {
		t.Fatalf("audit = %+v", last)
	}

	// A second call reuses the session upstream (no respawn).
	if r2 := c.roundTrip(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"t.get"}}`); r2.Error != nil {
		t.Fatalf("second call: %+v", r2)
	}
	if fu.spawns.Load() != 1 {
		t.Fatalf("spawns = %d, want 1 per session per server", fu.spawns.Load())
	}
}

func TestToolsCallRefusals(t *testing.T) {
	cfg, auditPath := testSettings(t, defaultLimits())
	fu := &fakeUpstream{tools: healthyTools()}
	c := startSession(t, cfg, fu.spawn, fpImplementer)

	cases := []struct {
		name, line, want string
	}{
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`, "unknown tool"},
		{"unknown params key", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"t.get","_meta":{"progressToken":1}}}`, "params"},
		{"duplicate params key", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"t.get","name":"nope"}}`, "duplicate key"},
		{"missing name", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{}}`, "name required"},
		{"shape violation: missing required", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"t.search","arguments":{}}}`, "required field"},
		{"shape violation: unknown field", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"t.search","arguments":{"query":"q","cursor":"x"}}}`, "unknown field"},
		{"shape violation: bad type", `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"t.search","arguments":{"query":"q","limit":1.5}}}`, "integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := c.roundTrip(tc.line)
			if res.Error == nil {
				t.Fatalf("accepted: %+v", res)
			}
			if !strings.Contains(res.Error.Message, tc.want) {
				t.Fatalf("error %q does not mention %q", res.Error.Message, tc.want)
			}
		})
	}
	if fu.spawns.Load() != 0 {
		t.Fatal("no upstream may be spawned for a refused call")
	}
	var denies int
	for _, e := range readAudit(t, auditPath) {
		if e.Verdict == "deny" {
			denies++
		}
	}
	if denies != len(cases) {
		t.Fatalf("audited denies = %d, want %d", denies, len(cases))
	}
}

func TestToolsCallRoleDefaultDeny(t *testing.T) {
	cfg, _ := testSettings(t, defaultLimits())
	// Rebind the fingerprint to a role granted nothing.
	cfg.Roles[fpImplementer] = "bystander"
	fu := &fakeUpstream{tools: healthyTools()}
	c := startSession(t, cfg, fu.spawn, fpImplementer)
	res := c.roundTrip(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t.search","arguments":{"query":"q"}}}`)
	if res.Error == nil || !strings.Contains(res.Error.Message, "default-deny") {
		t.Fatalf("ungranted role: %+v", res)
	}
	if fu.spawns.Load() != 0 {
		t.Fatal("no upstream may be spawned for an ungranted call")
	}
}

func TestPinningRefusalIsSticky(t *testing.T) {
	cfg, auditPath := testSettings(t, defaultLimits())
	fu := &fakeUpstream{tools: healthyTools()} // drift's advertisement is missing our pinned field q
	c := startSession(t, cfg, fu.spawn, fpImplementer)

	for i := 0; i < 2; i++ {
		res := c.roundTrip(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"t.drift","arguments":{"q":"x"}}}`, i+1))
		if res.Error == nil || !strings.Contains(res.Error.Message, "pinned schema mismatch") {
			t.Fatalf("drifted tool call %d: %+v", i, res)
		}
	}
	// A healthy tool of the same server still works.
	if res := c.roundTrip(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"t.get"}}`); res.Error != nil {
		t.Fatalf("healthy sibling tool: %+v", res)
	}
	if fu.spawns.Load() != 1 {
		t.Fatalf("spawns = %d", fu.spawns.Load())
	}
	var pinned bool
	for _, e := range readAudit(t, auditPath) {
		if e.Action == "t.drift" && e.Verdict == "deny" && strings.Contains(e.Reason, "pinned") {
			pinned = true
		}
	}
	if !pinned {
		t.Fatal("pinning refusal not audited")
	}
}

func TestServerInitiatedFlowsContained(t *testing.T) {
	cfg, _ := testSettings(t, defaultLimits())
	fu := &fakeUpstream{
		tools: healthyTools(),
		preCall: []mcpwire.Message{
			{JSONRPC: "2.0", Method: "notifications/tools/list_changed"},
			{JSONRPC: "2.0", ID: json.RawMessage(`"srv-req"`), Method: "sampling/createMessage", Params: json.RawMessage(`{}`)},
		},
	}
	c := startSession(t, cfg, fu.spawn, fpImplementer)
	res := c.roundTrip(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t.get"}}`)
	if res.Error != nil {
		t.Fatalf("call must complete despite server-initiated flows: %+v", res)
	}
	// The upstream's request got a -32601 back; nothing was surfaced to us
	// beyond the one call result (roundTrip already consumed exactly one).
	deadline := time.Now().Add(2 * time.Second)
	for fu.gotErrors.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fu.gotErrors.Load() != 1 {
		t.Fatalf("upstream request answers = %d, want 1 refusal", fu.gotErrors.Load())
	}
}

func TestUpstreamErrorRelayedAsCallOutcome(t *testing.T) {
	cfg, auditPath := testSettings(t, defaultLimits())
	fu := &fakeUpstream{
		tools: healthyTools(),
		onCall: func(string, json.RawMessage) mcpwire.Message {
			return mcpwire.NewError(nil, 123, "tool exploded")
		},
	}
	c := startSession(t, cfg, fu.spawn, fpImplementer)
	res := c.roundTrip(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t.get"}}`)
	if res.Error == nil || res.Error.Code != 123 || res.Error.Message != "tool exploded" {
		t.Fatalf("upstream error must relay verbatim: %+v", res)
	}
	last := readAudit(t, auditPath)
	if e := last[len(last)-1]; e.Verdict != "allow" {
		t.Fatalf("a relayed upstream error is the tool's own outcome (mediation allowed): %+v", e)
	}
}

func TestCallTimeoutFailsTheCall(t *testing.T) {
	cfg, _ := testSettings(t, `{"call_timeout": "200ms", "handshake_timeout": "2s"}`)
	fu := &fakeUpstream{tools: healthyTools(), silent: true}
	c := startSession(t, cfg, fu.spawn, fpImplementer)
	res := c.roundTrip(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t.get"}}`)
	if res.Error == nil || res.Error.Code != mcpwire.CodeUpstreamFailure {
		t.Fatalf("timeout: %+v", res)
	}
	// The failure latches: the next call errors without a respawn.
	res = c.roundTrip(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"t.get"}}`)
	if res.Error == nil || res.Error.Code != mcpwire.CodeUpstreamFailure {
		t.Fatalf("latched failure: %+v", res)
	}
	if fu.spawns.Load() != 1 {
		t.Fatalf("spawns = %d, want no respawn within the session", fu.spawns.Load())
	}
}

func TestOversizedUpstreamResponseFailsTheCall(t *testing.T) {
	cfg, _ := testSettings(t, `{"upstream_message_bytes": 256, "call_timeout": "2s", "handshake_timeout": "2s"}`)
	fu := &fakeUpstream{tools: map[string]json.RawMessage{"get": nil}}
	fu.onCall = func(string, json.RawMessage) mcpwire.Message {
		m, _ := mcpwire.NewResult(nil, map[string]string{"blob": strings.Repeat("x", 1000)})
		return m
	}
	// A config with only t.get so the handshake's tools/list stays small.
	cfg.Tools = map[string]mcpconfig.Tool{"t.get": cfg.Tools["t.get"]}
	c := startSession(t, cfg, fu.spawn, fpImplementer)
	res := c.roundTrip(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t.get"}}`)
	if res.Error == nil || res.Error.Code != mcpwire.CodeUpstreamFailure {
		t.Fatalf("oversized upstream response: %+v", res)
	}
}

func TestOversizedClientFrameIsFatal(t *testing.T) {
	cfg, auditPath := testSettings(t, `{"message_bytes": 512}`)
	fu := &fakeUpstream{tools: healthyTools()}
	c := startSession(t, cfg, fu.spawn, fpImplementer)
	c.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", 2000) + `"}}`)
	if _, err := c.recv(); err == nil {
		t.Fatal("an over-cap frame must end the session, not degrade it")
	}
	var fatal bool
	for _, e := range readAudit(t, auditPath) {
		if e.Verdict == "error" && strings.Contains(e.Reason, "framing") {
			fatal = true
		}
	}
	if !fatal {
		t.Fatal("fatal framing error not audited")
	}
}

func TestUpstreamDeathLatches(t *testing.T) {
	cfg, _ := testSettings(t, defaultLimits())
	// The fake exits (closing its transport) after answering one call.
	fu := &fakeUpstream{tools: healthyTools(), dieAfterCalls: 1}
	c := startSession(t, cfg, fu.spawn, fpImplementer)
	if res := c.roundTrip(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t.get"}}`); res.Error != nil {
		t.Fatalf("first call: %+v", res)
	}
	res := c.roundTrip(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"t.get"}}`)
	if res.Error == nil || res.Error.Code != mcpwire.CodeUpstreamFailure {
		t.Fatalf("dead upstream: %+v", res)
	}
	if fu.spawns.Load() != 1 {
		t.Fatalf("spawns = %d, want no respawn", fu.spawns.Load())
	}
}
