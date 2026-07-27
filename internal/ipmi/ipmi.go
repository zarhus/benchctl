// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

// Package ipmi reaches a BMC with ipmitool over the IPMI LAN interface. Its one
// use is the host console on the BMC's serial-over-LAN payload, needed on a
// bench whose motherboard serial port carries BMC output rather than host
// output.
//
// Commands go out through an exec.Runner rather than a local ipmitool, so they
// originate on the bench host whether benchctl runs there or drives it over SSH,
// the same way the flash driver runs flashrom and AC control runs curl.
package ipmi

import (
	"fmt"

	"github.com/zarhus/benchctl/exec"
)

// Client talks to one BMC.
type Client struct {
	runner   exec.Runner
	ip       string
	user     string
	password string
}

// New returns a client that reaches the BMC at ip through runner.
func New(runner exec.Runner, ip, user, password string) *Client {
	return &Client{runner: runner, ip: ip, user: user, password: password}
}

// argv prefixes an ipmitool subcommand with the connection arguments. lanplus is
// IPMI 2.0 RMCP+, which OpenBMC requires.
func (c *Client) argv(sub ...string) []string {
	argv := []string{"ipmitool", "-I", "lanplus", "-H", c.ip, "-U", c.user, "-P", c.password}
	return append(argv, sub...)
}

// SOLActivate attaches the terminal to the host console over the BMC's SOL
// payload and returns when the session ends. The session ends on "~." typed at
// the start of a line, so it runs through RunInteractiveNoEscape to keep an ssh
// hop from taking that sequence for itself.
func (c *Client) SOLActivate() error {
	if err := c.runner.RunInteractiveNoEscape(c.argv("sol", "activate")...); err != nil {
		return fmt.Errorf("ipmi %s: sol activate: %w", c.ip, err)
	}
	return nil
}

// SOLDeactivate closes the BMC's SOL payload, so that a following SOLActivate
// gets it. It reports nothing: with no payload open ipmitool exits non-zero,
// which is the ordinary state before a fresh attach, and a deactivate that fails
// for any other reason shows up as the activate refusing to run.
func (c *Client) SOLDeactivate() {
	_, _ = c.runner.Run(c.argv("sol", "deactivate")...)
}
