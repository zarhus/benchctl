// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package exec

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// PipedProcess is a running program whose terminal the caller holds: its
// output, its input, and a way to end it.
type PipedProcess interface {
	io.ReadWriter
	Wait() error
	Stop()
}

// Piped is a local program whose terminal the caller holds, for a console that
// benchctl relays rather than hands to the local terminal. The program runs on
// a pseudo-terminal of its own, so ipmitool and telnet find a terminal to put
// into raw mode, and its output, standard error included, comes from Read.
type Piped struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

// StartPiped starts argv locally, leading a session of its own, so Stop
// reaches everything it starts, such as ssh behind sshpass.
func StartPiped(argv []string) (*Piped, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &Piped{cmd: cmd, ptmx: ptmx}, nil
}

// Read reads the program's output. The terminal reports EIO once the program
// and everything it started have closed it, which Read reports as io.EOF.
func (piped *Piped) Read(p []byte) (int, error) {
	n, err := piped.ptmx.Read(p)
	if err != nil && errors.Is(err, syscall.EIO) {
		err = io.EOF
	}
	return n, err
}

// Write types p into the program's terminal.
func (piped *Piped) Write(p []byte) (int, error) { return piped.ptmx.Write(p) }

// Wait waits for the program to exit and closes its terminal. The caller reads
// its output to the end first.
func (piped *Piped) Wait() error {
	err := piped.cmd.Wait()
	piped.ptmx.Close()
	return err
}

// Stop sends SIGTERM to the program's process group.
func (piped *Piped) Stop() { stopProcessGroup(piped.cmd.Process) }
