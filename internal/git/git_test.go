package git

import "testing"

func TestValidRemoteName(t *testing.T) {
	ok := []string{"upstream", "origin", "u2", "my-remote", "my.remote"}
	bad := []string{"", "-upstream", "--force", "up stream", "up\tstream", "up\x01stream"}
	for _, n := range ok {
		if !ValidRemoteName(n) {
			t.Errorf("ValidRemoteName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if ValidRemoteName(n) {
			t.Errorf("ValidRemoteName(%q) = true, want false", n)
		}
	}
}
