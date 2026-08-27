// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

// Package ipmi builds ipmitool commands for a BMC on the IPMI LAN interface.
// Its one use is the host console on the BMC's serial-over-LAN payload, needed
// on a bench whose motherboard serial port carries BMC output rather than host
// output.
//
// The commands are argv for an exec.Runner rather than a local ipmitool, so the
// caller runs them on the bench host whether benchctl runs there or drives it
// over SSH, the same way the flash driver runs flashrom and AC control runs
// curl.
package ipmi

import (
	"fmt"
	"strings"
)

// Client builds commands for one BMC.
type Client struct {
	ip       string
	user     string
	password string
}

// New returns a client for the BMC at ip.
func New(ip, user, password string) *Client {
	return &Client{ip: ip, user: user, password: password}
}

// argv prefixes an ipmitool subcommand with the connection arguments. lanplus is
// IPMI 2.0 RMCP+, which OpenBMC requires.
func (c *Client) argv(sub ...string) []string {
	argv := []string{"ipmitool", "-I", "lanplus", "-H", c.ip, "-U", c.user, "-P", c.password}
	return append(argv, sub...)
}

// SOLActivate attaches the terminal to the host console over the BMC's SOL
// payload and runs until the session ends. The session ends on "~." typed at the
// start of a line, so the caller must keep an ssh hop from taking that sequence
// for itself.
func (c *Client) SOLActivate() []string {
	return c.argv("sol", "activate")
}

// SOLDeactivate closes the BMC's SOL payload, so that a following SOLActivate
// gets it. With no payload open it exits non-zero, which is the ordinary state
// before a fresh attach, so the caller ignores its result.
//
// It retries less than ipmitool's default, which takes about twenty seconds to
// give up on a BMC that never answers, long enough to look like a hang. The
// retransmission interval is left at the lanplus default of one second. A BMC
// that does answer replies in well under a second, so fewer retries cost nothing
// when the BMC is there.
func (c *Client) SOLDeactivate() []string {
	return c.argv("-R", "2", "sol", "deactivate")
}

// noSession is what ipmitool reports when it cannot open an RMCP+ session with
// the BMC, whatever the cause: no route to the address, a BMC that does not
// answer, credentials it refuses, or IPMI-over-LAN switched off.
const noSession = "Unable to establish IPMI v2 / RMCP+ session"

// Unreachable turns a failed ipmitool command into a report that the BMC did not
// answer, and returns nil for a command that reached the BMC and failed there.
// The caller passes the command's output and error, because ipmitool writes this
// to standard error and the runner folds that into the error.
func (c *Client) Unreachable(output string, err error) error {
	if err == nil {
		return nil
	}
	if !strings.Contains(output, noSession) && !strings.Contains(err.Error(), noSession) {
		return nil
	}
	return fmt.Errorf("cannot reach BMC %s as %s: no IPMI v2 / RMCP+ session (check the address and credentials, and that IPMI-over-LAN is enabled)", c.ip, c.user)
}
