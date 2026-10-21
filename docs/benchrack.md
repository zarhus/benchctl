<!--
SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>

SPDX-License-Identifier: Apache-2.0
-->

# BenchRack

The `benchrack` driver drives a BenchRack bench through its RTE: GPIO, the SPI
mux and host power over the RTE REST API, and `flashrom` over SSH. Run benchctl
on your PC to reach the RTE by IP, or on the RTE to act locally. It flashes the
host and BMC flashes through the SPI mux, switches the mains feed through a
Tasmota plug, and reaches the host console over the RTE's ser2net port, the
BMC's IPMI serial-over-LAN, or a read-only UART1 debug line.

From a PC, benchctl logs in to the RTE as `root` with the password `meta-rte`.
The REST API is on port 8000 of the RTE and benchctl calls it directly, so the
RTE has to be reachable from where benchctl runs.

| Setting      | Flag           | Environment           | Default         |
| ------------ | -------------- | --------------------- | --------------- |
| RTE          | `--host`, `-H` | `BENCHCTL_HOST`       | none, required  |
| SSH password | `--password`   | `BENCHCTL_PASSWORD`   | `meta-rte`      |
| Board        | `--board`      | `BENCHCTL_BOARD`      | `asrock-turin`  |
| Tasmota plug | `--tasmota-ip` | `BENCHCTL_TASMOTA_IP` | `192.168.66.50` |

`asrock-turin` is the only board profile. It has a 32 MiB host flash
(W25Q256JV_Q) and a 64 MiB BMC flash, both at 3.3 V, the console on ser2net port
13541, and UART1 on `/dev/ttyUSB0` at 115200 baud.

## Power

`power on`, `off` and `reset` press the DUT's power button and read its power
LED. `power on` presses the button for one second, and `power off` holds it for
six seconds to force the host off. Each waits for the button to come back up and
then up to 60 seconds for the LED to show the new state. `power reset` is
`power off` followed by `power on`. `power reset --hard` presses the reset
button for one second, and only while the host is on.

## Remote flashing

When you write from a PC, benchctl checks the firmware size against the board's
flash size locally, copies the file to `/data/rom.bin` on the RTE with `scp`,
runs the blocking flash, checks that `flashrom` succeeded, and removes the copy.
`--force` skips the size check. A `read` runs the other way: `flashrom` writes
the image to `/data/readback.bin` on the RTE, then benchctl copies it back to
your file and removes the copy. On the bench the file is already local and no
copy happens.

Both paths are on the RTE's persistent storage, because the RTE has little free
RAM and `flashrom` already holds about twice the flash size in memory. Each
flash and each read overwrites the last file.

`flash status` and `flash abort` report "not implemented", because `flashrom`
runs to completion within one command and leaves no update state to query or
abort.

## Flashing the BMC

A BMC flash removes mains first, through the same Tasmota plug as
`power ac off`, then waits five seconds for the board to discharge before it
energizes the bus. The BMC runs on standby power, so it keeps driving its flash
for as long as mains is applied and holds the bus for a moment afterward, which
no soft power off can change. Mains stays off when the flash finishes, so bring
the DUT back with `power ac on`. A board with no AC control cannot flash its
BMC.

`flash probe`, `flash read` and `flash write` take the same `--tasmota-ip`
override as `power ac`, and use the board's plug without it.

## AC power control

`power ac` switches the DUT's mains feed through a Tasmota smart plug, giving a
hard AC kill and cold boot that the front-panel power button cannot. It is a
separate layer from the soft `power on`/`off`/`reset` commands, which press the
power button and read the power LED.

Two caveats follow from what the plug controls:

- Mains on boots the host only when its firmware is set to power on after AC
  loss (on ASRock, "Restore on AC/Power Loss = Power On"). Otherwise mains on
  leaves the host in S5, and `power on` is still needed to start it. `power ac`
  never presses the power button.
- `power ac status` reports whether mains is applied, and the host may still be
  off. `power status`, the power LED, is the host-power signal.

`power ac cycle` waits two seconds between mains off and mains on.

On BenchRack the plug lives on the RTE's isolated wifi access point at
`192.168.66.50`, reachable only from the RTE, so the command issues `curl` on
the RTE. `curl` always runs on the RTE regardless of where you invoke benchctl:
locally when you run it on the RTE, over SSH when you run it from a PC with
`--host`. The RTE must have the wifi AP set up first.

Point `--tasmota-ip` (or `BENCHCTL_TASMOTA_IP`) at another address when the plug
is on a different network, such as the RTE's management LAN. Because `curl` runs
on the RTE, a custom address must be reachable from the RTE. Reaching it from
the PC is not enough.

A request the plug does not answer gives up after ten seconds and reports which
address failed, so a wrong address or a plug that is off the network is a short
error.

## Consoles

| Source  | What it reaches                     | Runs on the RTE  | Detach with `--detach-key none` |
| ------- | ----------------------------------- | ---------------- | ------------------------------- |
| `com1`  | the motherboard's COM1 over ser2net | `telnet`         | `Ctrl+]` then `quit`            |
| `sol`   | the host console over the BMC's SOL | `ipmitool`       | `~.` at the start of a line     |
| `uart1` | a read-only debug UART              | `stty` and `cat` | `Ctrl+C`                        |

`--source com1` attaches to the motherboard's COM1 port, which the RTE exports
over telnet with ser2net. That port carries host output only while the BMC
leaves it to the host. An OpenBMC configured to print its own logs there takes
the port over, and the host console then has to come from the BMC instead.

`--source sol --bmc-ip <ip>` reaches it that way, over the BMC's IPMI
serial-over-LAN payload:

```sh
bin/benchctl --host rte.local console --source sol --bmc-ip 192.168.50.11
```

| Setting      | Flag             | Environment             | Default         |
| ------------ | ---------------- | ----------------------- | --------------- |
| BMC address  | `--bmc-ip`       | `BENCHCTL_BMC_IP`       | none, required  |
| BMC user     | `--bmc-user`     | `BENCHCTL_BMC_USER`     | `admin`         |
| BMC password | `--bmc-password` | `BENCHCTL_BMC_PASSWORD` | `Administrator` |

`ipmitool` runs on the RTE, as `flashrom` and the Tasmota `curl` do, so the BMC
must be reachable from the RTE. It connects with `lanplus`, IPMI 2.0. Every
attach runs `sol deactivate` first and again on the way out, which the
[consoles] page explains. With `--detach-key none`, a SOL session over SSH still
disables ssh's own escape character, so that `~.` reaches `ipmitool`, which
closes the payload, and the SSH connection stays up.

`--source uart1` attaches read-only to a second debug UART some boards wire to
the RTE separately from COM1, typically host firmware output running alongside
the BMC's own console on COM1. It takes no input, so the attach is a `cat` of
the device after `stty` sets the line raw. Boards without a wired UART1 report
the command as not implemented.

## Disks

The `disk` commands report "not implemented" on BenchRack. The bench has no way
to present a disk image to the host.

## Dependencies

| Program    | Where   | Needed for                       |
| ---------- | ------- | -------------------------------- |
| `ssh`      | your PC | every command from a PC          |
| `scp`      | your PC | every command from a PC          |
| `sshpass`  | your PC | every command from a PC          |
| RteCtrl    | the RTE | every command, REST on port 8000 |
| `flashrom` | the RTE | `flash`                          |
| `telnet`   | the RTE | `console`                        |
| `curl`     | the RTE | `power ac`, a BMC flash          |
| `ipmitool` | the RTE | `console --source sol`           |
| `stty`     | the RTE | `console --source uart1`         |
| `cat`      | the RTE | `console --source uart1`         |

[consoles]: consoles.md
