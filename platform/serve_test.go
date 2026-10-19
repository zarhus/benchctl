// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bytes"
	"context"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeConsole is a console attach: the test writes its output and reads what
// the relay sent it.
type fakeConsole struct {
	out      *io.PipeReader
	console  *io.PipeWriter // the test writes console output here
	mu       sync.Mutex
	in       bytes.Buffer
	done     chan struct{}
	doneOnce sync.Once
}

func newFakeConsole() *fakeConsole {
	out, console := io.Pipe()
	return &fakeConsole{out: out, console: console, done: make(chan struct{})}
}

func (c *fakeConsole) Read(p []byte) (int, error) { return c.out.Read(p) }

func (c *fakeConsole) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.in.Write(p)
}

func (c *fakeConsole) input() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.in.String()
}

func (c *fakeConsole) Wait() error {
	<-c.done
	return nil
}

// Stop ends the attach, as the program would on SIGTERM.
func (c *fakeConsole) Stop() {
	c.doneOnce.Do(func() {
		c.console.Close()
		close(c.done)
	})
}

// syncWriter is a strings.Builder safe to read while the relay writes it.
type syncWriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// startRelay serves a relay whose attaches come from consoles, one per attach,
// on a free loopback port. It returns the address and the relay's log.
func startRelay(t *testing.T, consoles chan *fakeConsole) (string, *syncWriter) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	log := &syncWriter{}
	relay := newConsoleRelay("test console", func() (consoleProcess, error) { return <-consoles, nil }, log)
	relay.readyAfter = time.Hour
	relay.backoffMin = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.serve(ctx, addr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve = %v, want nil after cancel", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("serve did not end after cancel")
		}
	})
	waitFor(t, func() bool { return strings.Contains(log.String(), "Serving") }, "the listener")
	return addr, log
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if got := readN(t, conn, len(telnetCharMode)); !bytes.Equal(got, telnetCharMode) {
		t.Fatalf("client read %q on connect, want WILL ECHO and WILL SUPPRESS-GO-AHEAD", got)
	}
	return conn
}

func readN(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("reading %d bytes: %v", n, err)
	}
	return buf
}

func TestRelayCopiesBothWays(t *testing.T) {
	consoles := make(chan *fakeConsole, 1)
	console := newFakeConsole()
	consoles <- console
	addr, log := startRelay(t, consoles)
	conn := dial(t, addr)
	waitFor(t, func() bool { return strings.Contains(log.String(), "connected") }, "the client")

	go func() { _, _ = console.console.Write([]byte("login: \xff")) }()
	if got := readN(t, conn, 9); !bytes.Equal(got, []byte("login: \xff\xff")) {
		t.Errorf("client read %q, want the output with 0xFF doubled", got)
	}
	// IAC DO ECHO, then "root" and Enter as CR NUL, then a doubled 0xFF.
	if _, err := conn.Write([]byte("\xff\xfd\x01root\r\x00\xff\xff")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return console.input() == "root\r\xff" }, "the keys to reach the console")
}

func TestRelayIsNotReadyOnAFailedAttachsOutput(t *testing.T) {
	consoles := make(chan *fakeConsole, 2)
	console := newFakeConsole()
	consoles <- console
	consoles <- newFakeConsole() // the reattach
	_, log := startRelay(t, consoles)
	_, _ = console.console.Write([]byte("ssh: connect to host 192.0.2.1 port 2200: Connection refused\r\n"))
	console.Stop()
	waitFor(t, func() bool { return strings.Contains(log.String(), "ended") }, "the attach to end")
	if strings.Contains(log.String(), ConsoleReady) {
		t.Errorf("log = %q, want no %s from an attach that only printed an error", log, ConsoleReady)
	}
}

func TestRelayReplacesTheClient(t *testing.T) {
	consoles := make(chan *fakeConsole, 1)
	console := newFakeConsole()
	consoles <- console
	addr, log := startRelay(t, consoles)
	first := dial(t, addr)
	waitFor(t, func() bool { return strings.Contains(log.String(), "connected") }, "the first client")
	second := dial(t, addr)
	waitFor(t, func() bool { return strings.Contains(log.String(), "replaces") }, "the second client")

	_ = first.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := first.Read(make([]byte, 1)); err == nil {
		t.Error("the first client stayed connected")
	}
	go func() { _, _ = console.console.Write([]byte("ok")) }()
	if got := readN(t, second, 2); string(got) != "ok" {
		t.Errorf("second client read %q, want ok", got)
	}
}

func TestRelayReattachesWhenTheConsoleEnds(t *testing.T) {
	consoles := make(chan *fakeConsole, 2)
	first, second := newFakeConsole(), newFakeConsole()
	consoles <- first
	consoles <- second
	addr, log := startRelay(t, consoles)
	conn := dial(t, addr)
	waitFor(t, func() bool { return strings.Contains(log.String(), "connected") }, "the client")

	first.Stop()
	waitFor(t, func() bool { return strings.Count(log.String(), "Attaching") == 2 }, "the reattach")
	if !strings.Contains(log.String(), "reattaching") {
		t.Errorf("log = %q, want the reattach logged", log)
	}
	go func() { _, _ = second.console.Write([]byte("again")) }()
	if got := readN(t, conn, 5); string(got) != "again" {
		t.Errorf("client read %q after the reattach, want the same connection to carry on", got)
	}
}

func TestRelayDropsOutputWithNoClient(t *testing.T) {
	consoles := make(chan *fakeConsole, 1)
	console := newFakeConsole()
	consoles <- console
	addr, log := startRelay(t, consoles)
	if _, err := console.console.Write([]byte("lost")); err != nil {
		t.Fatal(err)
	}
	conn := dial(t, addr)
	waitFor(t, func() bool { return strings.Contains(log.String(), "connected") }, "the client")
	go func() { _, _ = console.console.Write([]byte("kept")) }()
	if got := readN(t, conn, 4); string(got) != "kept" {
		t.Errorf("client read %q, want only output from after it connected", got)
	}
}

func TestRelayPrintsReadyOnceTheConsoleHolds(t *testing.T) {
	log := &syncWriter{}
	console := newFakeConsole()
	relay := newConsoleRelay("test console", func() (consoleProcess, error) { return console, nil }, log)
	relay.readyAfter = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.serve(ctx, "127.0.0.1:0") }()
	waitFor(t, func() bool { return strings.Contains(log.String(), ConsoleReady) }, ConsoleReady)
	cancel()
	<-done
	if n := strings.Count(log.String(), ConsoleReady); n != 1 {
		t.Errorf("log = %q, want %s once", log, ConsoleReady)
	}
}

func TestCheckLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:2323", "[::1]:2323", "localhost:2323"} {
		if err := checkLoopback(addr); err != nil {
			t.Errorf("checkLoopback(%q) = %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:2323", ":2323", "10.0.0.5:2323", "2323"} {
		if err := checkLoopback(addr); err == nil {
			t.Errorf("checkLoopback(%q) accepted a non-loopback address", addr)
		}
	}
}

func TestIACFilterAcrossReads(t *testing.T) {
	var filter iacFilter
	var got []byte
	// IAC SB TTYPE ... IAC SE split across reads, then a WILL split after IAC.
	for _, chunk := range []string{"a\xff", "\xfa\x18\x00xterm\xff", "\xf0b\xff", "\xfb", "\x03c"} {
		got = append(got, filter.strip([]byte(chunk))...)
	}
	if string(got) != "abc" {
		t.Errorf("strip = %q, want abc", got)
	}
}

func TestBMCConsoleServeRelaysTheSSHConsole(t *testing.T) {
	driver, progress := newTestBMC(t, bmcwebSystem("On"), BMCAccess{}, &fakeRunner{})
	var started [][]string
	console := newFakeConsole()
	driver.startPiped = func(argv []string) (consoleProcess, error) {
		started = append(started, argv)
		return console, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- driver.ConsoleServe(ctx, "127.0.0.1:0") }()
	waitFor(t, func() bool { return strings.Contains(progress.String(), "Attaching") }, "the attach")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || !slices.Contains(started[0], "none") || !slices.Contains(started[0], "2200") {
		t.Errorf("started %q, want one ssh to port 2200 with its escape off", started)
	}
}

func TestBMCConsoleServeReleasesSOLAroundEachAttach(t *testing.T) {
	runner := &fakeRunner{}
	driver, progress := newTestBMC(t, bmcwebSystem("On"), BMCAccess{Console: "sol", Jump: "jumphost"}, runner)
	var started [][]string
	driver.startPiped = func(argv []string) (consoleProcess, error) {
		started = append(started, argv)
		return newFakeConsole(), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- driver.ConsoleServe(ctx, "127.0.0.1:0") }()
	waitFor(t, func() bool { return strings.Contains(progress.String(), "Attaching") }, "the attach")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// One release before the attach, one on the way out.
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %q, want sol deactivate before the attach and on exit", runner.calls)
	}
	for _, call := range runner.calls {
		if remote := call[len(call)-1]; !strings.Contains(remote, "sol deactivate") {
			t.Errorf("call %q, want sol deactivate", call)
		}
	}
	if remote := started[0][len(started[0])-1]; !strings.Contains(remote, "sol activate") {
		t.Errorf("started %q, want sol activate", started[0])
	}
}
