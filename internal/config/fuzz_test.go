package config

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoadFile (§5.5): arbitrary bytes never panic the loader; a config that
// loads cleanly also never panics Validate, round-trips its version guard, and
// loading is deterministic.
func FuzzLoadFile(f *testing.F) {
	f.Add([]byte(`{"format_version":1,"default_branch":"main","allowed_signers":"x","roles":{}}`))
	f.Add([]byte(`{"format_version":1,"roles":{"SHA256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"r"}}`))
	f.Add([]byte(`{"Roles":{"a":"b"},"roles":{}}`))
	f.Add([]byte(`{"format_version":1,"content_rules":{"version":1}}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		p := filepath.Join(dir, "cfg.json")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Skip()
		}
		s, err := LoadFile(p)
		if err != nil {
			return
		}
		// A clean load implies the exact supported version and a Validate that
		// must not panic.
		if s.FormatVersion != SupportedFormatVersion {
			t.Fatalf("clean load with version %d", s.FormatVersion)
		}
		_ = Validate(s)
		s2, err2 := LoadFile(p)
		if err2 != nil {
			t.Fatal("non-deterministic load")
		}
		if s2.FormatVersion != s.FormatVersion || len(s2.Roles) != len(s.Roles) {
			t.Fatal("non-deterministic decode")
		}
	})
}
