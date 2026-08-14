package mcpwire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	m, err := Parse([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Method != "ping" || string(m.ID) != "1" {
		t.Fatalf("parsed %+v", m)
	}
	for _, bad := range []string{
		`{"id":1,"method":"ping"}`,              // no jsonrpc
		`{"jsonrpc":"1.0","id":1,"method":"m"}`, // wrong version
		`[1,2,3]`,                               // not an object
		`not json`,                              // garbage
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestHasID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{`1`, true}, {`"abc"`, true}, {`0`, true}, {`null`, false}, {``, false},
	}
	for _, c := range cases {
		m := Message{ID: json.RawMessage(c.id)}
		if c.id == "" {
			m.ID = nil
		}
		if got := m.HasID(); got != c.want {
			t.Errorf("HasID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

func TestRouteOf(t *testing.T) {
	req := func(method string) Message {
		return Message{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: method}
	}
	notif := func(method string) Message { return Message{JSONRPC: "2.0", Method: method} }
	cases := []struct {
		msg  Message
		want Route
	}{
		{req("initialize"), RouteInitialize},
		{req("ping"), RoutePing},
		{req("tools/list"), RouteToolsList},
		{req("tools/call"), RouteToolsCall},
		{req("resources/list"), RouteUnknownRequest},
		{req("notifications/initialized"), RouteUnknownRequest}, // with an id it is a request, outside the subset
		{notif("notifications/initialized"), RouteInitialized},
		{notif("notifications/cancelled"), RouteUnknownNotification},
		{notif("tools/call"), RouteUnknownNotification}, // no id: not a request, and not the one known notification
		{Message{JSONRPC: "2.0", ID: json.RawMessage(`1`), Result: json.RawMessage(`{}`)}, RouteResponse},
		{Message{JSONRPC: "2.0", ID: json.RawMessage(`1`), Error: &RPCError{Code: 1}}, RouteResponse},
		{Message{JSONRPC: "2.0", ID: json.RawMessage(`1`)}, RouteInvalid},
		{Message{JSONRPC: "2.0", ID: json.RawMessage(`null`), Method: "ping"}, RouteUnknownNotification}, // null id = notification, outside the subset
	}
	for i, c := range cases {
		if got := RouteOf(c.msg); got != c.want {
			t.Errorf("case %d (%+v): RouteOf = %v, want %v", i, c.msg, got, c.want)
		}
	}
}

func TestReadFrame(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("abc\r\ndef\n"))
	f, err := ReadFrame(br, 100)
	if err != nil || string(f) != "abc" {
		t.Fatalf("frame 1: %q, %v", f, err)
	}
	f, err = ReadFrame(br, 100)
	if err != nil || string(f) != "def" {
		t.Fatalf("frame 2: %q, %v", f, err)
	}
	if _, err = ReadFrame(br, 100); !errors.Is(err, io.EOF) {
		t.Fatalf("clean end: want io.EOF, got %v", err)
	}

	// Unterminated trailing bytes are an error, not a frame.
	br = bufio.NewReader(strings.NewReader("partial"))
	if _, err := ReadFrame(br, 100); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("unterminated frame: want a non-EOF error, got %v", err)
	}

	// The cap refuses an oversized frame — including one far larger than the
	// bufio buffer (the cap must not depend on a single ReadSlice).
	big := strings.Repeat("x", 100_000) + "\n"
	if _, err := ReadFrame(bufio.NewReader(strings.NewReader(big)), 1000); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized frame: want ErrFrameTooLarge, got %v", err)
	}
	// Exactly at the cap passes.
	exact := strings.Repeat("x", 1000) + "\n"
	if f, err := ReadFrame(bufio.NewReader(strings.NewReader(exact)), 1000); err != nil || len(f) != 1000 {
		t.Fatalf("at-cap frame: len=%d, %v", len(f), err)
	}
}

func TestWriteFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, NewError(json.RawMessage(`7`), CodeMethodNotFound, "nope")); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("frame must be one newline-terminated line: %q", line)
	}
	m, err := Parse([]byte(strings.TrimSuffix(line, "\n")))
	if err != nil || m.Error == nil || m.Error.Code != CodeMethodNotFound || string(m.ID) != "7" {
		t.Fatalf("round-trip: %+v, %v", m, err)
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in, network, addr string
		ok                bool
	}{
		{"unix:/run/mcp.sock", "unix", "/run/mcp.sock", true},
		{"tcp:mediator:9000", "tcp", "mediator:9000", true},
		{"tcp:127.0.0.1:9000", "tcp", "127.0.0.1:9000", true},
		{"unix:", "", "", false},
		{"tcp:hostonly", "", "", false},
		{"tcp::9000", "", "", false},
		{"tcp:host:", "", "", false},
		{"udp:host:1", "", "", false},
		{"/run/mcp.sock", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		network, addr, err := ParseTarget(c.in)
		if c.ok != (err == nil) {
			t.Errorf("ParseTarget(%q) err=%v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && (network != c.network || addr != c.addr) {
			t.Errorf("ParseTarget(%q) = (%q,%q), want (%q,%q)", c.in, network, addr, c.network, c.addr)
		}
	}
}

func TestParseHeader(t *testing.T) {
	h, err := ParseHeader([]byte(`{"fingerprint":"SHA256:abc"}`))
	if err != nil || h.Fingerprint != "SHA256:abc" {
		t.Fatalf("header: %+v, %v", h, err)
	}
	for _, bad := range []string{
		`{}`,                            // no fingerprint
		`{"fingerprint":""}`,            // empty
		`{"fingerprint":"x","extra":1}`, // unknown field
		`{"fingerprint":"x"} trailing`,  // trailing content
		`"just a string"`,               // not an object
		``,                              // empty
	} {
		if _, err := ParseHeader([]byte(bad)); err == nil {
			t.Errorf("ParseHeader(%q) accepted", bad)
		}
	}
}

// FuzzRoute: Parse never panics, and every parsed message lands in exactly
// one route of the closed table.
func FuzzRoute(f *testing.F) {
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`)
	f.Add(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	f.Add(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	f.Add(`{"jsonrpc":"2.0","id":null,"method":"ping"}`)
	f.Add(`garbage`)
	f.Fuzz(func(t *testing.T, in string) {
		m, err := Parse([]byte(in))
		if err != nil {
			return
		}
		switch RouteOf(m) {
		case RouteInitialize, RoutePing, RouteToolsList, RouteToolsCall:
			if !m.HasID() {
				t.Fatalf("subset request route without an id: %+v", m)
			}
		case RouteInitialized, RouteUnknownNotification:
			if m.HasID() {
				t.Fatalf("notification route with an id: %+v", m)
			}
		case RouteUnknownRequest:
			if !m.HasID() || m.Method == "" {
				t.Fatalf("unknown-request route must have id+method: %+v", m)
			}
		case RouteResponse:
			if m.Method != "" {
				t.Fatalf("response route with a method: %+v", m)
			}
		case RouteInvalid:
		default:
			t.Fatalf("route outside the closed table for %+v", m)
		}
	})
}

// FuzzReadFrame: never panics, never returns a frame longer than the cap.
func FuzzReadFrame(f *testing.F) {
	f.Add("abc\ndef\n", 10)
	f.Add(strings.Repeat("x", 5000), 100)
	f.Add("\n\n\n", 1)
	f.Fuzz(func(t *testing.T, in string, max int) {
		if max < 0 || max > 1<<16 {
			return
		}
		br := bufio.NewReader(strings.NewReader(in))
		for {
			frame, err := ReadFrame(br, max)
			if err != nil {
				return
			}
			if len(frame) > max {
				t.Fatalf("frame of %d bytes exceeds cap %d", len(frame), max)
			}
		}
	})
}
