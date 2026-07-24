<!--
SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>

SPDX-License-Identifier: Apache-2.0
-->

# benchctl

`benchctl` controls test benches from a workstation or from the bench itself.
Each bench type is a platform driver behind one command interface, and a new
bench slots in as one more driver.

For BenchRack it drives the RTE: GPIO, the SPI mux, and power over the RTE REST
API, and `flashrom` over SSH. Run it on your PC to reach the RTE by IP, or on
the RTE to act locally.

## Install

```sh
go build -o bin/benchctl .
# or
task build
```

Both write the binary to `bin/benchctl` without installing it. The examples
below call it as `bin/benchctl`. Copy it onto your `PATH` (or `go install .`) to
run it as `benchctl`.

benchctl is pure Go, so it cross-compiles for a bench of another platform or
architecture from any host with no C toolchain, through the usual `GOOS` and
`GOARCH` settings.

## Usage

```sh
bin/benchctl power on                                 power on, wait until the host is on
bin/benchctl power off                                power off, wait until the host is off
bin/benchctl power status                             print the current power state
bin/benchctl power reset                              power-cycle off then on, wait until back on
bin/benchctl power reset --hard                       hard-reset the host (mechanism varies by platform)
bin/benchctl power ac on                              apply mains/AC power (Tasmota plug)
bin/benchctl power ac off                             remove mains/AC power
bin/benchctl power ac cycle                           AC power-cycle: mains off, brief wait, mains on
bin/benchctl power ac status                          print whether mains/AC is applied
bin/benchctl console                                  attach to the host serial console
bin/benchctl flash probe <host|bmc>                   detect and print the flash chip
bin/benchctl flash read  <host|bmc> <file>            read the flash into <file>
bin/benchctl flash write <host|bmc> <fw> [--force]    write firmware to the flash
bin/benchctl flash status [host|bmc]                  show the update status (default host)
bin/benchctl flash abort  [host|bmc]                  abort a stuck or stale update (default host)
```

### Selecting the bench

Each input resolves as flag, then environment variable, then a default:

| Input    | Flag         | Environment         | Default                       |
| -------- | ------------ | ------------------- | ----------------------------- |
| Host     | `--host, -H` | `BENCHCTL_HOST`     | none - required off the bench |
| Platform | `--platform` | `BENCHCTL_PLATFORM` | set by the binary             |
| Password | `--password` | `BENCHCTL_PASSWORD` | driver default                |
| Board    | `--board`    | `BENCHCTL_BOARD`    | driver default                |

On a bench, `BENCHCTL_HOST` is set to the localhost form, so no host is needed.
On your PC the variable is unset, so `--host` is required. Running `benchctl` on
your PC with no host fails rather than acting on localhost.

`--host` takes an IP or a hostname (for `<hostname>.local` resolution).
`--board` selects a board profile on multi-board platforms: BenchRack defaults
to `asrock-turin`.

```sh
# From a PC:
bin/benchctl --host rte.local power status
bin/benchctl --host rte.local flash write host ./image.bin

# On the bench:
bin/benchctl power status
```

### Remote flashing

When you write from a PC, `benchctl` checks the firmware size locally, copies
the file to a temporary path on the bench with `scp`, runs the blocking flash,
confirms it succeeded, and removes the temporary file. A `read` runs the other
way: `flashrom` writes the image to a temporary path on the bench, then
`benchctl` copies it back to your file and removes the temporary file. On the
bench the file is already local and no copy happens.

### AC power control

`power ac` switches the DUT's mains feed through a Tasmota smart plug, giving a
hard AC kill and cold boot that the front-panel power button cannot. It is a
separate layer from the soft `power on`/`off`/`reset` commands, which pulse the
power button and read the power LED.

Two caveats follow from what the plug controls:

- Mains on boots the host only when its firmware is set to power on after AC
  loss (on ASRock, "Restore on AC/Power Loss = Power On"). Otherwise mains on
  leaves the host in S5, and `power on` is still needed to start it. `power ac`
  never presses the power button.
- `power ac status` reports whether mains is applied, not whether the host
  booted. `power status` (the power LED) remains the real host-power signal.

On BenchRack the plug lives on the RTE's isolated wifi access point at
`192.168.66.50`, reachable only from the RTE, so the command issues `curl` on
the RTE. `curl` always runs on the RTE regardless of where you invoke benchctl:
locally when you run it on the RTE, over SSH when you run it from a PC with
`--host`. The RTE must have the wifi AP set up first.

Point `--tasmota-ip` (or `BENCHCTL_TASMOTA_IP`) at another address when the plug
is on a different network, such as the RTE's management LAN. Because `curl` runs
on the RTE, a custom address must be reachable from the RTE, not from the PC.

## Platforms

| Platform    | Status      | SSH user / default password |
| ----------- | ----------- | --------------------------- |
| `benchrack` | implemented | `root` / `meta-rte`         |

BenchRack flashes the host and BMC flashes through the SPI mux, controls host
power through the RTE, switches the mains feed through a Tasmota plug, and opens
the serial console over SSH. `flash status` and `flash abort` report "not
implemented", because `flashrom` runs to completion within one command and
leaves no update state to query or abort.

Adding another platform is one package that implements the `Platform` interface
and registers itself with `platform.Register()` from its `init()`. Drivers can
live in this repository (like `platform/benchrack.go`) or in a separate module
that imports this one and builds its own `main`.

## Dependencies

- On the PC: `ssh`, `scp`, `sshpass`.
- On a BenchRack RTE: `flashrom`, `telnet`, `curl` (for `power ac`), and the
  RteCtrl REST API (port 8000).

## Development

```sh
task          # fmt, vet, test, build
task test
task lint
```

`.pre-commit-config.yaml` runs gofmt, `go vet`, and `go test` alongside the
shared 3mdeb hooks. Install it with `pre-commit install`.
