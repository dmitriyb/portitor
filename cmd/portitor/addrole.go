package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/dmitriyb/portitor/internal/config"
)

// roleNameRe guards a role label: non-empty, no whitespace or path separators.
var roleNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// addRoleRun binds a signer-key fingerprint to a role inside an already-provisioned
// repo config (repos.d/<name>.json), a re-runnable init step. It upserts
// Roles[fingerprint]=role, optionally trusts a signing role's public key in the
// config's allowed_signers file, writes atomically, and re-validates. It never
// writes private key material. See spec/gate/arch_add_role.md.
func addRoleRun(repoV, roleV, fpV, pubV string) int {
	// Bind the cobra-parsed flag values to pointers so the read-decide-write body
	// below reads exactly as it did under the hand-rolled flag.FlagSet.
	repo, role, fp, pub := &repoV, &roleV, &fpV, &pubV

	// ---- flag validation (usage errors, exit 2) ----
	if *repo == "" {
		fmt.Fprintln(os.Stderr, "add-role: --repo is required")
		return 2
	}
	if !config.ValidName(*repo) {
		fmt.Fprintf(os.Stderr, "add-role: invalid repo name %q (allowed: letters, digits, '.', '_', '-')\n", *repo)
		return 2
	}
	if !config.ValidFingerprint(*fp) {
		fmt.Fprintf(os.Stderr, "add-role: invalid fingerprint %q (want SHA256: + 43 base64 chars)\n", *fp)
		return 2
	}
	if !roleNameRe.MatchString(*role) {
		fmt.Fprintf(os.Stderr, "add-role: invalid role %q (allowed: letters, digits, '.', '_', '-')\n", *role)
		return 2
	}

	cfgPath := filepath.Join(config.ReposDir(), *repo+".json")

	// ---- serialize: one writer at a time (operational errors, exit 1) ----
	// The exclusive lock spans the whole read-decide-write sequence over BOTH
	// files, including the identity-only guard's signers read — closing the
	// lost-update and check-then-act races between concurrent runs.
	unlock, err := acquireLock(cfgPath + ".lock")
	if err != nil {
		fmt.Fprintf(os.Stderr, "add-role: %v\n", err)
		return 1
	}
	defer unlock()

	// ---- load the existing config ONCE, from one buffer ----
	// The typed view and the preserved-raw view parse the same bytes, so a
	// hybrid write mixing two on-disk versions is impossible.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "add-role: %v (run init-repo/add-repo first)\n", err)
		return 1
	}
	s, err := config.Parse(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "add-role: config %s: %v\n", cfgPath, err)
		return 1
	}
	// A raw object view preserves every field we do not touch when we rewrite.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		fmt.Fprintf(os.Stderr, "add-role: parse config %s: %v\n", cfgPath, err)
		return 1
	}

	// The allowed_signers file may be SHARED across repos (one trust file, many
	// registry configs) — the per-config lock alone would let two runs on
	// different repos race on it. Second lock, always acquired after the config
	// lock (fixed order, no deadlock).
	if s.AllowedSigners != "" {
		// The signers file may not exist yet (first append creates it, parent
		// included) — the lock needs the parent directory now.
		if err := os.MkdirAll(filepath.Dir(s.AllowedSigners), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "add-role: %v\n", err)
			return 1
		}
		unlockSigners, err := acquireLock(s.AllowedSigners + ".lock")
		if err != nil {
			fmt.Fprintf(os.Stderr, "add-role: %v\n", err)
			return 1
		}
		defer unlockSigners()
	}

	// Signing-vs-identity classification is config, not code (portitor ships
	// no role names): the repo's identity_only_roles list decides.
	identityOnly := s.IdentityOnly(*role)

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
		warnHookDivergence(*repo, cfgPath)
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

	warnHookDivergence(*repo, cfgPath)

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

// warnHookDivergence cross-checks the baked hook path on every successful run
// (edits and idempotent no-ops alike): a repo whose pre-receive shim reads a
// DIFFERENT config than the registry file being edited would never see the
// grant. A deliberate split is the operator's right — but never silent.
func warnHookDivergence(repo, cfgPath string) {
	if baked, ok := bakedHookConfig(filepath.Join(config.ReposRoot(), repo+".git")); ok && !samePath(baked, cfgPath) {
		fmt.Fprintf(os.Stderr, "add-role: warning: repo %q's pre-receive hook reads %s, not %s — grants in this file will not reach that gate\n",
			repo, baked, cfgPath)
	}
}

// bakedHookConfig extracts the PORTITOR_CONFIG path baked into a bare repo's
// pre-receive shim. Like the shell, the LAST export line wins. ok is false when
// the repo, shim, or export line is absent (the repo may be provisioned
// elsewhere — no warning then).
func bakedHookConfig(bareDir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(bareDir, "hooks", "pre-receive"))
	if err != nil {
		return "", false
	}
	baked, found := "", false
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "export PORTITOR_CONFIG=")
		if !ok {
			continue
		}
		baked, found = shellUnquote(rest), true
	}
	return baked, found
}

// shellUnquote reverses shellQuote's single-quote wrapping (with the usual
// quote-close/escape/quote-reopen escape sequence for embedded single quotes);
// a value that isn't in that shape is returned as-is.
func shellUnquote(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") {
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'")
	}
	return s
}

// samePath compares two paths after normalization (best-effort abs + clean).
func samePath(a, b string) bool {
	na, err1 := filepath.Abs(filepath.Clean(a))
	nb, err2 := filepath.Abs(filepath.Clean(b))
	if err1 != nil || err2 != nil {
		return a == b
	}
	return na == nb
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
	kt, kd, _, ok := keyBlobFromFieldsAtIdx(f, start)
	return kt, kd, ok
}

// keyBlobFromFieldsAtIdx is keyBlobFromFieldsAt returning also the keytype's
// index (so callers can inspect the options region before it).
func keyBlobFromFieldsAtIdx(f []string, start int) (keyType, keyData string, idx int, ok bool) {
	for i := start; i < len(f); i++ {
		if isKeyType(f[i]) && i+1 < len(f) {
			return f[i], f[i+1], i, true
		}
	}
	return "", "", 0, false
}

// signerEntry is one parsed allowed_signers line, per the consumer's grammar:
// principal first, then options, then the keytype+keydata pair.
type signerEntry struct {
	keyType, keyData string
	certAuthority    bool
	gitNamespace     bool // namespaces option absent, or listing "git"
	timeBoxed        bool // valid-after / valid-before present
}

// live reports whether the entry would actually verify a git signature today,
// durably: git-namespace-valid and not time-boxed. Dedup counts ONLY live
// entries — a key blob appearing in a comment, a non-git-namespace line, or a
// time-boxed entry must not suppress appending the durable line the consumer
// needs.
func (e signerEntry) live() bool { return e.gitNamespace && !e.timeBoxed && !e.certAuthority }

// parseSignerLine parses one line. ok is false for blank lines, comments, and
// lines with no positional key blob (they cannot be keys the consumer trusts).
func parseSignerLine(line string) (signerEntry, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return signerEntry{}, false
	}
	f := strings.Fields(trimmed)
	kt, kd, idx, ok := keyBlobFromFieldsAtIdx(f, 1)
	if !ok {
		return signerEntry{}, false
	}
	e := signerEntry{keyType: kt, keyData: kd, gitNamespace: true}
	// The options region (fields between the principal and the keytype) is
	// itself a single comma-separated list per the allowed_signers grammar
	// (e.g. `cert-authority,namespaces="git"`), and option values may be quoted
	// and contain commas. Split each field on commas OUTSIDE quotes so a
	// compound option token cannot hide cert-authority from the guard.
	for _, field := range f[1:idx] {
		for _, opt := range splitOptions(field) {
			lower := strings.ToLower(opt)
			switch {
			case lower == "cert-authority":
				e.certAuthority = true
			case strings.HasPrefix(lower, "namespaces="):
				val := strings.Trim(opt[len("namespaces="):], `"`)
				e.gitNamespace = false
				for _, ns := range strings.Split(val, ",") {
					if strings.EqualFold(strings.TrimSpace(ns), "git") {
						e.gitNamespace = true
						break
					}
				}
			case strings.HasPrefix(lower, "valid-after="), strings.HasPrefix(lower, "valid-before="):
				e.timeBoxed = true
			}
		}
	}
	return e, true
}

// splitOptions splits an allowed_signers options field on commas that are not
// inside a double-quoted value (a quoted namespaces list like "git,file" stays
// one option).
func splitOptions(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())
	return out
}

// acquireLock takes an exclusive advisory flock on path (created 0600),
// returning the release func. Blocking: concurrent add-role runs serialize.
func acquireLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func isKeyType(tok string) bool {
	return strings.HasPrefix(tok, "ssh-") ||
		strings.HasPrefix(tok, "ecdsa-") ||
		strings.HasPrefix(tok, "sk-")
}

// allowedSignersHasBlob reports whether the allowed_signers file already carries
// a LIVE entry with the given key blob (dedup per the consumer's grammar: a
// blob appearing only in a comment, a non-git-namespace line, or a time-boxed
// entry does not count — the durable line is still needed). A missing file is
// not present (no error).
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
		e, ok := parseSignerLine(line)
		if ok && e.live() && e.keyData == keyData {
			return true, nil
		}
	}
	return false, nil
}

// allowedSignersHasFingerprint reports whether any key in the allowed_signers
// file has the given fingerprint. Used to guard rebinding a trusted key to an
// identity-only role, so its failure directions are conservative:
//   - an ssh-keygen failure on an existing entry is FATAL (a skipped line is
//     exactly a wrongly-passing guard), unlike a line with no key blob at the
//     positional slot, which cannot be a key the consumer trusts;
//   - a cert-authority entry anywhere refuses outright — certified keys cannot
//     be enumerated, so the fingerprint may be trusted indirectly;
//   - entries count regardless of namespace or validity window (the guard is
//     broad where the dedup is narrow).
//
// A missing file is not present (no error).
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
		e, ok := parseSignerLine(line)
		if !ok {
			continue
		}
		if e.certAuthority {
			return false, fmt.Errorf("allowed_signers %s contains a cert-authority entry; certified keys cannot be enumerated — refusing the identity-only rebind (reconcile allowed_signers out of band)", path)
		}
		got, err := keyBlobFingerprint(e.keyType, e.keyData)
		if err != nil {
			return false, fmt.Errorf("fingerprint existing allowed_signers entry: %w", err)
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
