// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"errors"
	"strings"
	"testing"

	"github.com/zarhus/benchctl/exec"
)

// testSession is a console with a release command, standing in for any of the
// real ones.
func testSession() ConsoleSession {
	return ConsoleSession{
		What:    "host console on sled 0",
		Detach:  "CTRL+A CTRL+X",
		Attach:  []string{"gateway-cli", "usart-attach", "0"},
		Release: []string{"gateway-cli", "usart-detach", "0"},
	}
}

// sequence renders the runner's calls, in order, one line per call.
func sequence(runner *fakeRunner) string {
	lines := make([]string, 0, len(runner.sequence))
	for _, argv := range runner.sequence {
		lines = append(lines, strings.Join(argv, " "))
	}
	return strings.Join(lines, "\n")
}

func TestConsoleSessionAnnouncesTheDetachSequence(t *testing.T) {
	// The operator reads the detach sequence off the line printed as the terminal
	// is handed over, so it has to name both the console and the way out.
	runner := &fakeRunner{}
	progress := &strings.Builder{}
	if err := testSession().Run(runner, progress); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"host console on sled 0", "CTRL+A CTRL+X"} {
		if !strings.Contains(progress.String(), want) {
			t.Errorf("progress = %q, want it to mention %q", progress, want)
		}
	}
}

func TestConsoleSessionReleasesAroundTheAttach(t *testing.T) {
	runner := &fakeRunner{}
	if err := testSession().Run(runner, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"gateway-cli usart-detach 0", // a stale attachment would fail the attach
		"gateway-cli usart-attach 0",
		"gateway-cli usart-detach 0", // leave the console free
	}, "\n")
	if got := sequence(runner); got != want {
		t.Errorf("calls =\n%s\nwant\n%s", got, want)
	}
}

func TestConsoleSessionReleasesAfterAFailedAttach(t *testing.T) {
	// The attach can fail with the console held, so the release still has to run.
	runner := &fakeRunner{interactiveErr: errors.New("exit status 1")}
	if err := testSession().Run(runner, &strings.Builder{}); err == nil {
		t.Fatal("Run should report a failed attach")
	}
	if len(runner.calls) != 2 {
		t.Errorf("release calls = %d, want 2 (before and after)\n%s", len(runner.calls), sequence(runner))
	}
}

func TestConsoleSessionWrapsFailureWithTheConsoleName(t *testing.T) {
	runner := &fakeRunner{interactiveErr: errors.New("exit status 1")}
	err := testSession().Run(runner, &strings.Builder{})
	if err == nil {
		t.Fatal("Run should report a failed attach")
	}
	if !strings.Contains(err.Error(), "host console on sled 0") {
		t.Errorf("error %q should name the console", err)
	}
}

func TestConsoleSessionIgnoresAFailedRelease(t *testing.T) {
	// With nothing attached the release command reports failure, which is the
	// ordinary state before a fresh attach.
	runner := &fakeRunner{handler: func(int, []string) (string, error) {
		return "", errors.New("no session to detach")
	}}
	if err := testSession().Run(runner, &strings.Builder{}); err != nil {
		t.Fatalf("Run should ignore a failed release: %v", err)
	}
	if len(runner.interactive) != 1 {
		t.Error("Run skipped the attach after a failed release")
	}
}

func TestConsoleSessionStopsWhenTheBenchIsUnreachable(t *testing.T) {
	// The release is the first command to reach the bench. When it cannot, the
	// attach would fail the same way, with ssh writing its own diagnosis to the
	// terminal under a line announcing the console.
	runner := &fakeRunner{handler: func(int, []string) (string, error) {
		return "", &exec.UnreachableError{Host: "192.168.50.10", Detail: "ssh: connect to host 192.168.50.10 port 22: No route to host"}
	}}
	err := testSession().Run(runner, &strings.Builder{})
	if err == nil {
		t.Fatal("Run should report an unreachable bench")
	}
	if len(runner.interactive) != 0 {
		t.Error("Run attached to the console with the bench unreachable")
	}
	if !strings.Contains(err.Error(), "192.168.50.10") {
		t.Errorf("error %q should name the bench", err)
	}
}

func TestConsoleSessionDoesNotBlameTheConsoleForAnUnreachableBench(t *testing.T) {
	// A console with nothing to release reaches the bench for the first time in
	// the attach, and the console name would point at the wrong host.
	runner := &fakeRunner{interactiveErr: &exec.UnreachableError{Host: "192.168.50.10", Status: errors.New("exit status 255")}}
	session := testSession()
	session.Release = nil
	err := session.Run(runner, &strings.Builder{})
	if err == nil {
		t.Fatal("Run should report an unreachable bench")
	}
	if strings.Contains(err.Error(), session.What) {
		t.Errorf("error %q blames the console for an unreachable bench", err)
	}
	var target *exec.UnreachableError
	if !errors.As(err, &target) {
		t.Errorf("error %q should stay an UnreachableError", err)
	}
}

func TestConsoleSessionWithoutReleaseOnlyAttaches(t *testing.T) {
	// A console reached by a tool with no detach command, such as telnet.
	runner := &fakeRunner{}
	session := ConsoleSession{
		What:   "host console via telnet on port 13541",
		Detach: `CTRL+] then "quit"`,
		Attach: []string{"telnet", "localhost", "13541"},
	}
	if err := session.Run(runner, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if want := "telnet localhost 13541"; sequence(runner) != want {
		t.Errorf("calls =\n%s\nwant\n%s", sequence(runner), want)
	}
}

func TestConsoleSessionKeepsTheSSHEscapeByDefault(t *testing.T) {
	runner := &fakeRunner{}
	if err := testSession().Run(runner, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if runner.noEscape[0] {
		t.Error("Run disabled the ssh escape for a console that does not need it")
	}
}

func TestConsoleSessionDisablesTheSSHEscapeOnRequest(t *testing.T) {
	runner := &fakeRunner{}
	session := testSession()
	session.NoEscape = true
	if err := session.Run(runner, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if !runner.noEscape[0] {
		t.Error("Run attached with the ssh escape character live despite NoEscape")
	}
}
