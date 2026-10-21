<!--
SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>

SPDX-License-Identifier: Apache-2.0
-->

# BMC

The `bmc` driver drives a host through its BMC alone, from your PC: host power
over Redfish and the host console over the BMC's SSH console, with IPMI SOL as
the fallback, and disk images as the BMC's virtual media. It is for a machine
that sits on no bench, such as one reached through an ssh jump host on someone
else's network. It leaves AC power and flash alone: `power ac` and `flash`
report "not implemented". `--host` is `localhost`.

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

## Virtual media

`disk mount` has the BMC present an image to the host as a read-only CD or USB
stick and sets a boot override to it:

1. A local file is served over HTTPS from this machine, see "Image server", or
   streamed over the BMC's NBD websocket with `--transport`, see "NBD
   websocket". An `https://` or `nfs://` URL goes to the BMC as it is.
1. Remote media is switched on where an AMI BMC reports it off, through the
   manager's `EnableRMedia` action, and the driver waits up to a minute for
   slots that take `InsertMedia`.
1. A URL whose protocol the slots' `InsertMedia` `ActionInfo` leaves out is
   refused here, before the host is touched.
1. The host is powered off (`ForceOff`).
1. Every inserted slot with an `EjectMedia` action is ejected.
1. The image goes into a free CD slot for an `.iso` and a USB stick slot for any
   other file, or the kind `--medium` names. The insert task may take up to
   three minutes.
1. The boot override is set to `Cd` or `Usb`, `Continuous` (`Once` with
   `--boot-once`), in `UEFI` mode where the BMC reports a mode. An OpenBMC BMC
   without `BootSourceOverrideMode` fails a PATCH that sets it.
1. `disk-ready` is printed on its own line. The host stays off, so a test runner
   can attach the console and then run `power on`. With `--power-on` the host is
   powered on before `disk-ready`.
1. A local file is served until SIGINT or SIGTERM. Then the driver ejects its
   slot and clears the boot override. A second signal exits at once. A URL mount
   returns after `disk-ready`.

A signal before `disk-ready` stops the mount at once while it waits for slots,
power or the BMC, and after the insert or eject task it is following, so the
undo sees where the media ended up. When the mount stops or fails after the
insert, the driver powers the host off, so that AMI takes the override PATCH,
then ejects the slots and clears the boot override as `disk eject` does.
`disk eject` clears the override even when an eject fails.

`disk eject` ejects every inserted slot with an `EjectMedia` action, or only
`--slot`, and sets the boot override to `Disabled`. Slots without the action,
such as one the BMC's web console fills, belong to another session and stay as
they are. `disk status` lists the slots, the boot override and, on AMI, the
remote media status.

| Setting        | Flag          | Environment                | Default                                    |
| -------------- | ------------- | -------------------------- | ------------------------------------------ |
| Media type     | `--medium`    | none                       | `cd` for `.iso`, `usb` otherwise           |
| Local file     | `--transport` | `BENCHCTL_MEDIA_TRANSPORT` | `https`                                    |
| Server address | `--listen`    | `BENCHCTL_MEDIA_LISTEN`    | `:8443`                                    |
| Image URL base | `--media-url` | `BENCHCTL_MEDIA_URL`       | `https://<address toward the BMC>:<port>/` |
| One boot only  | `--boot-once` | none                       | off, the override stays until eject        |
| Reset the BMC  | `--reset-bmc` | none                       | off                                        |
| Power on       | `--power-on`  | none                       | off, the host stays off                    |
| Slot to eject  | `--slot`      | none                       | every inserted slot                        |

```sh
bin/benchctl disk mount debian-13.7.0-amd64-netinst.iso   # serves until Ctrl+C
bin/benchctl disk mount https://images.example/disk.img --boot-once --power-on
bin/benchctl disk status
bin/benchctl disk eject
```

### NBD websocket

An OpenBMC BMC built with bmcweb's NBD proxy offers a `Slot_0` that answers a
`GET` by name, has `EjectMedia` and no `InsertMedia`, and links to the websocket
`/nbd/0`. `--transport websocket` streams a local file there and fails on a BMC
without that slot. `--transport auto` streams where the BMC offers the slot and
serves over HTTPS otherwise. `https`, the default, always serves the file as
"Image server" describes. `--listen` and `--media-url` apply to HTTPS only.

benchctl opens the websocket with the BMC's credentials, over the same
connection settings as Redfish, and acts as a read-only NBD server for the BMC's
NBD client. The mount follows the steps above with the stream in place of the
insert: the driver waits for `Slot_0` to report the image inserted, sets the
boot override and prints `disk-ready`. It logs the handshake, the first read and
then the read count and bytes served every 30 seconds.

SIGINT or SIGTERM closes the websocket, waits for the slot to empty and clears
the boot override. `disk eject` from another shell ejects `Slot_0`, the BMC's
client disconnects, and the mount clears the override and exits 0. Any other end
of the stream fails with the reason the BMC gave when it closed the websocket.

Some OpenBMC virtual media services crash when Redfish reads any virtual media
resource while the stream is up, and the crash holds the BMC's NBD device until
the BMC reboots. bmcweb reads every slot's properties for each virtual media
request, so `disk status`, `disk eject`, the mount's own wait and the BMC's web
interface all trigger it. A failed insert stays recoverable without a reboot, so
`https` is the default. When the stream ends before the slot reports the image
inserted, the error says to check the service's journal.

A failed `InsertMedia` on a BMC that offers `Slot_0` suggests
`--transport websocket`. On one OpenBMC build, bmcweb called the virtual media
service's `Mount` with an argument type the service did not take, and every
insert failed with 500 whatever the protocol.

### Image server

The server answers `HEAD` and byte ranges for the one file, at `/<file name>`
below the path of `--media-url`, with a self-signed certificate made for each
run. It logs the first request from each address, then the request count and
bytes served every 30 seconds while requests arrive.

The default URL uses the address this machine sends from toward the BMC. Through
a jump host that address belongs to the jump host's side, so `--media-url` is
required there, and the BMC has to reach this machine in any case. Some BMCs,
the AMI MegaRAC tested among them, fetch only from port 443. Either listen on
`:443`, which takes root, `cap_net_bind_service` or
`net.ipv4.ip_unprivileged_port_start=443`, or redirect 443 to the listen port
with iptables or nftables and set `--media-url` without a port. When the insert
fails after the BMC took the request, and the server saw no request, the error
lists these ways through. For a URL benchctl does not serve, AMI's
`InsertMediaError` brings up a shorter hint that names the port. A BMC that
refuses the `InsertMedia` request itself gets no hint, since it fetched nothing.
The AMI bench refuses an `.iso` URL on a USB stick slot that way.

The AMI bench lists HTTPS in its USB stick slots' `ActionInfo`, but it refuses
an `.iso` there and fails an `.img` without fetching it. It takes USB images
only over the share types its web interface offers for hard disk images. Any
failed insert of an `https://` URL into a USB stick slot therefore adds a hint:
an ISO can go into a CD slot with `--medium cd`, and a raw image can be exported
over NFS and mounted by its `nfs://` URL.

### BMC rules

- Slots come from the `VirtualMedia` links of the system and its managers. A
  slot fits by its `MediaTypes`, or by an ID that starts with `CD` or `USBStick`
  when it lists none.
- `InsertMedia` carries `TransferProtocolType` from the URL scheme, empty
  `UserName` and `Password` for HTTPS, and `MediaType` when the action's
  `ActionInfo` lists it. The driver follows a task until it completes and
  reports its messages when it ends in `Exception`.
- AMI moves media to the first free slot and reports the image as `//host/path`,
  so after the insert the driver finds the slot by host and path. It requests an
  image at the root of the server as `//<file name>`, and the image server
  cleans the path before it matches it.
- The boot override PATCH carries `If-Match` from a fresh GET and only the
  `Boot` fields it sets. AMI requires `BootSourceOverrideTarget` and
  `BootSourceOverrideMode` in every boot PATCH, so clearing the override sends
  `Disabled` with the target `None`. AMI reports no override as `null`, and the
  driver then sends no PATCH. AMI answers 503 while the host boots, and the
  driver retries for up to a minute.
- With no slots after remote media is on, the mount fails. `--reset-bmc` then
  posts `Manager.Reset` `ForceRestart`, waits up to five minutes for Redfish to
  go away and answer again, and tries once more. Consoles attached through the
  BMC drop with the reset.

## Dependencies

| Program    | Where                     | Needed for                     |
| ---------- | ------------------------- | ------------------------------ |
| `ssh`      | your PC                   | the jump host, the SSH console |
| `sshpass`  | your PC                   | the SSH console                |
| `ipmitool` | the jump host, or your PC | SOL                            |

[consoles]: consoles.md
