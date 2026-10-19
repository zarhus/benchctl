// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package exec

import (
	"os"

	"golang.org/x/sys/unix"
)

// NoEcho turns off the echo of the terminal in, for a command that reads no
// keys from it, and returns a function that restores it. Line editing and
// signals stay as they are, so Ctrl+C still interrupts. It does nothing when
// in is not a terminal.
func NoEcho(in *os.File) func() {
	fd := int(in.Fd())
	saved, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() {}
	}
	quiet := *saved
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return func() {}
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, saved) }
}
