// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

// Package redfish is a minimal Redfish client for one ComputerSystem: its power
// state, its reset action and the serial console it advertises. It speaks to the
// BMC with HTTP basic authentication, which OpenBMC's bmcweb and vendor BMCs
// accept alongside sessions.
package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"time"
)

// Client reaches one BMC's Redfish service.
type Client struct {
	// BaseURL is the service root's scheme and authority, as in
	// "https://10.0.0.5".
	BaseURL  string
	User     string
	Password string
	HTTP     *http.Client
}

// Transport describes how to reach the BMC's HTTPS port.
type Transport struct {
	// Jump, when set, is an ssh destination that forwards each connection with
	// "ssh -W". The user's OpenSSH configuration supplies its user, keys and any
	// further hops.
	Jump string
	// CAFile, when set, is a PEM bundle that verifies the BMC's certificate in
	// place of the system roots.
	CAFile string
	// Insecure skips certificate verification.
	Insecure bool
}

// requestTimeout bounds each Redfish request, connection set-up through the jump
// host included.
const requestTimeout = 30 * time.Second

// NewHTTPClient returns an HTTP client that reaches the BMC as transport says.
func NewHTTPClient(transport Transport) (*http.Client, error) {
	// Insecure is an explicit opt-out for a BMC with a self-signed certificate
	// and no CA bundle to hand.
	tlsConfig := &tls.Config{InsecureSkipVerify: transport.Insecure}
	if transport.CAFile != "" {
		pem, err := os.ReadFile(transport.CAFile)
		if err != nil {
			return nil, fmt.Errorf("BMC CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("BMC CA bundle %s holds no PEM certificates", transport.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	httpTransport := &http.Transport{
		TLSClientConfig:   tlsConfig,
		DisableKeepAlives: true,
	}
	if transport.Jump != "" {
		jump := transport.Jump
		httpTransport.DialContext = func(_ context.Context, _, addr string) (net.Conn, error) {
			return dialThrough(jump, addr)
		}
	}
	return &http.Client{Transport: httpTransport, Timeout: requestTimeout}, nil
}

// SystemsPath is the Redfish collection of ComputerSystems.
const SystemsPath = "/redfish/v1/Systems"

// collection is a Redfish resource collection.
type collection struct {
	Members []struct {
		ID string `json:"@odata.id"`
	} `json:"Members"`
}

// System is the part of a ComputerSystem the client reads.
type System struct {
	ID            string        `json:"Id"`
	PowerState    string        `json:"PowerState"`
	SerialConsole SerialConsole `json:"SerialConsole"`
	Actions       struct {
		Reset ResetAction `json:"#ComputerSystem.Reset"`
	} `json:"Actions"`
}

// ResetAction is ComputerSystem.Reset as the system advertises it.
type ResetAction struct {
	Target  string   `json:"target"`
	Allowed []string `json:"ResetType@Redfish.AllowableValues"`
}

// SerialConsole lists the protocols that carry the system's serial console
// (DMTF ComputerSystem, release 2020.3 and later).
type SerialConsole struct {
	SSH    *ConsoleProtocol `json:"SSH"`
	IPMI   *ConsoleProtocol `json:"IPMI"`
	Telnet *ConsoleProtocol `json:"Telnet"`
}

// ConsoleProtocol is one SerialConsole entry.
type ConsoleProtocol struct {
	ServiceEnabled        bool   `json:"ServiceEnabled"`
	Port                  int    `json:"Port"`
	SharedWithManagerCLI  bool   `json:"SharedWithManagerCLI"`
	ConsoleEntryCommand   string `json:"ConsoleEntryCommand"`
	HotKeySequenceDisplay string `json:"HotKeySequenceDisplay"`
}

// SystemPath returns the path of the system with id, or of the only system when
// id is empty. With several systems and no id it fails, naming them.
func (client *Client) SystemPath(id string) (string, error) {
	if id != "" {
		return SystemsPath + "/" + id, nil
	}
	var systems collection
	if err := client.Get(SystemsPath, &systems); err != nil {
		return "", err
	}
	switch len(systems.Members) {
	case 0:
		return "", fmt.Errorf("the BMC lists no systems under %s", SystemsPath)
	case 1:
		return systems.Members[0].ID, nil
	}
	var ids []string
	for _, member := range systems.Members {
		ids = append(ids, member.ID[strings.LastIndex(member.ID, "/")+1:])
	}
	return "", fmt.Errorf("the BMC lists several systems (%s): pick one with --bmc-system or BENCHCTL_BMC_SYSTEM", strings.Join(ids, ", "))
}

// System reads the ComputerSystem at path.
func (client *Client) System(path string) (System, error) {
	var system System
	err := client.Get(path, &system)
	return system, err
}

// Reset posts ComputerSystem.Reset with resetType to target.
func (client *Client) Reset(target, resetType string) error {
	return client.Post(target, map[string]string{"ResetType": resetType})
}

// Get reads the resource at path into out.
func (client *Client) Get(path string, out any) error {
	resp, err := client.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: decoding the response: %w", path, err)
	}
	return nil
}

// Post sends body as JSON to path and discards the response.
func (client *Client) Post(path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := client.do(http.MethodPost, path, data)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// do sends one request and fails on any status outside 2xx, carrying the
// BMC's message from the Redfish error body where it gives one.
func (client *Client) do(method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, client.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(client.User, client.Password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %s%s", method, path, resp.Status, errorMessage(resp.Body))
	}
	return resp, nil
}

// errorMessage extracts the message of a Redfish error response, prefixed for
// appending to an error, or returns empty.
func errorMessage(body io.Reader) string {
	var redfishErr struct {
		Error struct {
			Message  string `json:"message"`
			Extended []struct {
				Message string `json:"Message"`
			} `json:"@Message.ExtendedInfo"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&redfishErr); err != nil {
		return ""
	}
	if len(redfishErr.Error.Extended) > 0 && redfishErr.Error.Extended[0].Message != "" {
		return ": " + redfishErr.Error.Extended[0].Message
	}
	if redfishErr.Error.Message != "" {
		return ": " + redfishErr.Error.Message
	}
	return ""
}

// Dial connects to addr on the BMC's network the way Redfish requests do:
// through the ssh destination jump when it is set, directly otherwise.
func Dial(jump, addr string) (net.Conn, error) {
	if jump == "" {
		return net.DialTimeout("tcp", addr, 10*time.Second)
	}
	return dialThrough(jump, addr)
}

// dialThrough opens a connection to addr through the ssh destination jump, with
// "ssh -W" carrying the bytes on its standard input and output. The connection
// lasts as long as the ssh process, which Close ends.
func dialThrough(jump, addr string) (net.Conn, error) {
	cmd := osexec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-W", addr, jump)
	return startConn(cmd, jump, addr)
}

// startConn starts cmd and returns a connection over its standard input and
// output.
func startConn(cmd *osexec.Cmd, jump, addr string) (net.Conn, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	conn := &cmdConn{cmd: cmd, stdin: stdin, stdout: stdout, addr: addr, jump: jump}
	cmd.Stderr = &conn.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh -W %s %s: %w", addr, jump, err)
	}
	return conn, nil
}

// cmdConn is a net.Conn over a child process's standard input and output.
// Deadlines are accepted and ignored. The HTTP client's own timeout bounds each
// request.
type cmdConn struct {
	cmd      *osexec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   bytes.Buffer // written by os/exec until wait returns
	addr     string
	jump     string
	waitOnce sync.Once
}

// wait collects the ssh process once, after which stderr is complete.
func (conn *cmdConn) wait() {
	conn.waitOnce.Do(func() { _ = conn.cmd.Wait() })
}

func (conn *cmdConn) Read(p []byte) (int, error) {
	n, err := conn.stdout.Read(p)
	if err == io.EOF && n == 0 {
		// ssh exits when the jump host cannot reach the BMC, and its reason is
		// on standard error.
		conn.wait()
		if reason := strings.TrimSpace(conn.stderr.String()); reason != "" {
			return 0, fmt.Errorf("ssh -W %s %s: %s", conn.addr, conn.jump, reason)
		}
	}
	return n, err
}

func (conn *cmdConn) Write(p []byte) (int, error) { return conn.stdin.Write(p) }

func (conn *cmdConn) Close() error {
	_ = conn.stdin.Close()
	_ = conn.cmd.Process.Kill()
	conn.wait()
	return nil
}

func (conn *cmdConn) LocalAddr() net.Addr              { return pipeAddr("local") }
func (conn *cmdConn) RemoteAddr() net.Addr             { return pipeAddr(conn.addr) }
func (conn *cmdConn) SetDeadline(time.Time) error      { return nil }
func (conn *cmdConn) SetReadDeadline(time.Time) error  { return nil }
func (conn *cmdConn) SetWriteDeadline(time.Time) error { return nil }

// pipeAddr names one end of a cmdConn.
type pipeAddr string

func (addr pipeAddr) Network() string { return "ssh" }
func (addr pipeAddr) String() string  { return string(addr) }
