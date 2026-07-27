// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"errors"
	"fmt"
	"io"

	"github.com/zarhus/benchctl/exec"
)

// ConsoleSession is one way to reach a host console: what to call it, how to
// attach, how the operator gets back out, and how to release it. A driver names
// a tool and a detach sequence, and Run supplies the announcement, the release
// around the session, and the error wrapping, so every console behaves the same
// way whatever tool carries it.
//
// The detach sequence belongs to the program in Attach rather than to benchctl,
// which only spawns it, so it differs from one console to the next. Detach
// carries it into the line printed immediately before the terminal is handed
// over, where the operator needs it.
type ConsoleSession struct {
	// What names the console in messages, as in "host console via telnet on
	// port 13541".
	What string
	// Detach is the sequence that ends the session, written as it is typed.
	Detach string
	// Attach is the command that holds the terminal for the session.
	Attach []string
	// Release, when set, drops an existing attachment. It runs before the attach,
	// because a session that ended abnormally can leave the console held and the
	// attach then fails, and again afterwards to leave the console free. Its
	// result is ignored: with nothing attached these commands report failure,
	// which is the ordinary state around a session.
	Release []string
	// NoEscape disables the SSH client's own escape character for the attach,
	// for a program that takes "~" as its own.
	NoEscape bool
	// Unreachable, when set, reads a failed Release and reports whether the
	// console cannot be reached at all, in which case Run stops instead of
	// attaching. The release reaches the same place the attach does, so its
	// failure is the cheapest reachability answer available, and acting on it
	// spares the operator a second wait for the same timeout with the terminal
	// already handed over. It takes the release's output as well as its error,
	// because a tool may report the failure on either stream.
	Unreachable func(output string, err error) error
}

// Run announces the session, attaches the terminal to it, and returns once the
// operator detaches.
func (session ConsoleSession) Run(runner exec.Runner, progress io.Writer) error {
	if len(session.Release) > 0 {
		fmt.Fprintf(progress, "Releasing any open %s...\n", session.What)
		// The release is also the first command to reach the bench and the console,
		// so both kinds of unreachable show up here. Stop at either: the attach
		// would fail the same way, after the same wait, with the tool writing its
		// own diagnosis to the terminal under a line saying the console is being
		// attached.
		out, err := runner.Run(session.Release...)
		if benchUnreachable(err) {
			return err
		}
		if session.Unreachable != nil {
			if err := session.Unreachable(out, err); err != nil {
				return err
			}
		}
		defer func() { _, _ = runner.Run(session.Release...) }()
	}
	fmt.Fprintf(progress, "Attaching to %s. Detach with %s.\n", session.What, session.Detach)
	attach := runner.RunInteractive
	if session.NoEscape {
		attach = runner.RunInteractiveNoEscape
	}
	if err := attach(session.Attach...); err != nil {
		// An unreachable bench is not the console's failure, so naming the console
		// would point at the wrong host.
		if benchUnreachable(err) {
			return err
		}
		return fmt.Errorf("%s: %w", session.What, err)
	}
	return nil
}

// benchUnreachable reports whether err says the bench itself could not be
// reached, rather than that a command on it failed.
func benchUnreachable(err error) bool {
	var target *exec.UnreachableError
	return errors.As(err, &target)
}
