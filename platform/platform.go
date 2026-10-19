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

// DiskMedium selects how a mounted disk image is presented to the host -
// meaningful mainly to a platform (like fvp) that can model more than one
// attachment point; a platform with a single fixed one is free to ignore it.
type DiskMedium int

const (
	DiskAHCI   DiskMedium = iota // SATA/AHCI, e.g. an installer ISO or disk image
	DiskVirtio                   // virtio-blk
)

func (medium DiskMedium) String() string {
	switch medium {
	case DiskAHCI:
		return "ahci"
	case DiskVirtio:
		return "virtio"
	default:
		return "unknown"
	}
}

// ParseDiskMedium maps a CLI-facing name to a DiskMedium.
func ParseDiskMedium(name string) (DiskMedium, error) {
	switch name {
	case "ahci":
		return DiskAHCI, nil
	case "virtio":
		return DiskVirtio, nil
	default:
		return 0, fmt.Errorf("unknown disk medium %q (want ahci or virtio)", name)
	}
}

// BMC addresses a baseboard management controller over IPMI. The caller supplies
// the address, so drivers hold no default for it.
type BMC struct {
	IP       string
	User     string
	Password string
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
	ConsoleSOL(BMC) error                          // attach to the host console over the BMC's IPMI serial-over-LAN payload
	ConsoleUART1() error                           // read-only attach to the host firmware's UART1 debug output
	FlashProbe(t FlashTarget) error                // detect and report the flash chip
	FlashRead(t FlashTarget, outPath string) error // read the flash into outPath
	FlashWrite(t FlashTarget, fw string, force bool) error
	FlashStatus(t FlashTarget) (Status, error)
	FlashAbort(t FlashTarget) error
	DiskMount(image string, medium DiskMedium) error // record a disk image to attach on the next power on
	DiskEject() error                                // forget the recorded disk image
	DiskStatus() error                               // report the recorded disk image, if any
}

// Config carries resolved, driver-agnostic options into a driver constructor.
// A driver uses only the fields that apply to it.
type Config struct {
	Board     string    // selected board; drivers that support several boards use it
	TasmotaIP string    // override for the AC-control Tasmota plug's address; empty uses the board default
	BMC       BMCAccess // how the bmc driver reaches its BMC
}

// BMCAccess is how the bmc driver reaches a BMC from the workstation: Redfish
// for power, and the BMC's SSH console or IPMI SOL for the host console. Empty
// fields take the value the BMC reports over Redfish, or the driver's default.
type BMCAccess struct {
	IP           string // BMC address
	Jump         string // ssh destination the BMC is reached through; empty connects directly
	User         string // BMC account for Redfish, the SSH console and SOL
	Password     string // password for User
	System       string // Redfish ComputerSystem ID; empty uses the only one
	Console      string // "ssh" or "sol"; empty picks from SerialConsole
	ConsolePort  int    // SSH console port; zero takes it from SerialConsole
	ConsoleEntry string // command that switches an SSH login to the host console
	CAFile       string // PEM bundle that verifies the BMC's certificate
	Insecure     bool   // skip verifying the BMC's certificate
}

// Spec describes a platform driver: its name, default SSH credentials, and a
// constructor that binds it to a Runner and its config.
type Spec struct {
	Name            string
	DefaultUser     string
	DefaultPassword string
	New             func(exec.Runner, Config) Platform
}
