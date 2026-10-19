// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	relay.quietReadyAfter = time.Hour
	relay.writeTimeout = 50 * time.Millisecond
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

// serveForReady serves a relay on console with the given ready times and returns
// its log and a function that ends it.
func serveForReady(t *testing.T, consoles chan *fakeConsole, readyAfter, quietReadyAfter time.Duration) (*syncWriter, func()) {
	t.Helper()
	log := &syncWriter{}
	relay := newConsoleRelay("test console", func() (consoleProcess, error) { return <-consoles, nil }, log)
	relay.readyAfter = readyAfter
	relay.quietReadyAfter = quietReadyAfter
	relay.backoffMin = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.serve(ctx, "127.0.0.1:0") }()
	return log, func() {
		cancel()
		<-done
	}
}

func TestRelayPrintsReadyOnceTheConsoleHolds(t *testing.T) {
	consoles := make(chan *fakeConsole, 1)
	console := newFakeConsole()
	consoles <- console
	log, stop := serveForReady(t, consoles, time.Millisecond, time.Hour)
	go func() { _, _ = console.console.Write([]byte("login: ")) }()
	waitFor(t, func() bool { return strings.Contains(log.String(), ConsoleReady) }, ConsoleReady)
	stop()
	if n := strings.Count(log.String(), ConsoleReady); n != 1 {
		t.Errorf("log = %q, want %s once", log, ConsoleReady)
	}
}

func TestRelayPrintsReadyOnAQuietConsoleThatHolds(t *testing.T) {
	consoles := make(chan *fakeConsole, 1)
	consoles <- newFakeConsole()
	log, stop := serveForReady(t, consoles, time.Hour, time.Millisecond)
	defer stop()
	waitFor(t, func() bool { return strings.Contains(log.String(), ConsoleReady) }, ConsoleReady)
}

func TestRelayIsNotReadyOnASlowFailedAttach(t *testing.T) {
	consoles := make(chan *fakeConsole, 1)
	console := newFakeConsole()
	consoles <- console
	log, stop := serveForReady(t, consoles, 50*time.Millisecond, time.Hour)
	defer stop()
	// The attach is silent while it connects, then fails.
	time.Sleep(100 * time.Millisecond)
	_, _ = console.console.Write([]byte("ssh: connect to host 192.0.2.1 port 2200: Connection timed out\r\n"))
	console.Stop()
	waitFor(t, func() bool { return strings.Contains(log.String(), "ended") }, "the attach to end")
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(log.String(), ConsoleReady) {
		t.Errorf("log = %q, want no %s from an attach that failed", log, ConsoleReady)
	}
}

func TestRelayReleasesWhenCancelledBetweenAttaches(t *testing.T) {
	consoles := make(chan *fakeConsole, 1)
	console := newFakeConsole()
	consoles <- console
	log := &syncWriter{}
	relay := newConsoleRelay("test console", func() (consoleProcess, error) { return <-consoles, nil }, log)
	var releases atomic.Int32
	relay.release = func() error {
		releases.Add(1)
		return nil
	}
	relay.backoffMin = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.serve(ctx, "127.0.0.1:0") }()
	waitFor(t, func() bool { return strings.Contains(log.String(), "Attaching") }, "the attach")
	console.Stop()
	waitFor(t, func() bool { return strings.Contains(log.String(), "reattaching") }, "the backoff")
	cancel()
	<-done
	// One release before the attach, one on the way out.
	if n := releases.Load(); n != 2 {
		t.Errorf("releases = %d, want 2", n)
	}
}

func TestRelaySkipsTheAttachWhenCancelledDuringTheRelease(t *testing.T) {
	log := &syncWriter{}
	var starts atomic.Int32
	relay := newConsoleRelay("test console", func() (consoleProcess, error) {
		starts.Add(1)
		return newFakeConsole(), nil
	}, log)
	ctx, cancel := context.WithCancel(context.Background())
	relay.release = func() error {
		cancel()
		return nil
	}
	if err := relay.serve(ctx, "127.0.0.1:0"); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if n := starts.Load(); n != 0 {
		t.Errorf("attaches = %d, want 0", n)
	}
	if strings.Contains(log.String(), "Attaching") {
		t.Errorf("log names an attach after the cancel:\n%s", log.String())
	}
}

func TestRelayServesPastAClientThatStopsReading(t *testing.T) {
	consoles := make(chan *fakeConsole, 1)
	console := newFakeConsole()
	consoles <- console
	addr, log := startRelay(t, consoles)
	stuck := dial(t, addr)
	defer stuck.Close()
	waitFor(t, func() bool { return strings.Contains(log.String(), "connected") }, "the client")
	// More output than the socket buffers hold, which the client never reads.
	go func() {
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for {
			if _, err := console.console.Write(chunk); err != nil {
				return
			}
		}
	}()
	waitFor(t, func() bool { return strings.Contains(log.String(), "not reading") }, "the stalled client")
	next := dial(t, addr)
	waitFor(t, func() bool { return strings.Contains(log.String(), "replaces") }, "the next client")
	if got := readN(t, next, 1); string(got) != "x" {
		t.Errorf("next client read %q, want console output", got)
	}
}

func TestRelayDropsOutputAtOnceWhileTheClientIsStalled(t *testing.T) {
	consoles := make(chan *fakeConsole, 1)
	console := newFakeConsole()
	consoles <- console
	addr, log := startRelay(t, consoles)
	stuck := dial(t, addr)
	defer stuck.Close()
	waitFor(t, func() bool { return strings.Contains(log.String(), "connected") }, "the client")
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	go func() {
		for !strings.Contains(log.String(), "not reading") {
			if _, err := console.console.Write(chunk); err != nil {
				return
			}
		}
	}()
	waitFor(t, func() bool { return strings.Contains(log.String(), "not reading") }, "the stalled client")
	// 1 MiB is 256 reads of 4 KiB. A write that waited writeTimeout on each
	// read would take about 13 seconds.
	started := time.Now()
	for range 16 {
		if _, err := console.console.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("1 MiB of console output took %s with a stalled client, want it dropped at once", took)
	}
}

// cutConn is a client whose writes each take at most n bytes and then time out.
type cutConn struct {
	net.Conn
	n   int
	got []byte
}

func (c *cutConn) Write(p []byte) (int, error) {
	n := min(c.n, len(p))
	c.got = append(c.got, p[:n]...)
	if n < len(p) {
		return n, os.ErrDeadlineExceeded
	}
	return n, nil
}

func (c *cutConn) SetWriteDeadline(time.Time) error { return nil }
func (c *cutConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }

func TestRelayCompletesAnEscapedIACCutByTheDeadline(t *testing.T) {
	relay := newConsoleRelay("test console", nil, io.Discard)
	client := &cutConn{n: 2}
	relay.client = client
	// The cut falls between the two bytes of the escaped 0xFF, and the rest of
	// the first write is dropped.
	relay.toClient(escapeIAC([]byte("a\xffbc")))
	client.n = 100
	relay.toClient(escapeIAC([]byte("d")))
	if want := "a\xff\xffd"; string(client.got) != want {
		t.Errorf("client got %q, want %q", client.got, want)
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
	go func() { done <- driver.ConsoleServe(ctx, "127.0.0.1:0", ConsoleSource{Name: "com1"}) }()
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
	go func() { done <- driver.ConsoleServe(ctx, "127.0.0.1:0", ConsoleSource{Name: "com1"}) }()
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

func TestBMCConsoleServeSkipsTheAttachWhenTheBMCDoesNotAnswer(t *testing.T) {
	runner := &fakeRunner{handler: func(int, []string) (string, error) {
		return "", errors.New("exit status 1: Error: Unable to establish IPMI v2 / RMCP+ session")
	}}
	driver, progress := newTestBMC(t, bmcwebSystem("On"), BMCAccess{Console: "sol", Jump: "jumphost"}, runner)
	var started atomic.Int32
	driver.startPiped = func([]string) (consoleProcess, error) {
		started.Add(1)
		return newFakeConsole(), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- driver.ConsoleServe(ctx, "127.0.0.1:0", ConsoleSource{Name: "com1"}) }()
	waitFor(t, func() bool { return strings.Contains(progress.String(), "cannot reach BMC") }, "the failed release")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := started.Load(); n != 0 {
		t.Errorf("attaches = %d, want none while the BMC does not answer", n)
	}
}

func TestBMCConsoleServeSOLSourceTakesTheBMCPassed(t *testing.T) {
	runner := &fakeRunner{}
	driver, progress := newTestBMC(t, bmcwebSystem("On"), BMCAccess{}, runner)
	var started [][]string
	driver.startPiped = func(argv []string) (consoleProcess, error) {
		started = append(started, argv)
		return newFakeConsole(), nil
	}
	source := ConsoleSource{Name: "sol", BMC: BMC{IP: "192.0.2.9", User: "operator", Password: "secret"}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- driver.ConsoleServe(ctx, "127.0.0.1:0", source) }()
	waitFor(t, func() bool { return strings.Contains(progress.String(), "Attaching") }, "the attach")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := strings.Join(started[0], " ")
	if !strings.Contains(got, "sol activate") || !strings.Contains(got, "192.0.2.9") || !strings.Contains(got, "operator") {
		t.Errorf("started %q, want sol activate against the BMC passed", got)
	}
	if len(runner.calls) != 2 {
		t.Errorf("calls = %q, want sol deactivate before the attach and on exit", runner.calls)
	}
}

func TestBMCConsoleServeHasNoUART1(t *testing.T) {
	driver, _ := newTestBMC(t, bmcwebSystem("On"), BMCAccess{}, &fakeRunner{})
	if err := driver.ConsoleServe(context.Background(), "127.0.0.1:0", ConsoleSource{Name: "uart1"}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("ConsoleServe(uart1) = %v, want ErrNotImplemented", err)
	}
}
