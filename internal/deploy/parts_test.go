package deploy

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadConfigRefusals(t *testing.T) {
	base := `"volume":"vv","host_key_volume":"hh","config_dir":"c","authorized_keys":["k.pub"],"token_command":["t"],"host_key_file":"p"`
	cases := map[string]string{
		"unknown field":     `{"name":"gt",` + base + `,"extra":1}`,
		"bad name":          `{"name":"Gate!",` + base + `}`,
		"one-char name":     `{"name":"g",` + base + `}`,
		"trailing dash":     `{"name":"gate-",` + base + `}`,
		"double dot":        `{"name":"ga..te",` + base + `}`,
		"one-char volume":   `{"name":"gt","volume":"v","host_key_volume":"hk","config_dir":"c","authorized_keys":["k"],"token_command":["t"],"host_key_file":"p"}`,
		"same volumes":      `{"name":"gt","volume":"vv","host_key_volume":"vv","config_dir":"c","authorized_keys":["k"],"token_command":["t"],"host_key_file":"p"}`,
		"relative gate_bin": `{"name":"gt",` + base + `,"gate_bin":"bin/portitor-gate"}`,
		"bad package":       `{"name":"gt",` + base + `,"image":{"packages":["a;b"]}}`,
		"bad pin":           `{"name":"gt",` + base + `,"image":{"pin":{"rev":"main","sha256":"x"}}}`,
		"no token command":  `{"name":"gt","volume":"vv","host_key_volume":"hh","config_dir":"c","authorized_keys":["k"],"token_command":[],"host_key_file":"p"}`,
		"bad duration":      `{"name":"gt",` + base + `,"start_timeout":"soon"}`,
		"trailing data":     `{"name":"gt",` + base + `} {}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "d.json")
			must(t, os.WriteFile(p, []byte(body), 0o644))
			if _, err := LoadConfig(p); err == nil {
				t.Fatalf("accepted %s", body)
			}
		})
	}
}

func TestLoadConfigResolvesPathsAgainstItsDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "d.json")
	must(t, os.WriteFile(p, []byte(`{"name":"gt","volume":"vv","host_key_volume":"hh","config_dir":"c","authorized_keys":["k.pub","/abs/k2.pub"],
"token_command":["rel/cmd"],"host_key_file":"p","image":{"overlay":"o.nix"}}`), 0o644))
	c, err := LoadConfig(p)
	must(t, err)
	if c.ConfigDir != filepath.Join(dir, "c") || c.HostKeyFile != filepath.Join(dir, "p") ||
		c.Image.Overlay != filepath.Join(dir, "o.nix") || c.AuthorizedKeys[0] != filepath.Join(dir, "k.pub") || c.AuthorizedKeys[1] != "/abs/k2.pub" {
		t.Fatalf("paths not resolved: %+v", c)
	}
	if c.TokenCommand[0] != "rel/cmd" {
		t.Fatalf("token_command was path-resolved: %v", c.TokenCommand)
	}
	if c.startTimeout != defaultStartTimeout || c.stopTimeout != defaultStopTimeout {
		t.Fatalf("defaults: %s %s", c.startTimeout, c.stopTimeout)
	}
}

func stageFixture(t *testing.T) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "cfg", "repos.d"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "cfg", "repos.d", "r.json"), []byte(`{}`), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "cfg", "check.sh"), []byte("#!/bin/sh\n"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "cfg", ".hidden"), []byte("x"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "cfg", "r.json.lock"), []byte("x"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "gate"), []byte("ELF"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "overlay.nix"), []byte("self: super: {}"), 0o644))
	return &Config{Name: "g", ConfigDir: filepath.Join(dir, "cfg")}, dir
}

func tagOf(t *testing.T, c *Config, gate string) string {
	t.Helper()
	tag, err := stage(t.TempDir(), c, "amd64", gate)
	must(t, err)
	return tag
}

func TestStageNormalizesConfig(t *testing.T) {
	c, dir := stageFixture(t)
	out := t.TempDir()
	_, err := stage(out, c, "amd64", filepath.Join(dir, "gate"))
	must(t, err)
	for rel, mode := range map[string]os.FileMode{"config/repos.d/r.json": 0o644, "config/check.sh": 0o755, "portitor-gate": 0o755} {
		fi, err := os.Stat(filepath.Join(out, rel))
		must(t, err)
		if fi.Mode().Perm() != mode {
			t.Fatalf("%s mode %o, want %o", rel, fi.Mode().Perm(), mode)
		}
	}
	for _, rel := range []string{"config/.hidden", "config/r.json.lock"} {
		if _, err := os.Stat(filepath.Join(out, rel)); err == nil {
			t.Fatalf("%s was staged", rel)
		}
	}
}

func TestTagCoversEveryInput(t *testing.T) {
	c, dir := stageFixture(t)
	gate := filepath.Join(dir, "gate")
	if tagOf(t, c, gate) != tagOf(t, c, gate) {
		t.Fatal("equal inputs gave different tags")
	}
	changes := map[string]func(){
		"config bytes": func() {
			must(t, os.WriteFile(filepath.Join(dir, "cfg", "repos.d", "r.json"), []byte(`{"a":1}`), 0o600))
		},
		"config path": func() {
			must(t, os.Rename(filepath.Join(dir, "cfg", "check.sh"), filepath.Join(dir, "cfg", "check2.sh")))
		},
		"exec bit":    func() { must(t, os.Chmod(filepath.Join(dir, "cfg", "check.sh"), 0o600)) },
		"gate binary": func() { must(t, os.WriteFile(gate, []byte("ELF2"), 0o755)) },
		"overlay":     func() { c.Image.Overlay = filepath.Join(dir, "overlay.nix") },
		"package":     func() { c.Image.Packages = []string{"jq"} },
		"pin":         func() { c.Image.Pin = &Pin{Rev: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 52)} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			c2, dir2 := stageFixture(t)
			c, dir, gate = c2, dir2, filepath.Join(dir2, "gate")
			before := tagOf(t, c, gate)
			change()
			if after := tagOf(t, c, gate); after == before {
				t.Fatalf("changing the %s kept the tag", name)
			}
		})
	}
}

func TestTagCoversTheEmbeddedFiles(t *testing.T) {
	c, dir := stageFixture(t)
	gate := filepath.Join(dir, "gate")
	before := tagOf(t, c, gate)
	oldEntry, oldTmpl := entrypointScript, imageTemplate
	t.Cleanup(func() { entrypointScript, imageTemplate = oldEntry, oldTmpl })
	entrypointScript = append(append([]byte(nil), oldEntry...), "# changed\n"...)
	afterEntry := tagOf(t, c, gate)
	entrypointScript = oldEntry
	imageTemplate = oldTmpl + "\n# changed\n"
	afterTmpl := tagOf(t, c, gate)
	if afterEntry == before || afterTmpl == before {
		t.Fatalf("tag unchanged: entrypoint %v, expression %v", afterEntry == before, afterTmpl == before)
	}
}

func TestRenderedExpression(t *testing.T) {
	expr, err := renderExpr("g", "arm64", DefaultPin, []string{"jq", "python3Packages.pyyaml"}, true)
	must(t, err)
	s := string(expr)
	for _, want := range []string{
		`system = "aarch64-linux"`, DefaultPin.Rev, DefaultPin.SHA256, `(import ./overlay.nix)`,
		`pkgs."jq"`, `pkgs."python3Packages"."pyyaml"`, "pkgs.util-linux", "pkgs.gh", "pkgs.gitMinimal",
		`name = "portitor-gate/g"`, "HostKey /var/lib/portitor/hostkey/ssh_host_ed25519_key",
		"chmod 0700 run/secrets var/lib/portitor/hostkey", "{ tag }:",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("expression lacks %q", want)
		}
	}
	if strings.Contains(s, "Volumes") {
		t.Fatal("the image declares volumes")
	}
	plain, err := renderExpr("g", "amd64", DefaultPin, nil, false)
	must(t, err)
	if strings.Contains(string(plain), "overlay.nix") || !strings.Contains(string(plain), `"x86_64-linux"`) {
		t.Fatal("overlay or system wrong without an overlay")
	}
}

func TestCheckGateBinary(t *testing.T) {
	requireLinux(t)
	exe, err := os.Executable()
	must(t, err)
	own, err := FileBuildID(exe)
	must(t, err)
	must(t, checkGateBinary(exe, runtime.GOARCH, own))
	other := map[string]string{"amd64": "arm64", "arm64": "amd64"}[runtime.GOARCH]
	if err := checkGateBinary(exe, other, own); err == nil {
		t.Fatal("accepted a gate binary for the wrong arch")
	}
	if err := checkGateBinary(exe, runtime.GOARCH, BuildID{Version: "v9.9.9", Revision: own.Revision}); err == nil {
		t.Fatal("accepted a gate binary of another version")
	}
	notELF := filepath.Join(t.TempDir(), "x")
	must(t, os.WriteFile(notELF, []byte("#!/bin/sh\n"), 0o755))
	if err := checkGateBinary(notELF, runtime.GOARCH, own); err == nil {
		t.Fatal("accepted a non-ELF gate binary")
	}
}

func TestExecRunnerScrubsTokens(t *testing.T) {
	bin := t.TempDir()
	must(t, os.WriteFile(filepath.Join(bin, "envdump"), []byte("#!/bin/sh\nenv\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "leak1")
	t.Setenv("GITHUB_TOKEN", "leak2")
	t.Setenv("KEEP_ME", "kept")
	out, err := ExecRunner{}.Run(context.Background(), nil, "envdump")
	must(t, err)
	if strings.Contains(string(out), "leak") || !strings.Contains(string(out), "KEEP_ME=kept") {
		t.Fatalf("child env:\n%s", out)
	}
}

func TestTokenTar(t *testing.T) {
	b, err := tokenTar([]byte("tok"))
	must(t, err)
	tr := tar.NewReader(bytes.NewReader(b))
	h, err := tr.Next()
	must(t, err)
	got, err := io.ReadAll(tr)
	must(t, err)
	if h.Name != "gh_token" || h.Mode != 0o400 || string(got) != "tok" {
		t.Fatalf("entry %q mode %o content %q, want gh_token 0400 tok", h.Name, h.Mode, got)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("tar holds more than the token: %v", err)
	}
}
