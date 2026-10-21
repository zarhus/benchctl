<!--
SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>

SPDX-License-Identifier: Apache-2.0
-->

# Consoles

`benchctl console` attaches the terminal to the host console. `--serve` relays
the same console on a TCP port instead, for a test framework.

| Setting    | Flag           | Environment           | Default  |
| ---------- | -------------- | --------------------- | -------- |
| Source     | `--source`     |                       | `com1`   |
| Detach key | `--detach-key` | `BENCHCTL_DETACH_KEY` | `ctrl+]` |
| Relay      | `--serve`      |                       | off      |

## Sources

`--source` selects where `benchctl console` attaches:

| Source  | `benchrack`                     | `fvp`                    | `bmc`                                   |
| ------- | ------------------------------- | ------------------------ | --------------------------------------- |
| `com1`  | COM1 through ser2net on the RTE | the model's console UART | the BMC's SSH console or SOL, see [BMC] |
| `sol`   | IPMI SOL, `ipmitool` on the RTE | not implemented          | IPMI SOL, `ipmitool` on the jump host   |
| `uart1` | the read-only UART1 debug line  | not implemented          | not implemented                         |

The [BenchRack], [FVP] and [BMC] pages describe each one. On every platform but
`bmc`, `--bmc-ip`, `--bmc-user` and `--bmc-password` are read only with
`--source sol`. `--bmc-ip` falls back to `BENCHCTL_BMC_IP`, the same pattern as
`--host`, and the credentials default to `admin` and `Administrator`.

## Detaching

Every source detaches on `Ctrl+]`, which benchctl reads itself. The terminal
goes into raw mode for the session, every other key reaches the console program,
and the key stops that program and everything it started. The SSH hop to the
bench runs with its escape character disabled, so `~` reaches the console too.

`--detach-key` (or `BENCHCTL_DETACH_KEY`) picks another key, written as
`ctrl+<key>` or `^<key>` with a letter or one of `[ \ ] ^ _`.
`--detach-key none` hands the terminal to the console program, which then ends
on its own sequence:

| Console | Sequence with `--detach-key none`                 |
| ------- | ------------------------------------------------- |
| telnet  | `Ctrl+]` then `quit`                              |
| SOL     | `~.` at the start of a line                       |
| UART1   | `Ctrl+C`                                          |
| BMC SSH | the BMC's hot key, or `~.` at the start of a line |

## SOL sessions

Every SOL attach runs `sol deactivate` first, and again on the way out. A
session that ended any other way, such as a closed terminal or a dropped
connection, leaves the payload open on the BMC, and the next `sol activate` then
refuses to run. On a shared bench the opening `sol deactivate` also drops
whoever else is attached.

That opening `sol deactivate` opens a session of its own, so a BMC that does not
answer is reported there, within a few seconds. The attach then never starts,
and the terminal stays with you.

## Serving the console

`console --serve <addr>` relays the console `--source` picks on a loopback TCP
address instead of the terminal, for a test framework that reads the console
over telnet, such as Robot Framework's `Telnet` library:

```sh
bin/benchctl console --serve 127.0.0.1:2323
```

The address must be `localhost` or a loopback IP, since the relay has no
authentication. `benchrack` serves all three sources, `bmc` serves `com1` and
`sol`, and `fvp` reports `--serve` as not implemented.

It holds one console session for its whole life, so the console survives client
reconnects and host resets. When the session ends it reattaches after a wait
that starts at one second and doubles up to 30 seconds, logging each reattach. A
session that held for 30 seconds resets the wait.

| Behaviour          | What the relay does                                                   |
| ------------------ | --------------------------------------------------------------------- |
| Clients            | one at a time, a new connection replaces the old one                  |
| No client          | drops the console output                                              |
| Slow client        | after two seconds of unread output, drops output until it reads again |
| Telnet negotiation | offers `WILL ECHO` and `WILL SUPPRESS-GO-AHEAD` on connect            |
| Telnet commands    | removes them from the client's input, doubles 0xFF in the output      |
| Local terminal     | does not echo keys, since they reach no console                       |
| Ready line         | prints `console-ready` once the console holds                         |
| SIGINT, SIGTERM    | releases the console and exits, a second signal exits at once         |

The telnet options make a telnet client send each key as it is pressed, and
leave the console's own echo as the only one. The console holds once it has run
for two seconds after its first output, or for twelve seconds with no output,
which is longer than the ten-second connect timeout of each ssh hop. Then the
relay prints `console-ready` on a line of its own, for a launcher that waits on
its output. An attach that fails before then gets no `console-ready`: the relay
logs its error and reattaches.

Each source starts the same way the interactive console attaches to it. The SOL
source runs `sol deactivate` before each attach and on the way out, so a payload
left open by a dropped session never blocks the next attach, and anyone else on
the payload is dropped:

```sh
bin/benchctl --host rte.local console --serve 127.0.0.1:2323 --source sol --bmc-ip 192.168.50.11
```

A `sol deactivate` that gets no answer from the BMC ends the session before the
attach, as a failed attach does, and the relay reattaches after its wait.

On `benchrack`, `com1` runs `telnet` to the ser2net port on the RTE. That telnet
keeps its escape character, so a client that sends `Ctrl+]` reaches the telnet
prompt on the RTE. On `bmc`, `com1` is the console `console` picks, and `uart1`
is not implemented.

[benchrack]: benchrack.md
[bmc]: bmc.md
[fvp]: fvp.md
