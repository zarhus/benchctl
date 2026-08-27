// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package ipmi

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSOLActivateArgv(t *testing.T) {
	want := []string{
		"ipmitool", "-I", "lanplus",
		"-H", "192.168.50.11",
		"-U", "admin",
		"-P", "Administrator",
		"sol", "activate",
	}
	got := New("192.168.50.11", "admin", "Administrator").SOLActivate()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SOLActivate =\n %q\nwant\n %q", got, want)
	}
}

func TestSOLDeactivateArgv(t *testing.T) {
	want := []string{
		"ipmitool", "-I", "lanplus",
		"-H", "192.168.50.11",
		"-U", "admin",
		"-P", "Administrator",
		"-R", "2",
		"sol", "deactivate",
	}
	got := New("192.168.50.11", "admin", "Administrator").SOLDeactivate()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SOLDeactivate =\n %q\nwant\n %q", got, want)
	}
}

func TestUsesConfiguredCredentials(t *testing.T) {
	got := strings.Join(New("10.1.2.3", "operator", "s3cret").SOLActivate(), " ")
	for _, want := range []string{"-H 10.1.2.3", "-U operator", "-P s3cret"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv = %q, want %q", got, want)
		}
	}
}

func TestSOLActivateKeepsTheDefaultRetries(t *testing.T) {
	// The attach only runs once the deactivate has reached the BMC, so it keeps
	// ipmitool's retries for the session it holds afterwards.
	if got := strings.Join(New("192.168.50.11", "admin", "Administrator").SOLActivate(), " "); strings.Contains(got, "-R") {
		t.Errorf("SOLActivate = %q, want no retry override", got)
	}
}

func TestUnreachableReportsARefusedSession(t *testing.T) {
	// ipmitool writes this to standard error, which the runner folds into the
	// error it returns.
	client := New("192.168.10.192", "admin", "Administrator")
	err := client.Unreachable("", errors.New("ipmitool ...: exit status 1: Error: Unable to establish IPMI v2 / RMCP+ session"))
	if err == nil {
		t.Fatal("Unreachable should report a BMC that did not answer")
	}
	for _, want := range []string{"192.168.10.192", "admin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestUnreachableReadsEitherStream(t *testing.T) {
	client := New("192.168.10.192", "admin", "Administrator")
	if err := client.Unreachable("Error: Unable to establish IPMI v2 / RMCP+ session", errors.New("exit status 1")); err == nil {
		t.Error("Unreachable should also read the command's output")
	}
}

func TestUnreachablePassesOverAnAlreadyClosedPayload(t *testing.T) {
	// A BMC that answers still exits non-zero when no payload is open, which is
	// the ordinary state before a fresh attach and must not stop the attach.
	client := New("192.168.10.192", "admin", "Administrator")
	err := client.Unreachable("", errors.New("ipmitool ...: exit status 1: Info: SOL payload already de-activated"))
	if err != nil {
		t.Errorf("Unreachable = %v, want nil for a BMC that answered", err)
	}
}

func TestUnreachablePassesOverSuccess(t *testing.T) {
	client := New("192.168.10.192", "admin", "Administrator")
	if err := client.Unreachable("", nil); err != nil {
		t.Errorf("Unreachable = %v, want nil when the command succeeded", err)
	}
}

func TestSubcommandsShareOneConnection(t *testing.T) {
	// Both commands must reach the same BMC the same way, or the deactivate that
	// precedes an attach clears a payload on some other connection.
	const connection = 9 // ipmitool -I lanplus -H host -U user -P password
	client := New("192.168.50.11", "admin", "Administrator")
	activate, deactivate := client.SOLActivate(), client.SOLDeactivate()
	if !reflect.DeepEqual(activate[:connection], deactivate[:connection]) {
		t.Errorf("connection arguments differ:\n %q\n %q", activate[:connection], deactivate[:connection])
	}
}
