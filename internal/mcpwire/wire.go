// Package mcpwire implements the JSON-RPC 2.0 / MCP stdio framing and the
// closed method routing table for portitor-mcp (see spec/mcp/arch_mcp.md).
// Everything here is pure over bytes and messages, so the security-critical
// dispatch is unit- and fuzz-testable like the SSH shell's classify().
package mcpwire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Message is one JSON-RPC 2.0 message: a request/notification (Method set), or
// a response (Result or Error set). IDs stay raw bytes end to end — the
// mediator never interprets a caller's id, only echoes it.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the JSON-RPC error object of an error response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// JSON-RPC error codes used by the mediator. The refusal classes beyond the
// protocol-defined ones live in the -32000..-32099 server range.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	// CodeRefused is a mediation refusal (role, shape, pinning) — visible,
	// with the reason class in the message, never silent.
	CodeRefused = -32000
	// CodeUpstreamFailure is an operational upstream error (spawn, timeout,
	// death, oversized response).
	CodeUpstreamFailure = -32001
)

// HasID reports whether the message carries a usable id: present and not JSON
// null. A method-carrying message without one is a notification — JSON-RPC
// forbids replying to it.
func (m Message) HasID() bool {
	return len(m.ID) > 0 && !bytes.Equal(m.ID, []byte("null"))
}

// Parse decodes one frame into a Message, refusing anything that is not a
// JSON-RPC 2.0 object. It stays deliberately lenient beyond the version check
// — RouteOf classifies the kinds, and the shape validation that matters guards
// the forwarded surface (tools/call arguments), not the envelope.
func Parse(frame []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(frame, &m); err != nil {
		return m, fmt.Errorf("mcpwire: parse: %w", err)
	}
	if m.JSONRPC != "2.0" {
		return m, fmt.Errorf("mcpwire: jsonrpc must be \"2.0\", got %q", m.JSONRPC)
	}
	return m, nil
}

// NewResult builds a result response for id. Marshalling v is the caller's
// contract to satisfy; a marshal failure is a programming error surfaced loudly.
func NewResult(id json.RawMessage, v any) (Message, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Message{}, fmt.Errorf("mcpwire: marshal result: %w", err)
	}
	return Message{JSONRPC: "2.0", ID: id, Result: raw}, nil
}

// NewRawResult builds a result response for id relaying already-serialized
// result bytes verbatim (the upstream-relay path).
func NewRawResult(id, result json.RawMessage) Message {
	return Message{JSONRPC: "2.0", ID: id, Result: result}
}

// NewError builds an error response for id.
func NewError(id json.RawMessage, code int, msg string) Message {
	return Message{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
}

// ErrFrameTooLarge reports a frame exceeding the configured cap — a fatal
// framing error: the byte stream can no longer be trusted to frame, so the
// session (or upstream) does not degrade, it ends.
var ErrFrameTooLarge = errors.New("mcpwire: frame exceeds the configured cap")

// ReadFrame reads one newline-delimited frame of at most max bytes (delimiter
// excluded). io.EOF is returned only on a clean end (no partial frame);
// unterminated trailing bytes are an error, and an over-cap frame is
// ErrFrameTooLarge without reading it to completion.
func ReadFrame(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		// +2 admits the at-most-two delimiter bytes ("\r\n"); the exact check
		// runs on the trimmed frame below.
		if len(buf) > max+2 {
			return nil, ErrFrameTooLarge
		}
		switch {
		case err == nil:
			frame := bytes.TrimRight(buf, "\r\n")
			if len(frame) > max {
				return nil, ErrFrameTooLarge
			}
			return frame, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(buf) == 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("mcpwire: unterminated frame at stream end")
		default:
			return nil, fmt.Errorf("mcpwire: read frame: %w", err)
		}
	}
}

// WriteFrame writes one message as a single newline-terminated frame.
func WriteFrame(w io.Writer, m Message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("mcpwire: marshal frame: %w", err)
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("mcpwire: write frame: %w", err)
	}
	return nil
}

// ParseTarget parses the splice-target grammar shared by the git-side
// dispatcher (PORTITOR_MCP_TARGET) and the mediator's listener
// (--listen/$PORTITOR_MCP_LISTEN): "unix:<path>" or "tcp:<host>:<port>",
// returned as the (network, address) pair net.Dial/net.Listen take. The
// grammar is closed — anything else is an error, fail-closed.
func ParseTarget(s string) (network, address string, err error) {
	switch {
	case strings.HasPrefix(s, "unix:"):
		if p := strings.TrimPrefix(s, "unix:"); p != "" {
			return "unix", p, nil
		}
		return "", "", errors.New("mcpwire: unix target has an empty path")
	case strings.HasPrefix(s, "tcp:"):
		hostport := strings.TrimPrefix(s, "tcp:")
		host, port, ok := strings.Cut(hostport, ":")
		if ok && host != "" && port != "" {
			return "tcp", hostport, nil
		}
		return "", "", fmt.Errorf("mcpwire: tcp target %q is not host:port", hostport)
	case s == "":
		return "", "", errors.New("mcpwire: empty target")
	}
	return "", "", fmt.Errorf("mcpwire: target %q is not unix:<path> or tcp:<host>:<port>", s)
}

// Header is the one-line JSON header the git-side dispatcher writes on a
// spliced connection before any MCP byte: the caller identity it verified via
// SSH. Strict-decoded by the mediator; the splice target's reachability is
// the trust boundary for the assertion.
type Header struct {
	Fingerprint string `json:"fingerprint"`
}

// MaxHeaderBytes caps the header line — it carries one fingerprint, nothing
// else.
const MaxHeaderBytes = 4096

// ParseHeader strictly decodes one header frame (unknown fields refused).
func ParseHeader(frame []byte) (Header, error) {
	var h Header
	dec := json.NewDecoder(bytes.NewReader(frame))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return h, fmt.Errorf("mcpwire: parse header: %w", err)
	}
	if dec.More() {
		return h, errors.New("mcpwire: trailing content after the header")
	}
	if h.Fingerprint == "" {
		return h, errors.New("mcpwire: header has no fingerprint")
	}
	return h, nil
}

// Route is the closed classification of an incoming message.
type Route int

const (
	RouteInvalid Route = iota // neither a well-formed request/notification nor a response
	RouteInitialize
	RouteInitialized // notifications/initialized
	RoutePing
	RouteToolsList
	RouteToolsCall
	RouteUnknownRequest      // a request outside the subset: answered -32601
	RouteUnknownNotification // a notification outside the subset: dropped
	RouteResponse            // a result/error message (only upstreams send these to us)
)

// RouteOf classifies one message against the closed method table
// (spec/mcp/arch_mcp.md). Pure — the mediation loop dispatches on the result
// and nothing else decides what a method is allowed to be.
func RouteOf(m Message) Route {
	if m.Method == "" {
		if m.Result != nil || m.Error != nil {
			return RouteResponse
		}
		return RouteInvalid
	}
	if m.HasID() {
		switch m.Method {
		case "initialize":
			return RouteInitialize
		case "ping":
			return RoutePing
		case "tools/list":
			return RouteToolsList
		case "tools/call":
			return RouteToolsCall
		}
		return RouteUnknownRequest
	}
	if m.Method == "notifications/initialized" {
		return RouteInitialized
	}
	return RouteUnknownNotification
}
