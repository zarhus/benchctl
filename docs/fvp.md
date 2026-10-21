<!--
SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>

SPDX-License-Identifier: Apache-2.0
-->

# FVP

The `fvp` driver drives the Arm RD-V3 Fixed Virtual Platform (`FVP_RD_V3`)
directly. Mains power is the model process, host power is firmware execution
inside it, the console is a telnet connection to the model's console UART
(`ns_uart0`), and `flash write` records which image the next launch boots. The
driver starts the model itself, with no launcher script, the same way the
BenchRack driver drives the GPIO and SPI mux itself. The parameter names, their
defaults and the port announcement come from `$BENCHCTL_FVP_MODEL --list-params`
and from runs of the real binary, as `examples/rdv3-fvp/run.md` in the loopctl
repository describes.

The driver lets a port go through the same hardware-gated path, loopctl's
hardware confirmation with one benchctl command per step, before there is a real
bench. It is built into the same binary as the other drivers and selected at
runtime with `--platform fvp` or `BENCHCTL_PLATFORM=fvp`. It runs entirely
locally, so there is no SSH user and no board wiring. `--host localhost`, or
`BENCHCTL_HOST=localhost`, satisfies the host check every platform goes through.

| Setting      | Flag      | Environment                 | Default        |
| ------------ | --------- | --------------------------- | -------------- |
| Model binary |           | `BENCHCTL_FVP_MODEL`        | none, required |
| Console port |           | `BENCHCTL_FVP_CONSOLE_PORT` | `6000`         |
| Iris port    |           | `BENCHCTL_FVP_IRIS_PORT`    | `7300`         |
| Work dir     |           | `BENCHCTL_FVP_WORKDIR`      | `.fvp`         |
| Board        | `--board` | `BENCHCTL_BOARD`            | `rdv3`         |

`BENCHCTL_FVP_MODEL` is the path to the `FVP_RD_V3` binary, and every command
fails until it names an executable file. `rdv3` is the only board profile. A
second FVP-modelled reference design adds one in the driver.

The console port is the port requested for the console UART's telnet terminal,
and `console` connects to it. Every UART terminal defaults to port 5000 and
moves up from there on a collision. With every terminal present that cluster
occupies 5000 to 5012, so a request for 5000 can land on whatever port the
others left free. Port 6000 sits clear of the cluster, so the request succeeds
and `console` needs no port announcement from the model. The Iris port is where
the model's Iris debug and control server listens. Port 7300 sits clear of the
same cluster and of 7100 to 7109, the range Arm's Python Iris client scans.

A Fast Models license (`ARMLMD_LICENSE_FILE`) is needed for anything past a
short evaluation window.

## Power

| Command           | What it does on the FVP                                        |
| ----------------- | -------------------------------------------------------------- |
| `power ac on`     | launches the model with firmware execution paused              |
| `power on`        | starts firmware execution over Iris, needs AC on               |
| `power status`    | `on` while firmware runs, else `off` with the reason           |
| `power ac status` | `on` with the model's pid while the process runs               |
| `power ac off`    | asks Iris for a shutdown, then kills the model's process group |
| `power ac cycle`  | `power ac off`, then `power ac on`                             |
| `power off`       | not implemented                                                |
| `power reset`     | not implemented, `--hard` included                             |

`power ac on` waits until the model's log reports the console port listening, up
to 20 seconds, and fails with the end of that log when the model exits first.
`power status` reads `off (AC off)`, `off (AC on, firmware paused)`, or says
that Iris is unreachable. `power ac off` waits five seconds for the Iris
shutdown to take effect, then sends `SIGKILL` to the model's process group.

Restarting firmware from its reset vector needs a relaunch of the model, which
also drops any telnet connection to its console. A test suite that holds the
console open across power transitions needs that connection kept. The restart is
therefore `power ac off`, `power ac on` and `power on`, run on purpose, and
`power reset` reports "not implemented".

The launch sets `start_telnet=0` on every UART terminal, `ns_uart0` included, as
Arm's `run_model.sh` does. Left at the default, each terminal launches its own
xterm wherever a display is attached. `ns_uart0` still listens on the console
port, and every other UART writes a log file to the work directory.

## Boot images

`flash write host <image>` turns AC off, then records the image the next
`power ac on` boots. `flash probe host` prints the recorded image and its size,
and `flash read host <file>` copies it to `<file>`. `--force` has no effect.
`flash status`, `flash abort`, the `bmc` flash target, `console --source sol`,
`console --source uart1` and `console --serve` report "not implemented". The
model has no separate BMC, and the driver attaches only the console UART.

A FIP (`fip-uefi.bin`, recognised by its header) goes on
`ros.board.flashloader0`, as in Arm's `run_model.sh`. Only the RSE images are
preloaded, from the FIP's own directory:

- `tf_m_rom.bin`, `tf_m_flash.bin`, `tf_m_vm0_0.bin`, `tf_m_vm1_0.bin`

Any other image (`uefi.bin`, or a `coreboot.rom` built for the same entry point)
is BL33 for firmware built with `PLAT_RESET_TO_BL31=1`. It is preloaded into
DRAM at BL31's normal-world entry point, together with the RSE images and the
eight RESET_TO_BL31 companions (`scp_ramfw.bin`, `mcp_ramfw.bin`, `tf-bl31.bin`,
`rmm.img`, `<board>_spmc_sp_manifest.dtb`, `<board>_nt_fw_config.dtb`,
`stmm.pkg`, `hafnium.bin`). The image also goes on `ros.board.flashloader0`, the
flash a `coreboot.rom` reads its CBFS from.

`power ac on` with nothing flashed, or with a firmware file missing, fails
before the model is launched.

## Work directory

For the RESET_TO_BL31 flow, `power ac on` looks for the companions inside the
work directory's mirror of a firmware build's `output/<board>` tree, in the
layout that build produces, which matches the `$OUTDIR` and `$ROOTDIR` paths in
Arm's `run_model.sh`:

| Directory under the work dir         | Files                                                                      |
| ------------------------------------ | -------------------------------------------------------------------------- |
| `output/<board>/<board>/`            | the RSE images, `scp_ramfw.bin`, `mcp_ramfw.bin`, `tf-bl31.bin`, `rmm.img` |
| `output/<board>/components/<board>/` | `<board>_spmc_sp_manifest.dtb`, `<board>_nt_fw_config.dtb`, `stmm.pkg`     |
| `output/<board>/components/`         | `hafnium.bin`, one level higher, since a build shares it across boards     |

The first directory is the flat set a build also stages beside the FIP. A build
never flattens the second set next to the FIP. The simplest setup is a single
symlink, once:

```sh
ln -s /path/to/build/output "$BENCHCTL_FVP_WORKDIR/output"
```

The work directory also holds:

| File                               | What it is                                                                    |
| ---------------------------------- | ----------------------------------------------------------------------------- |
| `model.pid`                        | the running model's pid                                                       |
| `model-stdout.log`                 | the model process's own output                                                |
| `console.log`                      | the console UART's transcript                                                 |
| `*-uart.log`, `ns-uart1.log`       | the other UARTs' output                                                       |
| `boot-image`                       | the image `flash write host` recorded                                         |
| `disk-image`                       | the disk image `disk mount` recorded, with its medium                         |
| `nor1_flash.img`, `nor2_flash.img` | the two 64 MiB NOR flash images the platform expects, created on first launch |
| `example_pcie_hierarchy_<n>.json`  | optional PCIe hierarchy files, copied there by you                            |

The PCIe hierarchy files for `pcie_group_1` through `_3` are static examples
that ship with `model-scripts`, such as `example_pcie_hierarchy_1.json`. A
firmware build does not produce them. Drop a copy in the work directory, and the
launch wires in each group whose file is present. A missing one is omitted with
a note. `pcie_group_0` needs no file of its own and is always configured.

## Console

`console` needs the model running, so run `power ac on` first. Attaching before
`power on` catches the earliest boot output. A telnet attach that fails within
three seconds is retried for up to five seconds, with the failed attempts'
output held back, and `console` reports the last failure once that time runs
out. `Ctrl+]` detaches, or `Ctrl+]` then `quit` with `--detach-key none`.

## Disks

`disk mount <image>` turns AC off and records the image, and the next
`power ac on` attaches it until `disk eject` or another `disk mount` changes the
record. `disk eject` also turns AC off first. `disk status` prints the recorded
image, its medium and its size.

| `--medium` | Model parameter                                    |
| ---------- | -------------------------------------------------- |
| `virtio`   | `ros.board.virtioblockdevice.image_path`, default  |
| `ahci`     | `pcie_group_0.pcie4.pcie_rc.ahci0.ahci.image_path` |

The default is `virtio`, because an AHCI disk reproducibly crashes the reference
UEFI on RD-V3 with a synchronous exception in `ArmCpuDxe`, whatever image is
attached. The `cd` and `usb` media, `--boot-once`, `--reset-bmc`, `--power-on`
and `disk eject --slot` belong to the [BMC] driver and fail here.

[bmc]: bmc.md
