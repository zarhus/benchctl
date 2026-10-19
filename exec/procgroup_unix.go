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

// pollable returns file as a non-blocking file that Go's poller reads, so Close
// ends a Read that is waiting, and that programs started later do not inherit.
// It closes file. On a failure it returns file as it is.
func pollable(file *os.File) *os.File {
	// ForkLock keeps a program started meanwhile from inheriting the copy.
	syscall.ForkLock.RLock()
	fd, err := syscall.Dup(int(file.Fd()))
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return file
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return file
	}
	name := file.Name()
	file.Close()
	return os.NewFile(uintptr(fd), name)
}
