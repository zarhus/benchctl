// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package ipmi

import (
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

func TestSubcommandsShareOneConnection(t *testing.T) {
	// Both commands must reach the same BMC the same way, or the deactivate that
	// precedes an attach clears a payload on some other connection.
	client := New("192.168.50.11", "admin", "Administrator")
	activate, deactivate := client.SOLActivate(), client.SOLDeactivate()
	if !reflect.DeepEqual(activate[:len(activate)-2], deactivate[:len(deactivate)-2]) {
		t.Errorf("connection arguments differ:\n %q\n %q", activate, deactivate)
	}
}
