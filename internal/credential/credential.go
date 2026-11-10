// Package credential reads the gate's GitHub credential: the file portitor
// deploy copies into the gate container. portitor never logs in to GitHub; every
// consumer reads this file per call and hands the token to its own child only.
package credential

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Path is the gate's credential file. A variable only so tests can point it at
// a temporary file.
var Path = "/run/secrets/gh_token"

// Read returns the token and whether the credential file exists. An absent file
// is not an error — an operator machine has none and keeps gh's and git's
// ambient authentication — but a present file that is empty, unreadable or
// multi-line is, so a gate with a broken credential fails loudly instead of
// falling back to ambient state.
func Read() (token string, present bool, err error) {
	b, err := os.ReadFile(Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", true, fmt.Errorf("credential: read %s: %w", Path, err)
	}
	tok := strings.TrimSuffix(string(b), "\n")
	if tok == "" {
		return "", true, fmt.Errorf("credential: %s is empty", Path)
	}
	if strings.ContainsAny(tok, "\r\n") {
		return "", true, fmt.Errorf("credential: %s holds more than one line", Path)
	}
	return tok, true, nil
}
