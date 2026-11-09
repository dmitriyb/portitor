//go:build acceptance || docker

package main

// This file is the docker/ssh harness shared by the container-level suites —
// the docker tier (deploy_docker_test.go) and the real-GitHub acceptance suite
// (gate_accept_test.go): deploying a gate from THIS tree through the real
// `portitor deploy` sequence, with ephemeral role keys, and driving it over its
// real SSH front door (git push + `portitor pr`). See
// spec/delivery/test_deploy.md.
//
// Nothing here runs unless a top-level test's body executes — every function
// is a helper called FROM a test, never from an init/TestMain, so `go test
// -tags acceptance -run <a non-matching pattern>` never invokes docker at all.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmitriyb/portitor/internal/deploy"
)

// ---- docker plumbing ----

// dockerAvailable reports whether the docker CLI is on PATH and its daemon is
// reachable. Probed once per acceptance test (setupGateAccept) — never at
// package init/TestMain time, so an unmatched -run never touches docker.
func dockerAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Env = withoutTokens(os.Environ())
	return cmd.Run() == nil
}

// runDocker runs a docker subcommand, failing the test loudly on error. The PAT
// never travels in a docker argv any more (standUpGate copies it in as a file),
// so no caller needs runDockerSafe's redaction today.
func runDocker(t *testing.T, args ...string) string {
	t.Helper()
	return runDockerSafe(t, "", args...)
}

// runDockerSafe is runDocker with secret redacted from both the logged
// command and its captured output before either ever reaches a test failure
// message — kept for any future docker call that would have to embed the PAT.
func runDockerSafe(t *testing.T, secret string, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	cmd.Env = withoutTokens(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", redactSecret(strings.Join(args, " "), secret), err, redactSecret(string(out), secret))
	}
	return string(out)
}

// tryDocker runs a docker subcommand best-effort for teardown paths: a
// failure is logged, never fatal, so one cleanup step failing never aborts
// the rest of t.Cleanup.
func tryDocker(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("docker", args...)
	cmd.Env = withoutTokens(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("docker %s (cleanup, best-effort): %v\n%s", strings.Join(args, " "), err, out)
	}
}

func redactSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

// sanitizeTag makes t.Name() safe as a docker tag/name/volume component
// (lowercase, [a-z0-9._-] only).
func sanitizeTag(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// repoRoot locates the portitor working tree root (go.mod) from the test
// binary's working directory (cmd/portitor).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join(wd, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("could not locate the repo root (go.mod) from %s: %v", wd, err)
	}
	return root
}

var (
	gateBinOnce sync.Once
	gateBinPath string
	gateBinErr  error
)

// gateBinary builds portitor-gate from the CURRENT working tree (never a
// release) for the Docker daemon's architecture, once per test binary.
func gateBinary(t *testing.T) string {
	t.Helper()
	gateBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "portitor-gate-bin-")
		if err != nil {
			gateBinErr = err
			return
		}
		gateBinPath = filepath.Join(dir, "portitor-gate")
		cmd := exec.Command("go", "build", "-trimpath", "-o", gateBinPath, "./cmd/portitor")
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		if out, err := cmd.CombinedOutput(); err != nil {
			gateBinErr = fmt.Errorf("go build portitor-gate: %v\n%s", err, out)
		}
	})
	if gateBinErr != nil {
		t.Fatal(gateBinErr)
	}
	return gateBinPath
}

// ---- ephemeral role keys ----

// roleKey is one throwaway ed25519 keypair standing in for an agent/role
// identity — never the operator's own (YubiKey-backed) keys.
type roleKey struct {
	role        string
	priv, pub   string // file paths
	pubLine     string // "<keytype> <keydata>" (no comment) for AGENT_AUTHORIZED_KEY / allowed_signers
	fingerprint string // SHA256:... as git/ssh-keygen report it
}

// genRoleKeys generates one ed25519 keypair per role name under dir.
func genRoleKeys(t *testing.T, dir string, roles ...string) map[string]roleKey {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]roleKey, len(roles))
	for _, r := range roles {
		priv := filepath.Join(dir, r)
		if cout, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "gateaccept-"+r, "-f", priv).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen %s: %v\n%s", r, err, cout)
		}
		pubBytes, err := os.ReadFile(priv + ".pub")
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(string(pubBytes))
		if len(f) < 2 {
			t.Fatalf("unexpected pub key contents for %s: %q", r, pubBytes)
		}
		out[r] = roleKey{
			role:        r,
			priv:        priv,
			pub:         priv + ".pub",
			pubLine:     f[0] + " " + f[1],
			fingerprint: fingerprintOf(t, priv+".pub"),
		}
	}
	return out
}

func fingerprintOf(t *testing.T, pubPath string) string {
	t.Helper()
	out, err := exec.Command("ssh-keygen", "-lf", pubPath).Output()
	if err != nil {
		t.Fatalf("ssh-keygen -lf %s: %v", pubPath, err)
	}
	f := strings.Fields(string(out))
	if len(f) < 2 || !strings.HasPrefix(f[1], "SHA256:") {
		t.Fatalf("ssh-keygen -lf %s: unexpected output %q", pubPath, out)
	}
	return f[1]
}

// writeAllowedSigners writes an OpenSSH allowed_signers file trusting each
// given role's key for the "git" namespace (signing roles only — callers must
// not pass an identity-only role's key here).
func writeAllowedSigners(t *testing.T, path string, roles ...roleKey) {
	t.Helper()
	var b strings.Builder
	for _, rk := range roles {
		fmt.Fprintf(&b, "%s namespaces=\"git\" %s\n", rk.role, rk.pubLine)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- the gate container ----

// gateInstance is one stood-up gate container: its network/volume identity,
// mapped SSH port, and pinned host key, ready to be driven as any of the
// roles installed on it.
type gateInstance struct {
	name       string
	volume     string
	image      string
	port       int
	knownHosts string
	pinFile    string
}

// standUpGate deploys a gate from this tree through the real `portitor deploy`
// sequence (internal/deploy, with the real docker and nix): the given config
// directory baked in, the roles' public keys installed, the PAT delivered as the
// copied-in credential file, the SSH port published on 127.0.0.1. The PAT goes
// straight to the deployer, never through an env var a docker child could
// inherit. Every resource is torn down via t.Cleanup, even on failure.
func standUpGate(t *testing.T, cfgDir string, roles map[string]roleKey, pat string) *gateInstance {
	t.Helper()
	gi, _ := deployGate(t, cfgDir, roles, pat, nil)
	return gi
}

// deployGate is standUpGate returning the deploy config path as well; given
// prev, it re-deploys that gate (same name, volumes and pin).
func deployGate(t *testing.T, cfgDir string, roles map[string]roleKey, pat string, prev *gateInstance) (*gateInstance, string) {
	t.Helper()
	name, pinFile := "", ""
	if prev != nil {
		name, pinFile = prev.name, prev.pinFile
	}
	gate := gateBinary(t)
	work := filepath.Join(t.TempDir(), "deploy")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := name == ""
	if fresh {
		sum := sha256.Sum256([]byte(t.Name() + strconv.FormatInt(time.Now().UnixNano(), 10)))
		name = "pga-" + hex.EncodeToString(sum[:6])
	}
	var keys []string
	for role, rk := range roles {
		p := filepath.Join(work, role+".pub")
		if err := os.WriteFile(p, []byte(rk.pubLine+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, p)
	}
	if pinFile == "" {
		pinFile = filepath.Join(t.TempDir(), name+".hostkey")
	}
	cfg := map[string]any{
		"name":            name,
		"volume":          name + "-repos",
		"host_key_volume": name + "-hostkey",
		"ports":           []map[string]any{{"host_ip": "127.0.0.1", "host_port": 0}},
		"config_dir":      cfgDir,
		"authorized_keys": keys,
		"token_command":   []string{"unused-the-test-hands-the-token-over"},
		"host_key_file":   pinFile,
		"gate_bin":        gate,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(work, "deploy.json")
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Cleanup(func() {
			tryDocker(t, "rm", "-f", name)
			tryDocker(t, "volume", "rm", name+"-repos", name+"-hostkey")
		})
	}
	c, err := deploy.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	own, err := deploy.FileBuildID(gate)
	if err != nil {
		t.Fatal(err)
	}
	d := &deploy.Deployer{
		Run:     deploy.ExecRunner{},
		Token:   func(context.Context, []string) ([]byte, error) { return []byte(pat), nil },
		Log:     testWriter{t},
		Arch:    runtime.GOARCH,
		Own:     own,
		GateBin: gate,
		LockDir: t.TempDir(),
	}
	res, err := d.Deploy(context.Background(), c)
	if err != nil {
		logs, _ := dockerOut("logs", name)
		t.Fatalf("portitor deploy: %v\ngate logs:\n%s", err, logs)
	}
	t.Cleanup(func() { tryDocker(t, "rmi", res.Image) })
	gi := &gateInstance{name: name, volume: name + "-repos", image: res.Image, pinFile: pinFile}
	p, ok := gi.tryResolvePort()
	if !ok || !sshBannerReady(p) {
		t.Fatalf("deployed gate %s serves no SSH banner on its published port", name)
	}
	gi.port = p
	gi.pinHostKey(t, c.HostKeyFile)
	return gi, cfgPath
}

// testWriter sends deploy's progress lines to the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// sshBannerReady dials the mapped port and reads the SSH identification banner
// ("SSH-2.0-..."), the only signal that distinguishes a real serving sshd from
// Docker Desktop's port proxy accepting a connection before the container
// listens. Returns false (retry) on any dial/read error or a non-SSH prefix.
func sshBannerReady(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 4)
	n, rerr := conn.Read(buf)
	return rerr == nil && n == 4 && string(buf) == "SSH-"
}

func (gi *gateInstance) tryResolvePort() (int, bool) {
	out, err := dockerOut("port", gi.name, "22/tcp")
	if err != nil {
		return 0, false
	}
	line := strings.TrimSpace(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
	idx := strings.LastIndex(line, ":")
	if idx < 0 {
		return 0, false
	}
	p, err := strconv.Atoi(line[idx+1:])
	if err != nil {
		return 0, false
	}
	return p, true
}

// pinHostKey turns the pin deploy wrote into a throwaway known_hosts file, so
// the client connections below authenticate the SERVER too.
func (gi *gateInstance) pinHostKey(t *testing.T, pinFile string) {
	t.Helper()
	b, err := os.ReadFile(pinFile)
	if err != nil {
		t.Fatalf("read the host-key pin: %v", err)
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		t.Fatalf("unexpected pin content: %q", b)
	}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	line := fmt.Sprintf("[127.0.0.1]:%d %s %s\n", gi.port, f[0], f[1])
	if err := os.WriteFile(kh, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	gi.knownHosts = kh
}

// addRepo asserts the gate provisioned repo at boot from its config (the
// entrypoint's `add-repo --ensure`); running it again must be a no-op.
func (gi *gateInstance) addRepo(t *testing.T, repo, _ string) {
	t.Helper()
	runDocker(t, "exec", "-u", "git", gi.name, "portitor", "add-repo", "--ensure", "--repo", repo)
}

// ---- driving the gate over SSH ----

func (gi *gateInstance) sshBaseArgs(priv string) []string {
	return []string{
		"-i", priv,
		"-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=" + gi.knownHosts,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(gi.port),
	}
}

// gitSSHCommand renders GIT_SSH_COMMAND: a single shell-parsed string
// selecting priv as the client key and pinning the gate's host key.
func (gi *gateInstance) gitSSHCommand(priv string) string {
	parts := append([]string{"ssh"}, gi.sshBaseArgs(priv)...)
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellQuote(p)
	}
	return strings.Join(quoted, " ")
}

func (gi *gateInstance) remoteURL(repo string) string {
	return fmt.Sprintf("ssh://git@127.0.0.1:%d/srv/git/%s.git", gi.port, repo)
}

// cloneAsRole clones repo from the gate (exercising git-upload-pack through
// the forced-command dispatch) using rk's key.
func (gi *gateInstance) cloneAsRole(t *testing.T, rk roleKey, repo string) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "clone", "-q", gi.remoteURL(repo), dir)
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gi.gitSSHCommand(rk.priv))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone as %s: %v\n%s", rk.role, err, out)
	}
	return dir
}

// tryCloneAsRole is cloneAsRole without the test-fatal: for callers that must
// assert BOTH the accept and the reject path of a clone (e.g. an upstream
// unreachable at serve time must fail the clone loudly, not succeed stale).
func (gi *gateInstance) tryCloneAsRole(t *testing.T, rk roleKey, repo, dest string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", "clone", "-q", gi.remoteURL(repo), dest)
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gi.gitSSHCommand(rk.priv))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// pushAsRole pushes refspec from dir to the gate as rk (exercising
// pre/post-receive through git-receive-pack). It does NOT fail the test on a
// non-zero exit — callers assert both the accept and the reject paths.
func (gi *gateInstance) pushAsRole(t *testing.T, rk roleKey, dir, refspec string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "push", "origin", refspec)
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gi.gitSSHCommand(rk.priv))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runPRAs runs `portitor pr <args...>` over SSH as rk (exercising the
// forced-command's pr route), feeding stdin (body/--inline JSON) when
// non-empty. It does not fail the test — callers assert exit/stdout/stderr.
func (gi *gateInstance) runPRAs(t *testing.T, rk roleKey, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	remoteToks := append([]string{"portitor", "pr"}, args...)
	quoted := make([]string, len(remoteToks))
	for i, a := range remoteToks {
		quoted[i] = shellQuote(a)
	}
	full := append([]string{"ssh"}, gi.sshBaseArgs(rk.priv)...)
	full = append(full, "git@127.0.0.1", strings.Join(quoted, " "))

	cmd := exec.Command(full[0], full[1:]...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// gitConfigSigning points dir's git identity at rk's ephemeral key for SSH
// commit signing (git -c gpg.format=ssh ... commit -S), mirroring
// gate_test.go's newTestEnv / e2e_test.go's work-repo setup.
func gitConfigSigning(t *testing.T, dir string, rk roleKey, email string) {
	t.Helper()
	for _, c := range [][]string{
		{"user.name", rk.role},
		{"user.email", email},
		{"gpg.format", "ssh"},
		{"user.signingkey", rk.priv},
		{"commit.gpgsign", "true"},
	} {
		mustRun(t, "git", append([]string{"-C", dir, "config"}, c...)...)
	}
}

// chmodTreePrivate makes every file/dir under root owner-only (files 0600,
// dirs 0700), the way an operator's mktemp-then-mv tooling leaves a config: the
// gate's "git" user can then read it only if the bake normalizes modes, which a
// host bind mount never did.
func chmodTreePrivate(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return os.Chmod(path, 0o600)
	})
	if err != nil {
		t.Fatalf("chmodTreePrivate %s: %v", root, err)
	}
}

// withoutTokens drops the PAT variables the acceptance suite sets for gh and
// git, so no docker CLI the harness runs ever holds them.
func withoutTokens(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GH_TOKEN=") && !strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			out = append(out, kv)
		}
	}
	return out
}

// dockerOut runs a docker subcommand without the PAT variables in its env.
func dockerOut(args ...string) ([]byte, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = withoutTokens(os.Environ())
	return cmd.CombinedOutput()
}
