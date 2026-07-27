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
	"errors"
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

// timeouts bound one request to ten seconds. A plug on the local network answers
// in well under that, so waiting longer only delays the report that it is not
// there: connect-timeout covers an address where nothing completes the handshake,
// which the kernel alone retries for minutes, and max-time covers a plug that
// accepts the connection and then never replies.
var timeouts = []string{"--connect-timeout", "5", "--max-time", "10"}

// command runs one /cm request and parses the {"POWER":"ON|OFF"} reply. It
// always sends a Referer matching the device: recent Tasmota firmware rejects
// the /cm API with an empty reply when the request carries no Referer
// (SetOption128, the secure default), which curl reports as a non-zero exit.
func (c *Client) command(cmnd string) (bool, error) {
	referer := fmt.Sprintf("http://%s/", c.ip)
	url := fmt.Sprintf("http://%s/cm?cmnd=%s", c.ip, cmnd)
	argv := append([]string{"curl", "-fsS", "-e", referer}, timeouts...)
	out, err := c.runner.Run(append(argv, url)...)
	if err != nil {
		return false, c.requestError(err)
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

// curlReasons are the curl exit codes worth naming: the three that mean the
// request never reached a plug, and the shell's code for a missing program.
// Anything else keeps curl's own message, which is specific enough.
var curlReasons = map[int]string{
	6:   "the address did not resolve",
	7:   "nothing answered on port 80",
	28:  "the request timed out",
	127: "curl is not installed on the bench",
}

// requestError explains a failed request. An unreachable plug is a bench-setup
// problem, not a curl problem, so it reads as one instead of as a shell error
// with a URL in it.
func (c *Client) requestError(err error) error {
	// A bench that ssh could not reach already names itself, and curl never ran
	// there, so adding the plug's address to that would point at the wrong host.
	var unreachable *exec.UnreachableError
	if errors.As(err, &unreachable) {
		return err
	}
	code, ran := exec.ExitCode(err)
	reason, named := curlReasons[code]
	if !ran || !named {
		return fmt.Errorf("tasmota %s: %w", c.ip, err)
	}
	if code == 127 {
		return fmt.Errorf("cannot reach the Tasmota plug at %s: %s", c.ip, reason)
	}
	return fmt.Errorf("cannot reach the Tasmota plug at %s: %s. Check that the plug is powered and that this is its address (--tasmota-ip or BENCHCTL_TASMOTA_IP)", c.ip, reason)
}
