package rules

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// FuzzMatchGlob: pattern compilation and matching never panic, and matching is
// deterministic. Malformed patterns must fail compile, never half-match.
func FuzzMatchGlob(f *testing.F) {
	f.Add("registry/**", "registry/a/b")
	f.Add("a/**/b", "a/b")
	f.Add("*.md", "README.md")
	f.Add("**", "x/y")
	f.Add("a[", "a")
	f.Add("", "x")
	f.Fuzz(func(t *testing.T, pattern, path string) {
		if strings.Count(pattern, "/") > 24 || strings.Count(path, "/") > 24 {
			t.Skip("bounded: rule patterns are operator config, not adversarial input")
		}
		g, err := compileGlob(pattern)
		if err != nil {
			return
		}
		got := g.match(path)
		if again := g.match(path); again != got {
			t.Fatalf("non-deterministic match for %q on %q", pattern, path)
		}
	})
}

// FuzzDeltaUnits: unit decomposition never panics, only emits fields from the
// named set, classifies ids consistently, and is deterministic.
func FuzzDeltaUnits(f *testing.F) {
	f.Add([]byte(`{"r-1":{"stage":"draft","n":1}}`), []byte(`{"r-1":{"stage":"done","n":1},"r-2":{"stage":"new"}}`))
	f.Add([]byte(`{}`), []byte(`{}`))
	f.Add([]byte(`{"a":{"labels":["x"]}}`), []byte(`{"b":{"labels":null}}`))
	f.Fuzz(func(t *testing.T, oldJSON, newJSON []byte) {
		var old, new map[string]Record
		if json.Unmarshal(oldJSON, &old) != nil || json.Unmarshal(newJSON, &new) != nil {
			t.Skip()
		}
		file := &CompiledFile{namedFields: []string{"labels", "n", "stage"}}
		units := file.DeltaUnits(old, new)
		named := map[string]bool{"labels": true, "n": true, "stage": true}
		for _, u := range units {
			switch u.Kind {
			case UnitFieldChange:
				if !named[u.Field] {
					t.Fatalf("unit for unnamed field %q", u.Field)
				}
				if _, inOld := old[u.ID]; !inOld {
					t.Fatalf("field unit for id %q not in old", u.ID)
				}
				if _, inNew := new[u.ID]; !inNew {
					t.Fatalf("field unit for id %q not in new", u.ID)
				}
			case UnitRecordAdded:
				if _, inOld := old[u.ID]; inOld {
					t.Fatalf("added unit for id %q present in old", u.ID)
				}
			case UnitRecordRemoved:
				if _, inNew := new[u.ID]; inNew {
					t.Fatalf("removed unit for id %q present in new", u.ID)
				}
			}
		}
		if again := file.DeltaUnits(old, new); !reflect.DeepEqual(units, again) {
			t.Fatal("DeltaUnits is not deterministic")
		}
	})
}

// FuzzCompile: arbitrary JSON never panics Compile, and a clean compile is
// stable (same problems on recompile).
func FuzzCompile(f *testing.F) {
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte(`{"version":1,"structural":{"rules":[{"name":"r","paths":["**"],"operations":["add"],"effect":"deny"}]}}`))
	f.Add([]byte(`{"version":1,"semantic":{"files":[{"path":"f","check":{"command":["c"]},"rules":[{"name":"s","match":{"type":"record","change":"added"},"effect":"allow"}]}]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var cr ContentRules
		if json.Unmarshal(data, &cr) != nil {
			t.Skip()
		}
		_, p1 := Compile(&cr)
		_, p2 := Compile(&cr)
		if !reflect.DeepEqual(p1, p2) {
			t.Fatalf("Compile is not deterministic: %v vs %v", p1, p2)
		}
	})
}

// FuzzEvaluateSemantic: for a fixed ruleset, evaluation never panics and the
// verdict is a pure function of (units, role) — evaluated twice, equal.
func FuzzEvaluateSemantic(f *testing.F) {
	f.Add([]byte(`{"r-1":{"stage":"draft"}}`), []byte(`{"r-1":{"stage":"approved"}}`), "implementer")
	f.Add([]byte(`{}`), []byte(`{"r-1":{"stage":"approved","labels":["frozen"]}}`), "")
	f.Fuzz(func(t *testing.T, oldJSON, newJSON []byte, role string) {
		var old, new map[string]Record
		if json.Unmarshal(oldJSON, &old) != nil || json.Unmarshal(newJSON, &new) != nil {
			t.Skip()
		}
		var cr ContentRules
		cfg := `{"version":1,"semantic":{"files":[{
			"path":"f","check":{"command":["c"]},
			"rules":[
				{"name":"gate","match":{"type":"field","field":"stage","to":"approved"},"roles":{"not_in":["reviewer"]},"effect":"deny"},
				{"name":"frozen","match":{"type":"labels","contains":"frozen"},"roles":{"not_in":["owner"]},"effect":"deny"},
				{"name":"adds","match":{"type":"record","change":"added"},"effect":"allow"}
			],
			"default":"deny"}]}}`
		if err := json.Unmarshal([]byte(cfg), &cr); err != nil {
			t.Fatal(err)
		}
		c, problems := Compile(&cr)
		if len(problems) > 0 {
			t.Fatal(problems)
		}
		file := c.Files[0]
		units := file.DeltaUnits(old, new)
		d1 := file.Evaluate(units, role)
		d2 := file.Evaluate(units, role)
		if !reflect.DeepEqual(d1, d2) {
			t.Fatal("Evaluate is not deterministic")
		}
	})
}
