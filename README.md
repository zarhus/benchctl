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
bin/benchctl console                                  attach to the host serial console (COM1)
bin/benchctl console --source sol --bmc-ip <ip>       attach to the host console via BMC SOL
bin/benchctl console --source uart1                   attach read-only to the UART1 debug console
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

### Flashing the BMC

A BMC flash removes mains first, through the same Tasmota plug as
`power ac off`, then waits five seconds for the board to discharge before it
energizes the bus. The BMC runs on standby power, so it keeps driving its flash
for as long as mains is applied and holds the bus for a moment afterward, which
no soft power off can change. Mains stays off when the flash finishes, so bring
the DUT back with `power ac on`.

`flash probe`, `flash read`, and `flash write` take the same `--tasmota-ip`
override as `power ac`, and use the board's plug without it.

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

A request the plug does not answer gives up after ten seconds and reports which
address failed, so a wrong address or a plug that is off the network is a short
error rather than a long wait.

### Consoles

`--source` selects where `benchctl console` attaches: `com1` (the default),
`sol`, or `uart1`.

Every source detaches on `Ctrl+]`, which benchctl reads itself. The terminal
goes into raw mode for the session, every other key reaches the console program,
and the key stops that program and everything it started. The SSH hop to the
bench runs with its escape character disabled, so `~` reaches the console too.
`--detach-key` (or `BENCHCTL_DETACH_KEY`) picks another key, written as
`ctrl+<key>` with a letter or one of `[ \ ] ^ _`. `--detach-key none` hands the
terminal to the console program, which then ends on its own sequence: `Ctrl+]`
then `quit` for telnet, `~.` at the start of a line for SOL, `Ctrl+C` for UART1.

`--source com1` attaches to the motherboard's COM1 port, which the RTE exports
over telnet with ser2net. That port carries host output only while the BMC
leaves it to the host. An OpenBMC configured to print its own logs there takes
the port over, and the host console then has to come from the BMC instead.

`--source sol --bmc-ip <ip>` reaches it that way, over the BMC's IPMI
serial-over-LAN payload:

```sh
bin/benchctl --host rte.local console --source sol --bmc-ip 192.168.50.11
```

`--bmc-ip` falls back to `BENCHCTL_BMC_IP` when not given, the same pattern as
`--host`. The credentials default to `admin` and `Administrator`, overridden
with `--bmc-user` and `--bmc-password`.

Every attach runs `sol deactivate` first, and again on the way out, because a
session that ended any other way, such as a closed terminal or a dropped
connection, leaves the payload open on the BMC and the next `sol activate` then
refuses to run. On a shared bench the opening `sol deactivate` also drops
whoever else is attached.

That opening `sol deactivate` opens a session of its own, so a BMC that does not
answer is reported there, in a few seconds, rather than by an attach that waits
out ipmitool's full timeout a second time with the terminal already handed over.

`ipmitool` runs on the RTE, as `flashrom` and the Tasmota `curl` do, so the BMC
must be reachable from the RTE rather than from your PC. With
`--detach-key none`, a SOL session over SSH still disables ssh's own escape
character, so that `~.` reaches ipmitool instead of dropping the SSH connection
and stranding the payload.

`--source uart1` attaches read-only to a second debug UART some boards wire to
the RTE separately from COM1, typically host firmware output running alongside
the BMC's own console on COM1. There is nothing to type into it, so the attach
is a plain `stty`-configured `cat` of the device rather than an interactive
program. Boards without a wired UART1 report the command as not implemented. On
every platform but `bmc`, `--bmc-ip`, `--bmc-user`, and `--bmc-password` are
read only with `--source sol`.

## Platforms

| Platform    | Status      | SSH user / default password |
| ----------- | ----------- | --------------------------- |
| `benchrack` | implemented | `root` / `meta-rte`         |
| `fvp`       | implemented | none (always local)         |
| `bmc`       | implemented | none (always local)         |

BenchRack flashes the host and BMC flashes through the SPI mux, controls host
power through the RTE, switches the mains feed through a Tasmota plug, and
reaches the host console over the RTE's ser2net port, the BMC's IPMI
serial-over-LAN, or a read-only UART1 debug line. `flash status` and
`flash abort` report "not implemented", because `flashrom` runs to completion
within one command and leaves no update state to query or abort.

`fvp` (`platform/fvp.go`) drives the real Arm RD-V3 Fixed Virtual Platform
(`FVP_RD_V3`) directly: "power" is the model process's lifetime, "console" is a
direct telnet connection to its own console UART terminal, and "flash" records
which image (a FIP, a normal build's own `uefi.bin`, or a `coreboot.rom` built
for the same entry point) the next power-on boots. There is no external launcher
script - this driver knows how to invoke the model itself, the same way
`benchrack.go` drives BenchRack's GPIO and SPI mux directly rather than shelling
out to a board-specific script. Parameter names, defaults and the
port-announcement format come from `$BENCHCTL_FVP_MODEL --list-params` and from
running the real binary, confirmed empirically rather than guessed; see
`examples/rdv3-fvp/run.md` in the loopctl repository for how.

It exists so a port can be driven through the same hardware-gated path
(loopctl's hardware confirmation, one benchctl command per step) before there is
a real bench, and it is meant to be built into the same binary as a real
platform driver, selected at runtime with `--platform fvp` or
`BENCHCTL_PLATFORM=fvp` - the default stays whatever a binary's `main` sets with
`platform.SetDefault`. It runs entirely locally, so there is no SSH user, no
board wiring, and `--host`/`BENCHCTL_HOST` only need to be set to satisfy the
same "never silently act on localhost" check every platform goes through
(`localhost` is correct and sufficient). `power off` sends `SIGKILL` directly
rather than trying a graceful signal first: an FVP has no shutdown hook of its
own to catch it, only a booted guest OS does, over SSH, which is a different
operation entirely.

`power on` disables every UART terminal's own telnet except ns_uart0's: left at
their default, each one opens a port and - wherever a real display is attached -
auto-launches its own xterm (the model's default `terminal_command` behaviour),
one window per terminal, for consoles this driver never intended to expose.

Configure it with just two things:

- `BENCHCTL_FVP_MODEL` (required) - the path to the `FVP_RD_V3` model binary.
- `BENCHCTL_FVP_CONSOLE_PORT` (default `6000`) - the port requested for the
  console UART's telnet terminal, and what `console` trusts the model actually
  opened. Deliberately not `5000`, every UART terminal's own default: each one
  wants it and auto-increments from there on collision (confirmed against a real
  run - with every terminal present, that cluster occupies `5000`-`5012`), so
  requesting `5000` for ns_uart0 too risks landing on whatever the others left
  free rather than the port actually asked for. `6000` sits clear of that
  cluster, so the request reliably succeeds and `console` does not need to read
  the model's own announcement back out of its process log to find out where it
  actually landed.

Plus, less often:

- `BENCHCTL_FVP_WORKDIR` (default `.fvp`) - where its pidfile, the launched
  process's own stdout/stderr, the console UART's transcript, the two dummy NOR
  flash images the platform expects present (created automatically), and the
  recorded boot image all live.
- `--board` (default `rdv3`) - which of `platform/fvp.go`'s board profiles to
  use; a second FVP-modelled reference design adds an entry there.

`flash write host <image>` records the image the next power on boots. `AC*`,
`ConsoleSOL` and the `bmc` flash target all report "not implemented": a virtual
platform has no mains feed and no separate BMC.

A FIP (`fip-uefi.bin`, recognised by its header) goes on
`ros.board.flashloader0`, as in
`model-scripts/rdinfra/platforms/rdv3/run_model.sh`. Only the RSE images
(`tf_m_rom.bin`, `tf_m_flash.bin`, `tf_m_vm0_0.bin`, `tf_m_vm1_0.bin`) are
preloaded, from the FIP's own directory.

Any other image (`uefi.bin`, or a `coreboot.rom` built for the same entry point)
is BL33 for firmware built with `PLAT_RESET_TO_BL31=1`. It is preloaded into
DRAM at BL31's normal-world entry point, together with the RSE images and the
eight RESET_TO_BL31 companions (`scp_ramfw.bin`, `mcp_ramfw.bin`, `tf-bl31.bin`,
`rmm.img`, `<board>_spmc_sp_manifest.dtb`, `<board>_nt_fw_config.dtb`,
`stmm.pkg`, `hafnium.bin`).

`power on` with nothing flashed, or with a firmware file missing, fails before
the model is launched.

For the RESET_TO_BL31 flow, rather than a flat layout of its own invention,
`power on` looks for these files inside `BENCHCTL_FVP_WORKDIR`'s own mirror of a
real firmware build's own `output/<board>` tree, in exactly the layout that
build produces it (confirmed against
`model-scripts/rdinfra/platforms/rdv3/run_model.sh`'s own `$OUTDIR`/`$ROOTDIR`
paths):

- `output/<board>/<board>/` - the flat set a build also stages beside the FIP:
  `tf_m_rom.bin`, `tf_m_flash.bin`, `tf_m_vm0_0.bin`, `tf_m_vm1_0.bin`,
  `scp_ramfw.bin`, `mcp_ramfw.bin`, `tf-bl31.bin`, `rmm.img`.
- `output/<board>/components/<board>/` - the remaining three companions
  (`<board>_spmc_sp_manifest.dtb`, `<board>_nt_fw_config.dtb`, `stmm.pkg`),
  which a build never flattens next to the FIP.
- `output/<board>/components/` - `hafnium.bin`, one level higher than the rest
  since a real build shares it across boards.

The simplest setup is a single symlink, once:
`ln -s /path/to/build/output $BENCHCTL_FVP_WORKDIR/output`. A Fast Models
license (`ARMLMD_LICENSE_FILE`) is needed for anything past a short evaluation
window regardless.

PCIe hierarchy files for `pcie_group_1` through `_3` (static examples that ship
with `model-scripts`, e.g. `example_pcie_hierarchy_1.json`, not something a
firmware build produces) are looked for in `BENCHCTL_FVP_WORKDIR` instead - drop
a copy there, the same way the two NOR flash images live there - and wired in
per group when present; a missing one is just omitted, with a note, rather than
failing the launch. `pcie_group_0` needs no file of its own and is always
configured.

### `bmc`

`bmc` (`platform/bmc.go`) drives a host through its BMC alone, from your PC:
host power over Redfish and the host console over the BMC's SSH console, with
IPMI SOL as the fallback. It is for a machine that is not on a bench, such as
one reached through an ssh jump host on someone else's network. It never touches
AC power or flash, which report "not implemented", and `--host` is `localhost`.

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

```sh
export BENCHCTL_PLATFORM=bmc BENCHCTL_HOST=localhost
export BENCHCTL_BMC_IP=10.0.0.5 BENCHCTL_BMC_JUMP=client-jump
export BENCHCTL_BMC_USER=root BENCHCTL_BMC_PASSWORD=...
bin/benchctl power status
bin/benchctl power on
bin/benchctl console
```

The jump host is an ssh destination, best a `Host` entry in `~/.ssh/config`,
which supplies its user, keys and any further hops. It has to accept your key,
since the password prompt `sshpass` answers belongs to the BMC. Redfish
connections cross it with `ssh -W <bmc>:443`, one per request, and the SSH
console with `ssh -J`. The BMC's host key is recorded on first use
(`StrictHostKeyChecking=accept-new`).

Power maps onto `ComputerSystem.Reset`: `power on` sends `On`, `power off` sends
`ForceOff`, `power reset` sends `ForceRestart`, or `PowerCycle` where the BMC
does not allow `ForceRestart`, and `power reset --hard` sends `ForceRestart`
only. The driver reads `ResetType@Redfish.AllowableValues` to choose, and each
command polls `PowerState` until the host gets there.

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
forwards TCP only. The password reaches it in `IPMI_PASSWORD` through the remote
command line, and the SSH console's through `SSHPASS`, which keeps it out of the
local process list. `console --source sol` forces SOL with the same settings.
Like the BenchRack SOL console, it runs `sol deactivate` before and after the
attach, which drops anyone else on the payload, while the SSH console lets
several people attach at once.

`console --serve <addr>` relays the same console on a loopback TCP address for a
test framework that reads the console over telnet, such as Robot Framework's
`Telnet` library:

```sh
bin/benchctl console --serve 127.0.0.1:2323
```

It holds one console session for its whole life, so the console survives client
reconnects and host resets, and reattaches with a growing backoff when the
session ends, logging each reattach. It takes one client at a time, and a new
connection replaces the old one. Output with no client connected is dropped. On
connect it offers the client `WILL ECHO` and `WILL SUPPRESS-GO-AHEAD`, so a
telnet client sends each key as it is pressed and the console's own echo is the
only one. Telnet commands from the client are removed and 0xFF in the output is
doubled, as telnet expects. The terminal running `--serve` does not echo keys,
since they reach no console. Once the console has held for two seconds, it
prints `console-ready` on a line of its own, for a launcher that waits on its
output. Output alone does not count, since an attach that fails prints its error
first. SIGINT or SIGTERM ends it. Over SOL it runs `sol deactivate` before each
attach and on the way out, as the interactive console does, so a payload left
open by a dropped session never blocks the next attach, and anyone else on the
payload is dropped. Other platforms report `--serve` as not implemented.

Adding another platform is one package that implements the `Platform` interface
and registers itself with `platform.Register()` from its `init()`. Drivers can
live in this repository (like `platform/benchrack.go` and `platform/fvp.go`) or
in a separate module that imports this one and builds its own `main`.

## Dependencies

- On the PC: `ssh`, `scp`, `sshpass`.
- For `bmc`: `ssh` and `sshpass` on the PC, `ipmitool` on the jump host (or the
  PC without one) for SOL.
- On a BenchRack RTE: `flashrom`, `telnet`, `curl` (for `power ac`), `ipmitool`
  (for `console --source sol`), `stty` and `cat` (for `console --source uart1`),
  and the RteCtrl REST API (port 8000).

## Development

```sh
task          # fmt, vet, test, build
task test
task lint
```

`.pre-commit-config.yaml` runs gofmt, `go vet`, `go test`, and `reuse lint`
alongside the shared 3mdeb hooks. Install it with `pre-commit install`. The
`reuse lint` gate wants an `SPDX-FileCopyrightText` and an
`SPDX-License-Identifier` tag in every new file.
