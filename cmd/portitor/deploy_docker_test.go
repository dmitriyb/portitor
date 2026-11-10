//go:build docker

package main

// The docker tier (spec/delivery/test_deploy.md): a real daemon and a real nix
// build, a gate deployed from this tree through the real deploy sequence, a
// dummy PAT, and a repo config without an upstream, so nothing reaches GitHub.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const dockerTierPAT = "ghp_docker_tier_dummy_token"

func requireDockerTier(t *testing.T) {
	t.Helper()
	require := os.Getenv("PORTITOR_REQUIRE_USERNS") == "1"
	if !dockerAvailable() {
		if require {
			t.Fatal("PORTITOR_REQUIRE_USERNS=1 but no Docker daemon is reachable")
		}
		t.Skip("docker not available")
	}
	opts, err := exec.Command("docker", "info", "--format", "{{json .SecurityOptions}}").Output()
	if err != nil {
		t.Fatalf("docker info: %v", err)
	}
	remapped := strings.Contains(string(opts), "name=userns")
	t.Logf("daemon security options: %s (userns-remap: %v)", strings.TrimSpace(string(opts)), remapped)
	if require && !remapped {
		t.Fatal("PORTITOR_REQUIRE_USERNS=1 but the daemon runs without userns-remap")
	}
}

func TestDockerTierDeploy(t *testing.T) {
	requireDockerTier(t)
	tmp := t.TempDir()
	roles := genRoleKeys(t, filepath.Join(tmp, "keys"), "implementer")
	impl := roles["implementer"]

	cfgDir := filepath.Join(tmp, "gate-config")
	if err := os.MkdirAll(filepath.Join(cfgDir, "repos.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAllowedSigners(t, filepath.Join(cfgDir, "allowed_signers"), impl)
	const repo = "dockertier"
	writeRepoJSON(t, cfgDir, repo, map[string]any{
		"format_version":  1,
		"default_branch":  "main",
		"allowed_signers": "/etc/portitor/allowed_signers",
		"roles":           map[string]string{impl.fingerprint: "implementer"},
	})
	// Owner-only, as operator tooling leaves it: only the image's mode
	// normalization lets the gate's git user read it under userns-remap.
	chmodTreePrivate(t, cfgDir)

	gi, _ := deployGate(t, cfgDir, roles, dockerTierPAT, nil)

	// No host path, no token in the env.
	var ins []struct {
		Mounts []struct{ Type, Name, Destination string }
		Config struct{ Env []string }
	}
	if err := json.Unmarshal([]byte(runDocker(t, "container", "inspect", gi.name)), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("inspect: %v", err)
	}
	var mounts []string
	for _, m := range ins[0].Mounts {
		mounts = append(mounts, m.Type+" "+m.Name+" "+m.Destination)
	}
	slices.Sort(mounts)
	want := []string{"volume " + gi.name + "-hostkey /var/lib/portitor/hostkey", "volume " + gi.name + "-repos /srv/git"}
	if !slices.Equal(mounts, want) {
		t.Fatalf("mounts = %v, want %v", mounts, want)
	}
	for _, kv := range ins[0].Config.Env {
		if strings.HasPrefix(kv, "GH_TOKEN=") || strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Fatalf("the gate's env carries %s", strings.SplitN(kv, "=", 2)[0])
		}
	}

	// The credential file: git-owned, 0400, the delivered bytes.
	if st := strings.TrimSpace(runDocker(t, "exec", gi.name, "stat", "-c", "%U %a", "/run/secrets/gh_token")); st != "git 400" {
		t.Fatalf("token file is %q, want git 400", st)
	}
	if got := runDocker(t, "exec", gi.name, "cat", "/run/secrets/gh_token"); got != dockerTierPAT {
		t.Fatalf("token file holds %q", got)
	}

	// The gate provisioned the repo at boot; a signed feature push passes
	// pre-receive and a fetch through the forced command sees it.
	dir := filepath.Join(tmp, "work")
	mustRun(t, "git", "init", "-q", "--initial-branch=main", dir)
	gitConfigSigning(t, dir, impl, "implementer@dockertier.test")
	mustRun(t, "git", "-C", dir, "remote", "add", "origin", gi.remoteURL(repo))
	mustRun(t, "git", "-C", dir, "checkout", "-q", "-b", "feature/dockertier")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", dir, "add", "-A")
	mustRun(t, "git", "-C", dir, "commit", "-q", "-S", "-m", "dockertier: hello")
	if out, err := gi.pushAsRole(t, impl, dir, "feature/dockertier"); err != nil {
		t.Fatalf("a signed feature push was refused:\n%s", out)
	}
	head := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))
	lsRemote := func() string {
		cmd := exec.Command("git", "-C", dir, "ls-remote", "origin", "refs/heads/feature/dockertier")
		cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gi.gitSSHCommand(impl.priv))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ls-remote through the gate: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := lsRemote(); !strings.HasPrefix(out, head) {
		t.Fatalf("ls-remote = %q, want the pushed %s", out, head)
	}

	// A second deploy with a changed config replaces the gate, keeps its host
	// key (the pin check would refuse otherwise) and its mirror.
	if err := os.WriteFile(filepath.Join(cfgDir, "notes.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldKH, err := os.ReadFile(gi.knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	gi2, _ := deployGate(t, cfgDir, roles, dockerTierPAT, gi)
	newKH, err := os.ReadFile(gi2.knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if keyOf(string(oldKH)) != keyOf(string(newKH)) {
		t.Fatalf("the host key changed across deploys:\n%s\n%s", oldKH, newKH)
	}
	gi = gi2
	mustRun(t, "git", "-C", dir, "remote", "set-url", "origin", gi.remoteURL(repo))
	if out := lsRemote(); !strings.HasPrefix(out, head) {
		t.Fatalf("after a re-deploy ls-remote = %q, want the pushed %s kept in the mirror", out, head)
	}
}

func writeRepoJSON(t *testing.T, cfgDir, repo string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "repos.d", repo+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// keyOf is the "<type> <base64>" of a known_hosts line.
func keyOf(line string) string {
	f := strings.Fields(line)
	if len(f) < 3 {
		return ""
	}
	return f[1] + " " + f[2]
}

// TestDockerTierBootRejection proves the entrypoint refuses to start — exits
// non-zero, never reaching sshd — on a gate with no credential file and on a
// repo config that does not validate. The containers are created from a
// deployed gate's image directly, because deploy's own validation would refuse
// the broken config before any container exists.
func TestDockerTierBootRejection(t *testing.T) {
	requireDockerTier(t)
	tmp := t.TempDir()
	roles := genRoleKeys(t, filepath.Join(tmp, "keys"), "implementer")
	cfgDir := filepath.Join(tmp, "gate-config")
	if err := os.MkdirAll(filepath.Join(cfgDir, "repos.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAllowedSigners(t, filepath.Join(cfgDir, "allowed_signers"), roles["implementer"])
	writeRepoJSON(t, cfgDir, "dockertier", map[string]any{
		"format_version":  1,
		"default_branch":  "main",
		"allowed_signers": "/etc/portitor/allowed_signers",
		"roles":           map[string]string{roles["implementer"].fingerprint: "implementer"},
	})
	gi, _ := deployGate(t, cfgDir, roles, dockerTierPAT, nil)

	tokenFile := filepath.Join(tmp, "gh_token")
	if err := os.WriteFile(tokenFile, []byte(dockerTierPAT), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(tmp, "broken.json")
	// Valid in every respect but the retired reviews_log key, which strict
	// decode refuses: the refusal must be that key's, nothing else's.
	broken, err := json.Marshal(map[string]any{
		"format_version":  1,
		"default_branch":  "main",
		"allowed_signers": "/etc/portitor/allowed_signers",
		"roles":           map[string]string{roles["implementer"].fingerprint: "implementer"},
		"reviews_log":     "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		setup func(id string)
		says  string
		cause string
	}{
		{"no credential file", func(string) {}, "gh_token is missing or empty", ""},
		{"invalid repo config", func(id string) {
			runDocker(t, "cp", tokenFile, id+":/run/secrets/gh_token")
			runDocker(t, "cp", invalid, id+":/etc/portitor/repos.d/broken.json")
		}, "broken.json is invalid", "reviews_log"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := strings.TrimSpace(runDocker(t, "create", gi.image))
			t.Cleanup(func() { tryDocker(t, "rm", "-f", id) })
			c.setup(id)
			runDocker(t, "start", id)
			deadline := time.Now().Add(time.Minute)
			state := ""
			for time.Now().Before(deadline) {
				state = strings.TrimSpace(runDocker(t, "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", id))
				if strings.HasPrefix(state, "exited ") {
					break
				}
				time.Sleep(300 * time.Millisecond)
			}
			logs, _ := dockerOut("logs", id)
			if !strings.HasPrefix(state, "exited ") || state == "exited 0" {
				t.Fatalf("the gate should refuse to start, got state %q; logs:\n%s", state, logs)
			}
			if !strings.Contains(string(logs), "refusing to start") || !strings.Contains(string(logs), c.says) || !strings.Contains(string(logs), c.cause) {
				t.Fatalf("the refusal should say %q (cause %q); logs:\n%s", c.says, c.cause, logs)
			}
			if strings.Contains(string(logs), "Server listening") {
				t.Fatalf("sshd started despite the refusal; logs:\n%s", logs)
			}
		})
	}
}
