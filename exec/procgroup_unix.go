// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package exec

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
)

// stopProcessGroup sends SIGTERM to the process group process leads.
func stopProcessGroup(process *os.Process) {
	_ = syscall.Kill(-process.Pid, syscall.SIGTERM)
}

// followResize copies the size of the terminal in to ptmx whenever it changes,
// until the returned function is called.
func followResize(in, ptmx *os.File) func() {
	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-resized:
				_ = pty.InheritSize(in, ptmx)
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(resized)
		close(done)
	}
}
