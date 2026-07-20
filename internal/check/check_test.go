package check

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/rules"
)

// TestMain intercepts the internal-check-exec re-exec: Records spawns
// os.Executable() — in tests, this test binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "internal-check-exec" {
		os.Exit(TrampolineMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
}

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "check.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRecordsViaInputFile(t *testing.T) {
	// The simplest filler: the content IS the envelope and the command is cat.
	def := rules.CheckDef{Command: []string{"cat", "data.json"}, InputFile: "data.json", RecordsPath: "wrap.records"}
	recs, err := Records(def, []byte(`{"wrap":{"records":[{"id":"r-1","stage":"draft"},{"id":"r-2","stage":"done"}]}}`))
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 2 || recs["r-1"]["stage"] != "draft" || recs["r-2"]["stage"] != "done" {
		t.Fatalf("records = %+v", recs)
	}
}

func TestRecordsViaStdin(t *testing.T) {
	requireSh(t)
	s := script(t, "exec cat\n")                // echo stdin back: content is the envelope
	def := rules.CheckDef{Command: []string{s}} // no records_path: output IS the array
	recs, err := Records(def, []byte(`[{"id":"a"},{"id":"b"}]`))
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %+v", recs)
	}
}

func TestRecordsCustomIDField(t *testing.T) {
	def := rules.CheckDef{Command: []string{"cat", "d.json"}, InputFile: "d.json", IDField: "key"}
	recs, err := Records(def, []byte(`[{"key":"k-1","v":1}]`))
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if _, ok := recs["k-1"]; !ok {
		t.Fatalf("records = %+v", recs)
	}
}

func TestRecordsContentRejected(t *testing.T) {
	requireSh(t)
	s := script(t, "echo not valid here >&2\nexit 7\n")
	def := rules.CheckDef{Command: []string{s}}
	_, err := Records(def, []byte("junk"))
	var rej *ContentRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("want ContentRejectedError, got %v", err)
	}
	if rej.ExitCode != 7 || !strings.Contains(rej.Excerpt, "not valid here") {
		t.Fatalf("rejection = %+v", rej)
	}
}

func TestRecordsOperationalFailures(t *testing.T) {
	t.Run("command not found", func(t *testing.T) {
		def := rules.CheckDef{Command: []string{"/nonexistent/tool-xyz"}}
		_, err := Records(def, []byte("{}"))
		if err == nil {
			t.Fatal("want an error")
		}
		var rej *ContentRejectedError
		if errors.As(err, &rej) {
			t.Fatalf("an unrunnable command is operational, not a content verdict: %v", err)
		}
	})
	t.Run("non-JSON output", func(t *testing.T) {
		requireSh(t)
		s := script(t, "echo this is not json\n")
		_, err := Records(rules.CheckDef{Command: []string{s}}, []byte("{}"))
		if err == nil || strings.Contains(err.Error(), "rejected") {
			t.Fatalf("want an operational parse error, got %v", err)
		}
	})
	t.Run("records_path missing", func(t *testing.T) {
		requireSh(t)
		s := script(t, `echo '{"other":[]}'`+"\n")
		_, err := Records(rules.CheckDef{Command: []string{s}, RecordsPath: "records"}, []byte("{}"))
		if err == nil {
			t.Fatal("want an error for a missing records key")
		}
	})
	t.Run("duplicate ids", func(t *testing.T) {
		requireSh(t)
		s := script(t, `echo '[{"id":"x"},{"id":"x"}]'`+"\n")
		_, err := Records(rules.CheckDef{Command: []string{s}}, []byte("{}"))
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("want a duplicate-id error, got %v", err)
		}
	})
	t.Run("input over cap", func(t *testing.T) {
		def := rules.CheckDef{Command: []string{"cat"}}
		_, err := Records(def, make([]byte, MaxInput+1))
		if err == nil {
			t.Fatal("want an input-cap error")
		}
	})
}

func TestRecordsMinimalEnv(t *testing.T) {
	requireSh(t)
	// The check command must not inherit the ambient environment beyond
	// PATH/HOME — a stray env var cannot change extraction behavior.
	t.Setenv("PORTITOR_TEST_LEAK", "leaked")
	s := script(t, `if [ -n "$PORTITOR_TEST_LEAK" ]; then echo '[{"id":"leaked"}]'; else echo '[{"id":"clean"}]'; fi`+"\n")
	recs, err := Records(rules.CheckDef{Command: []string{s}}, []byte("{}"))
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if _, ok := recs["clean"]; !ok {
		t.Fatalf("environment leaked into the check command: %+v", recs)
	}
}
