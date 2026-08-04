package action

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ReviewRecord is one appended line of the config's reviews_log: the gate's
// own internal verdict for a `review` invocation, keyed for last-wins lookup
// by (PR, HeadSHA) — a new push (new head) invalidates any prior verdict by
// construction, since the record no longer matches the current head.
type ReviewRecord struct {
	Time        string   `json:"time"` // RFC3339, filled by AppendReview when empty
	PR          int      `json:"pr"`
	HeadSHA     string   `json:"head_sha"`
	Fingerprint string   `json:"fingerprint"`
	Role        string   `json:"role"`
	Event       string   `json:"event"` // approve | request-changes | comment
	Threads     []string `json:"threads,omitempty"`
}

// AppendReview appends one review record to path (created 0600, missing
// parent directories 0700, fsync'd) — the same file-creation discipline as
// the audit trail (internal/audit). An empty path disables recording (no
// error): a deployment whose merge_gate.review is "github" or "none" may
// legitimately carry no reviews_log at all.
func AppendReview(path string, r ReviewRecord) error {
	if path == "" {
		return nil
	}
	if r.Time == "" {
		r.Time = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("reviews_log: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("reviews_log: create dir for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("reviews_log: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("reviews_log: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("reviews_log: sync %s: %w", path, err)
	}
	return nil
}

// LastReview returns the last-wins record for (pr, headSHA) — the whole file
// is scanned since JSONL has no index, but the log is append-only so the last
// matching line IS the last-wins verdict. ok is false when no record matches
// (including an empty/absent path — no reviews_log configured is the same as
// no verdict on file). A malformed line is a hard error: the gate must never
// silently skip past corrupted verdict state.
func LastReview(path string, pr int, headSHA string) (ReviewRecord, bool, error) {
	if path == "" {
		return ReviewRecord{}, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ReviewRecord{}, false, nil
		}
		return ReviewRecord{}, false, fmt.Errorf("reviews_log: open %s: %w", path, err)
	}
	defer f.Close()

	var last ReviewRecord
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec ReviewRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return ReviewRecord{}, false, fmt.Errorf("reviews_log: malformed line: %w", err)
		}
		if rec.PR == pr && rec.HeadSHA == headSHA {
			last, found = rec, true
		}
	}
	if err := sc.Err(); err != nil {
		return ReviewRecord{}, false, fmt.Errorf("reviews_log: read %s: %w", path, err)
	}
	return last, found, nil
}

// InternalApproval reports whether reviews_log holds a last-wins approval for
// (pr, headSHA) from a role action_roles allows to review — the merge_gate
// "internal" review-precondition source.
func InternalApproval(path string, pr int, headSHA string, actionRoles map[string][]string) (bool, error) {
	rec, ok, err := LastReview(path, pr, headSHA)
	if err != nil {
		return false, err
	}
	if !ok || rec.Event != "approve" {
		return false, nil
	}
	return RoleCan(actionRoles, rec.Role, "review"), nil
}

// GateThreadIDs returns the union of thread ids the gate's own reviews have
// ever recorded for pr (across every head — an unresolved thread does not
// stop mattering just because the branch moved on), deduplicated in first-seen
// order. Used by `review --event approve`'s auto-resolve and `resolve
// --gate-threads`; human-authored threads never appear here since only the
// gate's own review submissions populate reviews_log.
func GateThreadIDs(path string, pr int) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reviews_log: open %s: %w", path, err)
	}
	defer f.Close()

	seen := map[string]bool{}
	var ids []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec ReviewRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("reviews_log: malformed line: %w", err)
		}
		if rec.PR != pr {
			continue
		}
		for _, id := range rec.Threads {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reviews_log: read %s: %w", path, err)
	}
	return ids, nil
}
