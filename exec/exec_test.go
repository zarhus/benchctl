// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package exec

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestTargetIsLocal(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", true},
		{"localhost", true},
		{"bench.local", false},
		{"10.0.0.5", false},
	}
	for _, tc := range cases {
		got := Target{Host: tc.host}.IsLocal()
		if got != tc.want {
			t.Errorf("Target{Host:%q}.IsLocal() = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestCheckDependenciesLocal(t *testing.T) {
	// A local target shells out to nothing, so the check passes even when the
	// remote programs are absent.
	for _, host := range []string{"", "localhost"} {
		if err := CheckDependencies(Target{Host: host}); err != nil {
			t.Errorf("CheckDependencies(Target{Host:%q}) = %v, want nil", host, err)
		}
	}
}

func TestCheckDependenciesMissing(t *testing.T) {
	// Point the check at programs that cannot exist on PATH and confirm a remote
	// target reports every one of them.
	orig := remoteDeps
	remoteDeps = []string{"benchctl-absent-a", "benchctl-absent-b"}
	defer func() { remoteDeps = orig }()

	err := CheckDependencies(Target{Host: "bench.local"})
	if err == nil {
		t.Fatal("CheckDependencies(remote) = nil, want error for missing programs")
	}
	for _, dep := range remoteDeps {
		if !strings.Contains(err.Error(), dep) {
			t.Errorf("error %q does not name missing program %q", err, dep)
		}
	}
}

func TestCheckDependenciesPresent(t *testing.T) {
	// A program that every host has stands in for the real dependencies: a
	// remote target passes when all listed programs resolve on PATH.
	orig := remoteDeps
	remoteDeps = []string{"sh"}
	defer func() { remoteDeps = orig }()

	if err := CheckDependencies(Target{Host: "bench.local"}); err != nil {
		t.Errorf("CheckDependencies(remote, deps present) = %v, want nil", err)
	}
}

func TestShellJoin(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"bench-tool", "power", "on"}, "bench-tool power on"},
		// A bracketed address contains shell globs and must be quoted as one word.
		{[]string{"bench-tool", "--addr", "[::1]:12225"}, "bench-tool --addr '[::1]:12225'"},
		// A path with spaces must be quoted as one word.
		{[]string{"rm", "/var/tmp/a b"}, "rm '/var/tmp/a b'"},
	}
	for _, tc := range cases {
		got := shellJoin(tc.argv)
		if got != tc.want {
			t.Errorf("shellJoin(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestSSHArgv(t *testing.T) {
	target := Target{Host: "bench.local", User: "root", Password: "root"}
	remote := []string{"bench-tool", "--addr", "[::1]:12225", "power", "on"}

	got := sshArgv(target, remote, noTTY)
	want := []string{
		"sshpass", "-p", "root", "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"root@bench.local",
		"bench-tool --addr '[::1]:12225' power on",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sshArgv() =\n %q\nwant\n %q", got, want)
	}
}

func TestSSHArgvTTY(t *testing.T) {
	target := Target{Host: "bench.local", User: "root", Password: "root"}
	got := sshArgv(target, []string{"bench-tool", "console"}, remoteTTY)

	// -tt must appear immediately after ssh to force a remote PTY.
	if len(got) < 5 || got[3] != "ssh" || got[4] != "-tt" {
		t.Errorf("sshArgv(remoteTTY) did not place -tt after ssh: %q", got)
	}
	// The default console escapes with CTRL+], so ssh keeps its own "~" escape.
	if slices.Contains(got, "-e") {
		t.Errorf("sshArgv(remoteTTY) disabled the ssh escape character: %q", got)
	}
}

func TestSSHArgvNoEscapeDisablesTilde(t *testing.T) {
	// An IPMI serial-over-LAN session ends on "~.", which ssh would otherwise take
	// as its own disconnect, leaving the SOL payload open on the BMC.
	target := Target{Host: "bench.local", User: "root", Password: "root"}
	got := sshArgv(target, []string{"ipmitool", "sol", "activate"}, remoteTTYNoEscape)

	if len(got) < 7 || got[3] != "ssh" || got[4] != "-tt" || got[5] != "-e" || got[6] != "none" {
		t.Errorf("sshArgv(remoteTTYNoEscape) = %q, want -tt -e none after ssh", got)
	}
}

func TestSSHArgvNoTTYHasNoEscapeOption(t *testing.T) {
	target := Target{Host: "bench.local", User: "root", Password: "root"}
	got := sshArgv(target, []string{"flashrom", "-w", "rom.bin"}, noTTY)

	if slices.Contains(got, "-tt") || slices.Contains(got, "-e") {
		t.Errorf("sshArgv(noTTY) = %q, want neither -tt nor -e", got)
	}
}

func TestScpArgv(t *testing.T) {
	target := Target{Host: "bench.local", User: "root", Password: "root"}
	got := scpArgv(target, "/local/fw.bin", "/var/tmp/benchctl-fw.bin")
	want := []string{
		"sshpass", "-p", "root", "scp",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"/local/fw.bin",
		"root@bench.local:/var/tmp/benchctl-fw.bin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scpArgv() =\n %q\nwant\n %q", got, want)
	}
}
