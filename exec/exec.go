// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

// Package exec runs bench commands, either locally (when the bench is the local
// host) or over SSH from a workstation. It also copies firmware to and from the
// bench for remote flashing.
package exec

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Target describes how to reach a bench.
type Target struct {
	Host     string // bench host; "" or "localhost" means run locally
	User     string // SSH user, e.g. "root"
	Password string // SSH password, supplied to ssh/scp via sshpass
}

// IsLocal reports whether commands run on the local host rather than over SSH.
func (target Target) IsLocal() bool {
	return target.Host == "" || target.Host == "localhost"
}

// sshFailureStatus is the exit status ssh reserves for its own failures, such as
// an unreachable host or a refused authentication. Any other status ssh returns
// comes from the command that ran on the bench.
const sshFailureStatus = 255

// UnreachableError reports that a bench command never ran, because ssh could not
// reach the bench. It is distinct from a command that ran and failed, so that a
// caller names the bench rather than blaming the command, and can stop before
// handing the terminal to a session that cannot open.
//
// A remote command that exits 255 itself is indistinguishable from ssh's own
// failure and is reported this way too.
type UnreachableError struct {
	Host   string // the bench that could not be reached
	Detail string // what ssh reported, empty when its output went to the terminal
	Status error  // the exit error from the ssh invocation
}

func (unreachable *UnreachableError) Error() string {
	if unreachable.Detail != "" {
		return fmt.Sprintf("cannot reach bench %s: %s", unreachable.Host, unreachable.Detail)
	}
	return fmt.Sprintf("cannot reach bench %s: ssh failed (%v)", unreachable.Host, unreachable.Status)
}

func (unreachable *UnreachableError) Unwrap() error { return unreachable.Status }

// remoteDeps are the external programs the SSH path shells out to: sshpass and
// ssh on every command, scp when pushing firmware. A local target uses none of
// them.
var remoteDeps = []string{"sshpass", "ssh", "scp"}

// CheckDependencies verifies that the programs needed to reach the target are on
// PATH. A local target needs none. A remote target needs sshpass, ssh, and scp,
// so this fails up front on a workstation that lacks them rather than partway
// through a command.
func CheckDependencies(target Target) error {
	if target.IsLocal() {
		return nil
	}
	var missing []string
	for _, dep := range remoteDeps {
		if _, err := exec.LookPath(dep); err != nil {
			missing = append(missing, dep)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing programs needed to reach a remote bench: %s (install the OpenSSH client and sshpass)", strings.Join(missing, ", "))
	}
	return nil
}

// sshOpts are the ssh/scp options used for every remote command. The connect
// timeout makes a mistyped or unreachable host fail in seconds.
var sshOpts = []string{
	"-o", "StrictHostKeyChecking=no",
	"-o", "UserKnownHostsFile=/dev/null",
	"-o", "LogLevel=ERROR",
	"-o", "ConnectTimeout=10",
}

// shellJoin quotes each argument as needed and joins them into a single command
// string suitable for a remote shell (ssh/scp run the string through the remote
// shell, so glob and whitespace characters must be quoted).
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// shellSafe matches arguments that need no quoting for a POSIX shell.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote single-quotes arg unless every character is shell-safe. An embedded
// single quote is rendered as the standard '\” escape.
func shellQuote(arg string) string {
	if arg != "" && shellSafe.MatchString(arg) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// ttyMode selects the terminal handling for a bench command run over SSH.
type ttyMode int

const (
	// noTTY runs the command with no remote terminal.
	noTTY ttyMode = iota
	// remoteTTY allocates a remote pseudo-terminal, needed by a console that puts
	// its stdout into raw mode, and leaves ssh's own "~" escape character in place.
	remoteTTY
	// remoteTTYNoEscape allocates a remote pseudo-terminal and disables ssh's
	// escape character, so that "~" reaches the program on the bench.
	remoteTTYNoEscape
)

// sshArgv builds the local argv that runs remote (a bench command) on the bench
// over SSH, with terminal handling per mode.
func sshArgv(target Target, remote []string, mode ttyMode) []string {
	argv := []string{"sshpass", "-p", target.Password, "ssh"}
	if mode != noTTY {
		argv = append(argv, "-tt")
	}
	if mode == remoteTTYNoEscape {
		argv = append(argv, "-e", "none")
	}
	argv = append(argv, sshOpts...)
	argv = append(argv, target.User+"@"+target.Host, shellJoin(remote))
	return argv
}

// scpArgv builds the local argv that copies local to remotePath on the bench.
func scpArgv(target Target, local, remotePath string) []string {
	argv := []string{"sshpass", "-p", target.Password, "scp"}
	argv = append(argv, sshOpts...)
	argv = append(argv, local, target.User+"@"+target.Host+":"+remotePath)
	return argv
}

// scpFromArgv builds the local argv that copies remotePath on the bench to local.
func scpFromArgv(target Target, remotePath, local string) []string {
	argv := []string{"sshpass", "-p", target.Password, "scp"}
	argv = append(argv, sshOpts...)
	argv = append(argv, target.User+"@"+target.Host+":"+remotePath, local)
	return argv
}
