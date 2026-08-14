// Package mcpschema is portitor-mcp's allowlist argument matcher: the
// deliberately minimal per-tool params grammar (required/optional fields,
// primitive types, enum/const — no objects, no arrays), strict validation of a
// tools/call's arguments against it, canonical re-serialization of the
// accepted values, the tools/list inputSchema synthesis, and the bounded
// upstream-advertisement pinning check. Pure throughout — no I/O — so every
// verdict is unit- and fuzz-testable (see spec/mcp/arch_mcp.md for the
// grammar and its semantics).
package mcpschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// Spec is one tool's params allowlist, as configured. A nil/empty Fields map
// allows no arguments at all — everything not explicitly allowed is refused.
type Spec struct {
	Fields map[string]Field `json:"fields"`
}

// Field is one allowed argument's predicate.
type Field struct {
	Type     string            `json:"type"` // string | integer | number | boolean
	Required bool              `json:"required,omitempty"`
	Enum     []json.RawMessage `json:"enum,omitempty"`
	Const    json.RawMessage   `json:"const,omitempty"`
}

// integerLit matches the only literal shape the grammar accepts as an
// integer: an optionally-signed run of digits — no fraction, no exponent. The
// check runs on the LITERAL (json.Number), so no float round-trip can smuggle
// a non-integer through.
var integerLit = regexp.MustCompile(`^-?[0-9]+$`)

// Compile returns the grammar problems of a spec (empty = valid), mirroring
// rules.Compile: every problem is named so validate-config can refuse
// fail-closed with the full list.
func Compile(s Spec) []string {
	var problems []string
	for name, f := range s.Fields {
		if name == "" {
			problems = append(problems, "params.fields has an empty field name")
			continue
		}
		p := func(format string, a ...any) {
			problems = append(problems, fmt.Sprintf("params.fields[%q]: ", name)+fmt.Sprintf(format, a...))
		}
		switch f.Type {
		case "string", "integer", "number", "boolean":
		default:
			p("type must be string|integer|number|boolean, got %q", f.Type)
			continue
		}
		if f.Enum != nil && f.Const != nil {
			p("enum and const are mutually exclusive")
		}
		if f.Enum != nil && len(f.Enum) == 0 {
			p("enum must be non-empty when present")
		}
		for i, v := range f.Enum {
			if _, err := decodeTyped(f.Type, v); err != nil {
				p("enum[%d]: %v", i, err)
			}
		}
		if f.Const != nil {
			if _, err := decodeTyped(f.Type, f.Const); err != nil {
				p("const: %v", err)
			}
		}
	}
	return problems
}

// decodeTyped decodes one raw JSON value under the grammar's typing: the
// value must be exactly the declared primitive kind (integers checked on the
// literal). The returned value is a string, json.Number, or bool.
func decodeTyped(typ string, raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing content after the value")
	}
	switch typ {
	case "string":
		if s, ok := v.(string); ok {
			return s, nil
		}
	case "boolean":
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case "number":
		if n, ok := v.(json.Number); ok {
			return n, nil
		}
	case "integer":
		if n, ok := v.(json.Number); ok {
			if !integerLit.MatchString(string(n)) {
				return nil, fmt.Errorf("not an integer literal: %s", n)
			}
			return n, nil
		}
	}
	return nil, fmt.Errorf("not a %s", typ)
}

// equalTyped compares two decoded values of one declared type: strings and
// booleans by value, numbers by literal (deliberate strictness — 1 and 1.0
// are different literals and do not match).
func equalTyped(a, b any) bool {
	if an, ok := a.(json.Number); ok {
		bn, ok := b.(json.Number)
		return ok && string(an) == string(bn)
	}
	return a == b
}

// Validate checks a tools/call's raw arguments against the spec and, on
// acceptance, returns the canonical re-serialization: keys sorted, numeric
// literals preserved verbatim, strings re-encoded. The canonical bytes are
// built from the very values the predicates saw — never a second parse with
// different tolerances. Absent (nil/empty) arguments mean the empty object.
//
// Refusals, all fail-closed: not a JSON object, duplicate keys (silent
// last-wins is the shadowing class the config discipline refuses everywhere),
// unknown fields, a missing required field, a type/enum/const violation.
func Validate(s Spec, rawArguments json.RawMessage) ([]byte, error) {
	args := map[string]json.RawMessage{}
	if len(rawArguments) > 0 {
		if err := CheckObject(rawArguments, "arguments"); err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(rawArguments))
		if err := dec.Decode(&args); err != nil {
			return nil, fmt.Errorf("arguments: %w", err)
		}
	}
	canonical := make(map[string]any, len(args))
	for name, raw := range args {
		f, ok := s.Fields[name]
		if !ok {
			return nil, fmt.Errorf("arguments: unknown field %q (everything not explicitly allowed is refused)", name)
		}
		v, err := decodeTyped(f.Type, raw)
		if err != nil {
			return nil, fmt.Errorf("arguments[%q]: %v", name, err)
		}
		if f.Const != nil {
			want, cerr := decodeTyped(f.Type, f.Const)
			if cerr != nil || !equalTyped(v, want) {
				return nil, fmt.Errorf("arguments[%q]: must equal the configured const", name)
			}
		}
		if f.Enum != nil {
			matched := false
			for _, e := range f.Enum {
				if want, eerr := decodeTyped(f.Type, e); eerr == nil && equalTyped(v, want) {
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("arguments[%q]: not one of the configured enum values", name)
			}
		}
		canonical[name] = v
	}
	for name, f := range s.Fields {
		if f.Required {
			if _, ok := args[name]; !ok {
				return nil, fmt.Errorf("arguments: required field %q is missing", name)
			}
		}
	}
	out, err := json.Marshal(canonical) // map marshal sorts keys; json.Number stays verbatim
	if err != nil {
		return nil, fmt.Errorf("arguments: canonicalize: %w", err)
	}
	return out, nil
}

// CheckObject token-walks raw and refuses a non-object top level, duplicate
// keys at any depth, and trailing content — the strict-decode discipline the
// config applies, applied to the attacker-authored documents the mediator
// decodes (a tools/call's params envelope and its arguments). what labels the
// errors ("params", "arguments").
func CheckObject(raw json.RawMessage, what string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("%s must be a JSON object", what)
	}
	if err := walkDupes(dec, what); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("%s: trailing content after the object", what)
	}
	return nil
}

// walkDupes consumes an object already opened on dec, refusing duplicate keys
// at every depth.
func walkDupes(dec *json.Decoder, what string) error {
	seen := map[string]bool{}
	for {
		t, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if d, ok := t.(json.Delim); ok && d == '}' {
			return nil
		}
		key, ok := t.(string)
		if !ok {
			return fmt.Errorf("%s: unexpected token %v", what, t)
		}
		if seen[key] {
			return fmt.Errorf("%s: duplicate key %q", what, key)
		}
		seen[key] = true
		if err := walkDupeValue(dec, what); err != nil {
			return err
		}
	}
}

func walkDupeValue(dec *json.Decoder, what string) error {
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if d, ok := t.(json.Delim); ok {
		switch d {
		case '{':
			return walkDupes(dec, what)
		case '[':
			for dec.More() {
				if err := walkDupeValue(dec, what); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return fmt.Errorf("%s: %w", what, err)
			}
		}
	}
	return nil
}

// SynthesizeInputSchema renders the spec as the JSON Schema tools/list
// advertises for it: type object, per-field properties, sorted required,
// additionalProperties false. Deterministic (map marshal sorts keys).
func SynthesizeInputSchema(s Spec) (json.RawMessage, error) {
	props := map[string]any{}
	var required []string
	for name, f := range s.Fields {
		prop := map[string]any{"type": f.Type}
		if f.Enum != nil {
			prop["enum"] = f.Enum
		}
		if f.Const != nil {
			prop["const"] = f.Const
		}
		props[name] = prop
		if f.Required {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("mcpschema: synthesize inputSchema: %w", err)
	}
	return raw, nil
}

// CheckAdvertised runs the bounded pinning check of spec/mcp/arch_mcp.md
// against an upstream's advertised inputSchema for this tool: every advertised
// required name must be a required field of the spec; when the advertisement
// declares properties, every spec field must appear among them, with primitive
// type agreement where the advertised property declares a string "type"
// (a spec integer accepts an advertised number). Only this slice of JSON
// Schema is read; an advertisement too malformed to read it refuses.
func CheckAdvertised(s Spec, inputSchema json.RawMessage) error {
	if len(inputSchema) == 0 || bytes.Equal(bytes.TrimSpace(inputSchema), []byte("null")) {
		return nil // no advertised schema: nothing to pin against
	}
	var adv struct {
		Required   json.RawMessage            `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(inputSchema, &adv); err != nil {
		return fmt.Errorf("advertised inputSchema is not an object: %w", err)
	}
	if adv.Required != nil {
		var required []string
		if err := json.Unmarshal(adv.Required, &required); err != nil {
			return fmt.Errorf("advertised required is not an array of strings: %w", err)
		}
		for _, name := range required {
			f, ok := s.Fields[name]
			if !ok || !f.Required {
				return fmt.Errorf("upstream requires field %q which the pinned schema does not require (drift)", name)
			}
		}
	}
	if adv.Properties != nil {
		for name, f := range s.Fields {
			prop, ok := adv.Properties[name]
			if !ok {
				return fmt.Errorf("pinned field %q is not among the upstream's advertised properties (drift)", name)
			}
			var p struct {
				Type json.RawMessage `json:"type"`
			}
			if err := json.Unmarshal(prop, &p); err != nil {
				continue // a non-object property schema: presence satisfied, type unreadable — skip
			}
			var advType string
			if err := json.Unmarshal(p.Type, &advType); err != nil {
				continue // absent or non-string type (e.g. a type list): skip the type check
			}
			if advType != f.Type && !(f.Type == "integer" && advType == "number") {
				return fmt.Errorf("pinned field %q: type %s conflicts with the upstream's advertised %q (drift)", name, f.Type, advType)
			}
		}
	}
	return nil
}
