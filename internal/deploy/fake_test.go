package deploy

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// fakeDaemon models just enough of docker and nix for the deploy sequence:
// containers with names, labels, state, mounts, env and files; images; and a
// host key per volume, generated when a container first starts on it.
type fakeDaemon struct {
	arch        string
	networks    map[string]bool
	containers  map[string]*fakeContainer
	images      map[string]bool
	volKeys     map[string]string
	lastTag     string
	imagePrefix string
	nextID      int
	calls       []fakeCall

	failOn       func(args []string) error // injected failure for one command shape
	exitOnStart  bool
	tokenStat    string
	validateFail bool
	githubFail   bool

	extraMount, extraEnv, corruptReadback, otherServedKey bool

	onCall   func(args []string)
	cancelOn func(args []string) bool
	cancel   func()
}

type fakeContainer struct {
	id, name, label, state string
	mounts                 []string // "volume <name> <dest>"
	env                    []string
	files                  map[string][]byte
	restarts               int
}

type fakeCall struct {
	name  string
	args  []string
	stdin []byte
}

func newFakeDaemon(arch string) *fakeDaemon {
	return &fakeDaemon{
		arch:       arch,
		networks:   map[string]bool{},
		containers: map[string]*fakeContainer{},
		images:     map[string]bool{},
		volKeys:    map[string]string{},
		tokenStat:  "git 400",
	}
}

func (f *fakeDaemon) byRef(ref string) *fakeContainer {
	if c, ok := f.containers[ref]; ok {
		return c
	}
	for _, c := range f.containers {
		if c.name == ref {
			return c
		}
	}
	return nil
}

func (f *fakeDaemon) Run(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	f.calls = append(f.calls, fakeCall{name: name, args: append([]string(nil), args...), stdin: in})
	if f.failOn != nil {
		if err := f.failOn(args); err != nil {
			return nil, err
		}
	}
	if f.onCall != nil {
		f.onCall(args)
	}
	if f.cancelOn != nil && f.cancelOn(args) {
		f.cancel()
		return nil, ctx.Err()
	}
	if name == "nix" {
		for i, a := range args {
			if a == "--argstr" && i+2 < len(args) {
				f.lastTag = args[i+2]
			}
		}
		return []byte("/nix/store/fake-image.tar.gz\n"), nil
	}
	return f.docker(args, in)
}

func (f *fakeDaemon) docker(args []string, in []byte) ([]byte, error) {
	out := func(s string) ([]byte, error) { return []byte(s + "\n"), nil }
	switch {
	case args[0] == "info":
		return out(f.arch)
	case args[0] == "network" && args[1] == "inspect":
		if f.networks[args[len(args)-1]] {
			return out(args[len(args)-1])
		}
		return nil, fmt.Errorf("network %s not found", args[len(args)-1])
	case args[0] == "network" && args[1] == "connect":
		return out("")
	case args[0] == "image" && args[1] == "inspect":
		if f.images[args[len(args)-1]] {
			return out("sha256:fake")
		}
		return nil, fmt.Errorf("no such image")
	case args[0] == "load":
		f.images[f.loadedName()] = true
		return out("Loaded image: " + f.loadedName())
	case args[0] == "container" && args[1] == "inspect" && len(args) == 3:
		c := f.byRef(args[2])
		if c == nil {
			return nil, fmt.Errorf("No such container: %s", args[2])
		}
		return f.inspectJSON(c), nil
	case args[0] == "container" && args[1] == "inspect":
		ref := args[len(args)-1]
		c := f.byRef(ref)
		if c == nil {
			return nil, fmt.Errorf("Error: No such container: %s", ref)
		}
		if strings.Contains(args[3], "RestartCount") {
			return out(c.state + "|" + strconv.Itoa(c.restarts))
		}
		return out(c.id + "|" + c.label + "|" + c.state)
	case args[0] == "ps":
		var lines []string
		want := strings.TrimPrefix(args[3], "label="+label+"=")
		for _, c := range f.containers {
			if c.label == want {
				lines = append(lines, c.id+"|"+c.name+"|"+c.state)
			}
		}
		return out(strings.Join(lines, "\n"))
	case args[0] == "run":
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "validate-config") {
			if f.validateFail {
				return nil, fmt.Errorf("validate-config: invalid")
			}
			return out("ok")
		}
		for i, a := range args {
			if a == "-v" {
				vol, _, _ := strings.Cut(args[i+1], ":")
				return out(f.volKeys[vol])
			}
		}
		return nil, fmt.Errorf("unexpected run %v", args)
	case args[0] == "create":
		f.nextID++
		c := &fakeContainer{id: fmt.Sprintf("id%d", f.nextID), state: "created", files: map[string][]byte{}}
		for i := 1; i < len(args)-1; i++ {
			switch args[i] {
			case "--name":
				c.name = args[i+1]
			case "--label":
				c.label = strings.TrimPrefix(args[i+1], label+"=")
			case "-v":
				vol, dest, _ := strings.Cut(args[i+1], ":")
				c.mounts = append(c.mounts, "volume "+vol+" "+dest)
			case "-e":
				c.env = append(c.env, args[i+1])
			}
		}
		if f.extraMount {
			c.mounts = append(c.mounts, "bind /home/op/cfg /etc/portitor")
		}
		if f.extraEnv {
			c.env = append(c.env, "GH_TOKEN=leaked")
		}
		f.containers[c.id] = c
		return out(c.id)
	case args[0] == "cp" && args[1] == "-":
		ref, dir, _ := strings.Cut(args[2], ":")
		c := f.byRef(ref)
		tr := tar.NewReader(bytes.NewReader(in))
		h, err := tr.Next()
		if err != nil {
			return nil, err
		}
		b, _ := io.ReadAll(tr)
		c.files[filepath.Join(dir, h.Name)] = b
		return out("")
	case args[0] == "cp":
		ref, path, _ := strings.Cut(args[1], ":")
		b, ok := f.byRef(ref).files[path]
		if !ok {
			return nil, fmt.Errorf("no such file %s", path)
		}
		if f.corruptReadback {
			b = append(append([]byte(nil), b...), 'x')
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: filepath.Base(path), Mode: 0o400, Size: int64(len(b))})
		_, _ = tw.Write(b)
		_ = tw.Close()
		return buf.Bytes(), nil
	case args[0] == "stop":
		f.byRef(args[len(args)-1]).state = "exited"
		return out("")
	case args[0] == "rename":
		f.byRef(args[1]).name = args[2]
		return out("")
	case args[0] == "start":
		c := f.byRef(args[1])
		if f.exitOnStart && c.state == "created" {
			c.state = "exited"
			return out("")
		}
		c.state = "running"
		for _, m := range c.mounts {
			if strings.HasSuffix(m, " "+hostKeyMount) {
				vol := strings.Fields(m)[1]
				if f.volKeys[vol] == "" {
					f.volKeys[vol] = "ssh-ed25519 KEY-" + vol + " root@gate"
				}
			}
		}
		return out("")
	case args[0] == "rm":
		delete(f.containers, f.byRef(args[len(args)-1]).id)
		return out("")
	case args[0] == "logs":
		return out("gate log")
	case args[0] == "exec":
		return f.exec(args)
	}
	return nil, fmt.Errorf("fake docker: unhandled %v", args)
}

func (f *fakeDaemon) exec(args []string) ([]byte, error) {
	rest := args[1:]
	if rest[0] == "-u" {
		rest = rest[2:]
	}
	c := f.byRef(rest[0])
	if c == nil || c.state != "running" {
		return nil, fmt.Errorf("container not running")
	}
	vol := ""
	for _, m := range c.mounts {
		if strings.HasSuffix(m, " "+hostKeyMount) {
			vol = strings.Fields(m)[1]
		}
	}
	switch rest[1] {
	case "ssh-keyscan":
		return []byte("# 127.0.0.1:22 SSH-2.0-OpenSSH\n127.0.0.1 " + keyLine(f.volKeys[vol]) + "\n"), nil
	case "stat":
		return []byte(f.tokenStat + "\n"), nil
	case "cat":
		if f.otherServedKey {
			return []byte("ssh-ed25519 SOMETHING-ELSE\n"), nil
		}
		return []byte(f.volKeys[vol] + "\n"), nil
	case "sh":
		if f.githubFail {
			return nil, fmt.Errorf("HTTP 401: Bad credentials")
		}
		return []byte(rest[len(rest)-2] + "\n"), nil
	}
	return nil, fmt.Errorf("fake exec: unhandled %v", rest)
}

func (f *fakeDaemon) inspectJSON(c *fakeContainer) []byte {
	var mounts []string
	for _, m := range c.mounts {
		p := strings.Fields(m)
		mounts = append(mounts, fmt.Sprintf(`{"Type":%q,"Name":%q,"Destination":%q}`, p[0], p[1], p[2]))
	}
	var env []string
	for _, e := range c.env {
		env = append(env, strconv.Quote(e))
	}
	return []byte(fmt.Sprintf(`[{"State":{"Running":%v},"Mounts":[%s],"Config":{"Env":[%s]}}]`,
		c.state == "running", strings.Join(mounts, ","), strings.Join(env, ",")))
}

func (f *fakeDaemon) loadedName() string { return f.imagePrefix + ":" + f.lastTag }

// mutating reports whether a call changes a container.
func mutating(c fakeCall) bool {
	if c.name != "docker" || len(c.args) == 0 {
		return false
	}
	switch c.args[0] {
	case "create", "rm", "stop", "rename", "start", "cp":
		return true
	case "network":
		return len(c.args) > 1 && c.args[1] == "connect"
	}
	return false
}
