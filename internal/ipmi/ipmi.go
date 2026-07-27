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
func (c *Client) SOLDeactivate() []string {
	return c.argv("sol", "deactivate")
}
