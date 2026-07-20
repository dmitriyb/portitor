package rules

import (
	"encoding/json"
	"strings"
	"testing"
)

func compileOK(t *testing.T, jsonCfg string) *Compiled {
	t.Helper()
	var cr ContentRules
	if err := json.Unmarshal([]byte(jsonCfg), &cr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c, problems := Compile(&cr)
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	return c
}

func TestCompileValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     string
		wantErr string // substring of a problem
	}{
		{"bad version", `{"version":2}`, "version 2 is not supported"},
		{"missing version", `{}`, "version 0 is not supported"},
		{"empty paths", `{"version":1,"structural":{"rules":[{"name":"r","paths":[],"operations":["add"],"effect":"deny"}]}}`, "paths is empty"},
		{"unknown op", `{"version":1,"structural":{"rules":[{"name":"r","paths":["a"],"operations":["chmod"],"effect":"deny"}]}}`, "unknown operation"},
		{"bad effect", `{"version":1,"structural":{"rules":[{"name":"r","paths":["a"],"operations":["add"],"effect":"block"}]}}`, "effect must be allow or deny"},
		{"both in and not_in", `{"version":1,"structural":{"rules":[{"name":"r","paths":["a"],"operations":["add"],"roles":{"in":["x"],"not_in":["y"]},"effect":"deny"}]}}`, "exactly one of in/not_in"},
		{"empty in", `{"version":1,"structural":{"rules":[{"name":"r","paths":["a"],"operations":["add"],"roles":{"in":[]},"effect":"deny"}]}}`, "roles.in is empty"},
		{"malformed glob", `{"version":1,"structural":{"rules":[{"name":"r","paths":["a[","b"],"operations":["add"],"effect":"deny"}]}}`, "bad path pattern"},
		{"duplicate names across families", `{"version":1,"structural":{"rules":[{"name":"r","paths":["a"],"operations":["add"],"effect":"deny"}]},"semantic":{"files":[{"path":"f","check":{"command":["c"]},"rules":[{"name":"r","match":{"type":"record","change":"added"},"effect":"deny"}]}]}}`, "duplicate rule name"},
		{"missing rule name", `{"version":1,"structural":{"rules":[{"paths":["a"],"operations":["add"],"effect":"deny"}]}}`, "missing name"},
		{"missing semantic path", `{"version":1,"semantic":{"files":[{"check":{"command":["c"]}}]}}`, "missing path"},
		{"duplicate semantic path", `{"version":1,"semantic":{"files":[{"path":"f","check":{"command":["c"]}},{"path":"f","check":{"command":["c"]}}]}}`, "duplicate path"},
		{"empty check command", `{"version":1,"semantic":{"files":[{"path":"f","check":{}}]}}`, "check.command is empty"},
		{"absolute input_file", `{"version":1,"semantic":{"files":[{"path":"f","check":{"command":["c"],"input_file":"/etc/x"}}]}}`, "clean relative path"},
		{"escaping input_file", `{"version":1,"semantic":{"files":[{"path":"f","check":{"command":["c"],"input_file":"../x"}}]}}`, "clean relative path"},
		{"empty records_path segment", `{"version":1,"semantic":{"files":[{"path":"f","check":{"command":["c"],"records_path":"a..b"}}]}}`, "empty segment"},
		{"bad default", `{"version":1,"semantic":{"files":[{"path":"f","check":{"command":["c"]},"default":"reject"}]}}`, "default must be allow or deny"},
		{"field matcher without predicate", `{"version":1,"semantic":{"files":[{"path":"f","check":{"command":["c"]},"rules":[{"name":"r","match":{"type":"field","field":"x"},"effect":"deny"}]}]}}`, "needs at least one of from/to/changed"},
		{"changed combined with to", `{"version":1,"semantic":{"files":[{"path":"f","check":{"command":["c"]},"rules":[{"name":"r","match":{"type":"field","field":"x","changed":true,"to":"v"},"effect":"deny"}]}]}}`, "changed cannot combine"},
		{"bad record change", `{"version":1,"semantic":{"files":[{"path":"f","check":{"command":["c"]},"rules":[{"name":"r","match":{"type":"record","change":"edited"},"effect":"deny"}]}]}}`, "change must be added or removed"},
		{"labels needs exactly one", `{"version":1,"semantic":{"files":[{"path":"f","check":{"command":["c"]},"rules":[{"name":"r","match":{"type":"labels","contains":"a","added":"b"},"effect":"deny"}]}]}}`, "exactly one of contains/added"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cr ContentRules
			if err := json.Unmarshal([]byte(tc.cfg), &cr); err != nil {
				t.Fatalf("unmarshal should succeed (validation is Compile's job): %v", err)
			}
			_, problems := Compile(&cr)
			if len(problems) == 0 {
				t.Fatalf("want a problem containing %q, got none", tc.wantErr)
			}
			joined := strings.Join(problems, "; ")
			if !strings.Contains(joined, tc.wantErr) {
				t.Fatalf("problems = %q, want substring %q", joined, tc.wantErr)
			}
		})
	}
}

func TestMatcherDecodeStrict(t *testing.T) {
	bad := []string{
		`{"type":"regex","pattern":"x"}`,               // unknown type
		`{"type":"field","field":"x","glob":"*"}`,      // unknown key for type
		`{"field":"x","to":"v"}`,                       // missing type
		`{"type":"field","field":"x","changed":false}`, // changed must be true
	}
	for _, b := range bad {
		var m Matcher
		if err := json.Unmarshal([]byte(b), &m); err == nil {
			t.Errorf("decode of %s should fail", b)
		}
	}
	// null vs absent from/to stay distinguishable.
	var m Matcher
	if err := json.Unmarshal([]byte(`{"type":"field","field":"x","to":null}`), &m); err != nil {
		t.Fatal(err)
	}
	if !m.HasTo || m.To != nil {
		t.Fatalf("to:null should be present-with-null, got %+v", m)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"registry/**", "registry/a", true},
		{"registry/**", "registry/a/b/c", true},
		{"registry/**", "registry", false}, // strictly under
		{"registry/**", "other/registry/a", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/b/c", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false}, // * stays within a segment
		{"**", "anything/at/all", true},
		{"**", "top", true},
		{"docs/*.md", "docs/x.md", true},
		{"docs/*.md", "docs/sub/x.md", false},
	}
	for _, tc := range cases {
		g, err := compileGlob(tc.pattern)
		if err != nil {
			t.Fatalf("compileGlob(%q): %v", tc.pattern, err)
		}
		if got := g.match(tc.path); got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
	if _, err := compileGlob("a["); err == nil {
		t.Error("malformed pattern should not compile")
	}
	if _, err := compileGlob(""); err == nil {
		t.Error("empty pattern should not compile")
	}
	if _, err := compileGlob("a//b"); err == nil {
		t.Error("empty segment should not compile")
	}
}

func TestStructuralPrecedence(t *testing.T) {
	c := compileOK(t, `{"version":1,"structural":{
		"rules":[
			{"name":"allow-review-delete","paths":["p/**"],"operations":["delete"],"roles":{"in":["reviewer"]},"effect":"allow"},
			{"name":"deny-delete","paths":["p/**"],"operations":["delete"],"effect":"deny"}
		],
		"defaults":[{"paths":["locked/**"],"effect":"deny"}]
	}}`)

	del := func(path string) []ChangeEvent {
		return []ChangeEvent{{Path: path, Ops: []Op{OpDelete}}}
	}
	// First-match: reviewer hits the allow first; others fall through to deny.
	if d := c.EvaluateStructural(del("p/x"), "reviewer"); len(d) != 0 {
		t.Fatalf("reviewer delete should be allowed by first match, got %v", d)
	}
	if d := c.EvaluateStructural(del("p/x"), "implementer"); len(d) != 1 || d[0].Rule != "deny-delete" {
		t.Fatalf("implementer delete should hit deny-delete, got %v", d)
	}
	// Per-path default fires only when no rule matched.
	if d := c.EvaluateStructural(del("locked/x"), "anyone"); len(d) != 1 || d[0].Rule != "structural-default" {
		t.Fatalf("locked path should hit the default, got %v", d)
	}
	// Global default allow.
	if d := c.EvaluateStructural(del("free/x"), "anyone"); len(d) != 0 {
		t.Fatalf("unmatched path should be allowed, got %v", d)
	}
	// Unmapped role "" is bound by not_in-style restriction via plain deny too.
	if d := c.EvaluateStructural(del("p/x"), ""); len(d) != 1 {
		t.Fatalf("empty role should be denied, got %v", d)
	}
}

func semFile(t *testing.T, jsonCfg string) *CompiledFile {
	t.Helper()
	c := compileOK(t, jsonCfg)
	if len(c.Files) != 1 {
		t.Fatalf("want 1 file, got %d", len(c.Files))
	}
	return c.Files[0]
}

const restrictCfg = `{"version":1,"semantic":{"files":[{
	"path":"f","check":{"command":["c"]},
	"rules":[
		{"name":"gate-approved","match":{"type":"field","field":"stage","to":"approved"},"roles":{"not_in":["reviewer","owner"]},"effect":"deny"},
		{"name":"impl-submit","match":{"type":"field","field":"stage","from":"draft","to":"review"},"roles":{"in":["implementer"]},"effect":"allow"},
		{"name":"review-owns-stage","match":{"type":"field","field":"stage","changed":true},"roles":{"in":["reviewer","owner"]},"effect":"allow"},
		{"name":"deny-priority-change","match":{"type":"field","field":"priority","changed":true},"roles":{"not_in":["owner"]},"effect":"deny"},
		{"name":"adds-ok","match":{"type":"record","change":"added"},"effect":"allow"}
	],
	"default":"deny"}]}}`

// TestRestrictScope is the regression for the restrict-scope pitfall: an allow
// that matches one field's transition must authorize exactly that unit — an
// unrelated named-field change in the same record still falls to its own rule
// or the default.
func TestRestrictScope(t *testing.T) {
	f := semFile(t, restrictCfg)

	old := map[string]Record{"r-1": {"stage": "draft", "priority": float64(1), "title": "t"}}
	new := map[string]Record{"r-1": {"stage": "review", "priority": float64(2), "title": "t2"}}
	units := f.DeltaUnits(old, new)
	// Named fields are stage + priority: title is outside the protection
	// surface and generates no unit.
	if len(units) != 2 {
		t.Fatalf("want 2 units (stage, priority), got %+v", units)
	}
	denials := f.Evaluate(units, "implementer")
	if len(denials) != 1 || denials[0].Rule != "deny-priority-change" {
		t.Fatalf("the allow on stage must not launder the priority change: %v", denials)
	}

	// The same transition by a role outside every allow: denied by default.
	denials = f.Evaluate(units, "stranger")
	if len(denials) != 2 {
		t.Fatalf("stranger should be denied on both units, got %v", denials)
	}
}

func TestMatcherFireTable(t *testing.T) {
	f := semFile(t, restrictCfg)

	t.Run("born at gated value trips the arrival gate", func(t *testing.T) {
		units := f.DeltaUnits(map[string]Record{}, map[string]Record{"r-1": {"stage": "approved"}})
		denials := f.Evaluate(units, "implementer")
		if len(denials) != 1 || denials[0].Rule != "gate-approved" {
			t.Fatalf("want gate-approved on the added record, got %v", denials)
		}
	})
	t.Run("from matcher never fires on added records", func(t *testing.T) {
		units := f.DeltaUnits(map[string]Record{}, map[string]Record{"r-1": {"stage": "review"}})
		// impl-submit (from draft to review) must not fire; adds-ok decides.
		if denials := f.Evaluate(units, "stranger"); len(denials) != 0 {
			t.Fatalf("record add should be allowed by adds-ok, got %v", denials)
		}
	})
	t.Run("removal decided only by record-removed rules", func(t *testing.T) {
		units := f.DeltaUnits(map[string]Record{"r-1": {"stage": "approved"}}, map[string]Record{})
		if len(units) != 1 || units[0].Kind != UnitRecordRemoved {
			t.Fatalf("want a single removed unit, got %+v", units)
		}
		// No record-removed rule exists; the deny default decides.
		denials := f.Evaluate(units, "owner")
		if len(denials) != 1 || denials[0].Rule != "semantic-default" {
			t.Fatalf("removal should fall to the default, got %v", denials)
		}
	})
}

func TestLabelsMatchers(t *testing.T) {
	f := semFile(t, `{"version":1,"semantic":{"files":[{
		"path":"f","check":{"command":["c"]},
		"rules":[
			{"name":"frozen-owner-only","match":{"type":"labels","contains":"frozen"},"roles":{"not_in":["owner"]},"effect":"deny"},
			{"name":"no-adding-urgent","match":{"type":"labels","added":"urgent"},"roles":{"not_in":["owner"]},"effect":"deny"}
		]}]}}`)

	t.Run("contains is record-scoped", func(t *testing.T) {
		old := map[string]Record{"r-1": {"labels": []any{"frozen"}, "stage": "draft"}}
		new := map[string]Record{"r-1": {"labels": []any{"frozen"}, "stage": "review"}}
		units := f.DeltaUnits(old, new)
		// Named set is only "labels" — the stage field is not named by any rule,
		// so the only possible unit source is labels (unchanged) => no units.
		if len(units) != 0 {
			t.Fatalf("no named-field units expected, got %+v", units)
		}
	})
	t.Run("added fires on the labels unit", func(t *testing.T) {
		old := map[string]Record{"r-1": {"labels": []any{}}}
		new := map[string]Record{"r-1": {"labels": []any{"urgent"}}}
		units := f.DeltaUnits(old, new)
		denials := f.Evaluate(units, "implementer")
		if len(denials) != 1 || denials[0].Rule != "no-adding-urgent" {
			t.Fatalf("want no-adding-urgent, got %v", denials)
		}
		if denials = f.Evaluate(units, "owner"); len(denials) != 0 {
			t.Fatalf("owner may add the label, got %v", denials)
		}
	})
	t.Run("contains gates any unit of a labeled record", func(t *testing.T) {
		old := map[string]Record{"r-1": {"labels": []any{"frozen", "x"}}}
		new := map[string]Record{"r-1": {"labels": []any{"frozen"}}}
		units := f.DeltaUnits(old, new) // labels changed => a labels unit
		denials := f.Evaluate(units, "implementer")
		if len(denials) != 1 || denials[0].Rule != "frozen-owner-only" {
			t.Fatalf("want frozen-owner-only, got %v", denials)
		}
	})
}

func TestDeltaUnitsMissingVsNull(t *testing.T) {
	f := semFile(t, `{"version":1,"semantic":{"files":[{
		"path":"f","check":{"command":["c"]},
		"rules":[{"name":"x-changed","match":{"type":"field","field":"x","changed":true},"effect":"deny"}]}]}}`)
	// null -> absent is a change (missing is a distinct value).
	old := map[string]Record{"r-1": {"x": nil}}
	new := map[string]Record{"r-1": {}}
	units := f.DeltaUnits(old, new)
	if len(units) != 1 || units[0].Field != "x" {
		t.Fatalf("null->absent should be a unit, got %+v", units)
	}
	// Identical null on both sides is no change.
	if u := f.DeltaUnits(old, map[string]Record{"r-1": {"x": nil}}); len(u) != 0 {
		t.Fatalf("null==null should not be a unit, got %+v", u)
	}
}

func TestRetiredAndEmpty(t *testing.T) {
	c, problems := Compile(nil)
	if len(problems) != 0 || !c.Empty() {
		t.Fatalf("nil rules should compile empty: %v", problems)
	}
}
