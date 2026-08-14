package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/dmitriyb/portitor/internal/audit"
	"github.com/dmitriyb/portitor/internal/mcpconfig"
	"github.com/dmitriyb/portitor/internal/mcpschema"
	"github.com/dmitriyb/portitor/internal/mcpwire"
)

// protocolVersion is the MCP protocol revision this mediator speaks. The
// subset it implements (initialize, tools/list, tools/call, ping) is stable
// across the published revisions; the mediator answers initialize with this
// version regardless of the client's requested one.
const protocolVersion = "2025-06-18"

// session is one spliced SSH connection's mediation state: the admitted
// caller, the config view, and the session-scoped upstreams.
type session struct {
	cfg  mcpconfig.Settings
	fp   string
	role string
	ups  *upstreamSet
	errw io.Writer // serve's stderr: audit-failure reports only
}

// runSession admits and serves one connection. Admission runs before any
// protocol exchange: a malformed header or unknown fingerprint closes the
// connection with nothing but an audit record. spawn is the upstream
// transport factory (injectable for tests).
func runSession(cfg mcpconfig.Settings, conn io.ReadWriter, spawn spawnFunc, errw io.Writer) {
	// The header read is deadline-bounded (limits.handshake_timeout): a
	// connection that never sends its header must not pin a goroutine
	// pre-admission. An admitted session may then idle freely — its lifetime
	// is the SSH connection's, and session-count bounds are deploy-level
	// (sshd MaxSessions, container process limits; see spec/mcp/arch_mcp.md).
	type deadliner interface{ SetReadDeadline(time.Time) error }
	if d, ok := conn.(deadliner); ok {
		_ = d.SetReadDeadline(time.Now().Add(cfg.Limits.HandshakeTimeoutOrDefault()))
	}
	br := bufio.NewReader(conn)
	auditOnly := func(fp, reason string) {
		e := audit.Event{Kind: "mcp", Fingerprint: fp, Action: "session", Verdict: "deny", Reason: reason}
		if err := audit.Append(cfg.AuditLog, e); err != nil {
			fmt.Fprintf(errw, "portitor-mcp: audit: %v\n", err)
		}
	}
	frame, err := mcpwire.ReadFrame(br, mcpwire.MaxHeaderBytes)
	if err != nil {
		auditOnly("", fmt.Sprintf("unreadable session header: %v", err))
		return
	}
	h, err := mcpwire.ParseHeader(frame)
	if err != nil {
		auditOnly("", fmt.Sprintf("malformed session header: %v", err))
		return
	}
	role := cfg.RoleFor(h.Fingerprint)
	if role == "" {
		auditOnly(h.Fingerprint, "unknown fingerprint (no role bound)")
		return
	}
	// Admitted: clear the header deadline — an idle admitted session is
	// legitimate for as long as the SSH connection lives.
	if d, ok := conn.(deadliner); ok {
		_ = d.SetReadDeadline(time.Time{})
	}
	s := &session{
		cfg:  cfg,
		fp:   h.Fingerprint,
		role: role,
		ups:  newUpstreamSet(cfg, spawn),
		errw: errw,
	}
	s.audit("session", "allow", "")
	defer s.ups.closeAll()
	s.loop(br, conn)
}

// audit appends one decision record; a write failure never changes a verdict
// — it is reported loudly instead.
func (s *session) audit(action, verdict, reason string) {
	e := audit.Event{Kind: "mcp", Fingerprint: s.fp, Role: s.role, Action: action, Verdict: verdict, Reason: reason}
	if err := audit.Append(s.cfg.AuditLog, e); err != nil {
		fmt.Fprintf(s.errw, "portitor-mcp: audit: %v\n", err)
	}
}

// loop serves the closed MCP subset, sequentially, until the client
// disconnects or the byte stream can no longer be framed (fatal — framing is
// the outermost trust boundary and does not degrade).
func (s *session) loop(br *bufio.Reader, out io.Writer) {
	maxMsg := s.cfg.Limits.MessageBytesOrDefault()
	respond := func(m mcpwire.Message) bool {
		if err := mcpwire.WriteFrame(out, m); err != nil {
			return false // client side gone; the session ends
		}
		return true
	}
	for {
		frame, err := mcpwire.ReadFrame(br, maxMsg)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			s.audit("session", "error", fmt.Sprintf("fatal framing error: %v", err))
			return
		}
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		msg, err := mcpwire.Parse(frame)
		if err != nil {
			// JSON-RPC 2.0: when the id is undetectable, the error response
			// carries an explicit null id, not an absent one.
			if !respond(mcpwire.NewError(json.RawMessage("null"), mcpwire.CodeParseError, "parse error")) {
				return
			}
			continue
		}
		var reply mcpwire.Message
		switch mcpwire.RouteOf(msg) {
		case mcpwire.RouteInitialize:
			r, err := mcpwire.NewResult(msg.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "portitor-mcp", "version": version},
			})
			if err != nil {
				r = mcpwire.NewError(msg.ID, mcpwire.CodeInvalidRequest, "internal error")
			}
			reply = r
		case mcpwire.RouteInitialized:
			continue // accepted, ignored
		case mcpwire.RoutePing:
			reply = mcpwire.NewRawResult(msg.ID, json.RawMessage(`{}`))
		case mcpwire.RouteToolsList:
			reply = s.toolsList(msg.ID)
		case mcpwire.RouteToolsCall:
			reply = s.toolsCall(msg)
		case mcpwire.RouteUnknownRequest:
			s.audit(msg.Method, "deny", "method outside the mediated subset")
			reply = mcpwire.NewError(msg.ID, mcpwire.CodeMethodNotFound, fmt.Sprintf("method %q is not mediated", msg.Method))
		case mcpwire.RouteUnknownNotification:
			continue // dropped: JSON-RPC forbids replying to a notification
		case mcpwire.RouteResponse:
			continue // a client-sent response correlates to nothing here: dropped
		default: // RouteInvalid
			if !msg.HasID() {
				continue
			}
			reply = mcpwire.NewError(msg.ID, mcpwire.CodeInvalidRequest, "not a request")
		}
		if !respond(reply) {
			return
		}
	}
}

// toolsList answers from config, pinned: exactly the caller's granted tools —
// the config's view, never an upstream's. No upstream is spawned for it.
func (s *session) toolsList(id json.RawMessage) mcpwire.Message {
	tools, err := s.cfg.ListTools(s.role)
	if err != nil {
		s.audit("tools/list", "error", err.Error())
		return mcpwire.NewError(id, mcpwire.CodeRefused, "tools/list failed")
	}
	if tools == nil {
		tools = []mcpconfig.ToolDescriptor{}
	}
	r, err := mcpwire.NewResult(id, map[string]any{"tools": tools})
	if err != nil {
		s.audit("tools/list", "error", err.Error())
		return mcpwire.NewError(id, mcpwire.CodeRefused, "tools/list failed")
	}
	return r
}

// callParams is the strict tools/call params shape: exactly name (+ optional
// arguments). Unknown keys — _meta included — are refused: everything the
// mediator does not understand is something it must not forward.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// parseCallParams strictly decodes a tools/call params object. The envelope
// gets the same duplicate-key refusal as the arguments and the config — the
// silent-last-wins shadowing class is refused everywhere an attacker-authored
// object is decoded.
func parseCallParams(raw json.RawMessage) (callParams, error) {
	var p callParams
	if len(raw) == 0 {
		return p, errors.New("params required")
	}
	if err := mcpschema.CheckObject(raw, "params"); err != nil {
		return p, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("params: %v", err)
	}
	if dec.More() {
		return p, errors.New("params: trailing content")
	}
	if p.Name == "" {
		return p, errors.New("params: name required")
	}
	return p, nil
}

// toolsCall runs the mediation path: strict params decode, default-deny role
// check, allowlist schema validation, canonical re-serialization, forward,
// relay. Every decision is audited.
func (s *session) toolsCall(msg mcpwire.Message) mcpwire.Message {
	p, err := parseCallParams(msg.Params)
	if err != nil {
		s.audit("tools/call", "deny", err.Error())
		return mcpwire.NewError(msg.ID, mcpwire.CodeInvalidParams, err.Error())
	}
	deny := func(reason string) mcpwire.Message {
		s.audit(p.Name, "deny", reason)
		return mcpwire.NewError(msg.ID, mcpwire.CodeRefused, reason)
	}
	fail := func(reason string) mcpwire.Message {
		s.audit(p.Name, "error", reason)
		return mcpwire.NewError(msg.ID, mcpwire.CodeUpstreamFailure, reason)
	}
	tool, ok := s.cfg.Tools[p.Name]
	if !ok {
		return deny(fmt.Sprintf("unknown tool %q (the tool list is pinned in config)", p.Name))
	}
	if !s.cfg.Granted(s.role, p.Name) {
		return deny(fmt.Sprintf("role %q may not call %q (tool_roles is default-deny)", s.role, p.Name))
	}
	canonical, err := mcpschema.Validate(tool.Spec(), p.Arguments)
	if err != nil {
		return deny(fmt.Sprintf("shape validation failed: %v", err))
	}
	up, err := s.ups.ensure(tool.Server)
	if err != nil {
		return fail(fmt.Sprintf("upstream %q: %v", tool.Server, err))
	}
	if reason, refused := up.toolRefusal[p.Name]; refused {
		return deny(fmt.Sprintf("pinned schema mismatch: %s", reason))
	}
	result, rpcErr, err := up.call(tool.UpstreamName, canonical, s.cfg.Limits.CallTimeoutOrDefault())
	if err != nil {
		return fail(fmt.Sprintf("upstream %q: %v", tool.Server, err))
	}
	s.audit(p.Name, "allow", "")
	if rpcErr != nil {
		// The upstream's own error is the tool's outcome — relayed, not
		// re-shaped, under the caller's id.
		return mcpwire.Message{JSONRPC: "2.0", ID: msg.ID, Error: rpcErr}
	}
	return mcpwire.NewRawResult(msg.ID, result)
}
