<!--
SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>

SPDX-License-Identifier: Apache-2.0
-->

# benchctl

`benchctl` controls test benches from a workstation or from the bench itself.
Each bench type is a platform driver behind one command interface, and a new
bench slots in as one more driver. It runs on Linux.

## Installation

```sh
task install   # build the current revision and install it into GOPATH/bin

# or the latest release, which pulls in any packages it needs
latest() {
  curl -sL https://api.github.com/repos/zarhus/benchctl/releases/latest |
    grep -o "https://[^\"]*\.$1" | head -1
}
sudo dnf install "$(latest rpm)"                               # Fedora
curl -LO "$(latest deb)" && sudo apt install ./benchctl_*.deb  # Debian, Ubuntu
```

The [releases] page also has a tar.gz archive with the binary, and a `.sha256`
file for every download. `task build` writes `bin/benchctl` without installing
it. benchctl is pure Go, so `GOARCH` builds it for a Linux bench of another
architecture with no C toolchain.

## Quick start

```sh
task build                                   # writes bin/benchctl
bin/benchctl --host rte.local power status   # a BenchRack, from your PC
bin/benchctl --host rte.local console        # its host console, Ctrl+] detaches
```

The examples call a source build as `bin/benchctl`. An installed binary runs as
`benchctl`.

## Platforms

| Platform    | What it drives                                       | Runs on            | SSH user / default password |
| ----------- | ---------------------------------------------------- | ------------------ | --------------------------- |
| `benchrack` | a BenchRack bench through its RTE                    | your PC or the RTE | `root` / `meta-rte`         |
| `fvp`       | the Arm RD-V3 FVP model, as a local process          | your PC            | none (always local)         |
| `bmc`       | a host through its Redfish BMC, with no bench at all | your PC            | none (always local)         |

The binary built here defaults to `benchrack`. `--platform` or
`BENCHCTL_PLATFORM` selects another. The [BenchRack], [FVP] and [BMC] pages
describe each driver.

## Commands

| Command                                      | What it does                                            |
| -------------------------------------------- | ------------------------------------------------------- |
| `power on`                                   | powers the host on and waits until it is on             |
| `power off`                                  | powers the host off and waits until it is off           |
| `power status`                               | prints the current power state                          |
| `power reset`                                | power-cycles the host off then on, waits until it is on |
| `power reset --hard`                         | hard-resets the host, the mechanism varies by platform  |
| `power ac on` / `off`                        | applies or removes mains (AC) power                     |
| `power ac cycle`                             | removes mains, waits briefly, applies it again          |
| `power ac status`                            | prints whether mains is applied                         |
| `console`                                    | attaches to the host serial console (COM1)              |
| `console --source sol`                       | attaches to the host console over the BMC's IPMI SOL    |
| `console --source uart1`                     | attaches read-only to the UART1 debug console           |
| `console --serve <addr>`                     | relays the console on a loopback TCP address            |
| `flash probe <host\|bmc>`                    | detects and prints the flash chip                       |
| `flash read <host\|bmc> <file>`              | reads the flash into `<file>`                           |
| `flash write <host\|bmc> <fw> [--force]`     | writes firmware to the flash                            |
| `flash status [host\|bmc]`                   | shows the update status, host by default                |
| `flash abort [host\|bmc]`                    | aborts a stuck or stale update, host by default         |
| `disk mount <image> [--medium virtio\|ahci]` | records a disk image to attach from the next power on   |
| `disk eject`                                 | forgets the recorded disk image                         |
| `disk status`                                | shows the recorded disk image                           |
| `--version`                                  | prints the build's commit and its default platform      |

benchctl exits with 0 on success, 3 when the selected platform does not
implement the operation, and 1 on any other error. A script can tell
"unsupported" from "failed" that way. The table below shows what each platform
implements, and `-` marks a command that exits with 3.

| Command group                  | `benchrack`       | `fvp`                  | `bmc`                 |
| ------------------------------ | ----------------- | ---------------------- | --------------------- |
| `power on`, `off`, `status`    | yes               | `on` and `status` only | yes                   |
| `power reset`, `reset --hard`  | yes               | -                      | yes                   |
| `power ac`                     | yes, Tasmota plug | yes, the model process | -                     |
| `console`                      | yes               | yes                    | yes                   |
| `console --source sol`         | yes               | -                      | yes                   |
| `console --source uart1`       | yes               | -                      | -                     |
| `console --serve`              | yes               | -                      | yes, `com1` and `sol` |
| `flash probe`, `read`, `write` | yes               | `host` only            | -                     |
| `flash status`, `abort`        | -                 | -                      | -                     |
| `disk`                         | -                 | yes                    | -                     |

The [consoles] page covers the console sources, detaching and `--serve`.

## Settings

Each setting resolves as flag, then environment variable, then default.

| Setting      | Flag              | Environment             | Default                             |
| ------------ | ----------------- | ----------------------- | ----------------------------------- |
| Host         | `--host`, `-H`    | `BENCHCTL_HOST`         | none, required                      |
| Platform     | `--platform`      | `BENCHCTL_PLATFORM`     | set by the binary, `benchrack` here |
| SSH password | `--password`      | `BENCHCTL_PASSWORD`     | driver default                      |
| Board        | `--board`         | `BENCHCTL_BOARD`        | driver default                      |
| Tasmota plug | `--tasmota-ip`    | `BENCHCTL_TASMOTA_IP`   | the board's plug                    |
| Detach key   | `--detach-key`    | `BENCHCTL_DETACH_KEY`   | `ctrl+]`                            |
| BMC address  | `--bmc-ip`        | `BENCHCTL_BMC_IP`       | none                                |
| BMC user     | `--bmc-user`      | `BENCHCTL_BMC_USER`     | `admin` for `--source sol`          |
| BMC password | `--bmc-password`  | `BENCHCTL_BMC_PASSWORD` | `Administrator` for `--source sol`  |
| Trace        | `--verbose`, `-v` |                         | off                                 |

On a bench, `BENCHCTL_HOST` is set to the localhost form, so no host is needed.
On your PC the variable is unset, so `--host` is required. Running `benchctl` on
your PC with no host fails rather than acting on localhost. `--host` takes an IP
or a hostname (for `<hostname>.local` resolution). The `fvp` and `bmc` drivers
run locally and take `--host localhost`.

`--platform` is left out of `--help`. `--board` selects a board profile on
multi-board platforms. The `bmc` driver has further settings, listed on the
[BMC] page, and the `fvp` driver reads its own environment variables, listed on
the [FVP] page.

```sh
# From a PC:
bin/benchctl --host rte.local power status
bin/benchctl --host rte.local flash write host ./image.bin

# On the bench:
bin/benchctl power status
```

## Dependencies

| Where                           | Programs                                                |
| ------------------------------- | ------------------------------------------------------- |
| your PC, for a remote bench     | `ssh`, `scp`, `sshpass`                                 |
| your PC, for `bmc`              | `ssh`, `sshpass`                                        |
| the `bmc` jump host, or your PC | `ipmitool`, for SOL                                     |
| a BenchRack RTE                 | `flashrom`, `telnet`, `curl`, `ipmitool`, `stty`, `cat` |

The RTE also serves the RteCtrl REST API on port 8000. The [BenchRack] page says
which command needs which program.

## More

- [BenchRack]: the RTE, remote flashing, the BMC flash and AC power.
- [FVP]: the RD-V3 model, its boot flows, firmware layout and disks.
- [BMC]: power over Redfish and the host console through a Redfish BMC.
- [Consoles]: console sources, detaching, and relaying a console with `--serve`.
- [Development]: the build tasks, the pre-commit checks and adding a platform.

[benchrack]: docs/benchrack.md
[bmc]: docs/bmc.md
[consoles]: docs/consoles.md
[development]: docs/development.md
[fvp]: docs/fvp.md
[releases]: https://github.com/zarhus/benchctl/releases
