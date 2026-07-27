// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package ipmi

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// fakeRunner is a test Runner. It records the argv of each call and which
// interactive variant ran, and returns canned errors.
type fakeRunner struct {
	run            []string
	interactive    []string
	noEscape       bool
	runErr         error
	interactiveErr error
}

func (f *fakeRunner) Run(argv ...string) (string, error) {
	f.run = argv
	return "", f.runErr
}

func (f *fakeRunner) RunInteractive(argv ...string) error {
	f.interactive = argv
	return f.interactiveErr
}

func (f *fakeRunner) RunInteractiveNoEscape(argv ...string) error {
	f.interactive = argv
	f.noEscape = true
	return f.interactiveErr
}

func (f *fakeRunner) Stream(argv ...string) (io.ReadCloser, func() error, error) {
	return io.NopCloser(strings.NewReader("")), func() error { return nil }, nil
}

func (f *fakeRunner) Push(localPath, remotePath string) (string, func() error, error) {
	return remotePath, func() error { return nil }, nil
}

func (f *fakeRunner) Pull(localPath, remotePath string) (string, func() error, error) {
	return remotePath, func() error { return nil }, nil
}

func (f *fakeRunner) Host() string { return "" }

func TestSOLActivateArgv(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, "192.168.50.11", "admin", "Administrator").SOLActivate(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ipmitool", "-I", "lanplus",
		"-H", "192.168.50.11",
		"-U", "admin",
		"-P", "Administrator",
		"sol", "activate",
	}
	if !reflect.DeepEqual(runner.interactive, want) {
		t.Errorf("SOLActivate argv =\n %q\nwant\n %q", runner.interactive, want)
	}
}

func TestSOLActivateDisablesSSHEscape(t *testing.T) {
	// The session ends on "~.", so an ssh hop must not claim that sequence.
	runner := &fakeRunner{}
	if err := New(runner, "192.168.50.11", "admin", "Administrator").SOLActivate(); err != nil {
		t.Fatal(err)
	}
	if !runner.noEscape {
		t.Error("SOLActivate used the plain interactive path, want the escape-free one")
	}
}

func TestSOLActivateReportsFailure(t *testing.T) {
	runner := &fakeRunner{interactiveErr: errors.New("exit status 1")}
	err := New(runner, "192.168.50.11", "admin", "Administrator").SOLActivate()
	if err == nil {
		t.Fatal("SOLActivate should report an ipmitool failure")
	}
	if !strings.Contains(err.Error(), "192.168.50.11") {
		t.Errorf("error %q should name the BMC", err)
	}
}

func TestSOLDeactivateArgv(t *testing.T) {
	runner := &fakeRunner{}
	New(runner, "192.168.50.11", "admin", "Administrator").SOLDeactivate()
	want := []string{
		"ipmitool", "-I", "lanplus",
		"-H", "192.168.50.11",
		"-U", "admin",
		"-P", "Administrator",
		"sol", "deactivate",
	}
	if !reflect.DeepEqual(runner.run, want) {
		t.Errorf("SOLDeactivate argv =\n %q\nwant\n %q", runner.run, want)
	}
	// It must not hold the terminal: deactivate is a plain command.
	if runner.interactive != nil {
		t.Errorf("SOLDeactivate ran interactively: %q", runner.interactive)
	}
}

func TestSOLDeactivateIgnoresFailure(t *testing.T) {
	// With no payload open ipmitool exits non-zero, which is the ordinary state
	// before a fresh attach and must not stop the caller.
	runner := &fakeRunner{runErr: errors.New("Info: SOL payload already de-activated")}
	New(runner, "192.168.50.11", "admin", "Administrator").SOLDeactivate()
	if runner.run == nil {
		t.Error("SOLDeactivate did not run ipmitool")
	}
}

func TestUsesConfiguredCredentials(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, "10.1.2.3", "operator", "s3cret").SOLActivate(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.interactive, " ")
	for _, want := range []string{"-H 10.1.2.3", "-U operator", "-P s3cret"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv = %q, want %q", got, want)
		}
	}
}
