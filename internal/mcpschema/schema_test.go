package mcpschema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func specSearch() Spec {
	return Spec{Fields: map[string]Field{
		"query": {Type: "string", Required: true},
		"limit": {Type: "integer"},
		"order": {Type: "string", Enum: []json.RawMessage{raw(`"asc"`), raw(`"desc"`)}},
		"exact": {Type: "boolean"},
		"score": {Type: "number"},
		"mode":  {Type: "string", Const: raw(`"fixed"`)},
	}}
}

func TestCompile(t *testing.T) {
	if p := Compile(specSearch()); len(p) != 0 {
		t.Fatalf("valid spec has problems: %v", p)
	}
	if p := Compile(Spec{}); len(p) != 0 {
		t.Fatalf("empty spec (no arguments allowed) has problems: %v", p)
	}
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{"bad type", Spec{Fields: map[string]Field{"a": {Type: "object"}}}, "type must be"},
		{"missing type", Spec{Fields: map[string]Field{"a": {}}}, "type must be"},
		{"empty enum", Spec{Fields: map[string]Field{"a": {Type: "string", Enum: []json.RawMessage{}}}}, "non-empty"},
		{"enum wrong type", Spec{Fields: map[string]Field{"a": {Type: "string", Enum: []json.RawMessage{raw(`1`)}}}}, "enum[0]"},
		{"const wrong type", Spec{Fields: map[string]Field{"a": {Type: "integer", Const: raw(`"x"`)}}}, "const"},
		{"const non-integer literal", Spec{Fields: map[string]Field{"a": {Type: "integer", Const: raw(`1.5`)}}}, "const"},
		{"enum and const", Spec{Fields: map[string]Field{"a": {Type: "string", Enum: []json.RawMessage{raw(`"x"`)}, Const: raw(`"x"`)}}}, "mutually exclusive"},
		{"empty field name", Spec{Fields: map[string]Field{"": {Type: "string"}}}, "empty field name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Compile(c.spec)
			if len(p) == 0 {
				t.Fatalf("expected problems")
			}
			if !strings.Contains(strings.Join(p, "; "), c.want) {
				t.Fatalf("problems %v do not mention %q", p, c.want)
			}
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	spec := specSearch()
	canonical, err := Validate(spec, raw(`{"query":"q","limit":10,"order":"asc","exact":true,"score":1.5,"mode":"fixed"}`))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// Canonical: keys sorted, literals preserved.
	want := `{"exact":true,"limit":10,"mode":"fixed","order":"asc","query":"q","score":1.5}`
	if string(canonical) != want {
		t.Fatalf("canonical = %s, want %s", canonical, want)
	}

	// Absent optional fields; absent arguments entirely for a no-required spec.
	if _, err := Validate(spec, raw(`{"query":"q"}`)); err != nil {
		t.Fatalf("optional fields absent: %v", err)
	}
	if out, err := Validate(Spec{}, nil); err != nil || string(out) != "{}" {
		t.Fatalf("no-args spec with absent arguments: %s, %v", out, err)
	}

	// A large integer literal survives canonicalization without float mangling.
	big := "9007199254740993" // 2^53+1: not representable as float64
	out, err := Validate(Spec{Fields: map[string]Field{"n": {Type: "integer"}}}, raw(`{"n":`+big+`}`))
	if err != nil || !strings.Contains(string(out), big) {
		t.Fatalf("big integer: %s, %v", out, err)
	}
}

func TestValidateRefusals(t *testing.T) {
	spec := specSearch()
	cases := []struct {
		name string
		args string
		want string
	}{
		{"missing required", `{"limit":1}`, "required field"},
		{"unknown field", `{"query":"q","extra":1}`, "unknown field"},
		{"wrong type string", `{"query":5}`, "not a string"},
		{"wrong type bool", `{"query":"q","exact":"yes"}`, "not a boolean"},
		{"float for integer", `{"query":"q","limit":1.5}`, "not an integer literal"},
		{"exponent for integer", `{"query":"q","limit":1e2}`, "not an integer literal"},
		{"string for integer", `{"query":"q","limit":"1"}`, "not a integer"},
		{"enum violation", `{"query":"q","order":"sideways"}`, "enum"},
		{"const violation", `{"query":"q","mode":"other"}`, "const"},
		{"not an object: array", `[1]`, "must be a JSON object"},
		{"not an object: string", `"x"`, "must be a JSON object"},
		{"not an object: number", `5`, "must be a JSON object"},
		{"not an object: null", `null`, "must be a JSON object"},
		{"duplicate key", `{"query":"a","query":"b"}`, "duplicate key"},
		{"trailing content", `{"query":"q"} {}`, "trailing content"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Validate(spec, raw(c.args))
			if err == nil {
				t.Fatalf("accepted %s", c.args)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestSynthesizeInputSchema(t *testing.T) {
	schema, err := SynthesizeInputSchema(specSearch())
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Type                 string                     `json:"type"`
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schema, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != "object" || decoded.AdditionalProperties {
		t.Fatalf("schema envelope: %s", schema)
	}
	if !reflect.DeepEqual(decoded.Required, []string{"query"}) {
		t.Fatalf("required = %v", decoded.Required)
	}
	if len(decoded.Properties) != 6 {
		t.Fatalf("properties: %v", decoded.Properties)
	}
	if !strings.Contains(string(decoded.Properties["order"]), `"enum":["asc","desc"]`) {
		t.Fatalf("order property: %s", decoded.Properties["order"])
	}
	// No required fields → the required key is absent entirely, not null.
	schema, err = SynthesizeInputSchema(Spec{Fields: map[string]Field{"a": {Type: "string"}}})
	if err != nil || strings.Contains(string(schema), `"required"`) {
		t.Fatalf("no-required schema: %s, %v", schema, err)
	}
}

func TestCheckAdvertised(t *testing.T) {
	spec := specSearch()
	ok := []string{
		``,     // no advertised schema at all
		`null`, // explicit null
		`{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"limit":{"type":"number"},"order":{"type":"string"},"exact":{"type":"boolean"},"score":{"type":"number"},"mode":{"type":"string"},"extra_upstream_only":{"type":"string"}}}`,
		`{"type":"object"}`, // neither required nor properties declared
		`{"properties":{"query":true,"limit":1,"order":{},"exact":{"type":["boolean","null"]},"score":{},"mode":{}}}`, // unreadable property slices: presence satisfied, type skipped
	}
	for i, adv := range ok {
		var in json.RawMessage
		if adv != "" {
			in = raw(adv)
		}
		if err := CheckAdvertised(spec, in); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := []struct {
		name, adv, want string
	}{
		{"upstream requires an optional field", `{"required":["limit"]}`, "does not require"},
		{"upstream requires an unknown field", `{"required":["cursor"]}`, "does not require"},
		{"pinned field dropped upstream", `{"properties":{"query":{}}}`, "not among the upstream's advertised properties"},
		{"type conflict", `{"properties":{"query":{"type":"number"},"limit":{},"order":{},"exact":{},"score":{},"mode":{}}}`, "conflicts"},
		{"non-object inputSchema", `"loose"`, "not an object"},
		{"non-array required", `{"required":"query"}`, "not an array"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			err := CheckAdvertised(spec, raw(c.adv))
			if err == nil {
				t.Fatalf("accepted %s", c.adv)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
	// integer accepts an advertised number, but not vice versa.
	ispec := Spec{Fields: map[string]Field{"n": {Type: "integer"}}}
	if err := CheckAdvertised(ispec, raw(`{"properties":{"n":{"type":"number"}}}`)); err != nil {
		t.Errorf("integer vs advertised number: %v", err)
	}
	nspec := Spec{Fields: map[string]Field{"n": {Type: "number"}}}
	if err := CheckAdvertised(nspec, raw(`{"properties":{"n":{"type":"integer"}}}`)); err == nil {
		t.Error("number vs advertised integer should conflict")
	}
}

// FuzzValidate: never panics; a nil error implies the canonical bytes
// re-decode to exactly the accepted field set, every field satisfying its
// spec — the canonicalization invariant (spec/mcp/test_mcp.md #13).
func FuzzValidate(f *testing.F) {
	f.Add(`{"query":"q","limit":3}`)
	f.Add(`{"query":"q","order":"asc"}`)
	f.Add(`{"unknown":1}`)
	f.Add(`[]`)
	f.Add(`{"query":"a","query":"b"}`)
	f.Add(``)
	f.Fuzz(func(t *testing.T, args string) {
		spec := specSearch()
		canonical, err := Validate(spec, json.RawMessage(args))
		if err != nil {
			return
		}
		// The canonical bytes must re-validate to the same bytes (idempotent)…
		again, err := Validate(spec, canonical)
		if err != nil {
			t.Fatalf("canonical bytes failed re-validation: %v (canonical=%s)", err, canonical)
		}
		if string(again) != string(canonical) {
			t.Fatalf("canonicalization not idempotent: %s -> %s", canonical, again)
		}
		// …and contain only known fields.
		var m map[string]json.RawMessage
		if err := json.Unmarshal(canonical, &m); err != nil {
			t.Fatalf("canonical is not an object: %v", err)
		}
		for k := range m {
			if _, ok := spec.Fields[k]; !ok {
				t.Fatalf("canonical carries unknown field %q", k)
			}
		}
	})
}
