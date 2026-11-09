package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Deploy path of spec/gate's Container component (test_pre_receive.md, "Deploy path"):
// deploy/run.sh, run for real against a fake docker that records every call.

const (
	fakeImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fakeToken   = "ghp_fake_token_for_run_sh_test"
	fakeName    = "pg"
)

// fakeDocker records each call as ASCII-unit-separated fields, one record per call
// (args may carry newlines, e.g. AGENT_AUTHORIZED_KEY). The first field says whether
// GH_TOKEN was in its environment, the rest are its args. It acts just enough for
// run.sh to proceed: build snapshots its context (modes kept) and prints $FAKE_ID, cp
// keeps a copy of the copied file and remembers its source, start refuses while that
// source still exists, and cp, start and validate-config fail on request.
const fakeDocker = `#!/usr/bin/env bash
{ printf 'env-gh=%s\x1f' "${GH_TOKEN+set}"; for a in "$@"; do printf '%s\x1f' "$a"; done; printf '\x1e'; } >>"$FAKE_LOG"
case "$1" in
build) cp -Rp "${@: -1}" "$FAKE_SNAP"; cat >"$FAKE_SNAP.dockerfile"; printf '%s\n' "$FAKE_ID" ;;
run) [[ " $* " == *" validate-config "* ]] && exit "${FAKE_VALIDATE_RC:-0}" ;;
cp) [[ -n "${FAKE_CP_RC:-}" ]] && exit "$FAKE_CP_RC"; printf '%s' "$2" >"$FAKE_CP_SRC"; cp "$2" "$FAKE_TOKEN_COPY" ;;
start) [[ -e "$(cat "$FAKE_CP_SRC")" ]] && exit 97; exit "${FAKE_START_RC:-0}" ;;
esac
exit 0
`

type dockerCall struct {
	ghTokenInEnv bool
	args         []string
}

type runShResult struct {
	err   error
	out   string
	calls []dockerCall
	cfg   string // the operator's config dir
	snap  string // the build context as docker saw it
	tmp   string // run.sh's TMPDIR
	token string // what docker cp received ("" if never called)
}

func runDeployRunSh(t *testing.T, extraEnv ...string) runShResult {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "deploy", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cfg")
	for _, f := range []struct {
		rel  string
		mode os.FileMode
		data string
	}{
		{"repos.d/r.json", 0o600, `{"default_branch":"main"}`},
		{"repos.d/r.json.lock", 0o600, ""},
		{".hidden", 0o600, "x"},
		{"checks/c.sh", 0o700, "#!/bin/sh\nexit 0\n"},
		{"../outside_signers", 0o600, "implementer ssh-ed25519 AAAA\n"},
	} {
		p := filepath.Join(cfg, f.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.data), f.mode); err != nil {
			t.Fatal(err)
		}
	}
	// A symlinked file must be baked as its content: the image cannot follow a host link.
	if err := os.Symlink(filepath.Join(dir, "outside_signers"), filepath.Join(cfg, "allowed_signers")); err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(dir, "implementer.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAAPUB implementer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	res := runShResult{cfg: cfg, snap: filepath.Join(dir, "snap"), tmp: tmp}
	logPath := filepath.Join(dir, "calls")
	tokenCopy := filepath.Join(dir, "token-copy")

	cmd := exec.Command(bash, script, "--config-dir", cfg, "--keys", pub, "--name", fakeName, "--image", "img")
	cmd.Env = append([]string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"HOME=" + dir,
		"TMPDIR=" + tmp,
		"GH_TOKEN=" + fakeToken,
		"FAKE_LOG=" + logPath,
		"FAKE_SNAP=" + res.snap,
		"FAKE_ID=" + fakeImageID,
		"FAKE_TOKEN_COPY=" + tokenCopy,
		"FAKE_CP_SRC=" + filepath.Join(dir, "cp-src"),
	}, extraEnv...)
	out, err := cmd.CombinedOutput()
	res.err, res.out = err, string(out)

	raw, rerr := os.ReadFile(logPath)
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatal(rerr)
	}
	for _, rec := range strings.Split(string(raw), "\x1e") {
		if rec == "" {
			continue
		}
		f := strings.Split(strings.TrimSuffix(rec, "\x1f"), "\x1f")
		res.calls = append(res.calls, dockerCall{ghTokenInEnv: f[0] == "env-gh=set", args: f[1:]})
	}
	if b, err := os.ReadFile(tokenCopy); err == nil {
		res.token = string(b)
	}
	return res
}

// firstCall returns the index of the first call whose subcommand is sub and whose
// args contain every one of want, or -1.
func firstCall(calls []dockerCall, sub string, want ...string) int {
	for i, c := range calls {
		if len(c.args) > 0 && c.args[0] == sub && containsAll(c.args, want) {
			return i
		}
	}
	return -1
}

func containsAll(c, want []string) bool {
	for _, w := range want {
		if !slices.Contains(c, w) {
			return false
		}
	}
	return true
}

// hasPair reports whether flag is immediately followed by value in args.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestDeployRunShSharesNoHostPath(t *testing.T) {
	r := runDeployRunSh(t)
	if r.err != nil {
		t.Fatalf("run.sh failed: %v\n%s", r.err, r.out)
	}
	if strings.Contains(r.out, fakeToken) {
		t.Errorf("run.sh printed the PAT")
	}

	for _, c := range r.calls {
		line := strings.Join(c.args, " ")
		if c.ghTokenInEnv {
			t.Errorf("`docker %s` ran with GH_TOKEN in its environment", line)
		}
		for i, a := range c.args {
			if strings.Contains(a, fakeToken) {
				t.Errorf("PAT appears in the argv of `docker %s`", line)
			}
			if strings.Contains(a, r.cfg) {
				t.Errorf("the host config dir appears in `docker %s`", line)
			}
			if a == "--mount" || strings.HasPrefix(a, "--mount=") || a == "--env-file" || strings.HasPrefix(a, "--env-file=") {
				t.Errorf("unexpected %s in `docker %s`", a, line)
			}
			if (a == "-e" || a == "--env") && i+1 < len(c.args) && strings.HasPrefix(c.args[i+1], "GH_TOKEN") {
				t.Errorf("GH_TOKEN forwarded into a container: `docker %s`", line)
			}
			if (a == "-v" || a == "--volume") && i+1 < len(c.args) && !strings.HasPrefix(c.args[i+1], "portitor-repos:") {
				t.Errorf("mount other than the repos volume: `docker %s`", line)
			}
		}
	}

	build := firstCall(r.calls, "build")
	validate := firstCall(r.calls, "run", fakeImageID, "validate-config", "/etc/portitor/repos.d/r.json")
	create := firstCall(r.calls, "create", fakeImageID)
	cp := firstCall(r.calls, "cp", fakeName+":/run/secrets/gh_token")
	start := firstCall(r.calls, "start", fakeName)
	if build < 0 || validate < 0 || create < 0 || cp < 0 || start < 0 {
		t.Fatalf("missing a step (build=%d validate=%d create=%d cp=%d start=%d) in %v", build, validate, create, cp, start, r.calls)
	}
	if !(build < validate && validate < create && create < cp && cp < start) {
		t.Errorf("wrong order: build=%d validate=%d create=%d cp=%d start=%d", build, validate, create, cp, start)
	}
	v := r.calls[validate].args
	if !slices.Contains(v, "--rm") || !hasPair(v, "--user", "git") || !hasPair(v, "--network", "none") ||
		!hasPair(v, "--entrypoint", "/usr/local/bin/portitor") || !hasPair(v, "--config", "/etc/portitor/repos.d/r.json") {
		t.Errorf("validation is not `run --rm --user git --network none --entrypoint portitor … --config <repo>`: %q", v)
	}
	if args := r.calls[create].args; args[len(args)-1] != fakeImageID {
		t.Errorf("container created from %q, want the built image ID", args[len(args)-1])
	}
	if !hasPair(r.calls[create].args, "-v", "portitor-repos:/srv/git") {
		t.Errorf("create lacks the repos volume: %q", r.calls[create].args)
	}
	if r.token != fakeToken {
		t.Errorf("docker cp delivered %q, want the PAT", r.token)
	}
	if left, _ := filepath.Glob(filepath.Join(r.tmp, "*")); len(left) > 0 {
		t.Errorf("run.sh left temporary files: %v", left)
	}

	df, err := os.ReadFile(r.snap + ".dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if want := "FROM img\nCOPY --chown=0:0 etc/portitor/ /etc/portitor/\n"; string(df) != want {
		t.Errorf("Dockerfile = %q, want %q", df, want)
	}
	root := filepath.Join(r.snap, "etc", "portitor")
	for rel, want := range map[string]os.FileMode{
		".":               0o755,
		"repos.d":         0o755,
		"repos.d/r.json":  0o644,
		"allowed_signers": 0o644,
		"checks":          0o755,
		"checks/c.sh":     0o755,
	} {
		fi, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("baked config lacks %s: %v", rel, err)
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("baked %s is a symlink; the image needs its content", rel)
			continue
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("baked %s mode %o, want %o", rel, got, want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "allowed_signers")); err != nil || string(b) != "implementer ssh-ed25519 AAAA\n" {
		t.Errorf("baked allowed_signers = %q (%v), want the symlink target's content", b, err)
	}
	for _, rel := range []string{"repos.d/r.json.lock", ".hidden"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err == nil {
			t.Errorf("baked config carries %s", rel)
		}
	}
}

func TestDeployRunShRefusesInvalidConfigBeforeTouchingTheGate(t *testing.T) {
	r := runDeployRunSh(t, "FAKE_VALIDATE_RC=1")
	if r.err == nil {
		t.Fatalf("run.sh succeeded although validate-config failed\n%s", r.out)
	}
	for _, sub := range []string{"rm", "create", "cp", "start"} {
		if i := firstCall(r.calls, sub); i >= 0 {
			t.Errorf("`docker %s` ran after a failed validation (a running gate must stay up)", sub)
		}
	}
	if r.token != "" {
		t.Errorf("the PAT was copied although validation failed")
	}
}

func TestDeployRunShRefusesBuildOutputWithoutAnImageID(t *testing.T) {
	r := runDeployRunSh(t, "FAKE_ID=some warning text")
	if r.err == nil {
		t.Fatalf("run.sh succeeded on build output without an image ID\n%s", r.out)
	}
	if i := firstCall(r.calls, "create"); i >= 0 {
		t.Errorf("a container was created from unparsed build output")
	}
}

func TestDeployRunShRemovesContainerWhenCopyFails(t *testing.T) {
	r := runDeployRunSh(t, "FAKE_CP_RC=1")
	if r.err == nil {
		t.Fatalf("run.sh succeeded although docker cp failed\n%s", r.out)
	}
	assertRemovedAfter(t, r, "cp")
	if i := firstCall(r.calls, "start"); i >= 0 {
		t.Errorf("the container was started without its PAT")
	}
}

func TestDeployRunShRemovesContainerWhenStartFails(t *testing.T) {
	r := runDeployRunSh(t, "FAKE_START_RC=1")
	if r.err == nil {
		t.Fatalf("run.sh succeeded although docker start failed\n%s", r.out)
	}
	assertRemovedAfter(t, r, "start")
}

// assertRemovedAfter fails unless `docker rm -f <name>` follows the first `sub` call.
func assertRemovedAfter(t *testing.T, r runShResult, sub string) {
	t.Helper()
	at := firstCall(r.calls, sub)
	if at < 0 {
		t.Fatalf("docker %s never ran: %v", sub, r.calls)
	}
	for _, c := range r.calls[at+1:] {
		if containsAll(c.args, []string{"rm", "-f", fakeName}) {
			return
		}
	}
	t.Errorf("the created container was not removed after the failed %s: %v", sub, r.calls[at:])
}
