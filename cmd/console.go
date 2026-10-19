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
        needed once the BMC has taken com1 over.
  uart1 a second, read-only debug UART some boards wire to the RTE separately
        from com1, typically host firmware output.

Every source detaches on Ctrl+], which benchctl reads itself, so "~" and every
other key reach the console. --detach-key (or BENCHCTL_DETACH_KEY) picks
another key, written as ctrl+<key>. "none" hands the terminal to the console
program and detaches on its own sequence: CTRL+] then "quit" for telnet, "~."
at the start of a line for sol, Ctrl+C for uart1.`,
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
				ip := resolveBMCIP(bmcIP)
				if ip == "" {
					return fmt.Errorf("--source sol needs --bmc-ip or BENCHCTL_BMC_IP")
				}
				bmc := platform.BMC{IP: ip, User: bmcUser, Password: bmcPassword}
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
	flags.StringVar(&flagDetachKey, "detach-key", "", "`key` that ends the session, as ctrl+<key>, or none (or BENCHCTL_DETACH_KEY; default ctrl+])")
	flags.StringVar(&bmcIP, "bmc-ip", "", "BMC `address` for --source sol (or BENCHCTL_BMC_IP)")
	flags.StringVar(&bmcUser, "bmc-user", "admin", "BMC IPMI user (with --source sol)")
	flags.StringVar(&bmcPassword, "bmc-password", "Administrator", "BMC IPMI password (with --source sol)")
	return console
}
