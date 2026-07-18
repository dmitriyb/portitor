package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	if err := Append(path, Event{Kind: "gate", Repo: "r", Verdict: "accept", Refs: []string{"refs/heads/x"}}); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, Event{Kind: "action", Repo: "r", Action: "merge", PR: 7, Verdict: "deny", Reason: "unmet"}); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		if e.Time == "" {
			t.Fatalf("event missing timestamp: %q", sc.Text())
		}
		events = append(events, e)
	}
	if len(events) != 2 || events[0].Verdict != "accept" || events[1].PR != 7 {
		t.Fatalf("events = %+v", events)
	}
}

func TestAppendDisabledAndFailing(t *testing.T) {
	// Empty path disables the trail silently.
	if err := Append("", Event{Kind: "gate", Verdict: "accept"}); err != nil {
		t.Fatalf("empty path should be a no-op, got %v", err)
	}
	// An unwritable path errors (the caller reports it without changing the verdict).
	if err := Append("/nonexistent-dir/audit.jsonl", Event{Kind: "gate", Verdict: "accept"}); err == nil {
		t.Fatal("unwritable path should error")
	}
}
