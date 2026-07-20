// Package rules implements portitor's content-protection rule engine: the
// versioned config schema for structural (file-operation) and semantic
// (record-transition) rules, strict fail-closed compilation, and the pure
// evaluation of both families over pre-computed inputs. It shells nothing and
// reads nothing — git facts belong to the gate and record extraction to the
// operator-configured check command (internal/check) — so every verdict here
// is unit- and fuzz-testable, and no tool identity exists in this package.
//
// See spec/gate/arch_content_rules.md for the model: fine-grained decision
// units, first-match precedence with role predicates, per-path/per-file
// defaults, and a global default of allow.
package rules

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// SupportedVersion is the only content_rules schema version this binary
// understands. Any other value refuses to gate (fail-closed) — never gate with
// a partially understood ruleset.
const SupportedVersion = 1

// ContentRules is the JSON shape of Config.content_rules.
type ContentRules struct {
	Version    int              `json:"version"`
	Structural *StructuralRules `json:"structural"`
	Semantic   *SemanticRules   `json:"semantic"`
}

// StructuralRules gate file-level operations by path glob and role.
type StructuralRules struct {
	Rules    []StructuralRule `json:"rules"`
	Defaults []PathDefault    `json:"defaults"`
}

// StructuralRule is one paths × operations × role-predicate → effect entry.
type StructuralRule struct {
	Name       string         `json:"name"`
	Paths      []string       `json:"paths"`
	Operations []string       `json:"operations"`
	Roles      *RolePredicate `json:"roles"`
	Effect     string         `json:"effect"`
}

// PathDefault is a per-path default effect, consulted when no rule matched.
type PathDefault struct {
	Paths  []string `json:"paths"`
	Effect string   `json:"effect"`
}

// RolePredicate accepts or rejects a role: exactly one of In/NotIn is set.
// A nil predicate accepts every role.
type RolePredicate struct {
	In    []string `json:"in"`
	NotIn []string `json:"not_in"`
}

func (p *RolePredicate) accepts(role string) bool {
	if p == nil {
		return true
	}
	if p.In != nil {
		return contains(p.In, role)
	}
	return !contains(p.NotIn, role)
}

// SemanticRules gate record-level transitions inside protected structured files.
type SemanticRules struct {
	Files []SemanticFile `json:"files"`
}

// SemanticFile is one protected file: its check command, rules, and default effect.
type SemanticFile struct {
	Path    string         `json:"path"`
	Check   CheckDef       `json:"check"`
	Rules   []SemanticRule `json:"rules"`
	Default string         `json:"default"` // "allow" (also the zero value) or "deny"
}

// CheckDef declares the operator-configured record extractor for a protected
// file — the seam any script, tool wrapper, or service client can fill.
// Portitor knows only this contract; the command is operator trust-root
// material of the same class as allowed_signers.
type CheckDef struct {
	// Command is the explicit argv. Pushed content never appears in it and
	// there is never a shell.
	Command []string `json:"command"`
	// InputFile, when set, is the relative path inside the private working
	// directory where a side's content is materialized before the command
	// runs. Empty means the content is piped to the command's stdin.
	InputFile string `json:"input_file"`
	// RecordsPath is the dotted JSON path to the record array in the
	// command's stdout. Empty means the stdout IS the array.
	RecordsPath string `json:"records_path"`
	// IDField is the field that keys a record. Empty means "id".
	IDField string `json:"id_field"`
}

// RecordsKeys returns the dotted RecordsPath split into keys ("" => none).
func (c CheckDef) RecordsKeys() []string {
	if c.RecordsPath == "" {
		return nil
	}
	return strings.Split(c.RecordsPath, ".")
}

// KeyField returns the record id field, applying the default.
func (c CheckDef) KeyField() string {
	if c.IDField == "" {
		return "id"
	}
	return c.IDField
}

// SemanticRule is one match × role-predicate → effect entry.
type SemanticRule struct {
	Name   string         `json:"name"`
	Match  Matcher        `json:"match"`
	Roles  *RolePredicate `json:"roles"`
	Effect string         `json:"effect"`
}

// Matcher is the v1 matcher vocabulary. It carries presence flags because JSON
// null and absent must stay distinguishable for from/to values.
type Matcher struct {
	Type string

	// type "field"
	Field   string
	From    any
	HasFrom bool
	To      any
	HasTo   bool
	Changed bool

	// type "record"
	Change string

	// type "labels"
	Contains    string
	HasContains bool
	Added       string
	HasAdded    bool
}

// matcherKeys lists the allowed JSON keys per matcher type. An unknown type or
// an unknown key is a hard decode error — the additive-extension guard: a
// config using future vocabulary makes this binary refuse to gate, never
// silently ignore the rule.
var matcherKeys = map[string]map[string]bool{
	"field":  {"type": true, "field": true, "from": true, "to": true, "changed": true},
	"record": {"type": true, "change": true},
	"labels": {"type": true, "contains": true, "added": true},
}

// UnmarshalJSON decodes a matcher strictly: unknown types, unknown keys, and
// wrongly-typed values are errors. Completeness (e.g. a field matcher with no
// from/to/changed) is checked in Compile so problems aggregate.
func (m *Matcher) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("matcher: %w", err)
	}
	typRaw, ok := raw["type"]
	if !ok {
		return fmt.Errorf("matcher: missing \"type\"")
	}
	var typ string
	if err := json.Unmarshal(typRaw, &typ); err != nil {
		return fmt.Errorf("matcher \"type\": %w", err)
	}
	allowed, ok := matcherKeys[typ]
	if !ok {
		return fmt.Errorf("matcher: unknown type %q", typ)
	}
	for k := range raw {
		if !allowed[k] {
			return fmt.Errorf("matcher type %q: unknown key %q", typ, k)
		}
	}
	m.Type = typ
	switch typ {
	case "field":
		if r, ok := raw["field"]; ok {
			if err := json.Unmarshal(r, &m.Field); err != nil {
				return fmt.Errorf("matcher \"field\": %w", err)
			}
		}
		if r, ok := raw["from"]; ok {
			m.HasFrom = true
			if err := json.Unmarshal(r, &m.From); err != nil {
				return fmt.Errorf("matcher \"from\": %w", err)
			}
		}
		if r, ok := raw["to"]; ok {
			m.HasTo = true
			if err := json.Unmarshal(r, &m.To); err != nil {
				return fmt.Errorf("matcher \"to\": %w", err)
			}
		}
		if r, ok := raw["changed"]; ok {
			if err := json.Unmarshal(r, &m.Changed); err != nil {
				return fmt.Errorf("matcher \"changed\": %w", err)
			}
			if !m.Changed {
				return fmt.Errorf("matcher \"changed\": must be true when present")
			}
		}
	case "record":
		if r, ok := raw["change"]; ok {
			if err := json.Unmarshal(r, &m.Change); err != nil {
				return fmt.Errorf("matcher \"change\": %w", err)
			}
		}
	case "labels":
		if r, ok := raw["contains"]; ok {
			m.HasContains = true
			if err := json.Unmarshal(r, &m.Contains); err != nil {
				return fmt.Errorf("matcher \"contains\": %w", err)
			}
		}
		if r, ok := raw["added"]; ok {
			m.HasAdded = true
			if err := json.Unmarshal(r, &m.Added); err != nil {
				return fmt.Errorf("matcher \"added\": %w", err)
			}
		}
	}
	return nil
}

// MarshalJSON keeps Matcher round-trippable (used by tests and tooling).
func (m Matcher) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": m.Type}
	switch m.Type {
	case "field":
		out["field"] = m.Field
		if m.HasFrom {
			out["from"] = m.From
		}
		if m.HasTo {
			out["to"] = m.To
		}
		if m.Changed {
			out["changed"] = true
		}
	case "record":
		out["change"] = m.Change
	case "labels":
		if m.HasContains {
			out["contains"] = m.Contains
		}
		if m.HasAdded {
			out["added"] = m.Added
		}
	}
	return json.Marshal(out)
}

// ---- operations ----

// Op is a structural file operation.
type Op string

const (
	OpAdd    Op = "add"
	OpModify Op = "modify"
	OpDelete Op = "delete"
	OpRename Op = "rename"
)

var knownOps = map[Op]bool{OpAdd: true, OpModify: true, OpDelete: true, OpRename: true}

// ChangeEvent is one structural evaluation event: a path with its effective
// operation set (a rename yields two events, {delete,rename} at the old path
// and {add,rename} at the new one, so it can never evade add/delete rules).
type ChangeEvent struct {
	Path string
	Ops  []Op
	Note string // human context for the violation detail, e.g. `renamed to "x"`
}

// Denial is one denied decision unit; Rule is the deciding rule's name (or
// "structural-default" / "semantic-default"), Detail the human-facing what.
type Denial struct {
	Rule   string
	Detail string
}

// ---- compilation ----

// Compiled is the validated, ready-to-evaluate form of ContentRules.
type Compiled struct {
	structRules    []compiledStructuralRule
	structDefaults []compiledDefault
	Files          []*CompiledFile
}

// Empty reports whether there is nothing to evaluate (lets the gate skip the
// per-commit git work entirely).
func (c *Compiled) Empty() bool {
	return !c.HasStructural() && len(c.Files) == 0
}

// HasStructural reports whether any structural rules or defaults exist.
func (c *Compiled) HasStructural() bool {
	return len(c.structRules) > 0 || len(c.structDefaults) > 0
}

type compiledStructuralRule struct {
	name  string
	globs []compiledGlob
	ops   map[Op]bool
	roles *RolePredicate
	deny  bool
}

type compiledDefault struct {
	globs []compiledGlob
	deny  bool
}

// CompiledFile is one protected semantic file with its compiled rules.
type CompiledFile struct {
	Path        string
	Check       CheckDef
	rules       []compiledSemanticRule
	defaultDeny bool
	namedFields []string // sorted; the semantic protection surface
}

type compiledSemanticRule struct {
	name  string
	match Matcher
	roles *RolePredicate
	deny  bool
}

// NamedFields returns the file's named-field set (the fields its field
// matchers name, plus "labels" if any labels matcher is present).
func (f *CompiledFile) NamedFields() []string { return f.namedFields }

// Compile validates cr (strictly, fail-closed) and returns its compiled form.
// A nil cr compiles to an empty ruleset. problems is empty iff the config is
// usable; the gate treats any problem as a refusal to run.
func Compile(cr *ContentRules) (*Compiled, []string) {
	c := &Compiled{}
	if cr == nil {
		return c, nil
	}
	var problems []string
	bad := func(format string, args ...any) {
		problems = append(problems, "content_rules: "+fmt.Sprintf(format, args...))
	}

	if cr.Version != SupportedVersion {
		bad("version %d is not supported by this binary (want %d); refusing to gate with a partially understood ruleset", cr.Version, SupportedVersion)
	}

	names := map[string]bool{}
	claimName := func(name, what string) {
		if name == "" {
			bad("%s: missing name", what)
			return
		}
		if names[name] {
			bad("duplicate rule name %q (names share one namespace across families)", name)
		}
		names[name] = true
	}
	checkRoles := func(what string, p *RolePredicate) {
		if p == nil {
			return
		}
		switch {
		case p.In != nil && p.NotIn != nil:
			bad("%s: roles must set exactly one of in/not_in", what)
		case p.In == nil && p.NotIn == nil:
			bad("%s: roles present but neither in nor not_in set", what)
		case p.In != nil && len(p.In) == 0:
			bad("%s: roles.in is empty (matches no role — a dead rule)", what)
		case p.NotIn != nil && len(p.NotIn) == 0:
			bad("%s: roles.not_in is empty (equivalent to omitting roles — say that instead)", what)
		}
	}
	checkEffect := func(what, effect string) bool {
		if effect != "allow" && effect != "deny" {
			bad("%s: effect must be allow or deny, got %q", what, effect)
			return false
		}
		return effect == "deny"
	}
	compileGlobs := func(what string, patterns []string) []compiledGlob {
		if len(patterns) == 0 {
			bad("%s: paths is empty", what)
			return nil
		}
		var globs []compiledGlob
		for _, p := range patterns {
			g, err := compileGlob(p)
			if err != nil {
				bad("%s: bad path pattern %q: %v", what, p, err)
				continue
			}
			globs = append(globs, g)
		}
		return globs
	}

	if s := cr.Structural; s != nil {
		for i, r := range s.Rules {
			what := fmt.Sprintf("structural rule %d (%q)", i, r.Name)
			claimName(r.Name, what)
			globs := compileGlobs(what, r.Paths)
			if len(r.Operations) == 0 {
				bad("%s: operations is empty", what)
			}
			ops := map[Op]bool{}
			for _, o := range r.Operations {
				if !knownOps[Op(o)] {
					bad("%s: unknown operation %q", what, o)
					continue
				}
				ops[Op(o)] = true
			}
			checkRoles(what, r.Roles)
			deny := checkEffect(what, r.Effect)
			c.structRules = append(c.structRules, compiledStructuralRule{
				name: r.Name, globs: globs, ops: ops, roles: r.Roles, deny: deny,
			})
		}
		for i, d := range s.Defaults {
			what := fmt.Sprintf("structural default %d", i)
			globs := compileGlobs(what, d.Paths)
			deny := checkEffect(what, d.Effect)
			c.structDefaults = append(c.structDefaults, compiledDefault{globs: globs, deny: deny})
		}
	}

	if sem := cr.Semantic; sem != nil {
		seenPaths := map[string]bool{}
		for i, f := range sem.Files {
			what := fmt.Sprintf("semantic file %d (%q)", i, f.Path)
			if f.Path == "" {
				bad("%s: missing path", what)
			} else if seenPaths[f.Path] {
				bad("%s: duplicate path", what)
			}
			seenPaths[f.Path] = true
			checkCheckDef(what, f.Check, bad)
			defaultDeny := false
			switch f.Default {
			case "", "allow":
			case "deny":
				defaultDeny = true
			default:
				bad("%s: default must be allow or deny, got %q", what, f.Default)
			}
			cf := &CompiledFile{Path: f.Path, Check: f.Check, defaultDeny: defaultDeny}
			fieldSet := map[string]bool{}
			for j, r := range f.Rules {
				rwhat := fmt.Sprintf("%s rule %d (%q)", what, j, r.Name)
				claimName(r.Name, rwhat)
				checkRoles(rwhat, r.Roles)
				deny := checkEffect(rwhat, r.Effect)
				switch r.Match.Type {
				case "field":
					if r.Match.Field == "" {
						bad("%s: field matcher: missing field", rwhat)
					}
					if !r.Match.HasFrom && !r.Match.HasTo && !r.Match.Changed {
						bad("%s: field matcher: needs at least one of from/to/changed (an under-specified matcher must never silently widen)", rwhat)
					}
					if r.Match.Changed && (r.Match.HasFrom || r.Match.HasTo) {
						bad("%s: field matcher: changed cannot combine with from/to", rwhat)
					}
					fieldSet[r.Match.Field] = true
				case "record":
					if r.Match.Change != "added" && r.Match.Change != "removed" {
						bad("%s: record matcher: change must be added or removed, got %q", rwhat, r.Match.Change)
					}
				case "labels":
					if r.Match.HasContains == r.Match.HasAdded {
						bad("%s: labels matcher: needs exactly one of contains/added", rwhat)
					}
					if (r.Match.HasContains && r.Match.Contains == "") || (r.Match.HasAdded && r.Match.Added == "") {
						bad("%s: labels matcher: label must be a non-empty string", rwhat)
					}
					fieldSet["labels"] = true
				case "":
					bad("%s: missing match", rwhat)
				default:
					// Unknown types are rejected at decode; "" means match was absent.
					bad("%s: unknown match type %q", rwhat, r.Match.Type)
				}
				cf.rules = append(cf.rules, compiledSemanticRule{name: r.Name, match: r.Match, roles: r.Roles, deny: deny})
			}
			for fld := range fieldSet {
				cf.namedFields = append(cf.namedFields, fld)
			}
			sort.Strings(cf.namedFields)
			c.Files = append(c.Files, cf)
		}
	}
	return c, problems
}

// checkCheckDef validates a check-command declaration: a non-empty explicit
// argv, an input_file that stays inside the private working directory, and
// well-formed records_path / id_field values.
func checkCheckDef(what string, c CheckDef, bad func(string, ...any)) {
	if len(c.Command) == 0 {
		bad("%s: check.command is empty", what)
	}
	for i, a := range c.Command {
		if a == "" {
			bad("%s: check.command[%d] is empty", what, i)
		}
		if strings.ContainsRune(a, 0) {
			bad("%s: check.command[%d] contains a NUL byte", what, i)
		}
	}
	if f := c.InputFile; f != "" {
		clean := filepath.Clean(f)
		if filepath.IsAbs(f) || clean != f || clean == "." || clean == ".." ||
			strings.HasPrefix(clean, "../") || strings.ContainsRune(f, 0) {
			bad("%s: check.input_file %q must be a clean relative path inside the working directory", what, f)
		}
	}
	for _, seg := range c.RecordsKeys() {
		if seg == "" {
			bad("%s: check.records_path %q has an empty segment", what, c.RecordsPath)
		}
	}
	// IDField: any non-empty string is fine; empty falls back to "id" via KeyField.
}

// ---- glob matching ----

// compiledGlob is a validated slash-split pattern: '*' and character classes
// match within one segment (path.Match), "**" as a whole segment spans any
// number of segments. A trailing "/**" requires at least one segment (strictly
// under the prefix), implemented by rewriting it to "**","*".
type compiledGlob struct {
	pattern string
	segs    []string
}

func compileGlob(pattern string) (compiledGlob, error) {
	if pattern == "" {
		return compiledGlob{}, fmt.Errorf("empty pattern")
	}
	segs := strings.Split(pattern, "/")
	var out []string
	for _, s := range segs {
		if s == "" {
			return compiledGlob{}, fmt.Errorf("empty segment (leading/trailing/double slash)")
		}
		if s == "**" {
			if len(out) > 0 && out[len(out)-1] == "**" {
				continue // collapse runs of **
			}
			out = append(out, s)
			continue
		}
		if _, err := path.Match(s, "probe"); err != nil {
			return compiledGlob{}, fmt.Errorf("segment %q: %w", s, err)
		}
		out = append(out, s)
	}
	if out[len(out)-1] == "**" {
		out = append(out, "*") // trailing ** means strictly under: at least one segment
	}
	return compiledGlob{pattern: pattern, segs: out}, nil
}

func (g compiledGlob) match(p string) bool {
	return matchSegs(g.segs, strings.Split(p, "/"))
}

func matchSegs(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegs(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], segs[1:])
}

func matchAny(globs []compiledGlob, p string) bool {
	for _, g := range globs {
		if g.match(p) {
			return true
		}
	}
	return false
}

// ---- structural evaluation ----

// EvaluateStructural decides every change event for the given role and returns
// the denials. Precedence per event: first matching rule (paths ∧ operations ∧
// role predicate) decides; else the first matching per-path default; else the
// global default allow.
func (c *Compiled) EvaluateStructural(events []ChangeEvent, role string) []Denial {
	var denials []Denial
	for _, ev := range events {
		decided := false
		for _, r := range c.structRules {
			op, opOK := firstIntersect(r.ops, ev.Ops)
			if !opOK || !matchAny(r.globs, ev.Path) || !r.roles.accepts(role) {
				continue
			}
			if r.deny {
				denials = append(denials, Denial{Rule: r.name, Detail: eventDetail(op, ev)})
			}
			decided = true
			break
		}
		if decided {
			continue
		}
		for _, d := range c.structDefaults {
			if !matchAny(d.globs, ev.Path) {
				continue
			}
			if d.deny {
				denials = append(denials, Denial{Rule: "structural-default", Detail: eventDetail(ev.Ops[0], ev)})
			}
			break
		}
	}
	return denials
}

func firstIntersect(ruleOps map[Op]bool, evOps []Op) (Op, bool) {
	for _, o := range evOps {
		if ruleOps[o] {
			return o, true
		}
	}
	return "", false
}

func eventDetail(op Op, ev ChangeEvent) string {
	d := fmt.Sprintf("%s of %q", op, ev.Path)
	if ev.Note != "" {
		d += " (" + ev.Note + ")"
	}
	return d
}

// ---- semantic evaluation ----

// Record is one extracted record: the full field map, values as decoded JSON.
type Record = map[string]any

// UnitKind classifies a semantic decision unit.
type UnitKind int

const (
	UnitFieldChange UnitKind = iota
	UnitRecordAdded
	UnitRecordRemoved
)

// Unit is one fine-grained semantic decision unit. An allow authorizes exactly
// its unit — never the surrounding record or commit.
type Unit struct {
	Kind  UnitKind
	ID    string
	Field string // set for UnitFieldChange
	Old   Record // nil for added
	New   Record // nil for removed
}

// DeltaUnits decomposes the old/new record sets into decision units:
// record-added and record-removed always; field-change only for fields in the
// file's named-field set (never whole-record — an extractor's derived or
// bookkeeping fields must not poison verdicts). Order is deterministic (by id,
// then field).
func (f *CompiledFile) DeltaUnits(old, new map[string]Record) []Unit {
	var units []Unit
	for _, id := range sortedKeys(new) {
		if _, ok := old[id]; !ok {
			units = append(units, Unit{Kind: UnitRecordAdded, ID: id, New: new[id]})
		}
	}
	for _, id := range sortedKeys(old) {
		if _, ok := new[id]; !ok {
			units = append(units, Unit{Kind: UnitRecordRemoved, ID: id, Old: old[id]})
			continue
		}
		o, n := old[id], new[id]
		for _, fld := range f.namedFields {
			ov, oOK := o[fld]
			nv, nOK := n[fld]
			if oOK != nOK || (oOK && !reflect.DeepEqual(ov, nv)) {
				units = append(units, Unit{Kind: UnitFieldChange, ID: id, Field: fld, Old: o, New: n})
			}
		}
	}
	return units
}

// Evaluate decides every unit for the given role and returns the denials.
// Precedence per unit: first rule whose matcher fires and whose role predicate
// accepts decides; else the file default; else allow.
func (f *CompiledFile) Evaluate(units []Unit, role string) []Denial {
	var denials []Denial
	for _, u := range units {
		decided := false
		for _, r := range f.rules {
			if !r.match.fires(u) || !r.roles.accepts(role) {
				continue
			}
			if r.deny {
				denials = append(denials, Denial{Rule: r.name, Detail: unitDetail(u)})
			}
			decided = true
			break
		}
		if !decided && f.defaultDeny {
			denials = append(denials, Denial{Rule: "semantic-default", Detail: unitDetail(u)})
		}
	}
	return denials
}

// fires implements the v1 matcher table (see arch_content_rules.md): each
// matcher fires on specific unit kinds and never on others. Record removal is
// decided only by record{removed} rules.
func (m *Matcher) fires(u Unit) bool {
	switch m.Type {
	case "field":
		switch u.Kind {
		case UnitFieldChange:
			if u.Field != m.Field {
				return false
			}
			if m.Changed {
				return true // the unit exists because the field changed
			}
			if m.HasFrom {
				ov, ok := u.Old[m.Field]
				if !ok || !reflect.DeepEqual(ov, m.From) {
					return false
				}
			}
			if m.HasTo {
				nv, ok := u.New[m.Field]
				if !ok || !reflect.DeepEqual(nv, m.To) {
					return false
				}
			}
			return true
		case UnitRecordAdded:
			if m.HasFrom {
				return false // no old side to depart from
			}
			if m.Changed {
				_, ok := u.New[m.Field]
				return ok // born with the field present
			}
			nv, ok := u.New[m.Field]
			return ok && reflect.DeepEqual(nv, m.To) // born at the gated value
		default: // UnitRecordRemoved: field matchers never fire
			return false
		}
	case "record":
		return (m.Change == "added" && u.Kind == UnitRecordAdded) ||
			(m.Change == "removed" && u.Kind == UnitRecordRemoved)
	case "labels":
		if u.Kind == UnitRecordRemoved {
			return false
		}
		newLabels := stringSlice(u.New["labels"])
		if m.HasContains {
			return contains(newLabels, m.Contains) // record-scoped: any unit of a labeled record
		}
		// "added": on the labels unit (or a record addition) only.
		if u.Kind == UnitFieldChange && u.Field != "labels" {
			return false
		}
		if !contains(newLabels, m.Added) {
			return false
		}
		return u.Kind == UnitRecordAdded || !contains(stringSlice(u.Old["labels"]), m.Added)
	}
	return false
}

func unitDetail(u Unit) string {
	switch u.Kind {
	case UnitRecordAdded:
		return fmt.Sprintf("record %q added", u.ID)
	case UnitRecordRemoved:
		return fmt.Sprintf("record %q removed", u.ID)
	default:
		return fmt.Sprintf("record %q: field %q %s -> %s",
			u.ID, u.Field, fieldValue(u.Old, u.Field), fieldValue(u.New, u.Field))
	}
}

func fieldValue(r Record, field string) string {
	v, ok := r[field]
	if !ok {
		return "(missing)"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "(unrepresentable)"
	}
	const max = 80
	s := string(b)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]Record) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
