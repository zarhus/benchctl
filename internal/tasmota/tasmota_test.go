// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package tasmota

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeRunner is a test Runner. It returns canned output for Run and records the
// argv it was given. The other Runner methods are unused here.
type fakeRunner struct {
	out  string
	err  error
	argv []string
}

func (f *fakeRunner) Run(argv ...string) (string, error) {
	f.argv = argv
	return f.out, f.err
}

func (f *fakeRunner) RunInteractive(argv ...string) error { return nil }

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

// argvString joins the recorded argv for substring assertions.
func (f *fakeRunner) argvString() string { return strings.Join(f.argv, " ") }

func TestPowerReadsState(t *testing.T) {
	runner := &fakeRunner{out: `{"POWER":"ON"}`}
	on, err := New(runner, "192.168.66.50").Power()
	if err != nil {
		t.Fatal(err)
	}
	if !on {
		t.Error("Power = off, want on")
	}
	got := runner.argvString()
	if !strings.Contains(got, "curl") || !strings.Contains(got, "cmnd=Power") {
		t.Errorf("argv = %q, want a curl of cmnd=Power", got)
	}
	// A read must not switch anything.
	if strings.Contains(got, "Power%20") {
		t.Errorf("argv = %q, a state read should send bare Power, not Power ON/OFF", got)
	}
}

func TestSetPowerSendsOnAndOff(t *testing.T) {
	cases := []struct {
		on   bool
		body string
		want string // encoded command that must appear in argv
	}{
		{true, `{"POWER":"ON"}`, "cmnd=Power%20ON"},
		{false, `{"POWER":"OFF"}`, "cmnd=Power%20OFF"},
	}
	for _, tc := range cases {
		runner := &fakeRunner{out: tc.body}
		got, err := New(runner, "192.168.66.50").SetPower(tc.on)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.on {
			t.Errorf("SetPower(%v) = %v, want %v", tc.on, got, tc.on)
		}
		if !strings.Contains(runner.argvString(), tc.want) {
			t.Errorf("SetPower(%v) argv = %q, want %q", tc.on, runner.argvString(), tc.want)
		}
	}
}

func TestCommandSendsReferer(t *testing.T) {
	// Without a Referer, recent Tasmota returns an empty reply; the client must
	// always send one matching the device.
	runner := &fakeRunner{out: `{"POWER":"OFF"}`}
	if _, err := New(runner, "192.168.66.50").Power(); err != nil {
		t.Fatal(err)
	}
	got := runner.argv
	refererIdx := -1
	for i, a := range got {
		if a == "-e" {
			refererIdx = i
			break
		}
	}
	if refererIdx < 0 || refererIdx+1 >= len(got) || got[refererIdx+1] != "http://192.168.66.50/" {
		t.Errorf("argv = %v, want -e http://192.168.66.50/ (Referer)", got)
	}
}

func TestUsesConfiguredIP(t *testing.T) {
	runner := &fakeRunner{out: `{"POWER":"OFF"}`}
	if _, err := New(runner, "10.1.2.3").Power(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runner.argvString(), "http://10.1.2.3/cm") {
		t.Errorf("argv = %q, want the configured IP 10.1.2.3", runner.argvString())
	}
}

func TestEmptyReplyIsAnError(t *testing.T) {
	// curl exits non-zero on the Referer-less empty reply; the Runner folds that
	// into an error, which the client must surface rather than parse "".
	runner := &fakeRunner{err: errors.New("curl: (52) Empty reply from server")}
	if _, err := New(runner, "192.168.66.50").Power(); err == nil {
		t.Fatal("Power should error when curl fails with an empty reply")
	}
}

func TestUnexpectedStateIsAnError(t *testing.T) {
	runner := &fakeRunner{out: `{"POWER":"MAYBE"}`}
	if _, err := New(runner, "192.168.66.50").Power(); err == nil {
		t.Fatal("Power should error on an unrecognized POWER value")
	}
}

func TestUnparseableReplyIsAnError(t *testing.T) {
	runner := &fakeRunner{out: `not json`}
	if _, err := New(runner, "192.168.66.50").Power(); err == nil {
		t.Fatal("Power should error when the reply is not JSON")
	}
}
