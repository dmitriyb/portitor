package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/dmitriyb/portitor/internal/mcpconfig"
	"github.com/dmitriyb/portitor/internal/mcpschema"
	"github.com/dmitriyb/portitor/internal/mcpwire"
)

// termGrace is how long a session-end SIGTERM waits before SIGKILL.
const termGrace = 2 * time.Second

// spawnFunc produces the transport to one upstream server plus its terminator.
// Production is spawnServer (a stdio child); tests inject in-process pipes.
type spawnFunc func(srv mcpconfig.Server) (io.ReadWriteCloser, func(), error)

// upstreamSet is one session's upstreams: spawned on first use, latched on
// failure (no respawn within a session — restart semantics are the next
// session's), reaped at session end.
type upstreamSet struct {
	cfg   mcpconfig.Settings
	spawn spawnFunc
	ups   map[string]*upstream
}

func newUpstreamSet(cfg mcpconfig.Settings, spawn spawnFunc) *upstreamSet {
	return &upstreamSet{cfg: cfg, spawn: spawn, ups: map[string]*upstream{}}
}

// ensure returns the session's upstream for serverName, spawning and
// handshaking it on first use. A spawn or handshake failure is latched: every
// later call to this server's tools fails with the same operational error.
func (u *upstreamSet) ensure(serverName string) (*upstream, error) {
	if up, ok := u.ups[serverName]; ok {
		if up.failed != nil {
			return nil, up.failed
		}
		return up, nil
	}
	up := &upstream{name: serverName, maxMsg: u.cfg.Limits.UpstreamMessageBytesOrDefault()}
	u.ups[serverName] = up
	rwc, kill, err := u.spawn(u.cfg.Servers[serverName])
	if err != nil {
		up.failed = fmt.Errorf("spawn: %w", err)
		return nil, up.failed
	}
	up.rwc, up.kill = rwc, kill
	up.frames = make(chan frameOrErr, 1)
	up.done = make(chan struct{})
	go up.readFrames()
	if err := up.handshake(u.cfg.Limits.HandshakeTimeoutOrDefault()); err != nil {
		up.failed = fmt.Errorf("handshake: %w", err)
		up.terminate()
		return nil, up.failed
	}
	up.pinTools(u.cfg, serverName)
	return up, nil
}

// closeAll reaps every upstream the session spawned.
func (u *upstreamSet) closeAll() {
	for _, up := range u.ups {
		up.terminate()
	}
}

// frameOrErr is one upstream frame (or the reader's terminal error).
type frameOrErr struct {
	frame []byte
	err   error
}

// upstream is one running (or failed) upstream server within a session.
type upstream struct {
	name   string
	rwc    io.ReadWriteCloser
	kill   func()
	maxMsg int
	frames chan frameOrErr
	// done, closed by terminate, releases the reader goroutine if nothing is
	// consuming frames anymore (no leak past session end).
	done   chan struct{}
	nextID int64
	// adv is the upstream's one-time tools/list advertisement (name →
	// inputSchema), captured at handshake.
	adv map[string]json.RawMessage
	// toolRefusal maps a configured tool name to its pinning refusal reason
	// — sticky for the session (spec/mcp/arch_mcp.md).
	toolRefusal map[string]string
	// failed latches the first operational failure.
	failed error
	term   sync.Once
}

// terminate ends the upstream (idempotent): close the transport (EOF for a
// well-behaved child) and run the terminator (TERM → grace → KILL → reap for
// a real process).
func (up *upstream) terminate() {
	up.term.Do(func() {
		if up.done != nil {
			close(up.done)
		}
		if up.rwc != nil {
			up.rwc.Close()
		}
		if up.kill != nil {
			up.kill()
		}
	})
}

// readFrames pumps upstream frames into the channel until a terminal error;
// the channel carries the error last, and awaitResponse latches it.
func (up *upstream) readFrames() {
	br := bufio.NewReader(up.rwc)
	for {
		frame, err := mcpwire.ReadFrame(br, up.maxMsg)
		fe := frameOrErr{frame: frame, err: err}
		select {
		case up.frames <- fe:
		case <-up.done:
			return
		}
		if err != nil {
			return
		}
	}
}

// send writes one message to the upstream.
func (up *upstream) send(m mcpwire.Message) error {
	if err := mcpwire.WriteFrame(up.rwc, m); err != nil {
		return fmt.Errorf("write to upstream: %w", err)
	}
	return nil
}

// request sends a request with a fresh mediator-assigned id and returns the id.
func (up *upstream) request(method string, params any) (json.RawMessage, error) {
	up.nextID++
	id := json.RawMessage(strconv.FormatInt(up.nextID, 10))
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}
	return id, up.send(mcpwire.Message{JSONRPC: "2.0", ID: id, Method: method, Params: raw})
}

// awaitResponse waits for the response matching id, containing every
// server-initiated flow on the way: upstream notifications are dropped,
// upstream requests are answered -32601 and never surfaced to the caller
// (sampling, elicitation, list_changed — none are forwarded). A deadline or
// reader error is an operational failure, latched — the upstream is not
// trusted to frame after a timeout abandons a partial exchange.
func (up *upstream) awaitResponse(id json.RawMessage, deadline time.Duration) (mcpwire.Message, error) {
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case fe := <-up.frames:
			if fe.err != nil {
				if errors.Is(fe.err, io.EOF) {
					return mcpwire.Message{}, errors.New("upstream exited")
				}
				return mcpwire.Message{}, fe.err
			}
			msg, err := mcpwire.Parse(fe.frame)
			if err != nil {
				continue // an unparseable upstream frame is dropped; the deadline bounds us
			}
			switch mcpwire.RouteOf(msg) {
			case mcpwire.RouteResponse:
				if string(msg.ID) == string(id) {
					return msg, nil
				}
				// A response to an abandoned (timed-out) exchange: dropped.
			case mcpwire.RouteUnknownNotification, mcpwire.RouteInitialized:
				// Server-initiated notifications are dropped, list_changed included.
			default:
				// A server-initiated request (sampling, elicitation, or any
				// mediated-subset method name — an upstream is a server, none
				// of them are valid from it): refused, never forwarded.
				if msg.HasID() {
					_ = up.send(mcpwire.NewError(msg.ID, mcpwire.CodeMethodNotFound, "server-initiated requests are not mediated"))
				}
			}
		case <-timer.C:
			return mcpwire.Message{}, fmt.Errorf("no response within %s", deadline)
		}
	}
}

// handshake runs the MCP client side against a fresh upstream: initialize →
// notifications/initialized → one pinning tools/list, all bounded by the
// handshake timeout.
func (up *upstream) handshake(timeout time.Duration) error {
	id, err := up.request("initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "portitor-mcp", "version": version},
	})
	if err != nil {
		return err
	}
	resp, err := up.awaitResponse(id, timeout)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize refused: %s", resp.Error.Message)
	}
	if err := up.send(mcpwire.Message{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		return err
	}
	id, err = up.request("tools/list", map[string]any{})
	if err != nil {
		return err
	}
	resp, err = up.awaitResponse(id, timeout)
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("tools/list refused: %s", resp.Error.Message)
	}
	var result struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("tools/list result: %w", err)
	}
	up.adv = make(map[string]json.RawMessage, len(result.Tools))
	for _, t := range result.Tools {
		up.adv[t.Name] = t.InputSchema
	}
	return nil
}

// pinTools computes the sticky pinning verdict for every configured tool of
// this server against the captured advertisement: absent upstream_name, or a
// drifted schema, refuses the tool for the whole session — a visible refusal
// fixed in config, not a silent hole.
func (up *upstream) pinTools(cfg mcpconfig.Settings, serverName string) {
	up.toolRefusal = map[string]string{}
	for name, tool := range cfg.Tools {
		if tool.Server != serverName {
			continue
		}
		advSchema, ok := up.adv[tool.UpstreamName]
		if !ok {
			up.toolRefusal[name] = fmt.Sprintf("upstream %q does not advertise %q", serverName, tool.UpstreamName)
			continue
		}
		if err := mcpschema.CheckAdvertised(tool.Spec(), advSchema); err != nil {
			up.toolRefusal[name] = err.Error()
		}
	}
}

// call forwards one canonical tools/call and returns the upstream's result or
// its relayable error. An operational failure latches the upstream failed.
func (up *upstream) call(upstreamName string, canonicalArgs []byte, timeout time.Duration) (json.RawMessage, *mcpwire.RPCError, error) {
	if up.failed != nil {
		return nil, nil, up.failed
	}
	id, err := up.request("tools/call", struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: upstreamName, Arguments: canonicalArgs})
	if err != nil {
		up.failed = err
		return nil, nil, err
	}
	resp, err := up.awaitResponse(id, timeout)
	if err != nil {
		up.failed = err
		return nil, nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error, nil
	}
	return resp.Result, nil, nil
}

// spawnServer is the production spawnFunc: start the operator-configured argv
// (no shell, ever) with a minimal environment — PATH and HOME plus exactly
// the configured env names, valued from the mediator's own environment. The
// terminator closes stdin (EOF), SIGTERMs, waits the grace, SIGKILLs, and
// always reaps.
func spawnServer(srv mcpconfig.Server) (io.ReadWriteCloser, func(), error) {
	if len(srv.Command) == 0 {
		return nil, nil, errors.New("empty command")
	}
	cmd := exec.Command(srv.Command[0], srv.Command[1:]...)
	var env []string
	for _, name := range []string{"PATH", "HOME"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for _, name := range srv.Env {
		v, ok := os.LookupEnv(name)
		if !ok {
			// serve refuses to boot on this; re-checked here fail-closed in
			// case the environment changed under a long-running mediator.
			return nil, nil, fmt.Errorf("env %q is not set in the mediator's environment", name)
		}
		env = append(env, name+"="+v)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr // upstream diagnostics surface in the mediator's log
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start %q: %w", srv.Command[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	kill := func() {
		stdin.Close()
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(termGrace):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	return &procPipe{out: stdout, in: stdin}, kill, nil
}

// procPipe is the stdio transport to a child process.
type procPipe struct {
	out io.ReadCloser
	in  io.WriteCloser
}

func (p *procPipe) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *procPipe) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *procPipe) Close() error {
	// Closing stdin is the EOF a well-behaved child exits on; stdout is
	// closed by the process exit (Wait).
	return p.in.Close()
}
