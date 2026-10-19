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
	var source string

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

On the bmc platform the default source is the BMC's own console: its SSH
console, or IPMI SOL, as --bmc-console or the BMC's Redfish SerialConsole picks.

Every source detaches on Ctrl+], which benchctl reads itself, so "~" and every
other key reach the console. --detach-key (or BENCHCTL_DETACH_KEY) picks
another key, written as ctrl+<key>. "none" hands the terminal to the console
program and detaches on its own sequence: CTRL+] then "quit" for telnet, "~."
at the start of a line for sol, Ctrl+C for uart1.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch source {
			case "com1":
				return withPlatform(func(p platform.Platform) error { return p.Console() })
			case "sol":
				ip := resolveBMCIP(flagBMC.ip)
				if ip == "" {
					return fmt.Errorf("--source sol needs --bmc-ip or BENCHCTL_BMC_IP")
				}
				bmc := platform.BMC{
					IP:       ip,
					User:     resolveSettingOr(flagBMC.user, "BENCHCTL_BMC_USER", "admin"),
					Password: resolveSettingOr(flagBMC.password, "BENCHCTL_BMC_PASSWORD", "Administrator"),
				}
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
	return console
}
