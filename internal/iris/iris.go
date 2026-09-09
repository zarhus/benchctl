// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

// Package iris is a minimal client for Arm's Iris debug/control protocol -
// the JSON-RPC-over-TCP interface an FVP model exposes when launched with
// --iris-server. It exists so the fvp platform driver can reset, pause,
// resume, and gracefully shut down a running model directly from Go,
// instead of shelling out to Arm's own Python bindings
// (Iris/Python/iris/iris.py in the FVP package, and its iris.debug.Model
// wrapper) the way a person debugging a model by hand would.
//
// It implements just enough of the protocol - the IrisRpc/1.0 handshake,
// client registration, and synchronous one-shot JSON-RPC calls - for a
// short-lived CLI process that makes one call and disconnects. It does not
// implement the reference client's bidirectional machinery for the server
// calling back into the client (event/breakpoint notifications, published
// functions): benchctl never publishes any functions of its own, so it
// never needs to receive a request, only send one and read the matching
// response.
//
// Requests are always sent as plain IrisJson text, never the binary
// IrisU64Json format - but the handshake still claims support for both
// (see handshake's own comment on why), and a response can come back in
// either; see u64json.go for the binary decoder that handles the latter.
//
// The wire protocol and RPC method names here (simulation_reset,
// simulationTime_run/stop/get, simulation_requestShutdown,
// instanceRegistry_registerInstance) were read directly out of the
// reference Python client shipped with the FVP package, not guessed or
// taken from documentation - see FVP_RD_V3/Iris/Python/iris/iris.py and
// FVP_RD_V3/Iris/Python/iris/debug/Model.py in the loopctl repository.
package iris

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Client is one connection to an Iris server.
type Client struct {
	conn    net.Conn
	r       *bufio.Reader
	nextID  int64
	instID  int64         // this connection's own registered instance id - see generateID
	timeout time.Duration // re-applied as a fresh deadline before every call
}

// generateID builds a request id the way Arm's own reference client does
// (iris.py's generateRequestId: (self.instId << 32) | self.nextRequestId) -
// not simply a unique sequential number, though it looks that way at the
// JSON-RPC level. Confirmed empirically (2026-09-16, against a real
// FVP_RD_V3 11.29.35 model, by replaying the reference client's own exact
// wire bytes and bisecting them field by field) that the server uses the
// id's own upper 32 bits to route a response back to the calling client
// instance for any call targeting an instance other than the implicit
// global one (instId 0) - simulationTime_get(instId=1), the one
// IsRunning/waitForIrisRunState depends on throughout, included. A plain
// incrementing counter has zero upper bits, which is why
// instanceRegistry_registerInstance (targeting the global instance, id 0)
// always worked fine with the old scheme while every instId-1 call never
// got a response at all, indistinguishable from the server hanging: the
// server was very likely answering, just never back to this connection.
// instID is 0 (matching the reference client's own IrisInstance() default)
// until Dial's own registerInstance call learns the real one from the
// server's response, so that very first call - the only one still made
// before this connection's own instId is known - keeps working the same
// way it always has.
func (c *Client) generateID() int64 {
	c.nextID++
	return (c.instID << 32) | c.nextID
}

// Dial connects to an Iris server at addr (host:port), performs the
// IrisRpc/1.0 format-negotiation handshake, and registers this connection
// as a named client instance - the same steps the reference Python client
// takes before the server will accept any other call. The whole exchange,
// not just the initial TCP connect, is bounded by timeout: a model whose
// Iris server accepted the connection but never finishes the handshake
// (for instance because --iris-server was not actually passed) would
// otherwise hang a caller indefinitely.
func Dial(addr string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return nil, err
	}
	c := &Client{conn: conn, r: bufio.NewReader(conn), timeout: timeout}
	if err := c.handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	result, err := c.call("instanceRegistry_registerInstance", map[string]any{
		"instName": "benchctl",
		"uniquify": true,
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("register with iris server: %w", err)
	}
	// Learn this connection's own instance id, needed from here on to
	// address every subsequent request correctly - see generateID.
	var registered struct {
		InstID int64 `json:"instId"`
	}
	if err := json.Unmarshal(result, &registered); err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse register response: %w", err)
	}
	c.instID = registered.InstID
	return c, nil
}

// handshake sends the IrisRpc/1.0 CONNECT request and reads response
// headers up to the blank line that terminates them, the same way an HTTP
// client would - simplest correct way to consume exactly the handshake
// bytes and leave any following framed message untouched in c.r for the
// next read.
//
// It claims support for IrisU64Json as well as the plain IrisJson format
// benchctl actually speaks, even though it never sends the former: this
// model's Iris server (confirmed empirically, 2026-09-16, against a real
// FVP_RD_V3 11.29.35 model) never answers anything - not even the
// handshake's own required first call, instanceRegistry_registerInstance -
// when a client's Supported-Formats claims IrisJson alone, exactly what
// this handshake used to send. Listing IrisU64Json too (as Arm's own
// reference client always does) is what makes the server respond at all;
// it then answers every call in IrisU64Json regardless of the order
// listed here or of which format the request itself was framed in - see
// u64json.go's decoder, which callTolerant/call read a response through
// for exactly that reason.
func (c *Client) handshake() error {
	const req = "CONNECT / IrisRpc/1.0\r\nSupported-Formats: IrisU64Json, IrisJson\r\n\r\n"
	if _, err := c.conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("iris handshake: %w", err)
	}
	var header strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("iris handshake: %w", err)
		}
		header.WriteString(line)
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if !strings.Contains(header.String(), "IrisJson") {
		return fmt.Errorf("iris server does not support the IrisJson format: %s", strings.TrimSpace(header.String()))
	}
	return nil
}

// writeMessage frames payload as "IrisJson:<byte length>:<payload>\n", the
// format iris.py's IrisTcpClient.sendMessage uses.
func (c *Client) writeMessage(payload []byte) error {
	buf := make([]byte, 0, len(payload)+24)
	buf = append(buf, []byte(fmt.Sprintf("IrisJson:%d:", len(payload)))...)
	buf = append(buf, payload...)
	buf = append(buf, '\n')
	_, err := c.conn.Write(buf)
	return err
}

// readMessage reads one framed message - either "IrisJson:<decimal
// len>:<payload>\n" (the format benchctl itself always sends) or
// "IrisU64Json:<4-byte little-endian len><payload>\n" (see handshake's own
// comment on why a response can come back in this format even though
// benchctl never requests it) - and returns which format it was plus its
// payload.
func (c *Client) readMessage() (wireFormat, []byte, error) {
	prefix, err := c.r.ReadString(':')
	if err != nil {
		return 0, nil, err
	}
	switch prefix {
	case "IrisJson:":
		lenField, err := c.r.ReadString(':')
		if err != nil {
			return 0, nil, err
		}
		n, err := strconv.Atoi(strings.TrimSuffix(lenField, ":"))
		if err != nil {
			return 0, nil, fmt.Errorf("bad message length %q: %w", lenField, err)
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(c.r, data); err != nil {
			return 0, nil, err
		}
		if _, err := c.r.ReadByte(); err != nil { // the trailing '\n'
			return 0, nil, err
		}
		return formatIrisJSON, data, nil
	case "IrisU64Json:":
		var lenBytes [4]byte
		if _, err := io.ReadFull(c.r, lenBytes[:]); err != nil {
			return 0, nil, err
		}
		n := binary.LittleEndian.Uint32(lenBytes[:])
		data := make([]byte, n)
		if _, err := io.ReadFull(c.r, data); err != nil {
			return 0, nil, err
		}
		if _, err := c.r.ReadByte(); err != nil { // the trailing '\n'
			return 0, nil, err
		}
		return formatIrisU64JSON, data, nil
	default:
		return 0, nil, fmt.Errorf("unexpected message prefix %q", prefix)
	}
}

// wireFormat is which framing a message arrived in - see readMessage.
type wireFormat int

const (
	formatIrisJSON wireFormat = iota
	formatIrisU64JSON
)

// decodeResponse turns a message read by readMessage into an rpcResponse,
// decoding IrisU64Json's binary shape (via u64json.go) into the same
// dynamic result shape json.Unmarshal would have produced from IrisJson, so
// every caller (call, callTolerant) handles both formats identically from
// here on.
func decodeResponse(format wireFormat, data []byte) (rpcResponse, error) {
	if format == formatIrisJSON {
		var resp rpcResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return rpcResponse{}, err
		}
		return resp, nil
	}
	id, result, isError, errCode, errMsg, err := decodeU64JSONResponse(data)
	if err != nil {
		return rpcResponse{}, err
	}
	if isError {
		return rpcResponse{ID: id, Error: &rpcError{Code: int(errCode), Message: errMsg}}, nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("re-encode u64json result: %w", err)
	}
	return rpcResponse{ID: id, Result: raw}, nil
}

type rpcRequest struct {
	ID      int64  `json:"id"`
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("iris error %d: %s", e.Code, e.Message)
}

// call makes one JSON-RPC 2.0 request and returns its result. Iris servers
// answer in order on a connection that never publishes functions of its
// own (true here: benchctl only ever calls out, never registers anything
// the server could call back into) - responses to any other id are not
// expected, so seeing one means something is wrong with call
// correlation rather than an ordinary race to filter past.
func (c *Client) call(method string, params any) (json.RawMessage, error) {
	if err := c.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, err
	}
	id := c.generateID()
	payload, err := json.Marshal(rpcRequest{ID: id, JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if err := c.writeMessage(payload); err != nil {
		return nil, fmt.Errorf("iris: send %s: %w", method, err)
	}
	format, data, err := c.readMessage()
	if err != nil {
		return nil, fmt.Errorf("iris: read response to %s: %w", method, err)
	}
	resp, err := decodeResponse(format, data)
	if err != nil {
		return nil, fmt.Errorf("iris: parse response to %s: %w", method, err)
	}
	if resp.ID != id {
		return nil, fmt.Errorf("iris: response id %d does not match request id %d for %s", resp.ID, id, method)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("iris: %s: %w", method, resp.Error)
	}
	return resp.Result, nil
}

// ackTimeout bounds how long callTolerant waits for a response before
// giving up on it specifically (see there for why that is not a failure).
// Deliberately short: unlike a real error, there is nothing to gain by
// waiting longer for an acknowledgment that - per the observed behavior
// callTolerant exists to handle - may not be coming at all.
const ackTimeout = 2 * time.Second

// callTolerant is call, except a timeout waiting for the response is not
// treated as failure: send the request and make a best-effort, short
// attempt to read its response, but if that specifically times out,
// return success rather than an error.
//
// This exists because simulationTime_run/_stop and simulation_reset were
// observed (2026-09-15, against a real model) to sometimes never receive a
// JSON-RPC response at all while - or because - the operation they
// triggered is genuinely happening: the model visibly keeps running
// (console output resumes) or resets (EDK2 setup menu no longer reachable)
// even though the client never received an acknowledgment for the call
// that caused it. At the time this looked like it might be the server not
// flushing a pending response until it next yielded from driving the
// simulation forward; it later turned out (2026-09-16) that the real,
// full explanation was generateID's id-routing bug - every call in that
// session targeted instId 1, so every single one of them was silently
// undeliverable the whole time, not just occasionally slow to flush. That
// bug is fixed now, so a call through here should normally get a genuine,
// prompt response like any other - callTolerant's tolerance stays anyway,
// on general principle for a busy or momentarily unresponsive model rather
// than because it is still the only thing standing between these calls
// and a permanent hang.
func (c *Client) callTolerant(method string, params any) error {
	id := c.generateID()
	payload, err := json.Marshal(rpcRequest{ID: id, JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	if err := c.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return err
	}
	if err := c.writeMessage(payload); err != nil {
		return fmt.Errorf("iris: send %s: %w", method, err)
	}
	if err := c.conn.SetDeadline(time.Now().Add(ackTimeout)); err != nil {
		return err
	}
	format, data, err := c.readMessage()
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil
		}
		return fmt.Errorf("iris: read response to %s: %w", method, err)
	}
	resp, err := decodeResponse(format, data)
	if err != nil {
		return fmt.Errorf("iris: parse response to %s: %w", method, err)
	}
	if resp.ID != id {
		// Not ours - possible if a previous call's callTolerant gave up on
		// a response that then arrived late, now sitting ahead of this
		// one's in the stream. Nothing productive to do with it as a
		// one-shot client; ignore rather than treat as this call's error.
		return nil
	}
	if resp.Error != nil {
		return fmt.Errorf("iris: %s: %w", method, resp.Error)
	}
	return nil
}

// Reset resets instID's simulation state back to what it was right after
// instantiation - Arm's own iris.debug.Model.reset(), without the Python
// bindings. allowPartialReset accepts a reset that cannot reach every
// component (simulation_reset's own escape hatch for platforms Iris does
// not fully control) - RD-V3 needs this true to reset anything at all, it
// is not a pure Fast-Models-only "ISIM" platform (confirmed empirically:
// false silently reset nothing). See callTolerant for why a timeout here
// is not treated as an error.
func (c *Client) Reset(instID int, allowPartialReset bool) error {
	return c.callTolerant("simulation_reset", map[string]any{
		"instId":            instID,
		"allowPartialReset": allowPartialReset,
	})
}

// Run resumes instID's simulation time - the counterpart to a model
// launched without -R/--run, which stays paused at reset until told to
// go. See callTolerant for why a timeout waiting for its acknowledgment is
// not treated as an error.
func (c *Client) Run(instID int) error {
	return c.callTolerant("simulationTime_run", map[string]any{"instId": instID})
}

// Stop pauses instID's simulation time. See callTolerant for why a timeout
// waiting for its acknowledgment is not treated as an error.
func (c *Client) Stop(instID int) error {
	return c.callTolerant("simulationTime_stop", map[string]any{"instId": instID})
}

// IsRunning reports whether instID's simulation time is currently running.
// A plain query, not a state change - unlike Run/Stop/Reset, expected to
// ack promptly, so this uses the strict call rather than callTolerant.
func (c *Client) IsRunning(instID int) (bool, error) {
	result, err := c.call("simulationTime_get", map[string]any{"instId": instID})
	if err != nil {
		return false, err
	}
	var v struct {
		Running bool `json:"running"`
	}
	if err := json.Unmarshal(result, &v); err != nil {
		return false, fmt.Errorf("iris: parse simulationTime_get result: %w", err)
	}
	return v.Running, nil
}

// RequestShutdown asks the model to shut itself down gracefully over this
// same connection - a real alternative to SIGKILL, available whenever the
// model was launched with an Iris server. Tolerant of a timeout for the
// same reason as Run/Stop/Reset (see callTolerant): the model exiting
// closes this very connection, which is at least as likely to be observed
// as a read timeout as a clean acknowledgment.
func (c *Client) RequestShutdown(instID int) error {
	return c.callTolerant("simulation_requestShutdown", map[string]any{"instId": instID})
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
