<!--
SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>

SPDX-License-Identifier: Apache-2.0
-->

# benchctl

benchctl is a Go CLI that controls test benches: host power, mains power, the
host console, firmware flashing and disk images. Each bench type is a platform
driver behind one command interface. The [README] lists the platforms, commands
and settings, and [docs/][docs] holds a page per driver plus consoles and
development.

## Layout

| Path                | What it holds                                                                |
| ------------------- | ---------------------------------------------------------------------------- |
| `main.go`           | sets the default platform (`benchrack`) and runs `cmd.Main`                  |
| `cmd/`              | cobra commands, flag and environment resolution (`resolve.go`), exit codes   |
| `platform/`         | the `Platform` interface, the registry, the drivers, console sessions, relay |
| `exec/`             | runs commands locally or over SSH, scp, detach keys, terminal handling       |
| `internal/redfish/` | Redfish client and the ssh `-W` transport through a jump host                |
| `internal/ipmi/`    | builds `ipmitool` SOL commands                                               |
| `internal/iris/`    | minimal client for the FVP model's Iris protocol                             |
| `internal/rte/`     | RTE REST client for GPIO                                                     |
| `internal/tasmota/` | Tasmota plug client, run through `curl` on the bench                         |

## Build and test

`task` runs `fmt`, `vet`, `test` and `build`, and `task lint` checks formatting
without rewriting. `Taskfile.yml` has the exact targets. The pre-commit hooks
run the same Go checks plus mdformat (`--wrap 80`), codespell and `reuse lint`.

## Conventions

- Commands in `cmd/` hold no platform logic. They resolve settings as flag, then
  `BENCHCTL_*` variable, then default, and call `Platform` methods.
- A driver registers a `platform.Spec` from `init()`. Its constructor takes an
  `exec.Runner` and a `platform.Config` and stores a configuration error that
  every method returns first.
- An unsupported operation returns `platform.ErrNotImplemented`, which exits
  with status 3. Optional capabilities are separate interfaces checked with a
  type assertion, as `platform.ConsoleServer` is for `console --serve`.
- Bench commands go through `exec.Runner`, so the same code runs on the bench
  and over SSH from a PC.
- Error messages are lower case, wrap causes with `%w`, and name the flag or
  variable that fixes the problem. Progress lines go to the driver's `progress`
  writer, which is stderr.
- Tests sit in the package they test. `platform/fake_test.go` has a fake
  `Runner`, and `cmd` tests swap `buildPlatform` for a fake platform. HTTP
  clients are tested against `httptest` servers, and the FVP tests run a script
  in place of the model. Delays and timeouts are struct fields, so tests shorten
  them.
- Every file carries `SPDX-FileCopyrightText` and `SPDX-License-Identifier`.

## Rules

- Keep the `bmc` driver generic: no vendor or product names for the machines it
  drives, in code, docs or commit messages. Write "BMC" or "Redfish BMC".
  Firmware names such as OpenBMC appear only where the code already uses them.
- benchctl is Linux only. Do not add or document other operating systems.
- Commits need a GPG signature and a DCO sign-off, which `conform` checks in the
  `commit-msg` hook: `git commit -S -s`. `conform` fails inside a linked git
  worktree, so commit in the main checkout.
- Subjects are imperative and in sentence case, under 80 characters, with no
  trailing period.

[docs]: docs/
[readme]: README.md
