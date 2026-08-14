package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dmitriyb/portitor/internal/mcpconfig"
	"github.com/dmitriyb/portitor/internal/mcpwire"
)

// TestMain doubles as the fake upstream: when re-exec'd with
// PORTITOR_MCP_TEST_UPSTREAM=1 the test binary becomes a real MCP stdio
// server child, so process supervision (spawn, env allowlist, reap) is tested
// against real processes — the same self-exec pattern internal/check's
// trampoline tests use.
func TestMain(m *testing.M) {
	if os.Getenv("PORTITOR_MCP_TEST_UPSTREAM") == "1" {
		fakeUpstreamMain()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeUpstreamMain speaks the MCP server side over stdio: it advertises one
// tool, "probe", whose call result reports the process's environ and argv.
// IGNORE_TERM=1 ignores SIGTERM and never exits on stdin EOF (the
// KILL-after-grace path).
func fakeUpstreamMain() {
	if os.Getenv("IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	br := bufio.NewReader(os.Stdin)
	for {
		frame, err := mcpwire.ReadFrame(br, 1<<20)
		if err != nil {
			if os.Getenv("IGNORE_TERM") == "1" {
				select {} // refuse to exit: only SIGKILL ends this process
			}
			return
		}
		msg, err := mcpwire.Parse(frame)
		if err != nil {
			continue
		}
		var reply mcpwire.Message
		switch msg.Method {
		case "initialize":
			reply, _ = mcpwire.NewResult(msg.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "fake-child", "version": "0"},
			})
		case "notifications/initialized":
			continue
		case "tools/list":
			reply, _ = mcpwire.NewResult(msg.ID, map[string]any{
				"tools": []map[string]any{{"name": "probe"}},
			})
		case "tools/call":
			reply, _ = mcpwire.NewResult(msg.ID, map[string]any{
				"env":  os.Environ(),
				"args": os.Args,
			})
		default:
			continue
		}
		if err := mcpwire.WriteFrame(os.Stdout, reply); err != nil {
			return
		}
	}
}

// probeSettings builds a config whose one server re-execs this test binary as
// the fake upstream, with extraEnv passed through the allowlist.
func probeSettings(t *testing.T, extraArgs, envNames []string) mcpconfig.Settings {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return mcpconfig.Settings{
		FormatVersion: mcpconfig.SupportedFormatVersion,
		Roles:         map[string]string{fpImplementer: "implementer"},
		Servers: map[string]mcpconfig.Server{
			"child": {Command: append([]string{exe}, extraArgs...), Env: envNames},
		},
		Tools: map[string]mcpconfig.Tool{
			"probe": {Server: "child", UpstreamName: "probe"},
		},
		ToolRoles: map[string][]string{"probe": {"implementer"}},
		Limits:    &mcpconfig.Limits{HandshakeTimeout: "10s", CallTimeout: "10s"},
	}
}

// callProbe spawns the child via the real spawnServer, calls probe, and
// returns the child's reported env and argv.
func callProbe(t *testing.T, cfg mcpconfig.Settings) (env, args []string, ups *upstreamSet) {
	t.Helper()
	ups = newUpstreamSet(cfg, spawnServer)
	up, err := ups.ensure("child")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	result, rpcErr, err := up.call("probe", []byte(`{}`), cfg.Limits.CallTimeoutOrDefault())
	if err != nil || rpcErr != nil {
		t.Fatalf("probe call: %v / %+v", err, rpcErr)
	}
	var body struct {
		Env  []string `json:"env"`
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	return body.Env, body.Args, ups
}

func TestSpawnEnvAllowlist(t *testing.T) {
	t.Setenv("PORTITOR_MCP_TEST_UPSTREAM", "1")
	t.Setenv("FAKE_SECRET", "s3cret-value")
	t.Setenv("UNRELATED_VAR", "must-not-leak")
	cfg := probeSettings(t, nil, []string{"PORTITOR_MCP_TEST_UPSTREAM", "FAKE_SECRET"})
	env, _, ups := callProbe(t, cfg)
	defer ups.closeAll()
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "FAKE_SECRET=s3cret-value") {
		t.Fatal("configured env name not injected")
	}
	if strings.Contains(joined, "UNRELATED_VAR") {
		t.Fatal("unconfigured env var leaked into the child")
	}
	if !strings.Contains(joined, "PATH=") {
		t.Fatal("PATH must be part of the minimal environment")
	}
}

func TestSpawnArgvVerbatimNoShell(t *testing.T) {
	t.Setenv("PORTITOR_MCP_TEST_UPSTREAM", "1")
	meta := `argv-$(pwned); & | "quoted"`
	cfg := probeSettings(t, []string{meta}, []string{"PORTITOR_MCP_TEST_UPSTREAM"})
	_, args, ups := callProbe(t, cfg)
	defer ups.closeAll()
	if len(args) != 2 || args[1] != meta {
		t.Fatalf("child argv = %q — shell metacharacters must pass verbatim", args)
	}
}

func TestSpawnRefusesUnsetEnv(t *testing.T) {
	cfg := probeSettings(t, nil, []string{"PORTITOR_MCP_DEFINITELY_UNSET_VAR"})
	if _, _, err := spawnServer(cfg.Servers["child"]); err == nil ||
		!strings.Contains(err.Error(), "not set") {
		t.Fatalf("unset configured env must refuse the spawn, got %v", err)
	}
}

func TestReapWellBehavedChild(t *testing.T) {
	t.Setenv("PORTITOR_MCP_TEST_UPSTREAM", "1")
	cfg := probeSettings(t, nil, []string{"PORTITOR_MCP_TEST_UPSTREAM"})
	_, _, ups := callProbe(t, cfg)
	start := time.Now()
	ups.closeAll() // returns only after the child is reaped
	if d := time.Since(start); d > termGrace {
		t.Fatalf("well-behaved child took %s to reap (should exit on EOF/TERM before the grace)", d)
	}
}

func TestReapTermIgnoringChildIsKilled(t *testing.T) {
	t.Setenv("PORTITOR_MCP_TEST_UPSTREAM", "1")
	t.Setenv("IGNORE_TERM", "1")
	cfg := probeSettings(t, nil, []string{"PORTITOR_MCP_TEST_UPSTREAM", "IGNORE_TERM"})
	_, _, ups := callProbe(t, cfg)
	start := time.Now()
	ups.closeAll()
	d := time.Since(start)
	if d < termGrace {
		t.Fatalf("TERM-ignoring child reaped in %s — the grace period was not honored", d)
	}
	if d > termGrace+5*time.Second {
		t.Fatalf("TERM-ignoring child took %s — KILL after the grace did not work", d)
	}
}
