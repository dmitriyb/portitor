//go:build acceptance

package main

// This file is the docker/ssh harness backing the container-level gate
// acceptance suite (gate_accept_test.go): building the gate image from THIS
// tree, standing a container up on a throwaway network with ephemeral role
// keys, and driving it over its real SSH front door (git push + `portitor
// pr`). See spec/proposals/2026-08-05-gate-acceptance-suite.md.
//
// Nothing here runs unless a top-level acceptance test's body executes —
// every function is a helper called FROM a test, never from an init/TestMain,
// so `go test -tags acceptance -run <a non-matching pattern>` never invokes
// docker at all (see gate_accept_test.go's header for the exact command).

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
	return exec.CommandContext(ctx, "docker", "info").Run() == nil
}

// runDocker runs a docker subcommand, failing the test loudly on error. Not
// for calls whose args may embed a secret (e.g. `run -e GH_TOKEN=...`) — use
// runDockerSafe for those.
func runDocker(t *testing.T, args ...string) string {
	t.Helper()
	return runDockerSafe(t, "", args...)
}

// runDockerSafe is runDocker with secret redacted from both the logged
// command and its captured output before either ever reaches a test failure
// message — the one docker invocation that embeds the live PAT (`docker run
// -e GH_TOKEN=...`) must never leak it into a log.
func runDockerSafe(t *testing.T, secret string, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
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
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
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

// repoRoot locates the portitor working tree root (holding the Dockerfile
// this suite must build) from the test binary's working directory
// (cmd/portitor).
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
	if _, err := os.Stat(filepath.Join(root, "Dockerfile")); err != nil {
		t.Fatalf("could not locate the repo root (Dockerfile) from %s: %v", wd, err)
	}
	return root
}

// buildGateImage builds the gate image from the CURRENT working tree (never a
// release) under a tag unique to this test run, and registers its removal.
func buildGateImage(t *testing.T) string {
	t.Helper()
	tag := fmt.Sprintf("portitor-gateaccept:%s-%d", sanitizeTag(t.Name()), time.Now().UnixNano())
	runDocker(t, "build", "-q", "-t", tag, repoRoot(t))
	t.Cleanup(func() { tryDocker(t, "rmi", "-f", tag) })
	return tag
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
	network    string
	volume     string
	image      string
	port       int
	knownHosts string
}

// standUpGate builds the gate image from this tree, runs it on a throwaway
// network with a throwaway repos volume, the given per-repo config directory
// mounted read-only at /etc/portitor, and the given roles' public keys
// installed via AGENT_AUTHORIZED_KEY (the same mechanism deploy/run.sh uses).
// pat is delivered exactly as the deploy path delivers it: GH_TOKEN in the
// container's env (see deploy/entrypoint.sh). Every resource is torn down via
// t.Cleanup, in reverse creation order, even on failure.
func standUpGate(t *testing.T, cfgDir string, roles map[string]roleKey, pat string) *gateInstance {
	t.Helper()
	image := buildGateImage(t)
	uniq := sanitizeTag(t.Name()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	network := "portitor-ga-net-" + uniq
	volume := "portitor-ga-repos-" + uniq
	name := "portitor-ga-" + uniq

	runDocker(t, "network", "create", network)
	t.Cleanup(func() { tryDocker(t, "network", "rm", network) })
	runDocker(t, "volume", "create", volume)
	t.Cleanup(func() { tryDocker(t, "volume", "rm", volume) })

	var authKeys strings.Builder
	for _, rk := range roles {
		authKeys.WriteString(rk.pubLine)
		authKeys.WriteString("\n")
	}

	// Cleanup registered BEFORE the run call returns, via defer-free
	// t.Cleanup ordering: even if `docker run` itself fails partway (daemon
	// created the container object before erroring), rm -f is safe/no-op on
	// an absent container.
	t.Cleanup(func() { tryDocker(t, "rm", "-f", name) })
	runDockerSafe(t, pat, "run", "-d", "--name", name, "--network", network,
		"-p", "127.0.0.1::22",
		"-v", cfgDir+":/etc/portitor:ro",
		"-v", volume+":/srv/git",
		"-e", "GH_TOKEN="+pat,
		"-e", "AGENT_AUTHORIZED_KEY="+authKeys.String(),
		image,
	)

	gi := &gateInstance{name: name, network: network, volume: volume, image: image}
	gi.waitReady(t)
	gi.pinHostKey(t)
	return gi
}

// waitReady polls until the container is running AND its sshd is actually
// serving — verified by reading the "SSH-" identification banner, NOT a bare
// TCP accept. On macOS Docker Desktop the port-forward proxy accepts the TCP
// connection before anything in the container listens on 22, so a bare
// DialTimeout is a false positive that races ahead of the entrypoint (before
// even `ssh-keygen -A`, so the host key pinHostKey needs does not yet exist).
// A real banner arrives only once sshd is up, which is after the host keys are
// generated and the config-validate boot loop passed — exactly the state the
// pin and every driving connection require. On timeout, fail with the
// container's logs (the diagnostic that matters when the boot-time
// validate-config loop refused to start it).
func (gi *gateInstance) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		state, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", gi.name).Output()
		if err == nil && strings.TrimSpace(string(state)) == "true" {
			if p, ok := gi.tryResolvePort(); ok && sshBannerReady(p) {
				gi.port = p
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", gi.name).CombinedOutput()
	t.Fatalf("gate container %s did not become ready (no SSH banner) within 30s; logs:\n%s", gi.name, logs)
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
	out, err := exec.Command("docker", "port", gi.name, "22/tcp").Output()
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

// pinHostKey reads the gate's own ed25519 host key (generated by entrypoint's
// `ssh-keygen -A` at boot) and pins it into a throwaway known_hosts file, so
// the client connections below authenticate the SERVER too, not just the
// other way around.
func (gi *gateInstance) pinHostKey(t *testing.T) {
	t.Helper()
	raw, cerr := exec.Command("docker", "exec", gi.name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub").CombinedOutput()
	if cerr != nil {
		status, _ := exec.Command("docker", "inspect", "-f", "{{.State.Status}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}}", gi.name).Output()
		logs, _ := exec.Command("docker", "logs", gi.name).CombinedOutput()
		t.Fatalf("cat host key failed: %v\noutput: %s\ncontainer state: %s\ncontainer logs:\n%s", cerr, raw, strings.TrimSpace(string(status)), logs)
	}
	out := string(raw)
	f := strings.Fields(out)
	if len(f) < 2 {
		t.Fatalf("unexpected host key output: %q", out)
	}
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	line := fmt.Sprintf("[127.0.0.1]:%d %s %s\n", gi.port, f[0], f[1])
	if err := os.WriteFile(kh, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	gi.knownHosts = kh
}

// addRepo provisions the disposable repo on the gate via the REAL `portitor
// add-repo` (run inside the container as the git user, exactly as an
// operator would) — creates the bare repo, bakes the hook shims, and seeds
// the default branch from upstream.
func (gi *gateInstance) addRepo(t *testing.T, repo, upstreamURL string) {
	t.Helper()
	runDocker(t, "exec", "-u", "git", gi.name, "portitor", "add-repo", "--repo", repo, "--upstream", upstreamURL)
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

// chmodTreeReadable makes every file/dir under root world-readable (files
// a+r, dirs a+rx) so the container's "git" user — a different uid than the
// host user who wrote these files — can read the bind-mounted config
// regardless of platform-specific bind-mount uid mapping.
func chmodTreeReadable(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o755)
		}
		return os.Chmod(path, 0o644)
	})
	if err != nil {
		t.Fatalf("chmodTreeReadable %s: %v", root, err)
	}
}
