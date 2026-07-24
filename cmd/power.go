// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/zarhus/benchctl/platform"
)

func powerCmd() *cobra.Command {
	power := &cobra.Command{
		Use:   "power",
		Short: "Host power control",
	}

	on := &cobra.Command{
		Use:   "on",
		Short: "Power on and wait until the host is on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error { return p.SetPower(platform.PowerOn) })
		},
	}

	off := &cobra.Command{
		Use:   "off",
		Short: "Power off and wait until the host is off",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error { return p.SetPower(platform.PowerOff) })
		},
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Print the current power state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error {
				status, err := p.PowerState()
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), status)
				return nil
			})
		},
	}

	var hard bool
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Power-cycle the host off and back on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error {
				if hard {
					return p.HardReset()
				}
				return p.PowerReset()
			})
		},
	}
	reset.Flags().BoolVar(&hard, "hard", false, "hard-reset through the platform's low-level path; use after an in-OS reboot leaves the host wedged")

	power.AddCommand(on, off, status, reset, acCmd())
	return power
}

// acCmd controls the host's mains/AC feed through a Tasmota plug, where the
// platform has one. It is a hard AC source that complements the soft power
// control, not a replacement: mains on boots the host only when its firmware is
// set to power on after AC loss, and AC state does not report whether the host
// actually booted.
func acCmd() *cobra.Command {
	ac := &cobra.Command{
		Use:   "ac",
		Short: "Switch the host's mains/AC feed (Tasmota plug)",
	}

	on := &cobra.Command{
		Use:   "on",
		Short: "Apply mains/AC power",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error { return p.SetACPower(platform.PowerOn) })
		},
	}

	off := &cobra.Command{
		Use:   "off",
		Short: "Remove mains/AC power",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error { return p.SetACPower(platform.PowerOff) })
		},
	}

	cycle := &cobra.Command{
		Use:   "cycle",
		Short: "AC power-cycle: mains off, brief wait, mains on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error { return p.ACPowerCycle() })
		},
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Print whether mains/AC is applied",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error {
				status, err := p.ACPowerState()
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), status)
				return nil
			})
		},
	}

	// Register --tasmota-ip as a local flag on ac and on each leaf, rather than a
	// persistent flag on ac. A persistent flag would show under "Global Flags" in
	// the leaf help, the heading cobra reserves for flags inherited from a parent,
	// whereas a local flag shows under "Flags". Listing it on ac too keeps it in
	// the `power ac -h` help, and cobra's interspersed parsing accepts it before
	// or after the subcommand.
	for _, c := range []*cobra.Command{ac, on, off, cycle, status} {
		c.Flags().StringVar(&flagTasmotaIP, "tasmota-ip", "", "Tasmota plug address (or BENCHCTL_TASMOTA_IP); overrides the board default")
	}

	ac.AddCommand(on, off, cycle, status)
	return ac
}
