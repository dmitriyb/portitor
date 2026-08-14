package mcpconfig

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/portitor/internal/mcpschema"
)

var testFP = "SHA256:" + strings.Repeat("a", 43)

// validBody is a fully-populated valid config the parse/validate tests mutate.
func validBody() string {
	return `{
  "format_version": 1,
  "roles": {"` + testFP + `": "implementer"},
  "servers": {"tracker": {"command": ["tracker-mcp-server", "--flag"], "env": ["TRACKER_TOKEN"]}},
  "tools": {
    "tracker.search": {
      "server": "tracker",
      "upstream_name": "search",
      "description": "Search tracker issues",
      "params": {"fields": {"query": {"type": "string", "required": true}, "maxResults": {"type": "integer"}}}
    },
    "tracker.get": {"server": "tracker", "upstream_name": "get"}
  },
  "tool_roles": {"tracker.search": ["implementer", "reviewer"], "tracker.get": []},
  "audit_log": "/tmp/audit.jsonl",
  "limits": {"message_bytes": 1024, "call_timeout": "10s"}
}`
}

func TestParseValid(t *testing.T) {
	s, err := Parse([]byte(validBody()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p := Validate(s); len(p) != 0 {
		t.Fatalf("valid config has problems: %v", p)
	}
	if s.Tools["tracker.search"].UpstreamName != "search" {
		t.Fatalf("tools: %+v", s.Tools)
	}
	// camelCase argument-field names are DATA (the fields map is exempt from
	// the lowercase rule) — maxResults must survive the raw-key walk.
	if _, ok := s.Tools["tracker.search"].Params.Fields["maxResults"]; !ok {
		t.Fatal("camelCase field name lost")
	}
}

func TestParseDiscipline(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"unknown top-level key", `{"format_version":1,"surprise":1}`, "unknown top-level key"},
		{"duplicate top-level key", `{"format_version":1,"roles":{},"roles":{}}`, "duplicate key"},
		{"duplicate nested key", `{"format_version":1,"servers":{"s":{"command":["x"],"command":["y"]}}}`, "duplicate key"},
		{"uppercase schema key", `{"format_version":1,"limits":{"Message_bytes":1}}`, "must be lowercase"},
		{"trailing content", `{"format_version":1} {}`, "trailing content"},
		{"non-object top level", `[1]`, "top level must be a JSON object"},
		{"missing format_version", `{"roles":{}}`, "format_version 0 is not supported"},
		{"future format_version", `{"format_version":2}`, "format_version 2 is not supported"},
		// A misspelled field-spec key inside params.fields must refuse the
		// whole config (DisallowUnknownFields reaches every schema object).
		{"unknown field-spec key", `{"format_version":1,"servers":{"s":{"command":["x"]}},` +
			`"tools":{"t":{"server":"s","upstream_name":"t",` +
			`"params":{"fields":{"a":{"type":"string","requird":true}}}}}}`, "unknown field"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.body))
			if err == nil {
				t.Fatalf("accepted %s", c.body)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestValidateRefusals(t *testing.T) {
	mutate := func(f func(*Settings)) []string {
		s, err := Parse([]byte(validBody()))
		if err != nil {
			t.Fatal(err)
		}
		f(&s)
		return Validate(s)
	}
	cases := []struct {
		name string
		f    func(*Settings)
		want string
	}{
		{"non-fingerprint roles key", func(s *Settings) { s.Roles["deadbeef"] = "r" }, "not a fingerprint"},
		{"empty role", func(s *Settings) { s.Roles[testFP] = "" }, "empty role name"},
		{"empty server command", func(s *Settings) { s.Servers["tracker"] = Server{Env: []string{"A"}} }, "command is empty"},
		{"empty argv element", func(s *Settings) { s.Servers["tracker"] = Server{Command: []string{"x", ""}} }, "command[1] is empty"},
		{"empty env name", func(s *Settings) { s.Servers["tracker"] = Server{Command: []string{"x"}, Env: []string{""}} }, "env[0] is empty"},
		{"unknown server", func(s *Settings) {
			tool := s.Tools["tracker.search"]
			tool.Server = "nope"
			s.Tools["tracker.search"] = tool
		}, "unknown server"},
		{"empty upstream_name", func(s *Settings) {
			tool := s.Tools["tracker.search"]
			tool.UpstreamName = ""
			s.Tools["tracker.search"] = tool
		}, "upstream_name is empty"},
		{"non-compiling params", func(s *Settings) {
			s.Tools["tracker.search"].Params.Fields["bad"] = mcpschema.Field{Type: "array"}
		}, "type must be"},
		{"tool_roles unknown tool", func(s *Settings) { s.ToolRoles["nope"] = []string{"r"} }, "unknown tool"},
		{"tool_roles empty role", func(s *Settings) { s.ToolRoles["tracker.get"] = []string{""} }, "empty role name"},
		{"negative message_bytes", func(s *Settings) { s.Limits.MessageBytes = -1 }, "must not be negative"},
		{"bad duration", func(s *Settings) { s.Limits.CallTimeout = "banana" }, "not a valid Go duration"},
		{"non-positive duration", func(s *Settings) { s.Limits.HandshakeTimeout = "-3s" }, "must be positive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := mutate(c.f)
			if len(p) == 0 {
				t.Fatal("expected problems")
			}
			if !strings.Contains(strings.Join(p, "; "), c.want) {
				t.Fatalf("problems %v do not mention %q", p, c.want)
			}
		})
	}
}

func TestViews(t *testing.T) {
	s, err := Parse([]byte(validBody()))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.RoleFor(testFP); got != "implementer" {
		t.Fatalf("RoleFor = %q", got)
	}
	if got := s.RoleFor("SHA256:unknown"); got != "" {
		t.Fatalf("unknown fingerprint role = %q", got)
	}
	// Default-deny in every shape: granted, other role, empty roles list,
	// absent tool, empty role.
	if !s.Granted("implementer", "tracker.search") {
		t.Fatal("granted role refused")
	}
	if s.Granted("merger", "tracker.search") {
		t.Fatal("unlisted role granted")
	}
	if s.Granted("implementer", "tracker.get") {
		t.Fatal("empty tool_roles list must refuse everyone")
	}
	if s.Granted("implementer", "nope") {
		t.Fatal("unknown tool granted")
	}
	if s.Granted("", "tracker.search") {
		t.Fatal("empty role granted")
	}

	tools, err := s.ListTools("implementer")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "tracker.search" {
		t.Fatalf("ListTools = %+v", tools)
	}
	if tools[0].Description != "Search tracker issues" {
		t.Fatalf("descriptor: %+v", tools[0])
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil || len(schema.Required) != 1 {
		t.Fatalf("inputSchema: %s (%v)", tools[0].InputSchema, err)
	}
	if none, err := s.ListTools("stranger"); err != nil || len(none) != 0 {
		t.Fatalf("stranger tools = %+v, %v", none, err)
	}
}

func TestLimitsDefaults(t *testing.T) {
	var l *Limits
	if l.MessageBytesOrDefault() != DefaultMessageBytes ||
		l.UpstreamMessageBytesOrDefault() != DefaultUpstreamMessageBytes ||
		l.HandshakeTimeoutOrDefault() != DefaultHandshakeTimeout ||
		l.CallTimeoutOrDefault() != DefaultCallTimeout {
		t.Fatal("nil limits must yield the defaults")
	}
	set := &Limits{MessageBytes: 42, CallTimeout: "1s"}
	if set.MessageBytesOrDefault() != 42 || set.CallTimeoutOrDefault() != time.Second {
		t.Fatal("set limits must win")
	}
	if set.UpstreamMessageBytesOrDefault() != DefaultUpstreamMessageBytes || set.HandshakeTimeoutOrDefault() != DefaultHandshakeTimeout {
		t.Fatal("unset fields must fall back")
	}
}
