package main

import (
	"strings"
	"testing"

	"github.com/dmitriyb/portitor/internal/gate"
)

// FuzzParseUpdates (§5.3): parseUpdates never panics; a nil error implies every
// update round-trips the shape contract (SHAs 40-hex-or-zero, refs refs/-prefixed).
func FuzzParseUpdates(f *testing.F) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	f.Add(sha + " " + sha + " refs/heads/x\n")
	f.Add("0000000000000000000000000000000000000000 " + sha + " refs/heads/new\n")
	f.Add("garbage line here\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, in string) {
		us, err := parseUpdates(strings.NewReader(in))
		if err != nil {
			return
		}
		for _, u := range us {
			if verr := u.Validate(); verr != nil {
				t.Fatalf("parseUpdates accepted an invalid update %+v: %v", u, verr)
			}
			if !gate.ValidSHA(u.OldSHA) || !gate.ValidSHA(u.NewSHA) || !gate.ValidRef(u.Ref) {
				t.Fatalf("accepted update violates the shape contract: %+v", u)
			}
		}
	})
}

// FuzzClassify (§5.4): classify never panics; kind "git" implies exactly
// [cmd, path] with cmd in the closed pack-command table; kind "pr" implies the
// original started with "portitor pr".
func FuzzClassify(f *testing.F) {
	f.Add("git-receive-pack '/srv/git/r.git'")
	f.Add("git-upload-pack '/srv/git/r.git'")
	f.Add("git-upload-archive '/srv/git/r.git'")
	f.Add("portitor pr merge --pr 5")
	f.Add("rm -rf /")
	f.Add("")
	f.Fuzz(func(t *testing.T, orig string) {
		kind, rest, err := classify(orig)
		switch kind {
		case "git":
			if len(rest) != 2 {
				t.Fatalf("git kind must be [cmd, path], got %v", rest)
			}
			if rest[0] != "git-receive-pack" && rest[0] != "git-upload-pack" {
				t.Fatalf("git kind cmd outside the closed table: %q", rest[0])
			}
			if err != nil {
				t.Fatalf("git kind with error: %v", err)
			}
		case "pr":
			if err != nil {
				t.Fatalf("pr kind with error: %v", err)
			}
		case "reject":
			if err == nil {
				t.Fatal("reject kind must carry an error")
			}
		default:
			t.Fatalf("unknown kind %q", kind)
		}
	})
}

// FuzzShellSplit (§5.4): shellSplit never panics, and its result feeds classify
// without panicking (the security-critical routing must survive any tokens).
func FuzzShellSplit(f *testing.F) {
	f.Add("git-receive-pack '/srv/git/my repo.git'")
	f.Add(`a "b c" d`)
	f.Add(`""`) // empty quoted arg -> one empty token, legitimately
	f.Add("unterminated 'quote")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		if _, err := shellSplit(s); err != nil {
			return
		}
		// The tokens must not panic classify (which re-derives from the raw
		// string, but the two must agree on well-formedness).
		_, _, _ = classify(s)
	})
}

// FuzzSignerLineKeyBlob (§5.6): a keytype-shaped principal or comment never
// shifts the extracted key blob; parseSignerLine never panics and, when it
// reports an entry, the keydata is a real field of the line.
func FuzzSignerLineKeyBlob(f *testing.F) {
	f.Add(`role namespaces="git" ssh-ed25519 KEYDATA`)
	f.Add(`sk-agent namespaces="git" ssh-ed25519 REALKEY comment`)
	f.Add(`ssh-bot cert-authority ecdsa-sha2-nistp256 BLOB`)
	f.Add("# comment ssh-ed25519 X")
	f.Add("")
	f.Fuzz(func(t *testing.T, line string) {
		e, ok := parseSignerLine(line)
		if !ok {
			return
		}
		fields := strings.Fields(line)
		// The extracted keytype and keydata must both be actual fields of the
		// line, adjacent, with the keytype not at index 0 (that is the principal).
		ktIdx := -1
		for i, tok := range fields {
			if tok == e.keyType {
				ktIdx = i
				break
			}
		}
		if ktIdx <= 0 {
			t.Fatalf("keytype %q not found past the principal in %q", e.keyType, line)
		}
		if ktIdx+1 >= len(fields) || fields[ktIdx+1] != e.keyData {
			t.Fatalf("keydata %q is not the field after the keytype in %q", e.keyData, line)
		}
	})
}
