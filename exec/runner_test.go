// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package exec

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
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

	got := runner.commandArgv(argv, false)
	if !reflect.DeepEqual(got, argv) {
		t.Errorf("local commandArgv = %q, want unchanged %q", got, argv)
	}
}

func TestCommandArgvRemoteWrapsInSSH(t *testing.T) {
	runner := &CmdRunner{Target: Target{Host: "bench.local", User: "root", Password: "root"}}
	argv := []string{"bench-tool", "console"}

	got := runner.commandArgv(argv, true)
	want := sshArgv(runner.Target, argv, true)
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
