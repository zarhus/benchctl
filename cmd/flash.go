// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/zarhus/benchctl/platform"
)

// flashTargetFromName maps a required host|bmc argument to a FlashTarget.
func flashTargetFromName(name string) (platform.FlashTarget, error) {
	switch name {
	case "host":
		return platform.FlashHost, nil
	case "bmc":
		return platform.FlashBMC, nil
	default:
		return 0, fmt.Errorf("unknown flash target %q (want host or bmc)", name)
	}
}

// parseFlashTarget maps an optional [host|bmc] argument to a FlashTarget,
// defaulting to host when no argument is given. Used by status and abort.
func parseFlashTarget(args []string) (platform.FlashTarget, error) {
	if len(args) == 0 {
		return platform.FlashHost, nil
	}
	return flashTargetFromName(args[0])
}

func flashCmd() *cobra.Command {
	flash := &cobra.Command{
		Use:   "flash",
		Short: "Flash firmware and manage updates",
	}

	probe := &cobra.Command{
		Use:   "probe <host|bmc>",
		Short: "Detect and print the flash chip",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := flashTargetFromName(args[0])
			if err != nil {
				return err
			}
			return withPlatform(func(p platform.Platform) error {
				return p.FlashProbe(target)
			})
		},
	}

	read := &cobra.Command{
		Use:   "read <host|bmc> <outfile>",
		Short: "Read the flash into a file",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := flashTargetFromName(args[0])
			if err != nil {
				return err
			}
			return withPlatform(func(p platform.Platform) error {
				return p.FlashRead(target, args[1])
			})
		},
	}

	var writeForce bool
	write := &cobra.Command{
		Use:   "write <host|bmc> <firmware>",
		Short: "Write firmware to the flash",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := flashTargetFromName(args[0])
			if err != nil {
				return err
			}
			return withPlatform(func(p platform.Platform) error {
				return p.FlashWrite(target, args[1], writeForce)
			})
		},
	}
	write.Flags().BoolVar(&writeForce, "force", false, "skip the firmware size check")

	status := &cobra.Command{
		Use:   "status [host|bmc]",
		Short: "Show the update status (default host)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := parseFlashTarget(args)
			if err != nil {
				return err
			}
			return withPlatform(func(p platform.Platform) error {
				status, err := p.FlashStatus(target)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "state: %s\n", status.State)
				return nil
			})
		},
	}

	abort := &cobra.Command{
		Use:   "abort [host|bmc]",
		Short: "Abort a stuck or stale update (default host)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := parseFlashTarget(args)
			if err != nil {
				return err
			}
			return withPlatform(func(p platform.Platform) error {
				return p.FlashAbort(target)
			})
		},
	}

	// A BMC flash removes mains through the Tasmota plug, so the bus-driving
	// commands take the same address override as `power ac`. It is registered as a
	// local flag on flash and on each leaf rather than as a persistent flag, for
	// the reason given in acCmd.
	for _, c := range []*cobra.Command{flash, probe, read, write} {
		c.Flags().StringVar(&flagTasmotaIP, "tasmota-ip", "", "Tasmota plug address (or BENCHCTL_TASMOTA_IP); overrides the board default")
	}

	flash.AddCommand(probe, read, write, status, abort)
	return flash
}
