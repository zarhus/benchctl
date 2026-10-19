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
	osexec "os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStreamReadsStdoutThenWaits(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "localhost"}}

	stdout, wait, err := runner.Stream("sh", "-c", "printf part1; printf part2")
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	data, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	if string(data) != "part1part2" {
		t.Errorf("stream = %q, want part1part2", data)
	}
	if err := wait(); err != nil {
		t.Errorf("wait() = %v, want nil", err)
	}
}

// TestRunInteractiveOutPreservesStdoutStderrOrder covers the reordering bug
// found live: a console session retrying a flaky telnet attach through
// RunInteractiveOut saw the process's own stderr diagnosis land ahead of an
// earlier stdout line, run together with no line break between them. Two
// independent, unsynchronized copying goroutines (Go's default when Stdout
// and Stderr are separate io.Writer-backed pipes) can service reads in
// either order regardless of which fd the child actually wrote to first.
// RunInteractiveOut instead shares one real pipe for both streams, so the
// child's own write(2) calls land in the exact order it issued them.
func TestRunInteractiveOutPreservesStdoutStderrOrder(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "localhost"}}
	var buf bytes.Buffer

	err := runner.RunInteractiveOut(&buf, "sh", "-c",
		"echo one; echo two >&2; echo three; echo four >&2")
	if err != nil {
		t.Fatalf("RunInteractiveOut: %v", err)
	}
	want := "one\ntwo\nthree\nfour\n"
	if buf.String() != want {
		t.Errorf("output = %q, want %q (stdout/stderr interleaved out of order)", buf.String(), want)
	}
}

func TestRunInteractiveOutSurfacesAFailure(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "localhost"}}
	var buf bytes.Buffer

	err := runner.RunInteractiveOut(&buf, "sh", "-c", "echo failing >&2; exit 1")
	if err == nil {
		t.Fatal("want an error from a failing command")
	}
	if !strings.Contains(buf.String(), "failing") {
		t.Errorf("output = %q, want it to still carry the process's own output", buf.String())
	}
}

func TestStreamWaitReportsExitError(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "localhost"}}

	stdout, wait, err := runner.Stream("sh", "-c", "echo boom >&2; exit 3")
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	_, _ = io.ReadAll(stdout)
	err = wait()
	if err == nil {
		t.Fatal("wait() = nil, want an exit error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("wait() error %q should include captured stderr", err)
	}
}

func TestExitCodeReadsThroughTheRunnerWrapping(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "localhost"}}

	_, err := runner.Run("sh", "-c", "exit 7")
	if err == nil {
		t.Fatal("Run of a failing command returned nil error")
	}
	code, ran := ExitCode(err)
	if !ran || code != 7 {
		t.Errorf("ExitCode = (%d, %v), want (7, true)", code, ran)
	}

	// An error that never came from a command has no exit status to report.
	if code, ran := ExitCode(errors.New("no command here")); ran || code != 0 {
		t.Errorf("ExitCode of a plain error = (%d, %v), want (0, false)", code, ran)
	}
}

func TestCommandArgvLocalIsUnchanged(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "localhost"}}
	argv := []string{"bench-tool", "--addr", "[::1]:12225", "power", "on"}

	got := runner.commandArgv(argv, noTTY)
	if !reflect.DeepEqual(got, argv) {
		t.Errorf("local commandArgv = %q, want unchanged %q", got, argv)
	}
}

func TestCommandArgvRemoteWrapsInSSH(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "bench.local", User: "root", Password: "root"}}
	argv := []string{"bench-tool", "console"}

	got := runner.commandArgv(argv, remoteTTY)
	want := sshArgv(runner.Target, argv, remoteTTY)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("remote commandArgv = %q, want %q", got, want)
	}
}

func TestHostReturnsTargetHost(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "bench.local"}}
	if got := runner.Host(); got != "bench.local" {
		t.Errorf("Host() = %q, want bench.local", got)
	}
}

// exitStatus returns the error a real process gives on exiting with status, so
// that the classification runs against the same *exec.ExitError it sees in
// production.
func exitStatus(t *testing.T, status int) error {
	t.Helper()
	err := osexec.Command("sh", "-c", fmt.Sprintf("exit %d", status)).Run()
	if err == nil {
		t.Fatalf("sh -c 'exit %d' succeeded", status)
	}
	return err
}

func TestUnreachableReportsAnSSHFailure(t *testing.T) {
	// ssh exits 255 when it cannot reach the host, before the bench command runs.
	runner := &CmdRunner{Target: Target{Host: "192.168.50.10", User: "root"}}
	err := runner.unreachable(exitStatus(t, 255), "ssh: connect to host 192.168.50.10 port 22: No route to host")
	var target *UnreachableError
	if !errors.As(err, &target) {
		t.Fatalf("unreachable(255) = %v, want an UnreachableError", err)
	}
	if target.Host != "192.168.50.10" {
		t.Errorf("Host = %q, want the bench host", target.Host)
	}
	if !strings.Contains(target.Error(), "No route to host") {
		t.Errorf("error %q should carry what ssh reported", target)
	}
}

func TestUnreachablePassesOverACommandFailure(t *testing.T) {
	// ssh passes the remote command's own status through, which is the command's
	// failure and not the bench's.
	runner := &CmdRunner{Target: Target{Host: "192.168.50.10", User: "root"}}
	if err := runner.unreachable(exitStatus(t, 1), "ipmitool: no response"); err != nil {
		t.Errorf("unreachable(1) = %v, want nil", err)
	}
}

func TestUnreachableIgnoresALocalTarget(t *testing.T) {
	// Run on the bench there is no ssh in the way, so 255 is the command's own.
	runner := &CmdRunner{Target: Target{Host: "localhost"}}
	if err := runner.unreachable(exitStatus(t, 255), ""); err != nil {
		t.Errorf("unreachable(255) on a local target = %v, want nil", err)
	}
}

func TestUnreachableErrorNamesTheBenchWithoutDetail(t *testing.T) {
	// An interactive command's ssh output goes to the terminal, so the message has
	// to stand on the host name alone.
	err := &UnreachableError{Host: "192.168.50.10", Status: exitStatus(t, 255)}
	if !strings.Contains(err.Error(), "192.168.50.10") || !strings.Contains(err.Error(), "ssh") {
		t.Errorf("error = %q, want it to name the bench and blame ssh", err)
	}
}

func TestPushLocalReturnsSamePath(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "localhost"}}

	remote, cleanup, err := runner.Push("/local/fw.bin", "/data/rom.bin")
	if err != nil {
		t.Fatalf("Push returned error: %v", err)
	}
	if remote != "/local/fw.bin" {
		t.Errorf("local Push remote = %q, want the local path unchanged", remote)
	}
	// Cleanup must be a no-op that succeeds: nothing was copied.
	if err := cleanup(); err != nil {
		t.Errorf("local Push cleanup returned error: %v", err)
	}
}

func TestPullLocalReturnsSamePath(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "localhost"}}

	local, fetch, err := runner.Pull("/local/dump.bin", "/data/readback.bin")
	if err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}
	if local != "/local/dump.bin" {
		t.Errorf("local Pull path = %q, want the local path unchanged", local)
	}
	// Fetch must be a no-op that succeeds: nothing was copied.
	if err := fetch(); err != nil {
		t.Errorf("local Pull fetch returned error: %v", err)
	}
}

func TestForwardUntilDropsTheKeyAndStops(t *testing.T) {
	var dst bytes.Buffer
	if !forwardUntil(&dst, strings.NewReader("ls\r\x1dquit\r"), 0x1D) {
		t.Fatal("forwardUntil did not report the key")
	}
	if dst.String() != "ls\r" {
		t.Errorf("forwarded %q, want the keys before the detach key only", dst.String())
	}
}

func TestForwardUntilReportsTheEndOfInput(t *testing.T) {
	var dst bytes.Buffer
	if forwardUntil(&dst, strings.NewReader("ls\r"), 0x1D) {
		t.Error("forwardUntil reported the key at the end of input")
	}
	if dst.String() != "ls\r" {
		t.Errorf("forwarded %q, want all of it", dst.String())
	}
}

// detachable runs argv locally under runDetachable, with keys read from a pipe
// the test writes, and returns its result and output once it ends.
func detachable(t *testing.T, keys string, argv ...string) (string, error) {
	t.Helper()
	in, keyboard, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); keyboard.Close() })
	runner := &CmdRunner{Target: Target{Host: "localhost"}}
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- runner.runDetachable(argv, remoteTTYNoEscape, &out, in, 0x1D) }()
	if _, err := keyboard.WriteString(keys); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(5 * time.Second):
		t.Fatalf("%q did not end after keys %q", argv, keys)
		return "", nil
	}
}

func TestRunDetachableStopsOnTheKey(t *testing.T) {
	// The shell waits on a sleep that holds the output open. Stopping the shell
	// alone would leave the sleep behind and the session waiting on it, as a
	// local uart1 attach would on its cat.
	_, err := detachable(t, "\x1d", "sh", "-c", "sleep 30; true")
	if err != nil {
		t.Errorf("runDetachable = %v, want nil for a session ended by the detach key", err)
	}
}

func TestRunDetachablePassesOtherKeysThrough(t *testing.T) {
	// The terminal is in its default line mode, so Enter ends the line.
	out, err := detachable(t, "abc\r", "sh", "-c", `read -r line; echo "read:$line"`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "read:abc") {
		t.Errorf("output %q, want the program to read the keys typed", out)
	}
}

func TestRunDetachableGivesTheProgramATerminal(t *testing.T) {
	// ipmitool and telnet put their standard input into raw mode, which needs a
	// terminal there.
	out, err := detachable(t, "", "sh", "-c", "test -t 0 && test -t 1 && echo terminal")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "terminal") {
		t.Errorf("output %q, want the program to find a terminal", out)
	}
}

func TestRunDetachableTurnsCtrlCIntoSIGINT(t *testing.T) {
	// A plain cat ends on Ctrl+C, as it does in a shell.
	_, err := detachable(t, "\x03", "cat")
	if err == nil || !strings.Contains(err.Error(), "interrupt") {
		t.Errorf("runDetachable = %v, want cat ended by SIGINT", err)
	}
}

func TestRunDetachableReportsTheProgramsOwnFailure(t *testing.T) {
	_, err := detachable(t, "", "sh", "-c", "exit 3")
	if code, ran := ExitCode(err); !ran || code != 3 {
		t.Errorf("runDetachable = %v, want exit status 3", err)
	}
}

func TestRunInteractiveOutKeepsTheOrderWithADetachKey(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "localhost"}, DetachKey: DefaultDetachKey}
	var buf bytes.Buffer

	err := runner.RunInteractiveOut(&buf, "sh", "-c", "echo one; echo two >&2; echo three")
	if err != nil {
		t.Fatalf("RunInteractiveOut: %v", err)
	}
	// The terminal ends each line with CR LF, as on a screen.
	if want := "one\r\ntwo\r\nthree\r\n"; buf.String() != want {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}
}

func TestDetachHintNamesTheKey(t *testing.T) {
	runner := &CmdRunner{DetachKey: DefaultDetachKey}
	if got := runner.DetachHint(); got != "Ctrl+]" {
		t.Errorf("DetachHint = %q, want Ctrl+]", got)
	}
	runner.DetachKey = 0
	if got := runner.DetachHint(); got != "" {
		t.Errorf("DetachHint with no key = %q, want empty", got)
	}
}

// syncBuffer is a bytes.Buffer safe to read while a command writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestStartPipedCarriesInputAndOutput(t *testing.T) {
	piped, err := StartPiped([]string{"sh", "-c", `test -t 0 && read -r line && echo "read:$line"`})
	if err != nil {
		t.Fatal(err)
	}
	// The terminal is in its default line mode, so Enter ends the line.
	if _, err := piped.Write([]byte("ping\r")); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(piped)
	if err != nil || !strings.Contains(string(out), "read:ping") {
		t.Errorf("read %q, %v, want the program to read ping from a terminal", out, err)
	}
	if err := piped.Wait(); err != nil {
		t.Errorf("Wait = %v", err)
	}
}

func TestStartPipedStopReachesChildren(t *testing.T) {
	// The shell waits on a sleep that holds the output open, as sshpass holds
	// ssh.
	piped, err := StartPiped([]string{"sh", "-c", "sleep 30; true"})
	if err != nil {
		t.Fatal(err)
	}
	piped.Stop()
	done := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(piped)
		_ = piped.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop left the program running")
	}
}

func TestStartPipedReadEndsWhenTheProgramLeavesAProcessBehind(t *testing.T) {
	// The sleep ignores the hangup the shell's exit sends, and holds the
	// terminal open after the shell exits.
	piped, err := StartPiped([]string{"sh", "-c", "trap \"\" HUP TERM; sleep 3 & echo started"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte)
	go func() {
		out, _ := io.ReadAll(piped)
		done <- out
	}()
	select {
	case out := <-done:
		if !strings.Contains(string(out), "started") {
			t.Errorf("read %q, want the program's output", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not end after the program exited")
	}
	if err := piped.Wait(); err != nil {
		t.Errorf("Wait = %v", err)
	}
}

func TestStartPipedTerminalStaysOutOfLaterPrograms(t *testing.T) {
	piped, err := StartPiped([]string{"sleep", "30"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		piped.Stop()
		_, _ = io.ReadAll(piped)
		_ = piped.Wait()
	}()
	out, err := osexec.Command("ls", "-l", "/proc/self/fd").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "ptmx") {
		t.Errorf("a later program holds the terminal:\n%s", out)
	}
}

func TestCmdRunnerStartPipedRunsLocally(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "localhost"}}
	piped, err := runner.StartPiped("sh", "-c", "test -t 0 && echo on-a-terminal")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(piped)
	if err := piped.Wait(); err != nil || !strings.Contains(string(out), "on-a-terminal") {
		t.Errorf("read %q, Wait = %v, want the program run locally on a terminal", out, err)
	}
}
