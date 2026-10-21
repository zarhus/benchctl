// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/zarhus/benchctl/exec"
)

// ConsoleServer is a driver that can relay a console on a TCP port, for a test
// framework that reads the console over telnet. Commands check for it with a
// type assertion.
type ConsoleServer interface {
	// ConsoleServe holds source attached and relays it on addr, a loopback
	// address, until ctx ends. A source the driver cannot relay is
	// ErrNotImplemented.
	ConsoleServe(ctx context.Context, addr string, source ConsoleSource) error
}

// ConsoleSource names the console to relay, as console --source does.
type ConsoleSource struct {
	Name string // "com1", "sol" or "uart1"
	BMC  BMC    // where SOL reaches, for "sol"
}

// unknownSource reports a source name no console matches.
func unknownSource(name string) error {
	return fmt.Errorf("unknown console source %q (want com1, sol, or uart1)", name)
}

// ConsoleReady is the line the relay prints once the listener is up and the
// console has held, for a launcher that waits on its output.
const ConsoleReady = "console-ready"

// DiskServer is a driver that attaches a local image by serving it while the
// command runs in the foreground. Commands check for it with a type assertion.
type DiskServer interface {
	// DiskServe serves image, attaches it as DiskMount does, prints DiskReady,
	// and serves until ctx ends. It then detaches the image and stops serving.
	DiskServe(ctx context.Context, image string, opts DiskServeOptions) error
}

// DiskServeOptions is how DiskServe attaches and serves a local image.
type DiskServeOptions struct {
	DiskOptions
	Listen   string // address the image server listens on, as ":8443"
	MediaURL string // URL the BMC fetches the image under, without the file name; empty derives it
}

// DiskReady is the line the bmc driver's DiskMount and DiskServe print once the
// image and the boot override are in place, and the host is on with PowerOn,
// for a launcher that waits on its output.
const DiskReady = "disk-ready"

// consoleProcess is a running console attach: its output, its input, and a way
// to end it.
type consoleProcess = exec.PipedProcess

// consoleRelay holds one console session at a time for its whole life and
// relays it to one TCP client at a time. A new client replaces the old one,
// since a test framework closes and reopens its connection around a power
// cycle. Console output with no client connected is dropped.
type consoleRelay struct {
	what     string
	start    func() (consoleProcess, error)
	progress io.Writer
	// release, when set, frees the console before each attach and after the
	// last one, for a console that admits one session at a time. An error from
	// it before an attach says the console cannot be reached, and the session
	// ends without attaching.
	release func() error

	// readyAfter is how long a session has to hold after its first output before
	// ConsoleReady, and quietReadyAfter how long it has to hold with no output.
	// An attach that fails prints its error and exits within readyAfter, and
	// quietReadyAfter is longer than the attach commands' ssh ConnectTimeout, so
	// an attach that is still connecting is not ready either.
	readyAfter      time.Duration
	quietReadyAfter time.Duration
	// writeTimeout is how long a write to the client may block. Output the
	// client does not take within it is dropped, as with no client connected.
	// While the client takes nothing, each write waits stalledWriteTimeout, so
	// the console output goes on at its own pace.
	writeTimeout time.Duration
	backoffMin   time.Duration // first wait before reattaching
	backoffMax   time.Duration
	stableFor    time.Duration // a session that held this long resets the backoff

	mu        sync.Mutex
	client    net.Conn
	proc      consoleProcess
	readyOnce sync.Once
	// Used by pump only: the client whose last write timed out, and the client
	// whose last write ended between the two bytes of an escaped 0xFF.
	stalled net.Conn
	openIAC net.Conn
}

func newConsoleRelay(what string, start func() (consoleProcess, error), progress io.Writer) *consoleRelay {
	return &consoleRelay{
		what:            what,
		start:           start,
		progress:        progress,
		readyAfter:      2 * time.Second,
		quietReadyAfter: 12 * time.Second,
		writeTimeout:    2 * time.Second,
		backoffMin:      time.Second,
		backoffMax:      30 * time.Second,
		stableFor:       30 * time.Second,
	}
}

// serveSession relays session on addr until ctx ends. start runs its Attach,
// and its Release, when set, runs before each attach and on the way out, so a
// console left held by a dropped session is free for the next attach.
func serveSession(ctx context.Context, addr string, session ConsoleSession, start func(argv []string) (consoleProcess, error), runner exec.Runner, progress io.Writer) error {
	relay := newConsoleRelay(session.What, func() (consoleProcess, error) { return start(session.Attach) }, progress)
	if len(session.Release) > 0 {
		relay.release = func() error {
			fmt.Fprintf(progress, "Releasing any open %s...\n", session.What)
			out, err := runner.Run(session.Release...)
			if benchUnreachable(err) {
				return err
			}
			if session.Unreachable != nil {
				return session.Unreachable(out, err)
			}
			return nil
		}
	}
	return relay.serve(ctx, addr)
}

// checkLoopback accepts only a loopback listen address, since the relay has no
// authentication of its own.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("console serve address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("console serve address %q is not a loopback address", addr)
}

// serve listens on addr and keeps the console attached until ctx ends.
func (relay *consoleRelay) serve(ctx context.Context, addr string) error {
	if err := checkLoopback(addr); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(relay.progress, "Serving %s on %s.\n", relay.what, listener.Addr())
	accepting := make(chan struct{})
	go func() {
		relay.accept(listener)
		close(accepting)
	}()
	defer func() {
		listener.Close()
		<-accepting
		relay.dropClient()
		if relay.release != nil {
			_ = relay.release()
		}
	}()

	backoff := relay.backoffMin
	for {
		started := time.Now()
		err := relay.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) >= relay.stableFor {
			backoff = relay.backoffMin
		}
		fmt.Fprintf(relay.progress, "The %s ended (%v), reattaching in %s.\n", relay.what, err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, relay.backoffMax)
	}
}

// session runs one console attach until it ends or ctx does.
func (relay *consoleRelay) session(ctx context.Context) error {
	if relay.release != nil {
		if err := relay.release(); err != nil {
			return err
		}
		// A cancel during the release ends the session before the attach.
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	proc, err := relay.start()
	if err != nil {
		return err
	}
	relay.mu.Lock()
	relay.proc = proc
	relay.mu.Unlock()
	defer func() {
		relay.mu.Lock()
		relay.proc = nil
		relay.mu.Unlock()
	}()
	fmt.Fprintf(relay.progress, "Attaching to the %s.\n", relay.what)
	readyTimer := time.AfterFunc(relay.quietReadyAfter, relay.markReady)
	defer readyTimer.Stop()

	done := make(chan error, 1)
	go func() {
		relay.pump(proc, readyTimer)
		done <- proc.Wait()
	}()
	select {
	case err := <-done:
		if err == nil {
			err = errors.New("the attach exited")
		}
		return err
	case <-ctx.Done():
		proc.Stop()
		<-done
		return nil
	}
}

// markReady prints ConsoleReady on a line of its own, once.
func (relay *consoleRelay) markReady() {
	relay.readyOnce.Do(func() { fmt.Fprintln(relay.progress, ConsoleReady) })
}

// pump copies console output to the client, until the console ends. The first
// output moves readyTimer to readyAfter.
func (relay *consoleRelay) pump(proc consoleProcess, readyTimer *time.Timer) {
	buf := make([]byte, 4096)
	seen := false
	for {
		n, err := proc.Read(buf)
		if n > 0 {
			if !seen {
				seen = true
				readyTimer.Reset(relay.readyAfter)
			}
			relay.toClient(escapeIAC(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

// stalledWriteTimeout is how long a write waits on a client whose last write
// timed out.
const stalledWriteTimeout = time.Millisecond

// toClient writes to the connected client outside the lock, so a client that
// stops reading holds up neither a new client nor the shutdown. It drops output
// the client does not take within writeTimeout, and a client that fails.
//
// A write cut off by its deadline can end between the two bytes of an escaped
// 0xFF. The next write to that client starts with the second byte, so the
// client reads a data byte, not a telnet command.
func (relay *consoleRelay) toClient(data []byte) {
	relay.mu.Lock()
	client := relay.client
	relay.mu.Unlock()
	if client == nil {
		return
	}
	open := relay.openIAC == client
	if open {
		data = append([]byte{telnetIAC}, data...)
	}
	timeout := relay.writeTimeout
	if relay.stalled == client {
		timeout = stalledWriteTimeout
	}
	_ = client.SetWriteDeadline(time.Now().Add(timeout))
	n, err := client.Write(data)
	for _, b := range data[:n] {
		if b == telnetIAC {
			open = !open
		}
	}
	relay.openIAC = nil
	if open {
		relay.openIAC = client
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		if relay.stalled != client {
			fmt.Fprintf(relay.progress, "Client %s is not reading, dropping console output.\n", client.RemoteAddr())
		}
		relay.stalled = client
		return
	}
	relay.stalled = nil
	if err != nil {
		relay.mu.Lock()
		if relay.client == client {
			relay.client = nil
		}
		relay.mu.Unlock()
		client.Close()
	}
}

// telnetCharMode tells a telnet client that the console echoes and that it
// sends no go-ahead, which puts the client in character mode: it sends each key
// as it is pressed and leaves the echo to the console.
var telnetCharMode = []byte{telnetIAC, telnetWILL, telnetOptEcho, telnetIAC, telnetWILL, telnetOptSGA}

// accept takes each new client in place of the previous one.
func (relay *consoleRelay) accept(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if _, err := conn.Write(telnetCharMode); err != nil {
			conn.Close()
			continue
		}
		relay.mu.Lock()
		if relay.client != nil {
			relay.client.Close()
			fmt.Fprintf(relay.progress, "Client %s replaces the previous one.\n", conn.RemoteAddr())
		} else {
			fmt.Fprintf(relay.progress, "Client %s connected.\n", conn.RemoteAddr())
		}
		relay.client = conn
		relay.mu.Unlock()
		go relay.fromClient(conn)
	}
}

// fromClient copies the client's keys to the console, with telnet commands
// removed, until the client goes.
func (relay *consoleRelay) fromClient(conn net.Conn) {
	var filter iacFilter
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if data := filter.strip(buf[:n]); len(data) > 0 {
			relay.mu.Lock()
			proc := relay.proc
			relay.mu.Unlock()
			if proc != nil {
				_, _ = proc.Write(data)
			}
		}
		if err != nil {
			relay.mu.Lock()
			if relay.client == conn {
				relay.client = nil
				fmt.Fprintf(relay.progress, "Client %s disconnected.\n", conn.RemoteAddr())
			}
			relay.mu.Unlock()
			conn.Close()
			return
		}
	}
}

func (relay *consoleRelay) dropClient() {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.client != nil {
		relay.client.Close()
		relay.client = nil
	}
}

// Telnet command bytes (RFC 854) and options (RFC 857, RFC 858).
const (
	telnetIAC  = 0xFF
	telnetSB   = 0xFA
	telnetSE   = 0xF0
	telnetWILL = 0xFB
	telnetDONT = 0xFE

	telnetOptEcho = 1
	telnetOptSGA  = 3
)

// escapeIAC doubles each 0xFF, which a telnet client reads as one data byte.
func escapeIAC(data []byte) []byte {
	if bytes.IndexByte(data, telnetIAC) < 0 {
		return data
	}
	out := make([]byte, 0, len(data)+8)
	for _, b := range data {
		out = append(out, b)
		if b == telnetIAC {
			out = append(out, telnetIAC)
		}
	}
	return out
}

// iacFilter removes telnet commands from a client's byte stream, across reads,
// turns a doubled 0xFF back into one data byte, and drops the NUL a telnet
// client sends after CR for Enter.
type iacFilter struct {
	state   int
	afterCR bool
}

const (
	iacData    = iota
	iacCommand // after IAC
	iacOption  // after IAC and an option verb, 251 to 254
	iacSub     // inside IAC SB ... IAC SE
	iacSubIAC  // IAC inside a subnegotiation
)

func (filter *iacFilter) strip(data []byte) []byte {
	out := data[:0:0]
	for _, b := range data {
		switch filter.state {
		case iacData:
			switch {
			case b == telnetIAC:
				filter.state = iacCommand
			case b == 0 && filter.afterCR:
			default:
				out = append(out, b)
			}
			filter.afterCR = b == '\r'
		case iacCommand:
			switch {
			case b == telnetIAC:
				out = append(out, b)
				filter.state = iacData
			case b >= telnetWILL && b <= telnetDONT:
				filter.state = iacOption
			case b == telnetSB:
				filter.state = iacSub
			default:
				filter.state = iacData
			}
		case iacOption:
			filter.state = iacData
		case iacSub:
			if b == telnetIAC {
				filter.state = iacSubIAC
			}
		case iacSubIAC:
			if b == telnetSE {
				filter.state = iacData
			} else {
				filter.state = iacSub
			}
		}
	}
	return out
}
