// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/zarhus/benchctl/platform"
)

func consoleCmd() *cobra.Command {
	var sol, bmcUser, bmcPassword string

	console := &cobra.Command{
		Use:   "console",
		Short: "Attach to the host serial console (detaches on exit)",
		Long: `Attach to the host serial console (detaches on exit).

Without --sol this is the motherboard serial port, which the platform exports
over the network. That port carries host output only while the BMC is configured
to leave it to the host: an OpenBMC set up to print its own logs there takes it
over, and the host console then has to come from the BMC instead. --sol reaches
it over the BMC's IPMI serial-over-LAN payload. Detach a SOL session with "~."
at the start of a line.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if sol == "" {
				// A BMC credential without --sol asks for the serial console with
				// arguments that do not apply to it.
				for _, name := range []string{"bmc-user", "bmc-password"} {
					if cmd.Flags().Changed(name) {
						return fmt.Errorf("--%s applies only with --sol", name)
					}
				}
				return withPlatform(func(p platform.Platform) error { return p.Console() })
			}
			bmc := platform.BMC{IP: sol, User: bmcUser, Password: bmcPassword}
			return withPlatform(func(p platform.Platform) error { return p.ConsoleSOL(bmc) })
		},
	}

	flags := console.Flags()
	flags.StringVar(&sol, "sol", "", "attach over the BMC's IPMI serial-over-LAN at this `address` instead of the motherboard serial port")
	flags.StringVar(&bmcUser, "bmc-user", "admin", "BMC IPMI user (with --sol)")
	flags.StringVar(&bmcPassword, "bmc-password", "Administrator", "BMC IPMI password (with --sol)")
	return console
}
