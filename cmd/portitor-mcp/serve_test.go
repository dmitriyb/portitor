package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/portitor/internal/mcpconfig"
	"github.com/dmitriyb/portitor/internal/mcpwire"
)

func TestListenTarget(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")

	ln, err := listenTarget("unix:" + sock)
	if err != nil {
		t.Fatalf("fresh unix listen: %v", err)
	}
	ln.Close()

	// A stale socket file (unclean shutdown) is removed and re-listened.
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket setup: %v", err)
	}
	ln, err = listenTarget("unix:" + sock)
	if err != nil {
		t.Fatalf("stale socket not recovered: %v", err)
	}
	ln.Close()

	// A non-socket at the path is an operator error, never deleted.
	regular := filepath.Join(dir, "file")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenTarget("unix:" + regular); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("regular file at socket path: %v", err)
	}
	if _, err := os.Stat(regular); err != nil {
		t.Fatal("the non-socket file must not be deleted")
	}

	if _, err := listenTarget("bogus"); err == nil {
		t.Fatal("bogus target accepted")
	}
}

func TestMissingServerEnv(t *testing.T) {
	t.Setenv("MCP_TEST_PRESENT", "1")
	s := mcpconfig.Settings{Servers: map[string]mcpconfig.Server{
		"a": {Command: []string{"x"}, Env: []string{"MCP_TEST_PRESENT"}},
		"b": {Command: []string{"x"}, Env: []string{"MCP_TEST_ABSENT_VAR"}},
	}}
	problems := missingServerEnv(s)
	if len(problems) != 1 || !strings.Contains(problems[0], "MCP_TEST_ABSENT_VAR") {
		t.Fatalf("problems = %v", problems)
	}
}

func TestValidateConfigRun(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "ok.json")
	body := `{"format_version":1,"roles":{"` + fpImplementer + `":"implementer"},` +
		`"servers":{"s":{"command":["srv"]}},` +
		`"tools":{"t":{"server":"s","upstream_name":"t"}},"tool_roles":{"t":["implementer"]}}`
	if err := os.WriteFile(valid, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := validateConfigRun(valid); rc != 0 {
		t.Fatalf("valid config rc = %d", rc)
	}
	invalid := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(invalid, []byte(`{"format_version":1,"tools":{"t":{"server":"nope","upstream_name":""}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := validateConfigRun(invalid); rc != 1 {
		t.Fatalf("invalid config rc = %d", rc)
	}
	if rc := validateConfigRun(""); rc != 2 {
		t.Fatalf("missing path rc = %d", rc)
	}
	if rc := validateConfigRun(filepath.Join(dir, "absent.json")); rc != 1 {
		t.Fatalf("absent file rc = %d", rc)
	}
}

func TestServeRunRefusals(t *testing.T) {
	dir := t.TempDir()
	var errw bytes.Buffer
	if rc := serveRun("", "unix:"+filepath.Join(dir, "s"), &errw); rc != 2 {
		t.Fatalf("no config: rc = %d", rc)
	}
	cfgPath := filepath.Join(dir, "cfg.json")
	body := `{"format_version":1,"roles":{"` + fpImplementer + `":"implementer"},` +
		`"servers":{"s":{"command":["srv"],"env":["MCP_SERVE_TEST_ABSENT"]}},` +
		`"tools":{"t":{"server":"s","upstream_name":"t"}},"tool_roles":{"t":["implementer"]}}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	errw.Reset()
	if rc := serveRun(cfgPath, "", &errw); rc != 2 {
		t.Fatalf("no listen target: rc = %d", rc)
	}
	// The configured credential env is unset: boot refuses, loudly naming it.
	errw.Reset()
	if rc := serveRun(cfgPath, "unix:"+filepath.Join(dir, "s"), &errw); rc != 1 ||
		!strings.Contains(errw.String(), "MCP_SERVE_TEST_ABSENT") {
		t.Fatalf("unset env: rc = %d, stderr = %q", rc, errw.String())
	}
}

// TestServeEndToEnd wires the whole mediator over a real unix socket: serveRun
// with a config whose server re-execs this test binary as the fake upstream,
// a client connecting like the git-side splice would (header line first),
// then a mediated call.
func TestServeEndToEnd(t *testing.T) {
	t.Setenv("PORTITOR_MCP_TEST_UPSTREAM", "1")
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(dir, "audit.jsonl")
	cfg := map[string]any{
		"format_version": 1,
		"roles":          map[string]string{fpImplementer: "implementer"},
		"servers": map[string]any{
			"child": map[string]any{"command": []string{exe}, "env": []string{"PORTITOR_MCP_TEST_UPSTREAM"}},
		},
		"tools":      map[string]any{"probe": map[string]any{"server": "child", "upstream_name": "probe"}},
		"tool_roles": map[string][]string{"probe": {"implementer"}},
		"audit_log":  auditPath,
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "m.sock")
	go serveRun(cfgPath, "unix:"+sock, io.Discard) // test-scoped daemon; ends with the test process

	var conn net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err = net.Dial("unix", sock)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mediator did not come up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	hdr, _ := json.Marshal(mcpwire.Header{Fingerprint: fpImplementer})
	if _, err := conn.Write(append(hdr, '\n')); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	roundTrip := func(line string) mcpwire.Message {
		t.Helper()
		if _, err := conn.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		frame, err := mcpwire.ReadFrame(br, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		m, err := mcpwire.Parse(frame)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if init := roundTrip(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); init.Error != nil {
		t.Fatalf("initialize: %+v", init)
	}
	res := roundTrip(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"probe"}}`)
	if res.Error != nil || !strings.Contains(string(res.Result), "PORTITOR_MCP_TEST_UPSTREAM=1") {
		t.Fatalf("mediated call: %+v", res)
	}
	if events := readAudit(t, auditPath); len(events) == 0 {
		t.Fatal("no audit trail written")
	}
}
