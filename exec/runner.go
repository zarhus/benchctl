// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package exec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ExitCode reports the exit status of the command behind err, and whether err
// came from a command that ran and exited non-zero at all. A bench command run
// over SSH exits with the remote command's status, so the status a caller reads
// here is the bench program's own, wherever it ran.
func ExitCode(err error) (int, bool) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// Runner runs commands on a bench and copies firmware to and from it. The driver
// depends on this interface so it can be tested with a fake.
type Runner interface {
	// Run executes argv on the bench and returns its standard output. The error,
	// when non-nil, includes the command and its standard error.
	Run(argv ...string) (string, error)
	// RunInteractive runs argv on the bench attached to the local terminal,
	// allocating a remote PTY over SSH. Used for the serial console.
	RunInteractive(argv ...string) error
	// Stream runs argv on the bench and returns a reader of its standard output
	// plus a wait function. The caller reads stdout to EOF, then calls wait,
	// which returns the command's exit error (including stderr) or nil. Used for
	// long commands that report progress as they run.
	Stream(argv ...string) (stdout io.ReadCloser, wait func() error, err error)
	// Push copies localPath to remotePath on the bench and returns the path to
	// use there plus a cleanup function that removes it. When the target is local
	// it is a no-op that returns localPath (remotePath is ignored) and a cleanup
	// that does nothing.
	Push(localPath, remotePath string) (path string, cleanup func() error, err error)
	// Pull arranges for a file the bench produces to reach localPath. It returns
	// the path the bench command should write to (localPath when local, remotePath
	// when remote) plus a fetch function that copies the file to localPath after
	// the command runs (a no-op when local) and removes the remote temp.
	Pull(localPath, remotePath string) (path string, fetch func() error, err error)
	// Host reports the bench host the runner reaches, so a driver can address a
	// second control surface on the same host (e.g. a REST API). It is
	// "localhost" (or "") when commands run locally.
	Host() string
}

// CmdRunner is the production Runner. It runs commands locally when the target
// is the local host and over SSH otherwise.
type CmdRunner struct {
	Target  Target
	Verbose bool
}

// commandArgv wraps a bench command for execution: unchanged when local, or an
// ssh invocation when remote. tty requests a remote PTY (ignored locally, where
// the process already inherits the terminal).
func (runner *CmdRunner) commandArgv(argv []string, tty bool) []string {
	if runner.Target.IsLocal() {
		return argv
	}
	return sshArgv(runner.Target, argv, tty)
}

func (runner *CmdRunner) trace(argv []string) {
	if runner.Verbose {
		fmt.Fprintf(os.Stderr, "+ %s\n", shellJoin(argv))
	}
}

func (runner *CmdRunner) Host() string {
	return runner.Target.Host
}

func (runner *CmdRunner) Run(argv ...string) (string, error) {
	full := runner.commandArgv(argv, false)
	runner.trace(full)
	cmd := exec.Command(full[0], full[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", shellJoin(argv), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func (runner *CmdRunner) RunInteractive(argv ...string) error {
	full := runner.commandArgv(argv, true)
	runner.trace(full)
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (runner *CmdRunner) Stream(argv ...string) (io.ReadCloser, func() error, error) {
	full := runner.commandArgv(argv, false)
	runner.trace(full)
	cmd := exec.Command(full[0], full[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	wait := func() error {
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("%s: %w: %s", shellJoin(argv), err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	return stdout, wait, nil
}

// StagePath is the default staging path for a pushed file on a remote bench:
// /var/tmp with a pid-tagged name so concurrent runs do not collide. A bench
// short on RAM (where /var/tmp is tmpfs) should stage to persistent storage
// instead and pass that path to Push.
func StagePath(localPath string) string {
	return fmt.Sprintf("/var/tmp/benchctl-%d-%s", os.Getpid(), filepath.Base(localPath))
}

func (runner *CmdRunner) Push(localPath, remotePath string) (string, func() error, error) {
	if runner.Target.IsLocal() {
		return localPath, func() error { return nil }, nil
	}
	argv := scpArgv(runner.Target, localPath, remotePath)
	runner.trace(argv)
	cmd := exec.Command(argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("scp %s: %w: %s", localPath, err, strings.TrimSpace(stderr.String()))
	}
	cleanup := func() error {
		_, err := runner.Run("rm", "-f", remotePath)
		return err
	}
	return remotePath, cleanup, nil
}

func (runner *CmdRunner) Pull(localPath, remotePath string) (string, func() error, error) {
	if runner.Target.IsLocal() {
		return localPath, func() error { return nil }, nil
	}
	fetch := func() error {
		argv := scpFromArgv(runner.Target, remotePath, localPath)
		runner.trace(argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("scp %s: %w: %s", remotePath, err, strings.TrimSpace(stderr.String()))
		}
		_, err := runner.Run("rm", "-f", remotePath)
		return err
	}
	return remotePath, fetch, nil
}
