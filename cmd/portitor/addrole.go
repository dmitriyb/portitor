package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dmitriyb/portitor/internal/config"
)

// fingerprintRe matches a signer-key fingerprint as git reports it via %GF:
// "SHA256:" followed by 43 chars of unpadded base64 (a SHA-256 digest).
var fingerprintRe = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// roleNameRe guards a role label: non-empty, no whitespace or path separators.
var roleNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// identityOnlyRoles is the closed denylist of roles that must NEVER be trusted to
// sign commits (landing-only credentials). Every other role name is a signing role.
// See spec/gate/arch_add_role.md.
var identityOnlyRoles = map[string]bool{"merger": true}

// addRole binds a signer-key fingerprint to a role inside an already-provisioned
// repo config (repos.d/<name>.json), a re-runnable init step. It upserts
// Roles[fingerprint]=role, optionally trusts a signing role's public key in the
// config's allowed_signers file, writes atomically, and re-validates. It never
// writes private key material. See spec/gate/arch_add_role.md.
func addRole(args []string) int {
	fs := flag.NewFlagSet("add-role", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository name (selects repos.d/<name>.json)")
	role := fs.String("role", "", "role to bind the fingerprint to")
	fp := fs.String("fingerprint", "", "signer key fingerprint (SHA256:...)")
	pub := fs.String("pub", "", "OpenSSH public key file to trust for a signing role (optional)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// ---- flag validation (usage errors, exit 2) ----
	if *repo == "" {
		fmt.Fprintln(os.Stderr, "add-role: --repo is required")
		return 2
	}
	if !config.ValidName(*repo) {
		fmt.Fprintf(os.Stderr, "add-role: invalid repo name %q (allowed: letters, digits, '.', '_', '-')\n", *repo)
		return 2
	}
	if !fingerprintRe.MatchString(*fp) {
		fmt.Fprintf(os.Stderr, "add-role: invalid fingerprint %q (want SHA256: + 43 base64 chars)\n", *fp)
		return 2
	}
	if !roleNameRe.MatchString(*role) {
		fmt.Fprintf(os.Stderr, "add-role: invalid role %q (allowed: letters, digits, '.', '_', '-')\n", *role)
		return 2
	}

	cfgPath := filepath.Join(config.ReposDir(), *repo+".json")

	// ---- load the existing config (operational errors, exit 1) ----
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "add-role: %v (run init-repo/add-repo first)\n", err)
		return 1
	}
	// Settings gives typed access to allowed_signers + the current role map.
	s, err := config.LoadFile(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "add-role: %v\n", err)
		return 1
	}
	// A raw object view preserves every field we do not touch when we rewrite.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		fmt.Fprintf(os.Stderr, "add-role: parse config %s: %v\n", cfgPath, err)
		return 1
	}

	identityOnly := identityOnlyRoles[*role]

	// ---- validate --pub BEFORE any write (so a bad pub never mutates state) ----
	var (
		pubKeyType string
		pubKeyData string
		havePub    bool
	)
	if *pub != "" {
		if identityOnly {
			fmt.Fprintf(os.Stderr, "add-role: role %q is identity-only; refusing --pub (its key must never gain commit-signing trust)\n", *role)
			return 1
		}
		gotFP, err := sshKeygenFingerprint(*pub)
		if err != nil {
			fmt.Fprintf(os.Stderr, "add-role: %v\n", err)
			return 1
		}
		if gotFP != *fp {
			fmt.Fprintf(os.Stderr, "add-role: --pub fingerprint %s does not match --fingerprint %s\n", gotFP, *fp)
			return 1
		}
		kt, kd, ok := pubKeyBlob(*pub)
		if !ok {
			fmt.Fprintf(os.Stderr, "add-role: %s is not a well-formed OpenSSH public key\n", *pub)
			return 1
		}
		pubKeyType, pubKeyData, havePub = kt, kd, true
	}

	// ---- overwrite-onto-identity-only guard (exit 1, before any write) ----
	// Rebinding a fingerprint that is currently trusted in allowed_signers to an
	// identity-only role would leave that key both landing-only AND able to sign.
	if identityOnly {
		trusted, err := allowedSignersHasFingerprint(s.AllowedSigners, *fp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "add-role: %v\n", err)
			return 1
		}
		if trusted {
			fmt.Fprintf(os.Stderr, "add-role: fingerprint %s is trusted in allowed_signers; refusing to rebind it to identity-only role %q (reconcile allowed_signers first)\n", *fp, *role)
			return 1
		}
	}

	// ---- decide what changes ----
	cur := s.Roles[*fp]
	roleChanged := cur != *role

	needAppend := false
	if havePub {
		present, err := allowedSignersHasBlob(s.AllowedSigners, pubKeyData)
		if err != nil {
			fmt.Fprintf(os.Stderr, "add-role: %v\n", err)
			return 1
		}
		needAppend = !present
	}

	if !roleChanged && !needAppend {
		fmt.Printf("add-role: %s %s -> %s (unchanged)\n", *repo, *fp, *role)
		return 0
	}

	// ---- write the role binding (atomic) ----
	if roleChanged {
		roles := s.Roles
		if roles == nil {
			roles = map[string]string{}
		}
		roles[*fp] = *role
		rolesJSON, err := json.Marshal(roles)
		if err != nil {
			fmt.Fprintf(os.Stderr, "add-role: %v\n", err)
			return 1
		}
		obj["roles"] = rolesJSON
		out, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "add-role: %v\n", err)
			return 1
		}
		out = append(out, '\n')
		mode := os.FileMode(0o644)
		if fi, err := os.Stat(cfgPath); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := atomicWrite(cfgPath, out, mode); err != nil {
			fmt.Fprintf(os.Stderr, "add-role: write config: %v\n", err)
			return 1
		}
	}

	// ---- append the trusted signer line (atomic, deduped) ----
	if needAppend {
		line := fmt.Sprintf("%s namespaces=\"git\" %s %s\n", *role, pubKeyType, pubKeyData)
		if err := appendAllowedSigner(s.AllowedSigners, line); err != nil {
			fmt.Fprintf(os.Stderr, "add-role: update allowed_signers: %v\n", err)
			return 1
		}
	}

	// ---- post-write validation: surface any (latent) config problem loudly ----
	final, err := config.LoadFile(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "add-role: reload config: %v\n", err)
		return 1
	}
	if problems := config.Validate(final); len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "add-role: %s is INVALID after write:\n", cfgPath)
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
		return 1
	}

	status := "added"
	switch {
	case cur != "" && roleChanged:
		status = fmt.Sprintf("was %s", cur)
	case !roleChanged:
		status = "role unchanged"
	}
	if needAppend {
		status += ", +allowed_signers"
	}
	fmt.Printf("add-role: %s %s -> %s (%s)\n", *repo, *fp, *role, status)
	return 0
}

// sshKeygenTimeout bounds the ssh-keygen subprocess (a local computation; a hang
// would otherwise block add-role indefinitely).
const sshKeygenTimeout = 15 * time.Second

// sshKeygenFingerprint computes the SHA256 fingerprint of an OpenSSH public key
// file by delegating to `ssh-keygen -lf` (mirroring the gate's "delegate the crypto
// to the tool" stance). A read/parse failure surfaces as an error.
func sshKeygenFingerprint(pubPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sshKeygenTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-l", "-f", pubPath)
	cmd.WaitDelay = 5 * time.Second
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh-keygen -lf %s: %w: %s", pubPath, err, strings.TrimSpace(errb.String()))
	}
	fields := strings.Fields(out.String())
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "SHA256:") {
		return "", fmt.Errorf("ssh-keygen -lf %s: unexpected output %q", pubPath, strings.TrimSpace(out.String()))
	}
	return fields[1], nil
}

// pubKeyBlob extracts the "<keytype> <keydata>" of an OpenSSH public key file
// (dropping any principals/options/comment), for the allowed_signers append.
func pubKeyBlob(pubPath string) (keyType, keyData string, ok bool) {
	b, err := os.ReadFile(pubPath)
	if err != nil {
		return "", "", false
	}
	return keyBlobFromFields(strings.Fields(strings.TrimSpace(string(b))))
}

// keyBlobFromFields finds the "<keytype> <keydata>" pair inside a pub-key line's
// token stream (which may carry a trailing comment after the key blob). A pub file
// has no leading principal, so the keytype is found by scanning from the first field.
func keyBlobFromFields(f []string) (keyType, keyData string, ok bool) {
	return keyBlobFromFieldsAt(f, 0)
}

// signerLineKeyBlob extracts the "<keytype> <keydata>" pair from an allowed_signers
// line, parsed POSITIONALLY: field[0] is the principal (here, the role name),
// optionally followed by options tokens, then the keytype/keydata pair. The keytype
// search therefore starts at index 1, never 0 — otherwise a keytype-shaped principal
// (a role literally named e.g. "sk-agent" or "ssh-bot", all of which pass roleNameRe)
// would be mis-read as the keytype and the real key blob mis-identified, which would
// silently defeat the identity-only rebind guard (allowedSignersHasFingerprint).
func signerLineKeyBlob(f []string) (keyType, keyData string, ok bool) {
	return keyBlobFromFieldsAt(f, 1)
}

// keyBlobFromFieldsAt finds the first "<keytype> <keydata>" pair at or after index
// start.
func keyBlobFromFieldsAt(f []string, start int) (keyType, keyData string, ok bool) {
	for i := start; i < len(f); i++ {
		if isKeyType(f[i]) && i+1 < len(f) {
			return f[i], f[i+1], true
		}
	}
	return "", "", false
}

func isKeyType(tok string) bool {
	return strings.HasPrefix(tok, "ssh-") ||
		strings.HasPrefix(tok, "ecdsa-") ||
		strings.HasPrefix(tok, "sk-")
}

// allowedSignersHasBlob reports whether the allowed_signers file already carries a
// line with the given key blob (dedup). A missing file is not present (no error).
func allowedSignersHasBlob(path, keyData string) (bool, error) {
	if path == "" || keyData == "" {
		return false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read allowed_signers %s: %w", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		for _, tok := range strings.Fields(line) {
			if tok == keyData {
				return true, nil
			}
		}
	}
	return false, nil
}

// allowedSignersHasFingerprint reports whether any key in the allowed_signers file
// has the given fingerprint. Used to guard rebinding a trusted key to an
// identity-only role. A missing file is not present (no error).
func allowedSignersHasFingerprint(path, fp string) (bool, error) {
	if path == "" {
		return false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read allowed_signers %s: %w", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		kt, kd, ok := signerLineKeyBlob(strings.Fields(line))
		if !ok {
			continue
		}
		got, err := keyBlobFingerprint(kt, kd)
		if err != nil {
			// A malformed existing line is skipped, not fatal.
			continue
		}
		if got == fp {
			return true, nil
		}
	}
	return false, nil
}

// keyBlobFingerprint computes the fingerprint of a "<keytype> <keydata>" pair by
// writing it to a temp pub file and delegating to ssh-keygen.
func keyBlobFingerprint(keyType, keyData string) (string, error) {
	tmp, err := os.CreateTemp("", "portitor-key-*.pub")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(keyType + " " + keyData + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return sshKeygenFingerprint(tmp.Name())
}

// appendAllowedSigner appends one line to the allowed_signers file, creating it
// (and any missing parent) at mode 0644 if absent. The write is atomic
// (temp-then-rename in the same dir); the caller has already deduped.
func appendAllowedSigner(path, line string) error {
	if path == "" {
		return fmt.Errorf("config has no allowed_signers path")
	}
	mode := os.FileMode(0o644)
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if fi, serr := os.Stat(path); serr == nil {
			mode = fi.Mode().Perm()
		}
	case os.IsNotExist(err):
		if derr := os.MkdirAll(filepath.Dir(path), 0o755); derr != nil {
			return derr
		}
		existing = nil
	default:
		return fmt.Errorf("read allowed_signers %s: %w", path, err)
	}
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		existing = append(existing, '\n')
	}
	return atomicWrite(path, append(existing, []byte(line)...), mode)
}

// atomicWrite writes data to a temp file in the target's directory and renames it
// over the target (atomic on one filesystem), so a reader never sees a partial file
// and a crash mid-write leaves the previous content intact.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	// fsync the temp file's contents before the rename, so a crash after the rename
	// cannot leave a renamed-but-empty file; combined with the parent-dir fsync below
	// this makes "a crash mid-write leaves the previous content intact" hold across a
	// power-loss/kernel crash, not just concurrent readers.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	// fsync the parent directory so the rename (the directory entry swap) is itself
	// durable. Best-effort: a directory that cannot be opened/synced does not undo the
	// already-committed rename.
	if dirf, err := os.Open(dir); err == nil {
		_ = dirf.Sync()
		_ = dirf.Close()
	}
	return nil
}
