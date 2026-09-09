// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package iris

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeServer is a minimal stand-in for a real Iris server: enough of the
// IrisRpc/1.0 handshake and IrisJson framing to exercise Client against,
// plus a handler for scripting how it answers each call by method name.
// It intentionally does not implement anything Client does not itself use
// (event notifications, IrisU64Json, calling back into the client).
type fakeServer struct {
	t       *testing.T
	ln      net.Listener
	handler func(method string, params json.RawMessage) (result any, errCode int, errMsg string)
}

func newFakeServer(t *testing.T, handler func(method string, params json.RawMessage) (any, int, string)) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, ln: ln, handler: handler}
	go s.serveOne()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

// errNoResponse is a sentinel a handler returns as errMsg to mean "never
// send a response to this call at all" - see serveOne.
const errNoResponse = "\x00 test sentinel: send no response at all"

func (s *fakeServer) serveOne() {
	conn, err := s.ln.Accept()
	if err != nil {
		return // listener closed by test cleanup
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	// Consume the CONNECT handshake request up to the blank line.
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if _, err := conn.Write([]byte("IrisRpc/1.0 200 OK\r\nFormat: IrisJson\r\n\r\n")); err != nil {
		return
	}

	for {
		prefix, err := r.ReadString(':')
		if err != nil || prefix != "IrisJson:" {
			return
		}
		lenField, err := r.ReadString(':')
		if err != nil {
			return
		}
		n, err := strconv.Atoi(strings.TrimSuffix(lenField, ":"))
		if err != nil {
			return
		}
		data := make([]byte, n)
		if _, err := readFull(r, data); err != nil {
			return
		}
		if _, err := r.ReadByte(); err != nil { // trailing '\n'
			return
		}

		var req rpcRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return
		}
		result, errCode, errMsg := s.handler(req.Method, mustMarshal(req.Params))
		if errMsg == errNoResponse {
			// Simulates the real, empirically-observed model behavior
			// callTolerant exists to handle: the request is processed
			// (the handler already ran, and may have recorded the call),
			// but no acknowledgment is ever sent for it - keep serving
			// later requests on this same connection regardless.
			continue
		}
		resp := map[string]any{"id": req.ID, "jsonrpc": "2.0"}
		if errMsg != "" {
			resp["error"] = map[string]any{"code": errCode, "message": errMsg}
		} else {
			resp["result"] = result
		}
		respData, err := json.Marshal(resp)
		if err != nil {
			return
		}
		msg := fmt.Sprintf("IrisJson:%d:%s\n", len(respData), respData)
		if _, err := conn.Write([]byte(msg)); err != nil {
			return
		}
	}
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func TestDialHandshakeAndRegister(t *testing.T) {
	var gotMethods []string
	srv := newFakeServer(t, func(method string, params json.RawMessage) (any, int, string) {
		gotMethods = append(gotMethods, method)
		if method == "instanceRegistry_registerInstance" {
			return map[string]any{"instId": 42, "instName": "benchctl-1"}, 0, ""
		}
		return nil, 1, "unexpected call"
	})

	c, err := Dial(srv.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if len(gotMethods) != 1 || gotMethods[0] != "instanceRegistry_registerInstance" {
		t.Errorf("server saw calls %v, want exactly [instanceRegistry_registerInstance]", gotMethods)
	}
}

func TestReset(t *testing.T) {
	var gotParams json.RawMessage
	srv := newFakeServer(t, func(method string, params json.RawMessage) (any, int, string) {
		if method == "instanceRegistry_registerInstance" {
			return map[string]any{"instId": 1}, 0, ""
		}
		if method != "simulation_reset" {
			return nil, 1, "unexpected method " + method
		}
		gotParams = params
		return nil, 0, ""
	})

	c, err := Dial(srv.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Reset(1, false); err != nil {
		t.Fatal(err)
	}
	want := `{"allowPartialReset":false,"instId":1}`
	if string(gotParams) != want {
		t.Errorf("simulation_reset params = %s, want %s", gotParams, want)
	}
}

// TestRunToleratesNoAcknowledgment covers the real bug this exists to fix:
// a model that never sends a response to simulationTime_run at all (it was
// confirmed to still actually start running regardless) must not surface a
// timeout waiting for one as an error, and must not wait anywhere near the
// connection's own (deliberately much longer, for other purposes) timeout
// to give up on it.
func TestRunToleratesNoAcknowledgment(t *testing.T) {
	srv := newFakeServer(t, func(method string, params json.RawMessage) (any, int, string) {
		if method == "instanceRegistry_registerInstance" {
			return map[string]any{"instId": 1}, 0, ""
		}
		if method != "simulationTime_run" {
			return nil, 1, "unexpected method " + method
		}
		return nil, 0, errNoResponse
	})

	c, err := Dial(srv.addr(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	start := time.Now()
	if err := c.Run(1); err != nil {
		t.Fatalf("Run with no acknowledgment: want nil (tolerated), got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run with no acknowledgment took %s, want it bounded by ackTimeout (%s), not the connection's own 10s timeout", elapsed, ackTimeout)
	}
}

// TestResetAndRunStillSurfaceAGenuineError covers that callTolerant only
// changes how a *timeout* is treated - an actual error response, arriving
// within ackTimeout, must still surface normally.
func TestResetAndRunStillSurfaceAGenuineError(t *testing.T) {
	srv := newFakeServer(t, func(method string, params json.RawMessage) (any, int, string) {
		if method == "instanceRegistry_registerInstance" {
			return map[string]any{"instId": 1}, 0, ""
		}
		return nil, 999, "deliberately rejected"
	})

	c, err := Dial(srv.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = c.Reset(1, true)
	if err == nil || !strings.Contains(err.Error(), "deliberately rejected") {
		t.Errorf("Reset error = %v, want it to surface the server's rejection", err)
	}
}

func TestIsRunning(t *testing.T) {
	srv := newFakeServer(t, func(method string, params json.RawMessage) (any, int, string) {
		switch method {
		case "instanceRegistry_registerInstance":
			return map[string]any{"instId": 1}, 0, ""
		case "simulationTime_get":
			return map[string]any{"running": true}, 0, ""
		default:
			return nil, 1, "unexpected method " + method
		}
	})

	c, err := Dial(srv.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	running, err := c.IsRunning(1)
	if err != nil {
		t.Fatal(err)
	}
	if !running {
		t.Error("IsRunning = false, want true")
	}
}

func TestCallSurfacesServerError(t *testing.T) {
	srv := newFakeServer(t, func(method string, params json.RawMessage) (any, int, string) {
		if method == "instanceRegistry_registerInstance" {
			return map[string]any{"instId": 1}, 0, ""
		}
		return nil, 1234, "target is busy"
	})

	c, err := Dial(srv.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = c.Run(1)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "target is busy") || !strings.Contains(err.Error(), "1234") {
		t.Errorf("Run error = %q, want it to mention the server's code and message", err)
	}
}

func TestDialFailsWhenFormatUnsupported(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil || line == "\r\n" || line == "\n" {
				break
			}
		}
		conn.Write([]byte("IrisRpc/1.0 200 OK\r\nFormat: IrisU64Json\r\n\r\n"))
	}()

	if _, err := Dial(ln.Addr().String(), time.Second); err == nil {
		t.Fatal("want an error when the server does not offer IrisJson")
	}
}

func TestDialTimesOutOnUnresponsivePeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(2 * time.Second) // never answers within the client's timeout
	}()

	start := time.Now()
	_, err = Dial(ln.Addr().String(), 100*time.Millisecond)
	if err == nil {
		t.Fatal("want an error from an unresponsive peer")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Dial took %s, want it bounded by the timeout", elapsed)
	}
}
