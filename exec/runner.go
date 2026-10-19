// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package exec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
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
	// RunInteractiveNoEscape is RunInteractive with the SSH client's own escape
	// character disabled, for a bench program that takes "~" as its own escape.
	// An IPMI serial-over-LAN session ends on "~.", which ssh would otherwise
	// consume as its disconnect sequence, dropping the connection and leaving the
	// SOL payload open on the BMC. Running locally there is no ssh in the way and
	// this is identical to RunInteractive.
	RunInteractiveNoEscape(argv ...string) error
	// RunInteractiveOut is RunInteractive but writes the process's own standard
	// output and standard error to out instead of straight to the local
	// terminal; standard input still goes to the local terminal directly, so
	// real interactivity (typing into the session) is unaffected. Used by a
	// caller that retries a flaky attach and wants to withhold a fast-failing
	// attempt's own output until it is known worth showing, without losing
	// live output once a session actually holds.
	RunInteractiveOut(out io.Writer, argv ...string) error
	// Stream runs argv on the bench and returns a reader of its standard output
	// plus a wait function. The caller reads stdout to EOF, then calls wait,
	// which returns the command's exit error (including stderr) or nil. Used for
	// long commands that report progress as they run.
	Stream(argv ...string) (stdout io.ReadCloser, wait func() error, err error)
	// StartPiped starts argv on the bench on a terminal the caller holds instead
	// of the local one, with the SSH client's own escape character disabled. Used
	// for a console that benchctl relays.
	StartPiped(argv ...string) (PipedProcess, error)
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
	// DetachHint names the key that ends an interactive run, as typed, such as
	// "Ctrl+]". It is empty when the program's own detach sequence ends it.
	DetachHint() string
}

// CmdRunner is the production Runner. It runs commands locally when the target
// is the local host and over SSH otherwise.
type CmdRunner struct {
	Target  Target
	Verbose bool
	// DetachKey, when non-zero, is the key that ends every interactive run. The
	// runner reads the terminal itself, in raw mode, passes every other key to
	// the program, and stops the program on this key. The SSH hop then runs with
	// its escape character disabled, so "~" reaches the program on the bench.
	// Zero leaves the terminal to the program and its own detach sequence.
	DetachKey byte
}

// commandArgv wraps a bench command for execution: unchanged when local, or an
// ssh invocation when remote. mode selects the remote terminal handling and is
// ignored locally, where the process already inherits the terminal and no ssh
// sits between it and the user.
func (runner *CmdRunner) commandArgv(argv []string, mode ttyMode) []string {
	if runner.Target.IsLocal() {
		return argv
	}
	return sshArgv(runner.Target, argv, mode)
}

func (runner *CmdRunner) trace(argv []string) {
	if runner.Verbose {
		fmt.Fprintf(os.Stderr, "+ %s\n", shellJoin(argv))
	}
}

func (runner *CmdRunner) Host() string {
	return runner.Target.Host
}

func (runner *CmdRunner) DetachHint() string {
	if runner.DetachKey == 0 {
		return ""
	}
	return DetachKeyName(runner.DetachKey)
}

// unreachable classifies a failed ssh invocation: it returns an UnreachableError
// when ssh could not reach the bench, and nil when the failure belongs to the
// command that ran there. detail is ssh's own output where the caller captured
// it. Locally there is no ssh in the way, so every exit status is the command's
// own.
func (runner *CmdRunner) unreachable(err error, detail string) error {
	if runner.Target.IsLocal() {
		return nil
	}
	if code, ran := ExitCode(err); !ran || code != sshFailureStatus {
		return nil
	}
	return &UnreachableError{Host: runner.Target.Host, Detail: detail, Status: err}
}

func (runner *CmdRunner) Run(argv ...string) (string, error) {
	full := runner.commandArgv(argv, noTTY)
	runner.trace(full)
	cmd := exec.Command(full[0], full[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if unreachable := runner.unreachable(err, detail); unreachable != nil {
			return "", unreachable
		}
		return "", fmt.Errorf("%s: %w: %s", shellJoin(argv), err, detail)
	}
	return string(out), nil
}

func (runner *CmdRunner) RunInteractive(argv ...string) error {
	return runner.runInteractive(argv, remoteTTY, os.Stdout, os.Stderr)
}

func (runner *CmdRunner) RunInteractiveNoEscape(argv ...string) error {
	return runner.runInteractive(argv, remoteTTYNoEscape, os.Stdout, os.Stderr)
}

// RunInteractiveOut uses a single real pipe for both standard output and
// standard error, rather than the two independent io.Writer-backed pipes
// Go's os/exec would otherwise set up (one copying goroutine each). Two
// separate pipes let an unsynchronized stdout-copying goroutine and
// stderr-copying goroutine race: confirmed live (via a console session
// retrying a flaky telnet attach through this method) to reorder and garble
// output - the process's own stderr diagnosis landing ahead of an earlier
// stdout line, run together without a line break between them. Sharing one
// pipe end for both streams means the process's own write(2) calls, to
// either fd, land in the exact order it issued them, the same as a real
// terminal would serialize them.
func (runner *CmdRunner) RunInteractiveOut(out io.Writer, argv ...string) error {
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("%s: %w", shellJoin(argv), err)
	}

	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, r)
		close(copyDone)
	}()

	runErr := runner.runInteractive(argv, remoteTTY, w, w)
	w.Close()
	<-copyDone
	r.Close()
	return runErr
}

// runInteractive runs argv attached to the local terminal, with its output to
// stdout and stderr. With a detach key set, runDetachable reads the terminal
// instead.
func (runner *CmdRunner) runInteractive(argv []string, mode ttyMode, stdout, stderr io.Writer) error {
	if runner.DetachKey != 0 {
		return runner.runDetachable(argv, remoteTTYNoEscape, stdout, os.Stdin, runner.DetachKey)
	}
	full := runner.commandArgv(argv, mode)
	runner.trace(full)
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return runner.interactiveErr(cmd.Run())
}

// runDetachable runs argv like runInteractive, but reads the keys from in
// itself and stops the program on key. The program's output, standard error
// included, goes to out.
//
// The program runs on a pseudo-terminal of its own, which benchctl reads and
// writes, so it finds a terminal where it expects one: ipmitool and telnet put
// it into raw mode, and Ctrl+C reaches a plain cat as SIGINT. The local terminal
// goes into raw mode for the session, as ssh would put it if it read the
// terminal itself, so each key arrives as typed rather than a line at a time or
// as a signal to benchctl.
//
// The program leads a session of its own, so the stop reaches everything it
// started: a local shell's cat, which would otherwise hold the output open, or
// ssh behind sshpass, which passes SIGTERM on. ssh then drops the connection,
// and the bench's pty hangs up the program on the far side.
func (runner *CmdRunner) runDetachable(argv []string, mode ttyMode, out io.Writer, in *os.File, key byte) error {
	full := runner.commandArgv(argv, mode)
	runner.trace(full)
	cmd := exec.Command(full[0], full[1:]...)
	inFd := int(in.Fd())
	local := term.IsTerminal(inFd)
	if local {
		state, err := term.MakeRaw(inFd)
		if err != nil {
			return fmt.Errorf("terminal raw mode: %w", err)
		}
		defer func() { _ = term.Restore(inFd, state) }()
	}
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("%s: %w", shellJoin(argv), err)
	}
	defer ptmx.Close()
	if local {
		_ = pty.InheritSize(in, ptmx)
		defer followResize(in, ptmx)()
	}

	copied := make(chan struct{})
	go func() {
		// The read fails once the program and everything it started have closed
		// the terminal, which is the end of its output.
		_, _ = io.Copy(out, ptmx)
		close(copied)
	}()
	var detached atomic.Bool
	go func() {
		// The read in flight when the session ends has nowhere to go, and ends
		// with the process.
		if forwardUntil(ptmx, in, key) {
			detached.Store(true)
			stopProcessGroup(cmd.Process)
		}
	}()
	err = cmd.Wait()
	// A background process the program left behind can hold the terminal open,
	// so the output gets a moment to drain and then ends with the program.
	select {
	case <-copied:
	case <-time.After(outputDrain):
		ptmx.Close()
		<-copied
	}
	// The stop makes the program exit with a signal, and ssh with 255, which
	// would otherwise read as an unreachable bench.
	if detached.Load() {
		return nil
	}
	return runner.interactiveErr(err)
}

// outputDrain is how long runDetachable reads output after the program exits.
const outputDrain = 200 * time.Millisecond

// forwardUntil copies src to dst up to key, which it drops. It reports whether
// it stopped on key, rather than on the end of src or a failed write.
func forwardUntil(dst io.Writer, src io.Reader, key byte) bool {
	buf := make([]byte, 256)
	for {
		n, err := src.Read(buf)
		chunk := buf[:n]
		i := bytes.IndexByte(chunk, key)
		if i >= 0 {
			chunk = chunk[:i]
		}
		if len(chunk) > 0 {
			if _, werr := dst.Write(chunk); werr != nil {
				return false
			}
		}
		if i >= 0 {
			return true
		}
		if err != nil {
			return false
		}
	}
}

// interactiveErr classifies the result of an interactive session.
func (runner *CmdRunner) interactiveErr(err error) error {
	if err == nil {
		return nil
	}
	// ssh wrote its own diagnosis to the session's output, so there is nothing to
	// carry in Detail.
	if unreachable := runner.unreachable(err, ""); unreachable != nil {
		return unreachable
	}
	return err
}

func (runner *CmdRunner) StartPiped(argv ...string) (PipedProcess, error) {
	full := runner.commandArgv(argv, remoteTTYNoEscape)
	runner.trace(full)
	piped, err := StartPiped(full)
	if err != nil {
		return nil, err
	}
	return piped, nil
}

func (runner *CmdRunner) Stream(argv ...string) (io.ReadCloser, func() error, error) {
	full := runner.commandArgv(argv, noTTY)
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
			detail := strings.TrimSpace(stderr.String())
			if unreachable := runner.unreachable(err, detail); unreachable != nil {
				return unreachable
			}
			return fmt.Errorf("%s: %w: %s", shellJoin(argv), err, detail)
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
