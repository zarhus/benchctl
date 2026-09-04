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
	var source, bmcIP, bmcUser, bmcPassword string

	console := &cobra.Command{
		Use:   "console",
		Short: "Attach to the host serial console (detaches on exit)",
		Long: `Attach to the host serial console (detaches on exit).

--source selects where the console comes from:

  com1  (default) the motherboard serial port, which the platform exports over
        the network. That port carries host output only while the BMC is
        configured to leave it to the host: an OpenBMC set up to print its own
        logs there takes it over, and the host console then has to come from
        the BMC instead.
  sol   the same console reached over the BMC's IPMI serial-over-LAN payload,
        needed once the BMC has taken com1 over. Detach with "~." at the start
        of a line.
  uart1 a second, read-only debug UART some boards wire to the RTE separately
        from com1, typically host firmware output. Detach with Ctrl+C.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if source != "sol" {
				// A BMC flag with another source asks for a console with arguments
				// that do not apply to it.
				for _, name := range []string{"bmc-ip", "bmc-user", "bmc-password"} {
					if cmd.Flags().Changed(name) {
						return fmt.Errorf("--%s applies only with --source sol", name)
					}
				}
			}
			switch source {
			case "com1":
				return withPlatform(func(p platform.Platform) error { return p.Console() })
			case "sol":
				if bmcIP == "" {
					return fmt.Errorf("--source sol needs --bmc-ip")
				}
				bmc := platform.BMC{IP: bmcIP, User: bmcUser, Password: bmcPassword}
				return withPlatform(func(p platform.Platform) error { return p.ConsoleSOL(bmc) })
			case "uart1":
				return withPlatform(func(p platform.Platform) error { return p.ConsoleUART1() })
			default:
				return fmt.Errorf("unknown --source %q (want com1, sol, or uart1)", source)
			}
		},
	}

	flags := console.Flags()
	flags.StringVar(&source, "source", "com1", "console to attach to: `com1`, sol, or uart1")
	flags.StringVar(&bmcIP, "bmc-ip", "", "BMC `address` for --source sol")
	flags.StringVar(&bmcUser, "bmc-user", "admin", "BMC IPMI user (with --source sol)")
	flags.StringVar(&bmcPassword, "bmc-password", "Administrator", "BMC IPMI password (with --source sol)")
	return console
}
