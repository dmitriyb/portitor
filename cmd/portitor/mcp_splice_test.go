package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/portitor/internal/mcpwire"
)

// TestSpliceMCPRefusesCleanly: the git-side binary tolerates a missing
// mediator — an unset or undialable target is a one-line refusal, exit 1,
// nothing on stdout.
func TestSpliceMCPRefusesCleanly(t *testing.T) {
	fp := "SHA256:" + strings.Repeat("a", 43)
	cases := []struct {
		name, target, want string
	}{
		{"unset target", "", "not configured"},
		{"malformed target", "definitely-not-a-target", "mcp target"},
		{"undialable target", "unix:/nonexistent/mediator.sock", "unavailable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			rc := spliceMCPTo(fp, c.target, strings.NewReader(""), &out, &errw)
			if rc != 1 {
				t.Fatalf("rc = %d, want 1", rc)
			}
			if !strings.Contains(errw.String(), c.want) {
				t.Fatalf("stderr %q does not mention %q", errw.String(), c.want)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused splice must write nothing to stdout, got %q", out.String())
			}
		})
	}
}

// TestSpliceMCPCarriesHeaderThenBytes: the first line the mediator side reads
// is the strict fingerprint header; everything after passes through verbatim
// both ways, and the mediator's EOF ends the splice with exit 0.
func TestSpliceMCPCarriesHeaderThenBytes(t *testing.T) {
	fp := "SHA256:" + strings.Repeat("b", 43)
	sock := filepath.Join(t.TempDir(), "m.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type mediatorSaw struct {
		header mcpwire.Header
		body   string
	}
	sawCh := make(chan mediatorSaw, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(conn)
		frame, err := mcpwire.ReadFrame(br, mcpwire.MaxHeaderBytes)
		if err != nil {
			return
		}
		h, err := mcpwire.ParseHeader(frame)
		if err != nil {
			return
		}
		// Echo one reply line, then read the client body until its half-close.
		if _, err := conn.Write([]byte("reply-from-mediator\n")); err != nil {
			return
		}
		var body bytes.Buffer
		line, err := br.ReadString('\n')
		for err == nil {
			body.WriteString(line)
			line, err = br.ReadString('\n')
		}
		body.WriteString(line)
		sawCh <- mediatorSaw{header: h, body: body.String()}
	}()

	in := strings.NewReader("hello-from-agent\n")
	var out, errw bytes.Buffer
	rc := spliceMCPTo(fp, "unix:"+sock, in, &out, &errw)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, errw.String())
	}
	if out.String() != "reply-from-mediator\n" {
		t.Fatalf("stdout = %q", out.String())
	}
	select {
	case saw := <-sawCh:
		if saw.header.Fingerprint != fp {
			t.Fatalf("mediator saw fingerprint %q, want %q", saw.header.Fingerprint, fp)
		}
		if saw.body != "hello-from-agent\n" {
			t.Fatalf("mediator saw body %q", saw.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mediator side never completed")
	}
}

// TestSpliceHeaderIsValidStrictJSON pins the header shape the dispatcher
// emits against the mediator's strict parser (the cross-binary contract).
func TestSpliceHeaderShapeContract(t *testing.T) {
	fp := "SHA256:" + strings.Repeat("c", 43)
	raw, err := json.Marshal(mcpwire.Header{Fingerprint: fp})
	if err != nil {
		t.Fatal(err)
	}
	h, err := mcpwire.ParseHeader(raw)
	if err != nil || h.Fingerprint != fp {
		t.Fatalf("round-trip: %+v, %v", h, err)
	}
}
