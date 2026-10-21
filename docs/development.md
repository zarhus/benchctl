<!--
SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>

SPDX-License-Identifier: Apache-2.0
-->

# Development

benchctl builds with Go and [Task]. The Go version is pinned in `go.mod`.

## Tasks

| Command        | What it does                                         |
| -------------- | ---------------------------------------------------- |
| `task`         | runs `fmt`, `vet`, `test` and `build`, in that order |
| `task fmt`     | formats every Go file in place with `gofmt -w`       |
| `task vet`     | runs `go vet ./...`                                  |
| `task test`    | runs `go test ./...`                                 |
| `task build`   | builds `bin/benchctl`                                |
| `task install` | installs the binary into `GOPATH/bin`                |
| `task lint`    | fails if any Go file needs `gofmt`, then runs vet    |

## Pre-commit

`.pre-commit-config.yaml` runs gofmt, `go vet`, `go test` and `reuse lint`
alongside the shared 3mdeb hooks, mdformat (wrapped at 80 columns), codespell
and yamllint. Install it with `pre-commit install`, which also installs the
`commit-msg` hook that runs `conform`. `conform` requires every commit to be
GPG-signed and to carry a DCO sign-off, so commit with `git commit -S -s`. A
check also keeps the Go version in the pre-commit config equal to the one in
`go.mod`.

The `reuse lint` gate wants an `SPDX-FileCopyrightText` and an
`SPDX-License-Identifier` tag in every new file.

## Versions and releases

`--version` prints the commit the binary was built from, with `-dirty` when the
working tree had uncommitted changes, and the binary's default platform. A tag
pushed to the repository builds a release with GoReleaser: a Linux amd64 archive
whose `--version` names the tag and the commit.

## Adding a platform

Adding another platform is one package that implements the `Platform` interface
and registers itself with `platform.Register()` from its `init()`. Drivers can
live in this repository, as the `benchrack`, `fvp` and `bmc` drivers do in
`platform/`, or in a separate module that imports this one and builds its own
`main`. A binary's `main` picks its default platform with `platform.SetDefault`.
This repository's `main` picks `benchrack`.

A driver returns `platform.ErrNotImplemented` for an operation its platform
lacks, which benchctl reports with exit status 3. A driver that can relay a
console for `console --serve` also implements `platform.ConsoleServer`.

[task]: https://taskfile.dev
