// Package mcpconfig loads, strictly decodes, and validates portitor-mcp's
// configuration — its own file in its own trust domain (never repos.d/; the
// MCP surface is not per-repo and the mediator is a separate privilege
// domain). The decode discipline mirrors internal/config's fail-closed raw-key
// walk; the package deliberately does not import internal/config (that would
// pull the whole git side into the mediator binary for a regexp).
package mcpconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dmitriyb/portitor/internal/mcpschema"
)

// SupportedFormatVersion is the only mediator-config format_version this
// binary operates with — missing, lower, or higher refuses at load, same
// posture as the gate config.
const SupportedFormatVersion = 1

// Default limits (see spec/mcp/arch_mcp.md). Overridable per deployment via
// the config's limits block; the defaults live here as the fallback the
// nil-safe accessors apply.
const (
	DefaultMessageBytes         = 1 << 20 // 1 MiB: client→mediator frame cap
	DefaultUpstreamMessageBytes = 8 << 20 // 8 MiB: upstream→mediator frame cap
	DefaultHandshakeTimeout     = 30 * time.Second
	DefaultCallTimeout          = 120 * time.Second
)

// Settings is the mediator's configuration.
type Settings struct {
	FormatVersion int `json:"format_version"`
	// Roles maps a caller's SSH key fingerprint to a role, as in repos.d.
	Roles map[string]string `json:"roles"`
	// Servers are the upstream MCP servers: argv (no shell, ever) + the env
	// NAMES injected from the mediator's own environment. Credentials never
	// appear in this file.
	Servers map[string]Server `json:"servers"`
	// Tools is the pinned tool list: exposed name → owning server, the name
	// the upstream advertises, and the allowlist params schema.
	Tools map[string]Tool `json:"tools"`
	// ToolRoles maps each exposed tool to the roles allowed to call it —
	// default-deny, the action_roles shape: a tool absent here, or listed
	// with no roles, is refused for everyone.
	ToolRoles map[string][]string `json:"tool_roles"`
	// AuditLog, when set, receives one JSON line per decision. Empty disables
	// the trail. Write failures never change a verdict.
	AuditLog string `json:"audit_log"`
	// Limits are the mechanism bounds (frame caps, timeouts); absent block or
	// fields fall back to the documented defaults via the OrDefault accessors.
	Limits *Limits `json:"limits"`
}

// Server is one upstream MCP server definition.
type Server struct {
	Command []string `json:"command"`
	Env     []string `json:"env"`
}

// Tool is one exposed tool definition.
type Tool struct {
	Server       string `json:"server"`
	UpstreamName string `json:"upstream_name"`
	Description  string `json:"description"`
	// Params is the allowlist argument schema. Absent means the tool accepts
	// no arguments at all (everything not explicitly allowed is refused).
	Params *mcpschema.Spec `json:"params"`
}

// Spec returns the tool's params allowlist, absent meaning "no arguments".
func (t Tool) Spec() mcpschema.Spec {
	if t.Params == nil {
		return mcpschema.Spec{}
	}
	return *t.Params
}

// Limits are the mediator's mechanism bounds.
type Limits struct {
	MessageBytes         int    `json:"message_bytes"`
	UpstreamMessageBytes int    `json:"upstream_message_bytes"`
	HandshakeTimeout     string `json:"handshake_timeout"`
	CallTimeout          string `json:"call_timeout"`
}

// The nil-safe accessors: absent block, absent field, or (for the durations)
// a value that fails to parse positive falls back to the default. Validate
// flags malformed values so a typo surfaces at validate-config/boot rather
// than silently falling back here.

func (l *Limits) MessageBytesOrDefault() int {
	if l == nil || l.MessageBytes <= 0 {
		return DefaultMessageBytes
	}
	return l.MessageBytes
}

func (l *Limits) UpstreamMessageBytesOrDefault() int {
	if l == nil || l.UpstreamMessageBytes <= 0 {
		return DefaultUpstreamMessageBytes
	}
	return l.UpstreamMessageBytes
}

func (l *Limits) HandshakeTimeoutOrDefault() time.Duration {
	return durationOrDefault(l, func(l *Limits) string { return l.HandshakeTimeout }, DefaultHandshakeTimeout)
}

func (l *Limits) CallTimeoutOrDefault() time.Duration {
	return durationOrDefault(l, func(l *Limits) string { return l.CallTimeout }, DefaultCallTimeout)
}

func durationOrDefault(l *Limits, get func(*Limits) string, def time.Duration) time.Duration {
	if l == nil {
		return def
	}
	d, err := time.ParseDuration(get(l))
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// LoadFile reads and strictly parses one mediator config file.
func LoadFile(path string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("read config %s: %w", path, err)
	}
	s, err = Parse(b)
	if err != nil {
		return s, fmt.Errorf("config %s: %w", path, err)
	}
	return s, nil
}

// Parse decodes one config buffer through the full discipline — token-level
// key check (exact top-level keys, duplicates refused everywhere, lowercase
// schema keys with the data maps exempt), strict decode, trailing-content
// rejection, and the format-version guard — all fail-closed before any
// consumer sees the config.
func Parse(b []byte) (Settings, error) {
	var s Settings
	if err := checkRawKeys(b); err != nil {
		return s, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("parse: %w", err)
	}
	if dec.More() {
		return s, fmt.Errorf("trailing content after the config object")
	}
	if s.FormatVersion != SupportedFormatVersion {
		return s, fmt.Errorf("format_version %d is not supported by this binary (want %d); refusing to operate with a partially understood config",
			s.FormatVersion, SupportedFormatVersion)
	}
	return s, nil
}

// topLevelKeys is the exact (byte-exact, case-sensitive) allowed top-level key
// set of the mediator config.
var topLevelKeys = map[string]bool{
	"format_version": true,
	"roles":          true,
	"servers":        true,
	"tools":          true,
	"tool_roles":     true,
	"audit_log":      true,
	"limits":         true,
}

// dataMapKeys names the keys whose object values are DATA maps (fingerprints,
// server names, tool names, argument-field names) rather than schema objects:
// they keep the duplicate-key check but are exempt from the lowercase-key
// rule. "fields" is the params matcher's argument-name map at any depth —
// upstream tools legitimately name camelCase arguments.
var dataMapKeys = map[string]bool{
	"roles": true, "servers": true, "tools": true, "tool_roles": true, "fields": true,
}

// checkRawKeys walks the raw JSON tokens and rejects, fail-closed: a
// non-object top level, unknown or non-byte-exact top-level keys, duplicate
// keys in any object, and non-lowercase keys in schema objects (Go's
// case-insensitive field matching must never let a stale "Roles" resurrect a
// revoked binding).
func checkRawKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("top level must be a JSON object")
	}
	return walkObject(dec, true, false)
}

func walkObject(dec *json.Decoder, top, dataMap bool) error {
	seen := map[string]bool{}
	for {
		t, err := dec.Token()
		if err != nil {
			return fmt.Errorf("parse: %w", err)
		}
		if d, ok := t.(json.Delim); ok && d == '}' {
			return nil
		}
		key, ok := t.(string)
		if !ok {
			return fmt.Errorf("parse: unexpected token %v", t)
		}
		if seen[key] {
			return fmt.Errorf("duplicate key %q (silent last-wins could shadow a live value)", key)
		}
		seen[key] = true
		switch {
		case top && !topLevelKeys[key]:
			return fmt.Errorf("unknown top-level key %q (keys are byte-exact; known: lowercase snake_case)", key)
		case !top && !dataMap && key != strings.ToLower(key):
			return fmt.Errorf("key %q must be lowercase (a differently-cased key can shadow the real field)", key)
		}
		// The data-map exemption applies to the object VALUE of a data-map key
		// (its keys are data); "fields" carries it at any depth, the rest at
		// the top level only.
		if err := walkValue(dec, dataMapKeys[key] && (top || key == "fields")); err != nil {
			return err
		}
	}
}

func walkValue(dec *json.Decoder, dataMap bool) error {
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if d, ok := t.(json.Delim); ok {
		switch d {
		case '{':
			return walkObject(dec, false, dataMap)
		case '[':
			for dec.More() {
				if err := walkValue(dec, false); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return fmt.Errorf("parse: %w", err)
			}
		}
	}
	return nil
}

// fingerprintRe matches an SSH key fingerprint ("SHA256:" + 43 chars of
// unpadded base64), the same shape internal/config.ValidFingerprint pins for
// the gate's roles map (duplicated here — see the package comment).
var fingerprintRe = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// Validate returns the problems with a config (empty slice = valid) — every
// refusal named, so validate-config and serve-at-boot fail closed with the
// full list (see spec/mcp/arch_mcp.md).
func Validate(s Settings) []string {
	var problems []string
	if s.FormatVersion != SupportedFormatVersion {
		problems = append(problems, fmt.Sprintf("format_version %d is not supported by this binary (want %d)", s.FormatVersion, SupportedFormatVersion))
	}
	for fp, role := range s.Roles {
		if !fingerprintRe.MatchString(fp) {
			problems = append(problems, fmt.Sprintf("roles key %q is not a fingerprint (want SHA256: + 43 base64 chars); it can never match a real caller", fp))
		}
		if role == "" {
			problems = append(problems, fmt.Sprintf("roles[%q] has an empty role name", fp))
		}
	}
	for name, srv := range s.Servers {
		if name == "" {
			problems = append(problems, "servers has an empty server name")
		}
		if len(srv.Command) == 0 {
			problems = append(problems, fmt.Sprintf("servers[%q]: command is empty", name))
		}
		for i, a := range srv.Command {
			if a == "" {
				problems = append(problems, fmt.Sprintf("servers[%q]: command[%d] is empty", name, i))
			}
		}
		for i, e := range srv.Env {
			if e == "" {
				problems = append(problems, fmt.Sprintf("servers[%q]: env[%d] is empty", name, i))
			}
		}
	}
	for name, tool := range s.Tools {
		if name == "" {
			problems = append(problems, "tools has an empty tool name")
		}
		if _, ok := s.Servers[tool.Server]; !ok {
			problems = append(problems, fmt.Sprintf("tools[%q]: unknown server %q", name, tool.Server))
		}
		if tool.UpstreamName == "" {
			problems = append(problems, fmt.Sprintf("tools[%q]: upstream_name is empty", name))
		}
		for _, p := range mcpschema.Compile(tool.Spec()) {
			problems = append(problems, fmt.Sprintf("tools[%q]: %s", name, p))
		}
	}
	for name, roles := range s.ToolRoles {
		if _, ok := s.Tools[name]; !ok {
			problems = append(problems, fmt.Sprintf("tool_roles: unknown tool %q", name))
		}
		for i, r := range roles {
			if r == "" {
				problems = append(problems, fmt.Sprintf("tool_roles[%q][%d]: empty role name", name, i))
			}
		}
	}
	if s.Limits != nil {
		if s.Limits.MessageBytes < 0 {
			problems = append(problems, fmt.Sprintf("limits.message_bytes must not be negative, got %d", s.Limits.MessageBytes))
		}
		if s.Limits.UpstreamMessageBytes < 0 {
			problems = append(problems, fmt.Sprintf("limits.upstream_message_bytes must not be negative, got %d", s.Limits.UpstreamMessageBytes))
		}
		for field, v := range map[string]string{
			"limits.handshake_timeout": s.Limits.HandshakeTimeout,
			"limits.call_timeout":      s.Limits.CallTimeout,
		} {
			if v == "" {
				continue
			}
			if d, err := time.ParseDuration(v); err != nil {
				problems = append(problems, fmt.Sprintf("%s %q is not a valid Go duration: %v", field, v, err))
			} else if d <= 0 {
				problems = append(problems, fmt.Sprintf("%s %q must be positive", field, v))
			}
		}
	}
	return problems
}

// RoleFor resolves a caller fingerprint to its role ("" = unknown caller).
func (s Settings) RoleFor(fingerprint string) string { return s.Roles[fingerprint] }

// Granted reports whether role may call tool — default-deny: a tool absent
// from tool_roles, or listed with no roles, is refused for everyone.
func (s Settings) Granted(role, tool string) bool {
	if role == "" {
		return false
	}
	for _, r := range s.ToolRoles[tool] {
		if r == role {
			return true
		}
	}
	return false
}

// ToolDescriptor is one tools/list entry — the config's pinned view, never an
// upstream's advertisement.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListTools assembles the tools/list response body for one role: exactly the
// granted tools, sorted by name, each with the inputSchema synthesized from
// its params allowlist.
func (s Settings) ListTools(role string) ([]ToolDescriptor, error) {
	var out []ToolDescriptor
	for name, tool := range s.Tools {
		if !s.Granted(role, name) {
			continue
		}
		schema, err := mcpschema.SynthesizeInputSchema(tool.Spec())
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", name, err)
		}
		out = append(out, ToolDescriptor{Name: name, Description: tool.Description, InputSchema: schema})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
