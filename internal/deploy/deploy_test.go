package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const testToken = "ghp_TESTTOKEN0123456789"

type fixture struct {
	t      *testing.T
	dir    string
	cfg    *Config
	fake   *fakeDaemon
	d      *Deployer
	token  []byte
	tokErr error
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the test binary stands in for portitor-gate, which must be a linux ELF")
	}
}

func newFixture(t *testing.T, repoJSON string) *fixture {
	t.Helper()
	requireLinux(t)
	dir := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(content), mode))
	}
	if repoJSON == "" {
		repoJSON = `{"format_version":1,"default_branch":"main"}`
	}
	write("gate/repos.d/r.json", repoJSON, 0o600)
	write("gate/allowed_signers", "", 0o600)
	write("keys/implementer.pub", "ssh-ed25519 AAAAimpl implementer@host\n", 0o644)
	write("deploy.json", `{
  "name": "portitor-test",
  "volume": "test-repos",
  "host_key_volume": "test-hostkey",
  "networks": [{"name": "test-net", "aliases": ["portitor-test"]}],
  "config_dir": "gate",
  "authorized_keys": ["keys/implementer.pub"],
  "token_command": ["print-token"],
  "host_key_file": "test.hostkey",
  "start_timeout": "200ms",
  "stop_timeout": "1s"
}`, 0o644)
	cfg, err := LoadConfig(filepath.Join(dir, "deploy.json"))
	must(t, err)
	exe, err := os.Executable()
	must(t, err)
	own, err := FileBuildID(exe)
	must(t, err)
	fake := newFakeDaemon(map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH])
	fake.networks["test-net"] = true
	fake.imagePrefix = "portitor-gate/portitor-test"
	fx := &fixture{t: t, dir: dir, cfg: cfg, fake: fake, token: []byte(testToken + "\n")}
	fx.d = &Deployer{
		Run:     fake,
		Token:   func(ctx context.Context, argv []string) ([]byte, error) { return fx.token, fx.tokErr },
		Arch:    runtime.GOARCH,
		Own:     own,
		GateBin: exe,
		LockDir: t.TempDir(),
		Poll:    5 * time.Millisecond,
	}
	return fx
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (fx *fixture) deploy() (*Result, error) {
	fx.t.Helper()
	fx.fake.calls = nil
	return fx.d.Deploy(context.Background(), fx.cfg)
}

func (fx *fixture) gate() *fakeContainer {
	return fx.fake.byRef(fx.cfg.Name)
}

func (fx *fixture) assertNoMutation() {
	fx.t.Helper()
	for _, c := range fx.fake.calls {
		if mutating(c) {
			fx.t.Fatalf("a refused deploy changed a container: docker %v", c.args)
		}
	}
}

func (fx *fixture) assertTokenNeverInArgv() {
	fx.t.Helper()
	for _, c := range fx.fake.calls {
		for _, a := range c.args {
			if strings.Contains(a, testToken) {
				fx.t.Fatalf("the PAT appeared in an argv: %s %v", c.name, c.args)
			}
		}
	}
}

func stepOf(err error) string {
	var se *StepError
	if errors.As(err, &se) {
		return se.Step
	}
	return ""
}

func TestDeployFreshGate(t *testing.T) {
	fx := newFixture(t, "")
	res, err := fx.deploy()
	must(t, err)
	g := fx.gate()
	if g == nil || g.state != "running" || g.id != res.ContainerID || g.label != fx.cfg.Name {
		t.Fatalf("gate after deploy: %+v, result %+v", g, res)
	}
	if string(g.files[tokenFile]) != testToken {
		t.Fatalf("delivered token = %q, want the trimmed PAT", g.files[tokenFile])
	}
	if !strings.HasPrefix(res.Image, "portitor-gate/portitor-test:") {
		t.Fatalf("image = %s", res.Image)
	}
	pin, err := os.ReadFile(fx.cfg.HostKeyFile)
	must(t, err)
	if strings.TrimSpace(string(pin)) != "ssh-ed25519 KEY-test-hostkey" {
		t.Fatalf("pin = %q", pin)
	}
	fx.assertTokenNeverInArgv()
	if len(fx.fake.containers) != 1 {
		t.Fatalf("containers after a fresh deploy: %d, want 1", len(fx.fake.containers))
	}
}

func TestDeployOrderNothingChangesBeforeChecks(t *testing.T) {
	fx := newFixture(t, "")
	_, err := fx.deploy()
	must(t, err)
	firstMutation := -1
	seen := map[string]int{}
	for i, c := range fx.fake.calls {
		key := c.name + " " + c.args[0]
		if c.name == "nix" {
			key = "nix build"
		}
		if c.name == "docker" && c.args[0] == "run" {
			if strings.Contains(strings.Join(c.args, " "), "validate-config") {
				key = "validate"
			} else {
				key = "hostkey"
			}
		}
		if _, ok := seen[key]; !ok {
			seen[key] = i
		}
		if firstMutation < 0 && mutating(c) {
			firstMutation = i
		}
	}
	for _, k := range []string{"docker info", "nix build", "validate", "hostkey"} {
		at, ok := seen[k]
		if !ok || at > firstMutation {
			t.Fatalf("%s at call %d (seen=%v), want before the first container change at %d", k, at, ok, firstMutation)
		}
	}
	var cpIn, start int
	for i, c := range fx.fake.calls {
		if c.name == "docker" && c.args[0] == "cp" && c.args[1] == "-" {
			cpIn = i
		}
		if c.name == "docker" && c.args[0] == "start" {
			start = i
		}
	}
	if cpIn == 0 || start < cpIn {
		t.Fatalf("PAT copied at %d, gate started at %d: want copy before start", cpIn, start)
	}
}

func TestDeployReplacesAndKeepsHostKey(t *testing.T) {
	fx := newFixture(t, "")
	first, err := fx.deploy()
	must(t, err)
	must(t, os.WriteFile(filepath.Join(fx.dir, "gate", "repos.d", "r.json"), []byte(`{"format_version":1,"default_branch":"trunk"}`), 0o600))
	cfg, err := LoadConfig(filepath.Join(fx.dir, "deploy.json"))
	must(t, err)
	fx.cfg = cfg
	second, err := fx.deploy()
	must(t, err)
	if first.Image == second.Image {
		t.Fatal("a changed config kept the image tag")
	}
	if fx.fake.byRef(first.ContainerID) != nil {
		t.Fatal("the previous gate was not removed on commit")
	}
	if len(fx.fake.containers) != 1 || fx.gate().id != second.ContainerID {
		t.Fatalf("containers after a replacing deploy: %d", len(fx.fake.containers))
	}
	if fx.fake.volKeys["test-hostkey"] != "ssh-ed25519 KEY-test-hostkey root@gate" {
		t.Fatal("the host key changed across deploys")
	}
	third, err := fx.deploy()
	must(t, err)
	for _, c := range fx.fake.calls {
		if c.name == "nix" {
			t.Fatal("an unchanged config ran nix build again")
		}
	}
	if third.Image != second.Image {
		t.Fatal("an unchanged config changed the tag")
	}
}

func TestDeployRefusesBeforeAnyContainerChange(t *testing.T) {
	type refusal struct {
		breakIt    func(fx *fixture)
		step, says string
	}
	cases := map[string]refusal{
		"daemon arch":     {func(fx *fixture) { fx.fake.arch = "s390x" }, "preflight", "Docker daemon runs s390x"},
		"missing network": {func(fx *fixture) { delete(fx.fake.networks, "test-net") }, "preflight", "network test-net"},
		"foreign container": {func(fx *fixture) {
			fx.fake.containers["x"] = &fakeContainer{id: "x", name: fx.cfg.Name, state: "running", files: map[string][]byte{}}
		}, "preflight", "not a portitor deploy"},
		"invalid repo config": {func(fx *fixture) { fx.fake.validateFail = true }, "validate", "does not validate as git"},
		"host key mismatch": {func(fx *fixture) {
			fx.fake.volKeys["test-hostkey"] = "ssh-ed25519 OTHER"
			must(fx.t, os.WriteFile(fx.cfg.HostKeyFile, []byte("ssh-ed25519 PINNED\n"), 0o644))
		}, "host key", "pin"},
		"token command fails": {func(fx *fixture) { fx.tokErr = errors.New("no such secret") }, "token", "no such secret"},
		"empty token":         {func(fx *fixture) { fx.token = []byte("\n") }, "token", "printed nothing"},
		"multi-line token":    {func(fx *fixture) { fx.token = []byte("a\nb\n") }, "token", "more than one line"},
		"gate build mismatch": {func(fx *fixture) { fx.d.Own.Revision = "0000000" }, "preflight", "reinstall or upgrade the pair"},
		"gate arch mismatch": {func(fx *fixture) {
			fx.d.Arch = map[string]string{"amd64": "arm64", "arm64": "amd64"}[runtime.GOARCH]
			fx.fake.arch = map[string]string{"amd64": "aarch64", "arm64": "x86_64"}[runtime.GOARCH]
		}, "preflight", "want a linux"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, "")
			c.breakIt(fx)
			_, err := fx.deploy()
			if err == nil || stepOf(err) != c.step || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want a %s refusal saying %q", err, c.step, c.says)
			}
			fx.assertNoMutation()
		})
	}
}

func TestDeployRestoresThePreviousGateWhenInterrupted(t *testing.T) {
	fx := newFixture(t, "")
	first, err := fx.deploy()
	must(t, err)
	must(t, os.WriteFile(filepath.Join(fx.dir, "gate", "allowed_signers"), []byte("changed\n"), 0o600))
	cfg, err := LoadConfig(filepath.Join(fx.dir, "deploy.json"))
	must(t, err)
	fx.cfg = cfg
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.fake.cancelOn = func(a []string) bool { return a[0] == "exec" && slices.Contains(a, "ssh-keyscan") }
	fx.fake.cancel = cancel
	fx.fake.calls = nil
	if _, err := fx.d.Deploy(ctx, fx.cfg); err == nil {
		t.Fatal("an interrupted deploy succeeded")
	}
	g := fx.gate()
	if g == nil || g.id != first.ContainerID || g.state != "running" || len(fx.fake.containers) != 1 {
		t.Fatalf("after an interrupt the gate is %+v (%d containers), want the previous %s running alone", g, len(fx.fake.containers), first.ContainerID)
	}
}

func TestDeployNeverWritesThePATToAFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	fx := newFixture(t, "")
	// Built at run time: a literal would sit in the test binary, which the
	// fixture stages as the gate binary.
	tok := "ghp_" + strings.Repeat(time.Now().Format("150405"), 3)
	fx.token = []byte(tok + "\n")
	var leaked []string
	fx.fake.onCall = func([]string) {
		_ = filepath.WalkDir(tmp, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), tok) {
					leaked = append(leaked, p)
				}
			}
			return nil
		})
	}
	_, err := fx.deploy()
	must(t, err)
	if len(leaked) > 0 {
		t.Fatalf("the PAT was written to %v", leaked)
	}
}

func TestDeployBracketsIPv6HostIP(t *testing.T) {
	fx := newFixture(t, "")
	fx.cfg.Ports = []Port{{HostIP: "::1", HostPort: 2222}, {HostIP: "127.0.0.1", HostPort: 0}}
	_, err := fx.deploy()
	must(t, err)
	var got []string
	for _, c := range fx.fake.calls {
		if c.args[0] == "create" {
			for i, a := range c.args {
				if a == "-p" {
					got = append(got, c.args[i+1])
				}
			}
		}
	}
	if !slices.Equal(got, []string{"[::1]:2222:22", "127.0.0.1:0:22"}) {
		t.Fatalf("published ports %v", got)
	}
}

func TestDeployRollsBackToThePreviousGate(t *testing.T) {
	cases := map[string]func(fx *fixture){
		"deliver": func(fx *fixture) {
			fx.fake.failOn = func(a []string) error {
				if a[0] == "cp" && a[1] == "-" {
					return errors.New("cp failed")
				}
				return nil
			}
		},
		"swap":             func(fx *fixture) { fx.fake.failOn = failNthStart(fx) },
		"wait":             func(fx *fixture) { fx.fake.exitOnStart = true },
		"verify owner":     func(fx *fixture) { fx.fake.tokenStat = "root 644" },
		"deliver readback": func(fx *fixture) { fx.fake.corruptReadback = true },
		"verify mounts":    func(fx *fixture) { fx.fake.extraMount = true },
		"verify env":       func(fx *fixture) { fx.fake.extraEnv = true },
		"verify host key":  func(fx *fixture) { fx.fake.otherServedKey = true },
		"verify github":    func(fx *fixture) { fx.fake.githubFail = true },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, `{"format_version":1,"default_branch":"main","upstream_slug":"o/r"}`)
			first, err := fx.deploy()
			must(t, err)
			must(t, os.WriteFile(filepath.Join(fx.dir, "gate", "allowed_signers"), []byte("changed\n"), 0o600))
			cfg, err := LoadConfig(filepath.Join(fx.dir, "deploy.json"))
			must(t, err)
			fx.cfg = cfg
			breakIt(fx)
			if _, err := fx.deploy(); err == nil {
				t.Fatal("deploy succeeded")
			}
			fx.fake.exitOnStart = false
			g := fx.gate()
			if g == nil || g.id != first.ContainerID || g.state != "running" {
				t.Fatalf("after a failed deploy the gate is %+v, want the previous %s running", g, first.ContainerID)
			}
			if len(fx.fake.containers) != 1 {
				t.Fatalf("containers after rollback: %d, want 1", len(fx.fake.containers))
			}
			for _, c := range fx.fake.calls {
				if c.args[0] == "volume" {
					t.Fatalf("a deploy touched a volume: %v", c.args)
				}
			}
		})
	}
}

// failNthStart fails the start of the new gate (the previous one was already
// started once by the first deploy, so only starts of ids not yet running fail).
func failNthStart(fx *fixture) func([]string) error {
	return func(a []string) error {
		if a[0] == "start" {
			if c := fx.fake.byRef(a[1]); c != nil && c.state == "created" {
				return errors.New("start failed")
			}
		}
		return nil
	}
}

func TestDeployResolvesLeftovers(t *testing.T) {
	fx := newFixture(t, "")
	first, err := fx.deploy()
	must(t, err)
	g := fx.gate()
	g.name, g.state = fx.cfg.Name+"-prev-dead", "exited"
	fx.fake.containers["stale"] = &fakeContainer{id: "stale", name: fx.cfg.Name + "-next-dead", label: fx.cfg.Name, state: "created", files: map[string][]byte{}}
	_, err = fx.deploy()
	must(t, err)
	if fx.fake.byRef("stale") != nil {
		t.Fatal("an unstarted -next- leftover was not removed")
	}
	if fx.fake.byRef(first.ContainerID) != nil {
		t.Fatal("the restored previous gate was not replaced by the new one")
	}
	if len(fx.fake.containers) != 1 {
		t.Fatalf("containers: %d, want 1", len(fx.fake.containers))
	}
}

func TestDeployRefusesPrevBesideCurrent(t *testing.T) {
	fx := newFixture(t, "")
	_, err := fx.deploy()
	must(t, err)
	fx.fake.containers["p"] = &fakeContainer{id: "p", name: fx.cfg.Name + "-prev-x", label: fx.cfg.Name, state: "exited", files: map[string][]byte{}}
	if _, err := fx.deploy(); err == nil || stepOf(err) != "preflight" {
		t.Fatalf("deploy with a -prev- beside the gate: %v, want a preflight refusal", err)
	}
	fx.assertNoMutation()
}

func TestDeployLockSerializesOneName(t *testing.T) {
	fx := newFixture(t, "")
	unlock, err := fx.d.lock(fx.cfg.Name)
	must(t, err)
	done := make(chan struct{})
	go func() {
		u, err := fx.d.lock(fx.cfg.Name)
		if err == nil {
			u()
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("a second lock of the same name did not block")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the second lock never acquired after release")
	}
}

func TestDeployNamesALostHostKeyVolume(t *testing.T) {
	fx := newFixture(t, "")
	must(t, os.WriteFile(fx.cfg.HostKeyFile, []byte("ssh-ed25519 PINNED\n"), 0o644))
	_, err := fx.deploy()
	if err == nil || stepOf(err) != "host key" || !strings.Contains(err.Error(), "holds no host key") {
		t.Fatalf("an empty host-key volume with a pin: %v, want a host key refusal naming the empty volume", err)
	}
	fx.assertNoMutation()
}
