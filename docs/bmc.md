<!--
SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>

SPDX-License-Identifier: Apache-2.0
-->

# BMC

The `bmc` driver drives a host through its BMC alone, from your PC: host power
over Redfish and the host console over the BMC's SSH console, with IPMI SOL as
the fallback. It is for a machine that sits on no bench, such as one reached
through an ssh jump host on someone else's network. It leaves AC power, flash
and disks alone: `power ac`, `flash` and `disk mount`/`eject`/`status` report
"not implemented". `--host` is `localhost`.

| Setting           | Flag                  | Environment                  | Default                         |
| ----------------- | --------------------- | ---------------------------- | ------------------------------- |
| BMC address       | `--bmc-ip`            | `BENCHCTL_BMC_IP`            | none, required                  |
| BMC user          | `--bmc-user`          | `BENCHCTL_BMC_USER`          | none, required                  |
| BMC password      | `--bmc-password`      | `BENCHCTL_BMC_PASSWORD`      | none, required                  |
| Jump host         | `--bmc-jump`          | `BENCHCTL_BMC_JUMP`          | empty, connect directly         |
| Redfish system ID | `--bmc-system`        | `BENCHCTL_BMC_SYSTEM`        | the only member of `Systems`    |
| Console method    | `--bmc-console`       | `BENCHCTL_BMC_CONSOLE`       | from `SerialConsole`, else ssh  |
| SSH console port  | `--bmc-console-port`  | `BENCHCTL_BMC_CONSOLE_PORT`  | from `SerialConsole`, else 2200 |
| Console entry     | `--bmc-console-entry` | `BENCHCTL_BMC_CONSOLE_ENTRY` | from `ConsoleEntryCommand`      |
| TLS CA bundle     | `--bmc-ca`            | `BENCHCTL_BMC_CA`            | system roots                    |
| Skip TLS checks   | `--bmc-insecure`      | `BENCHCTL_BMC_INSECURE`      | off                             |

The console method is `ssh` or `sol`. The CA bundle is a PEM file.
`BENCHCTL_BMC_INSECURE` takes a boolean such as `1`. A BMC that lists several
systems needs the system ID.

```sh
export BENCHCTL_PLATFORM=bmc BENCHCTL_HOST=localhost
export BENCHCTL_BMC_IP=10.0.0.5 BENCHCTL_BMC_JUMP=client-jump
export BENCHCTL_BMC_USER=root BENCHCTL_BMC_PASSWORD=...
bin/benchctl power status
bin/benchctl power on
bin/benchctl console
```

## Jump host

The jump host is an ssh destination, best a `Host` entry in `~/.ssh/config`,
which supplies its user, keys and any further hops. It has to accept your key,
since the password prompt `sshpass` answers belongs to the BMC. Redfish
connections cross it with `ssh -W <bmc>:443`, one per request, and the SSH
console with `ssh -W` as its `ProxyCommand`. Both connect to the jump host with
a ten-second timeout and `BatchMode=yes`. The BMC's host key is recorded on
first use (`StrictHostKeyChecking=accept-new`). Each Redfish request gives up
after 30 seconds.

## Power

Power maps onto `ComputerSystem.Reset`:

| Command              | Reset type                                               |
| -------------------- | -------------------------------------------------------- |
| `power on`           | `On`                                                     |
| `power off`          | `ForceOff`                                               |
| `power reset`        | `ForceRestart`, or `PowerCycle` where the BMC refuses it |
| `power reset --hard` | `ForceRestart` only                                      |

The driver reads `ResetType@Redfish.AllowableValues` to choose, and each command
polls `PowerState` every two seconds, for up to three minutes, until the host
gets there. `power on` or `power off` sends nothing to a host already in that
state. `power reset` on a host that is off powers it on. `power reset --hard`
works only while the host is on, and reports "not implemented" when the BMC does
not allow `ForceRestart`. `power status` prints `on` or `off`, with a
transitional Redfish state in parentheses.

## Console

`console` reads the system's `SerialConsole`. An enabled `SSH` entry gives the
port, and `SharedWithManagerCLI` with `ConsoleEntryCommand` gives the command
that reaches the host console from the BMC's own command line, as on Dell iDRAC
(`console com2`). Without `SSH`, an enabled `IPMI` entry selects SOL. A BMC that
reports neither, or a Redfish service that does not answer, gets SSH on port
2200, OpenBMC's `obmc-console-ssh`, when an SSH server answers there, reached
the same way as Redfish, and SOL when none does. The settings override each of
these, and with the method and port both set the console needs no Redfish
request.

SOL runs `ipmitool` on the jump host, since IPMI is UDP and the jump host
forwards TCP only. Without a jump host it runs on your PC. The password reaches
`ipmitool` in `IPMI_PASSWORD` through the remote command line, and the SSH
console's through `SSHPASS`, which keeps it out of the local process list.
`console --source sol` forces SOL with the same settings. Like the BenchRack SOL
console, it runs `sol deactivate` before and after the attach, which drops
anyone else on the payload, while the SSH console lets several people attach at
once.

With `--detach-key none`, the SSH console ends on the hot key sequence the BMC
reports in `SerialConsole`, or on ssh's `~.` at the start of a line. `uart1` is
not implemented.

`console --serve` relays this console, or SOL with `--source sol`, for a test
framework, as the [consoles] page describes.

## Dependencies

| Program    | Where                     | Needed for                     |
| ---------- | ------------------------- | ------------------------------ |
| `ssh`      | your PC                   | the jump host, the SSH console |
| `sshpass`  | your PC                   | the SSH console                |
| `ipmitool` | the jump host, or your PC | SOL                            |

[consoles]: consoles.md
