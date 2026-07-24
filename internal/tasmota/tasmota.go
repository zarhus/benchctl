// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

// Package tasmota is a client for a Tasmota smart plug's HTTP command API. The
// plug switches a DUT's mains/AC feed, giving a hard AC kill and cold boot that
// the front-panel power button cannot.
//
// Requests go out through curl on an exec.Runner rather than net/http. The plug
// lives on the RTE's isolated wifi AP subnet, which only the RTE can reach, so
// a request must originate on the RTE whether benchctl runs there or drives it
// over SSH from a PC. Running curl through the Runner satisfies both cases with
// one code path, the same way the flash driver runs flashrom.
package tasmota

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zarhus/benchctl/exec"
)

// Client controls one Tasmota plug at a fixed address.
type Client struct {
	runner exec.Runner
	ip     string
}

// New returns a client that reaches the plug at ip through runner.
func New(runner exec.Runner, ip string) *Client {
	return &Client{runner: runner, ip: ip}
}

// Power reports whether mains is currently applied. It changes nothing.
func (c *Client) Power() (bool, error) {
	return c.command("Power")
}

// SetPower switches mains on or off and returns the resulting state.
func (c *Client) SetPower(on bool) (bool, error) {
	cmnd := "Power%20OFF"
	if on {
		cmnd = "Power%20ON"
	}
	return c.command(cmnd)
}

// command runs one /cm request and parses the {"POWER":"ON|OFF"} reply. It
// always sends a Referer matching the device: recent Tasmota firmware rejects
// the /cm API with an empty reply when the request carries no Referer
// (SetOption128, the secure default), which curl reports as a non-zero exit.
func (c *Client) command(cmnd string) (bool, error) {
	referer := fmt.Sprintf("http://%s/", c.ip)
	url := fmt.Sprintf("http://%s/cm?cmnd=%s", c.ip, cmnd)
	out, err := c.runner.Run("curl", "-fsS", "-e", referer, url)
	if err != nil {
		return false, fmt.Errorf("tasmota %s: %w", c.ip, err)
	}
	var reply struct {
		Power string `json:"POWER"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return false, fmt.Errorf("tasmota %s: parse reply %q: %w", c.ip, strings.TrimSpace(out), err)
	}
	switch strings.ToUpper(reply.Power) {
	case "ON":
		return true, nil
	case "OFF":
		return false, nil
	default:
		return false, fmt.Errorf("tasmota %s: unexpected power state %q", c.ip, reply.Power)
	}
}
