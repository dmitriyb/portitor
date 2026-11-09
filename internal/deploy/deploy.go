package deploy

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	label          = "portitor.deploy"
	mirrorMount    = "/srv/git"
	hostKeyMount   = "/var/lib/portitor/hostkey"
	hostKeyPub     = hostKeyMount + "/ssh_host_ed25519_key.pub"
	tokenFile      = "/run/secrets/gh_token"
	gateBinaryName = "portitor-gate"
)

var slugRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Deployer runs one deploy. Every external effect goes through Run (docker,
// nix) or Token (the operator's token_command), so tests replace both.
type Deployer struct {
	Run     Runner
	Token   func(ctx context.Context, argv []string) ([]byte, error)
	Log     io.Writer
	Arch    string  // the host's GOARCH
	Own     BuildID // the running portitor's build
	GateBin string  // default gate binary path, used when the config names none
	LockDir string
	Poll    time.Duration
}

// StepError names the deploy step that failed.
type StepError struct {
	Step string
	Err  error
}

func (e *StepError) Error() string { return e.Step + ": " + e.Err.Error() }
func (e *StepError) Unwrap() error { return e.Err }

func step(name string, err error) error {
	if err == nil {
		return nil
	}
	return &StepError{Step: name, Err: err}
}

// Result is what a committed deploy produced.
type Result struct {
	ContainerID string
	Image       string
}

// container is one existing container of this deploy name.
type container struct {
	ID, Name, State, Label string
}

func (d *Deployer) logf(format string, a ...any) {
	if d.Log != nil {
		fmt.Fprintf(d.Log, "deploy: "+format+"\n", a...)
	}
}

func (d *Deployer) docker(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return d.Run.Run(ctx, stdin, "docker", args...)
}

func (d *Deployer) dockerS(ctx context.Context, args ...string) (string, error) {
	out, err := d.docker(ctx, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// Deploy converges the gate to c. Steps 1–8 change nothing the running gate
// depends on; from the swap on, a failure restores the previous gate.
func (d *Deployer) Deploy(ctx context.Context, c *Config) (*Result, error) {
	unlock, err := d.lock(c.Name)
	if err := step("lock", err); err != nil {
		return nil, err
	}
	defer unlock()

	prev, err := d.preflight(ctx, c)
	if err := step("preflight", err); err != nil {
		return nil, err
	}

	stageDir, err := os.MkdirTemp("", "portitor-deploy-")
	if err := step("build", err); err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageDir)
	image, err := d.build(ctx, c, stageDir)
	if err := step("build", err); err != nil {
		return nil, err
	}

	if err := step("validate", d.validate(ctx, image, stageDir)); err != nil {
		return nil, err
	}
	volKey, pin, err := d.checkHostKey(ctx, c, image)
	if err := step("host key", err); err != nil {
		return nil, err
	}
	token, err := d.readToken(ctx, c)
	if err := step("token", err); err != nil {
		return nil, err
	}
	auth, err := authorizedKeys(c.AuthorizedKeys)
	if err := step("create", err); err != nil {
		return nil, err
	}

	id, err := d.create(ctx, c, image, auth)
	if err := step("create", err); err != nil {
		return nil, err
	}
	// An interrupt cancels ctx; cleanup must still restore the previous gate.
	cleanup := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), c.stopTimeout+c.startTimeout)
	}
	if err := step("deliver", d.deliver(ctx, id, token)); err != nil {
		cctx, cancel := cleanup()
		defer cancel()
		d.remove(cctx, id)
		return nil, err
	}

	prevName, err := d.swap(ctx, c, prev, id)
	if err == nil {
		var scanned string
		if scanned, err = d.wait(ctx, c, id); err != nil {
			err = step("wait", err)
		} else if err = d.verify(ctx, c, id, stageDir, scanned, volKey, pin); err != nil {
			err = step("verify", err)
		} else {
			if err := step("commit", d.commit(ctx, c, prev, scanned, pin)); err != nil {
				return nil, err
			}
			d.logf("committed %s (%s)", id, image)
			return &Result{ContainerID: id, Image: image}, nil
		}
	} else {
		err = step("swap", err)
	}
	cctx, cancel := cleanup()
	defer cancel()
	d.rollback(cctx, c, prev, prevName, id)
	return nil, err
}

// ---- 1. lock ----

func (d *Deployer) lock(name string) (func(), error) {
	dir := d.LockDir
	if dir == "" {
		dir = defaultLockDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "deploy-"+name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func defaultLockDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "portitor")
	}
	return filepath.Join(os.TempDir(), "portitor-"+strconv.Itoa(os.Getuid()))
}

// ---- 2. preflight ----

var dockerArch = map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}

func (d *Deployer) gateBin(c *Config) string {
	if c.GateBin != "" {
		return c.GateBin
	}
	return d.GateBin
}

func (d *Deployer) preflight(ctx context.Context, c *Config) (*container, error) {
	if err := checkGateBinary(d.gateBin(c), d.Arch, d.Own); err != nil {
		return nil, err
	}
	a, err := d.dockerS(ctx, "info", "--format", "{{.Architecture}}")
	if err != nil {
		return nil, err
	}
	if dockerArch[a] != d.Arch {
		return nil, fmt.Errorf("the Docker daemon runs %s, this host is %s: the gate binary would not run", a, d.Arch)
	}
	for _, n := range c.Networks {
		if _, err := d.dockerS(ctx, "network", "inspect", "--format", "{{.Name}}", n.Name); err != nil {
			return nil, fmt.Errorf("network %s: %w (deploy never creates networks)", n.Name, err)
		}
	}
	cur, err := d.inspectByName(ctx, c.Name)
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.Label != c.Name {
		return nil, fmt.Errorf("container %s exists but is not a portitor deploy of %s (label %s=%q); remove it once by hand", c.Name, c.Name, label, cur.Label)
	}
	return d.resolveLeftovers(ctx, c, cur)
}

func (d *Deployer) inspectByName(ctx context.Context, name string) (*container, error) {
	out, err := d.dockerS(ctx, "container", "inspect", "--format",
		`{{.Id}}|{{index .Config.Labels "`+label+`"}}|{{.State.Status}}`, name)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such container") {
			return nil, nil
		}
		return nil, err
	}
	f := strings.Split(out, "|")
	if len(f) != 3 {
		return nil, fmt.Errorf("unexpected inspect output for %s: %q", name, out)
	}
	return &container{ID: f[0], Name: name, Label: f[1], State: f[2]}, nil
}

// resolveLeftovers clears what an interrupted deploy left: an unstarted new
// gate is removed; a previous gate with nothing in its place is restored.
func (d *Deployer) resolveLeftovers(ctx context.Context, c *Config, cur *container) (*container, error) {
	out, err := d.dockerS(ctx, "ps", "-a", "--filter", "label="+label+"="+c.Name, "--format", "{{.ID}}|{{.Names}}|{{.State}}")
	if err != nil {
		return nil, err
	}
	var prevs []container
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 3 {
			continue
		}
		k := container{ID: f[0], Name: f[1], State: f[2], Label: c.Name}
		switch {
		case strings.HasPrefix(k.Name, c.Name+"-next-"):
			d.logf("removing %s left by an interrupted deploy", k.Name)
			if _, err := d.dockerS(ctx, "rm", "-f", k.ID); err != nil {
				return nil, err
			}
		case strings.HasPrefix(k.Name, c.Name+"-prev-"):
			prevs = append(prevs, k)
		}
	}
	if len(prevs) == 0 {
		return cur, nil
	}
	if cur != nil || len(prevs) > 1 {
		return nil, fmt.Errorf("an interrupted deploy left %s beside %s; inspect them and remove the one you do not want", prevs[0].Name, c.Name)
	}
	p := prevs[0]
	d.logf("restoring %s left by an interrupted deploy", p.Name)
	if _, err := d.dockerS(ctx, "rename", p.ID, c.Name); err != nil {
		return nil, err
	}
	if _, err := d.dockerS(ctx, "start", p.ID); err != nil {
		return nil, err
	}
	p.Name = c.Name
	return &p, nil
}

// ---- 3. build ----

func (d *Deployer) build(ctx context.Context, c *Config, dir string) (string, error) {
	tag, err := stage(dir, c, d.Arch, d.gateBin(c))
	if err != nil {
		return "", err
	}
	// The staged copy is what the image bakes; checking it closes the window
	// in which an upgrade could swap the file after preflight read it.
	if err := checkGateBinary(filepath.Join(dir, gateBinaryName), d.Arch, d.Own); err != nil {
		return "", err
	}
	image := "portitor-gate/" + c.Name + ":" + tag
	if _, err := d.dockerS(ctx, "image", "inspect", "--format", "{{.Id}}", image); err == nil {
		d.logf("image %s exists, build skipped", image)
		return image, nil
	}
	d.logf("building %s with nix", image)
	out, err := d.Run.Run(ctx, nil, "nix", "--extra-experimental-features", "nix-command",
		"build", "--no-link", "--print-out-paths", "--file", filepath.Join(dir, "image.nix"), "--argstr", "tag", tag)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if path == "" || strings.Contains(path, "\n") {
		return "", fmt.Errorf("nix build printed %q, want one store path", path)
	}
	if _, err := d.dockerS(ctx, "load", "-i", path); err != nil {
		return "", err
	}
	if _, err := d.dockerS(ctx, "image", "inspect", "--format", "{{.Id}}", image); err != nil {
		return "", fmt.Errorf("docker load of %s did not produce %s: %w", path, image, err)
	}
	return image, nil
}

// ---- 4. validate ----

func repoConfigs(stageDir string) ([]string, error) {
	m, err := filepath.Glob(filepath.Join(stageDir, "config", "repos.d", "*.json"))
	if err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("config dir has no repos.d/*.json")
	}
	names := make([]string, len(m))
	for i, p := range m {
		names[i] = filepath.Base(p)
	}
	return names, nil
}

func (d *Deployer) validate(ctx context.Context, image, stageDir string) error {
	names, err := repoConfigs(stageDir)
	if err != nil {
		return err
	}
	for _, n := range names {
		if _, err := d.dockerS(ctx, "run", "--rm", "--network", "none", "--user", "git",
			"--entrypoint", "/usr/local/bin/portitor", image,
			"validate-config", "--config", "/etc/portitor/repos.d/"+n); err != nil {
			return fmt.Errorf("repos.d/%s does not validate as git inside the image: %w", n, err)
		}
	}
	return nil
}

// ---- 5. host key ----

// keyLine reduces an OpenSSH public key or keyscan line to "<type> <base64>".
func keyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if strings.HasPrefix(f[i], "ssh-") {
				return f[i] + " " + f[i+1]
			}
		}
	}
	return ""
}

func readPin(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	pin := keyLine(string(b))
	if pin == "" {
		return "", fmt.Errorf("pin file %s holds no host key line", path)
	}
	return pin, nil
}

func (d *Deployer) checkHostKey(ctx context.Context, c *Config, image string) (volKey, pin string, err error) {
	out, err := d.dockerS(ctx, "run", "--rm", "--network", "none", "--entrypoint", "/bin/sh",
		"-v", c.HostKeyVolume+":"+hostKeyMount, image,
		"-c", `if [ -f "$1" ]; then cat "$1"; fi`, "sh", hostKeyPub)
	if err != nil {
		return "", "", err
	}
	volKey = keyLine(out)
	if pin, err = readPin(c.HostKeyFile); err != nil {
		return "", "", err
	}
	switch {
	case pin != "" && volKey == "":
		return "", "", fmt.Errorf("pin %s exists but volume %s holds no host key; to rotate, remove both the volume and the pin", c.HostKeyFile, c.HostKeyVolume)
	case pin != "" && volKey != pin:
		return "", "", fmt.Errorf("volume %s holds host key %s, pin %s says %s; to rotate, remove both the volume and the pin", c.HostKeyVolume, volKey, c.HostKeyFile, pin)
	}
	return volKey, pin, nil
}

// ---- 6. token ----

func (d *Deployer) readToken(ctx context.Context, c *Config) ([]byte, error) {
	out, err := d.Token(ctx, c.TokenCommand)
	if err != nil {
		return nil, err
	}
	tok := bytes.TrimSuffix(out, []byte("\n"))
	if len(tok) == 0 {
		return nil, fmt.Errorf("token_command printed nothing")
	}
	if bytes.ContainsAny(tok, "\r\n") {
		return nil, fmt.Errorf("token_command printed more than one line")
	}
	return tok, nil
}

func authorizedKeys(paths []string) (string, error) {
	var sb strings.Builder
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		f := strings.Fields(string(b))
		if len(f) < 2 {
			return "", fmt.Errorf("%s is not an OpenSSH public key", p)
		}
		sb.WriteString(f[0] + " " + f[1] + "\n")
	}
	return sb.String(), nil
}

// ---- 7. create ----

func randSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (d *Deployer) create(ctx context.Context, c *Config, image, auth string) (string, error) {
	args := []string{"create", "--name", c.Name + "-next-" + randSuffix(),
		"--label", label + "=" + c.Name, "--restart", "unless-stopped",
		"-v", c.Volume + ":" + mirrorMount, "-v", c.HostKeyVolume + ":" + hostKeyMount,
		"-e", "AGENT_AUTHORIZED_KEY=" + auth}
	for _, p := range c.Ports {
		ip := p.HostIP
		if strings.Contains(ip, ":") {
			ip = "[" + ip + "]"
		}
		args = append(args, "-p", fmt.Sprintf("%s:%d:22", ip, p.HostPort))
	}
	id, err := d.dockerS(ctx, append(args, image)...)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("docker create printed no container ID")
	}
	for _, n := range c.Networks {
		a := []string{"network", "connect"}
		for _, al := range n.Aliases {
			a = append(a, "--alias", al)
		}
		if _, err := d.dockerS(ctx, append(a, n.Name, id)...); err != nil {
			d.remove(ctx, id)
			return "", err
		}
	}
	return id, nil
}

// ---- 8. deliver ----

func tokenTar(token []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "gh_token", Mode: 0o400, Size: int64(len(token)), ModTime: time.Unix(0, 0)}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(token); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (d *Deployer) deliver(ctx context.Context, id string, token []byte) error {
	t, err := tokenTar(token)
	if err != nil {
		return err
	}
	if _, err := d.docker(ctx, bytes.NewReader(t), "cp", "-", id+":"+filepath.Dir(tokenFile)+"/"); err != nil {
		return err
	}
	out, err := d.docker(ctx, nil, "cp", id+":"+tokenFile, "-")
	if err != nil {
		return err
	}
	tr := tar.NewReader(bytes.NewReader(out))
	h, err := tr.Next()
	if err != nil {
		return fmt.Errorf("read back %s: %w", tokenFile, err)
	}
	got, err := io.ReadAll(tr)
	if err != nil {
		return fmt.Errorf("read back %s: %w", tokenFile, err)
	}
	if h.Name != "gh_token" || !bytes.Equal(got, token) {
		return fmt.Errorf("%s read back differs from the token delivered", tokenFile)
	}
	return nil
}

// ---- 9. swap ----

func (d *Deployer) swap(ctx context.Context, c *Config, prev *container, id string) (prevName string, err error) {
	if prev != nil {
		secs := strconv.Itoa(int(c.stopTimeout.Round(time.Second) / time.Second))
		if _, err := d.dockerS(ctx, "stop", "-t", secs, prev.ID); err != nil {
			return "", err
		}
		prevName = c.Name + "-prev-" + randSuffix()
		if _, err := d.dockerS(ctx, "rename", prev.ID, prevName); err != nil {
			return "", err
		}
	}
	if _, err := d.dockerS(ctx, "rename", id, c.Name); err != nil {
		return prevName, err
	}
	if _, err := d.dockerS(ctx, "start", id); err != nil {
		return prevName, err
	}
	return prevName, nil
}

// ---- 10. wait ----

func (d *Deployer) wait(ctx context.Context, c *Config, id string) (string, error) {
	poll := d.Poll
	if poll == 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(c.startTimeout)
	for {
		st, err := d.dockerS(ctx, "container", "inspect", "--format", "{{.State.Status}}|{{.RestartCount}}", id)
		if err != nil {
			return "", err
		}
		status, restarts, _ := strings.Cut(st, "|")
		if status == "exited" || status == "dead" || (restarts != "" && restarts != "0") {
			logs, _ := d.docker(ctx, nil, "logs", "--tail", "50", id)
			return "", fmt.Errorf("the new gate stopped (%s, %s restarts):\n%s", status, restarts, tail(string(logs), 4096))
		}
		if status == "running" {
			if out, err := d.dockerS(ctx, "exec", id, "ssh-keyscan", "-t", "ed25519", "127.0.0.1"); err == nil {
				if k := keyLine(out); k != "" {
					return k, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the new gate's sshd did not answer within %s", c.startTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ---- 11. verify ----

type inspected struct {
	State struct {
		Running bool
	}
	Mounts []struct {
		Type, Name, Destination string
	}
	Config struct {
		Env []string
	}
}

func (d *Deployer) verify(ctx context.Context, c *Config, id, stageDir, scanned, volKey, pin string) error {
	out, err := d.docker(ctx, nil, "container", "inspect", id)
	if err != nil {
		return err
	}
	var ins []inspected
	if err := json.Unmarshal(out, &ins); err != nil || len(ins) != 1 {
		return fmt.Errorf("parse docker inspect: %v", err)
	}
	in := ins[0]
	if !in.State.Running {
		return fmt.Errorf("the new gate is not running")
	}
	var mounts []string
	for _, m := range in.Mounts {
		mounts = append(mounts, m.Type+" "+m.Name+" "+m.Destination)
	}
	slices.Sort(mounts)
	want := []string{"volume " + c.HostKeyVolume + " " + hostKeyMount, "volume " + c.Volume + " " + mirrorMount}
	slices.Sort(want)
	if !slices.Equal(mounts, want) {
		return fmt.Errorf("mounts are %v, want exactly %v", mounts, want)
	}
	for _, kv := range in.Config.Env {
		for _, s := range scrubbedVars {
			if strings.HasPrefix(kv, s+"=") {
				return fmt.Errorf("the gate's env carries %s", s)
			}
		}
	}
	if st, err := d.dockerS(ctx, "exec", id, "stat", "-c", "%U %a", tokenFile); err != nil {
		return err
	} else if st != "git 400" {
		return fmt.Errorf("%s is %q, want owner git and mode 400", tokenFile, st)
	}
	served, err := d.dockerS(ctx, "exec", id, "cat", hostKeyPub)
	if err != nil {
		return err
	}
	if k := keyLine(served); k != scanned {
		return fmt.Errorf("sshd serves %s but the volume holds %s", scanned, k)
	}
	if volKey != "" && scanned != volKey {
		return fmt.Errorf("the host key changed during the deploy: %s, was %s", scanned, volKey)
	}
	if pin != "" && scanned != pin {
		return fmt.Errorf("sshd serves %s, the pin says %s", scanned, pin)
	}
	return d.checkGitHub(ctx, id, stageDir)
}

// checkGitHub proves GitHub accepts the delivered PAT for every repo with an
// upstream; a deploy whose repos have none makes no GitHub call.
func (d *Deployer) checkGitHub(ctx context.Context, id, stageDir string) error {
	names, err := repoConfigs(stageDir)
	if err != nil {
		return err
	}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(stageDir, "config", "repos.d", n))
		if err != nil {
			return err
		}
		var rc struct {
			UpstreamSlug string `json:"upstream_slug"`
		}
		if err := json.Unmarshal(b, &rc); err != nil {
			return fmt.Errorf("repos.d/%s: %w", n, err)
		}
		if rc.UpstreamSlug == "" {
			continue
		}
		if !slugRE.MatchString(rc.UpstreamSlug) {
			return fmt.Errorf("repos.d/%s: upstream_slug %q is not owner/name", n, rc.UpstreamSlug)
		}
		if _, err := d.dockerS(ctx, "exec", "-u", "git", id, "sh", "-c",
			`GH_TOKEN=$(cat "$2") exec gh api "repos/$1" --jq .full_name`, "sh", rc.UpstreamSlug, tokenFile); err != nil {
			return fmt.Errorf("GitHub refuses the PAT for %s: %w", rc.UpstreamSlug, err)
		}
	}
	return nil
}

// ---- 12. commit / rollback ----

func (d *Deployer) commit(ctx context.Context, c *Config, prev *container, scanned, pin string) error {
	if prev != nil {
		if _, err := d.dockerS(ctx, "rm", prev.ID); err != nil {
			return fmt.Errorf("remove the previous gate %s: %w", prev.ID, err)
		}
	}
	if pin != "" {
		return nil
	}
	tmp := c.HostKeyFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(scanned+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.HostKeyFile); err != nil {
		return err
	}
	if got, err := readPin(c.HostKeyFile); err != nil || got != scanned {
		return fmt.Errorf("pin %s reads back %q (%v), want %q", c.HostKeyFile, got, err, scanned)
	}
	return nil
}

func (d *Deployer) remove(ctx context.Context, id string) {
	if _, err := d.dockerS(ctx, "rm", "-f", id); err != nil {
		d.logf("could not remove %s: %v", id, err)
	}
}

// rollback removes the new gate and puts the previous one back under its name.
func (d *Deployer) rollback(ctx context.Context, c *Config, prev *container, prevName, id string) {
	d.remove(ctx, id)
	if prev == nil {
		return
	}
	if prevName != "" {
		if _, err := d.dockerS(ctx, "rename", prev.ID, c.Name); err != nil {
			d.logf("could not rename the previous gate back: %v", err)
			return
		}
	}
	if _, err := d.dockerS(ctx, "start", prev.ID); err != nil {
		d.logf("could not restart the previous gate: %v", err)
		return
	}
	d.logf("restored the previous gate %s", prev.ID)
}
