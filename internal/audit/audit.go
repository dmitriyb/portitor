// Package audit appends portitor's decision trail: one JSON line per L1 gate
// decision, L2 action decision, or auto-open outcome. The trail is
// observability, not enforcement — a write failure never changes a verdict;
// callers report it loudly and proceed (a deliberate, documented trade-off so
// an audit-disk problem cannot block landing work).
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Event is one audit record. Zero-valued fields are omitted from the line.
type Event struct {
	Time        string   `json:"time"` // RFC3339, filled by Append when empty
	Kind        string   `json:"kind"` // "gate" | "action" | "auto-pr"
	Repo        string   `json:"repo,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Role        string   `json:"role,omitempty"`
	Action      string   `json:"action,omitempty"`
	PR          int      `json:"pr,omitempty"`
	Refs        []string `json:"refs,omitempty"`
	Verdict     string   `json:"verdict"` // accept|reject|allow|deny|error
	Reason      string   `json:"reason,omitempty"`
}

// Append writes one event to the trail at path (created 0600 if absent, with
// any missing parent directory created 0700) and fsyncs it — the record must
// survive a crash that follows the decision. An empty path disables the trail
// (no error).
func Append(path string, e Event) error {
	if path == "" {
		return nil
	}
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("audit: create dir for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("audit: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("audit: sync %s: %w", path, err)
	}
	return nil
}
