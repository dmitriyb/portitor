package gate

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/dmitriyb/portitor/internal/check"
	"github.com/dmitriyb/portitor/internal/git"
	"github.com/dmitriyb/portitor/internal/rules"
)

// contentViolations evaluates the structural and semantic content rules for
// one introduced commit (see spec/gate/arch_content_rules.md). recCache is a
// per-Check cache of extracted record sets keyed by "<file-path>:<blob-oid>",
// so a blob shared between a commit and its child parses once.
func contentViolations(repoDir, ref, commit, role string, comp *rules.Compiled, recCache map[string]map[string]rules.Record) ([]Violation, error) {
	if comp.Empty() {
		return nil, nil
	}
	parents, err := commitParents(repoDir, commit)
	if err != nil {
		return nil, err
	}

	var vs []Violation
	if comp.HasStructural() {
		events, err := structuralEvents(repoDir, commit, parents)
		if err != nil {
			return nil, err
		}
		for _, d := range comp.EvaluateStructural(events, role) {
			vs = append(vs, Violation{
				Ref:    ref,
				Rule:   d.Rule,
				Detail: fmt.Sprintf("commit %s: %s is denied for role %s", shortSHA(commit), d.Detail, roleLabel(role)),
			})
		}
	}
	for _, f := range comp.Files {
		fvs, err := semanticViolations(repoDir, ref, commit, parents, role, f, recCache)
		if err != nil {
			return nil, err
		}
		vs = append(vs, fvs...)
	}
	return vs, nil
}

// commitParents returns a commit's parent hashes (empty for a root commit).
func commitParents(repoDir, commit string) ([]string, error) {
	out, err := git.OutputHermetic(repoDir, "rev-list", "--parents", "-n", "1", commit, "--")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return nil, fmt.Errorf("rev-list --parents: empty output for %s", shortSHA(commit))
	}
	return fields[1:], nil
}

// ---- structural extraction ----

// nsEntry is one parsed `git diff-tree --name-status -z` entry.
type nsEntry struct {
	status  byte   // A M T C D R (anything else is a fail-closed error downstream)
	path    string // the target path (dst for R/C)
	oldPath string // src for R/C
}

// structuralEvents computes a commit's change events over the full,
// rename-aware diff. Non-merge commits diff against their (first) parent (the
// empty tree for roots). Merge commits use git's combined-diff file set (the
// paths that differ from EVERY parent — a clean merge introduces nothing) to
// decide what the merge introduces, and evaluate the first-parent entry (the
// branch's own line of development) for those paths.
func structuralEvents(repoDir, commit string, parents []string) ([]rules.ChangeEvent, error) {
	if len(parents) <= 1 {
		from := ""
		if len(parents) == 1 {
			from = parents[0]
		}
		entries, err := diffTreeEntries(repoDir, from, commit)
		if err != nil {
			return nil, err
		}
		return eventsFromEntries(entries)
	}

	// Merge: the introduced set is git's combined diff — paths differing from
	// all parents. Computing it directly (not by intersecting per-parent
	// name-status by path) is essential: rename detection in the first-parent
	// diff relabels a delete-of-X as a rename-X→Y, and a path-keyed
	// intersection would then drop the deletion of a protected path. The
	// first-parent entries still supply the OPERATION (with rename
	// double-visibility); we keep only the events whose path is in the
	// introduced set — so the delete-side of a laundering rename is retained
	// and the add-side (content that came from an already-authorized parent) is
	// excluded.
	introduced, err := combinedDiffPaths(repoDir, commit)
	if err != nil {
		return nil, err
	}
	first, err := diffTreeEntries(repoDir, parents[0], commit)
	if err != nil {
		return nil, err
	}
	events, err := eventsFromEntries(first)
	if err != nil {
		return nil, err
	}
	kept := events[:0]
	for _, ev := range events {
		if introduced[ev.Path] {
			kept = append(kept, ev)
		}
	}
	return kept, nil
}

// combinedDiffPaths returns the paths in a merge commit's combined diff — those
// differing from every parent. `-c` produces output only for merges.
func combinedDiffPaths(repoDir, commit string) (map[string]bool, error) {
	out, err := git.OutputHermetic(repoDir, "diff-tree", "-c", "-r", "-z", "--name-only", "--no-commit-id", commit, "--")
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths[p] = true
		}
	}
	return paths, nil
}

// diffTreeEntries runs the full-diff name-status (rename- and copy-aware,
// never pathspec-filtered) between from and to; an empty from means the empty
// tree (root commit).
func diffTreeEntries(repoDir, from, to string) ([]nsEntry, error) {
	args := []string{"diff-tree", "-r", "-M", "-C", "-z", "--name-status", "--no-commit-id"}
	if from == "" {
		args = append(args, "--root", to, "--")
	} else {
		args = append(args, from, to, "--")
	}
	out, err := git.OutputHermetic(repoDir, args...)
	if err != nil {
		return nil, err
	}
	return parseNameStatusZ(out)
}

// parseNameStatusZ parses `--name-status -z` output: STATUS NUL PATH NUL, with
// R/C carrying two paths (src, dst). Anything unparseable is an error — input
// the gate cannot fully understand is never partially enforced.
func parseNameStatusZ(out string) ([]nsEntry, error) {
	toks := strings.Split(out, "\x00")
	var entries []nsEntry
	i := 0
	for i < len(toks) {
		st := toks[i]
		if st == "" { // trailing terminator
			i++
			continue
		}
		letter := st[0]
		if letter == 'R' || letter == 'C' {
			if i+2 >= len(toks) || toks[i+1] == "" || toks[i+2] == "" {
				return nil, fmt.Errorf("malformed name-status entry %q", st)
			}
			entries = append(entries, nsEntry{status: letter, oldPath: toks[i+1], path: toks[i+2]})
			i += 3
			continue
		}
		if i+1 >= len(toks) || toks[i+1] == "" {
			return nil, fmt.Errorf("malformed name-status entry %q", st)
		}
		entries = append(entries, nsEntry{status: letter, path: toks[i+1]})
		i += 2
	}
	return entries, nil
}

// eventsFromEntries maps name-status letters to change events. A rename is
// double-visible ({delete,rename} at the old path, {add,rename} at the new)
// so it cannot evade add/delete protection. An unknown letter is an error —
// fail-closed against future git status classes.
func eventsFromEntries(entries []nsEntry) ([]rules.ChangeEvent, error) {
	var events []rules.ChangeEvent
	for _, e := range entries {
		switch e.status {
		case 'A':
			events = append(events, rules.ChangeEvent{Path: e.path, Ops: []rules.Op{rules.OpAdd}})
		case 'M', 'T':
			events = append(events, rules.ChangeEvent{Path: e.path, Ops: []rules.Op{rules.OpModify}})
		case 'C':
			events = append(events, rules.ChangeEvent{Path: e.path, Ops: []rules.Op{rules.OpAdd}, Note: fmt.Sprintf("copied from %q", e.oldPath)})
		case 'D':
			events = append(events, rules.ChangeEvent{Path: e.path, Ops: []rules.Op{rules.OpDelete}})
		case 'R':
			events = append(events,
				rules.ChangeEvent{Path: e.oldPath, Ops: []rules.Op{rules.OpDelete, rules.OpRename}, Note: fmt.Sprintf("renamed to %q", e.path)},
				rules.ChangeEvent{Path: e.path, Ops: []rules.Op{rules.OpAdd, rules.OpRename}, Note: fmt.Sprintf("renamed from %q", e.oldPath)},
			)
		default:
			return nil, fmt.Errorf("unknown name-status letter %q for %q (fail-closed)", string(e.status), e.path)
		}
	}
	return events, nil
}

// ---- semantic evaluation ----

// semanticViolations evaluates one protected file for one commit: trigger on
// blob difference vs every parent, extract both sides via the configured check
// command, decompose into change units, and evaluate.
func semanticViolations(repoDir, ref, commit string, parents []string, role string, f *rules.CompiledFile, recCache map[string]map[string]rules.Record) ([]Violation, error) {
	newOID, newExists, err := blobAt(repoDir, commit, f.Path)
	if err != nil {
		return nil, err
	}
	oldOID, oldExists := "", false
	if len(parents) > 0 {
		oldOID, oldExists, err = blobAt(repoDir, parents[0], f.Path)
		if err != nil {
			return nil, err
		}
	}
	// Introduced only if the blob differs from every parent (root commits have
	// none, so any present side counts).
	if len(parents) == 0 && !newExists {
		return nil, nil
	}
	for _, p := range parents {
		pOID, pExists, err := blobAt(repoDir, p, f.Path)
		if err != nil {
			return nil, err
		}
		if pExists == newExists && pOID == newOID {
			return nil, nil // this side came from an already-authorized parent
		}
	}

	sideRecords := func(oid string, exists bool, side string) (map[string]rules.Record, []Violation, error) {
		if !exists {
			return map[string]rules.Record{}, nil, nil
		}
		key := f.Path + ":" + oid
		if recs, ok := recCache[key]; ok {
			return recs, nil, nil
		}
		content, err := blobContent(repoDir, oid)
		if err != nil {
			return nil, nil, err
		}
		recs, err := check.Records(f.Check, content)
		if err != nil {
			var rej *check.ContentRejectedError
			if errors.As(err, &rej) {
				return nil, []Violation{{
					Ref:  ref,
					Rule: "semantic-check-failed",
					Detail: fmt.Sprintf("commit %s: %s (%s side): %v",
						shortSHA(commit), f.Path, side, rej),
				}}, nil
			}
			return nil, nil, fmt.Errorf("%s (%s side of %s): %w", f.Path, side, shortSHA(commit), err)
		}
		recCache[key] = recs
		return recs, nil, nil
	}

	oldRecs, vs, err := sideRecords(oldOID, oldExists, "parent")
	if err != nil || len(vs) > 0 {
		return vs, err
	}
	newRecs, vs, err := sideRecords(newOID, newExists, "commit")
	if err != nil || len(vs) > 0 {
		return vs, err
	}

	units := f.DeltaUnits(oldRecs, newRecs)
	var out []Violation
	for _, d := range f.Evaluate(units, role) {
		out = append(out, Violation{
			Ref:    ref,
			Rule:   d.Rule,
			Detail: fmt.Sprintf("commit %s: %s: %s is denied for role %s", shortSHA(commit), f.Path, d.Detail, roleLabel(role)),
		})
	}
	return out, nil
}

// blobAt resolves rev:path to a blob oid. Exit status 1 means the path does
// not exist at that rev; a non-blob object (a tree or gitlink) at a protected
// path is an error — fail-closed rather than misread.
func blobAt(repoDir, rev, path string) (oid string, exists bool, err error) {
	out, err := git.OutputHermetic(repoDir, "rev-parse", "-q", "--verify", rev+":"+path)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	oid = strings.TrimSpace(out)
	typ, err := git.OutputHermetic(repoDir, "cat-file", "-t", oid)
	if err != nil {
		return "", false, err
	}
	if t := strings.TrimSpace(typ); t != "blob" {
		return "", false, fmt.Errorf("protected path %q at %s is a %s, not a file (fail-closed)", path, shortSHA(rev), t)
	}
	return oid, true, nil
}

// blobContent reads a blob, enforcing the input cap before the read.
func blobContent(repoDir, oid string) ([]byte, error) {
	sizeOut, err := git.OutputHermetic(repoDir, "cat-file", "-s", oid)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeOut), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("cat-file -s %s: unparseable size %q", oid, strings.TrimSpace(sizeOut))
	}
	if size > check.MaxInput {
		return nil, fmt.Errorf("protected blob %s is %d bytes, cap %d (fail-closed)", oid, size, check.MaxInput)
	}
	out, err := git.OutputHermetic(repoDir, "cat-file", "blob", oid)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}
