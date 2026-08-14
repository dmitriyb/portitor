package mcpconfig

import "testing"

// FuzzParse: the strict decode never panics; anything it accepts carries the
// supported format version and survives Validate without panic.
func FuzzParse(f *testing.F) {
	f.Add(validBody())
	f.Add(`{"format_version":1}`)
	f.Add(`{"format_version":1,"roles":{"SHA256:x":"r"},"roles":{"SHA256:y":"r"}}`)
	f.Add(`{"Format_Version":1}`)
	f.Add(``)
	f.Fuzz(func(t *testing.T, in string) {
		s, err := Parse([]byte(in))
		if err != nil {
			return
		}
		if s.FormatVersion != SupportedFormatVersion {
			t.Fatalf("accepted config with format_version %d", s.FormatVersion)
		}
		_ = Validate(s)
	})
}
