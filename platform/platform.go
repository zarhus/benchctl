// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

// Package platform defines the bench-control interface that benchctl commands
// call, plus a registry of platform drivers. A new platform is added as one
// file that implements Platform and registers a Spec in its init().
package platform

import (
	"errors"
	"fmt"

	"github.com/zarhus/benchctl/exec"
)

// ErrNotImplemented is returned by a driver for an operation its platform does
// not support, such as a BMC flash on a platform that has no separate BMC.
// Commands render it distinctly from a genuine failure.
var ErrNotImplemented = errors.New("not implemented")

// Power is a requested host power state. A driver maps it onto whatever its
// platform calls the corresponding hardware state.
type Power int

const (
	PowerOff Power = iota
	PowerOn
)

func (power Power) String() string {
	if power == PowerOn {
		return "on"
	}
	return "off"
}

// PowerStatus is a host's reported power state: the generic on/off reading plus
// the platform's own state string, which is empty when the platform has no
// finer-grained name for it.
type PowerStatus struct {
	Power  Power
	Detail string
}

func (status PowerStatus) String() string {
	if status.Detail == "" {
		return status.Power.String()
	}
	return fmt.Sprintf("%s (%s)", status.Power, status.Detail)
}

// FlashTarget selects which flash chip a flash operation addresses.
type FlashTarget int

const (
	FlashHost FlashTarget = iota
	FlashBMC
)

func (target FlashTarget) String() string {
	switch target {
	case FlashHost:
		return "host"
	case FlashBMC:
		return "bmc"
	default:
		return "unknown"
	}
}

// Status is the state of a flash/update operation on a bench.
type Status struct {
	State string
	ID    string
}

// Platform is one bench's control surface. Drivers implement it over whatever
// control mechanism the platform provides.
type Platform interface {
	PowerState() (PowerStatus, error)
	SetPower(Power) error                          // polls until the requested state is reached
	PowerReset() error                             // power-reset the host, polls until it is back on
	HardReset() error                              // force a host reset through the platform's low-level path
	ACPowerState() (PowerStatus, error)            // whether mains/AC is applied, where the platform has AC control
	SetACPower(Power) error                        // switch the mains/AC feed and confirm the new state
	ACPowerCycle() error                           // AC power-cycle: mains off, brief wait, mains on
	Console() error                                // attach to the host serial console, detach on exit
	FlashProbe(t FlashTarget) error                // detect and report the flash chip
	FlashRead(t FlashTarget, outPath string) error // read the flash into outPath
	FlashWrite(t FlashTarget, fw string, force bool) error
	FlashStatus(t FlashTarget) (Status, error)
	FlashAbort(t FlashTarget) error
}

// Config carries resolved, driver-agnostic options into a driver constructor.
// A driver uses only the fields that apply to it.
type Config struct {
	Board     string // selected board; drivers that support several boards use it
	TasmotaIP string // override for the AC-control Tasmota plug's address; empty uses the board default
}

// Spec describes a platform driver: its name, default SSH credentials, and a
// constructor that binds it to a Runner and its config.
type Spec struct {
	Name            string
	DefaultUser     string
	DefaultPassword string
	New             func(exec.Runner, Config) Platform
}
