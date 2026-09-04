// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"strings"
	"testing"

	"github.com/zarhus/benchctl/platform"
)

func TestResolveHostFlagWins(t *testing.T) {
	t.Setenv("BENCHCTL_HOST", "from-env")
	got, err := resolveHost("from-flag")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-flag" {
		t.Errorf("resolveHost = %q, want from-flag", got)
	}
}

func TestResolveHostEnvFallback(t *testing.T) {
	t.Setenv("BENCHCTL_HOST", "bench.local")
	got, err := resolveHost("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "bench.local" {
		t.Errorf("resolveHost = %q, want bench.local", got)
	}
}

func TestResolveHostErrorsWhenUnset(t *testing.T) {
	t.Setenv("BENCHCTL_HOST", "")
	_, err := resolveHost("")
	if err == nil {
		t.Fatal("resolveHost should error when neither flag nor env is set")
	}
	if !strings.Contains(err.Error(), "--host") {
		t.Errorf("error %q should mention --host", err)
	}
}

func TestResolvePlatformUsesRegistryDefault(t *testing.T) {
	t.Setenv("BENCHCTL_PLATFORM", "")
	platform.SetDefault("default-from-main")
	defer platform.SetDefault("")
	if got := resolvePlatform(""); got != "default-from-main" {
		t.Errorf("resolvePlatform = %q, want default-from-main", got)
	}
}

func TestResolvePlatformEmptyWithoutDefault(t *testing.T) {
	t.Setenv("BENCHCTL_PLATFORM", "")
	platform.SetDefault("")
	if got := resolvePlatform(""); got != "" {
		t.Errorf("resolvePlatform = %q, want empty when no default is set", got)
	}
}

func TestResolvePlatformEnvFallback(t *testing.T) {
	t.Setenv("BENCHCTL_PLATFORM", "benchrack")
	if got := resolvePlatform(""); got != "benchrack" {
		t.Errorf("resolvePlatform = %q, want benchrack", got)
	}
}

func TestResolvePasswordDriverDefault(t *testing.T) {
	t.Setenv("BENCHCTL_PASSWORD", "")
	if got := resolvePassword("", "meta-rte"); got != "meta-rte" {
		t.Errorf("resolvePassword = %q, want meta-rte", got)
	}
}

func TestResolvePasswordFlagWins(t *testing.T) {
	t.Setenv("BENCHCTL_PASSWORD", "from-env")
	if got := resolvePassword("from-flag", "root"); got != "from-flag" {
		t.Errorf("resolvePassword = %q, want from-flag", got)
	}
}

func TestResolveBoardFlagWins(t *testing.T) {
	t.Setenv("BENCHCTL_BOARD", "from-env")
	if got := resolveBoard("from-flag"); got != "from-flag" {
		t.Errorf("resolveBoard = %q, want from-flag", got)
	}
}

func TestResolveBoardEnvFallback(t *testing.T) {
	t.Setenv("BENCHCTL_BOARD", "asrock-turin")
	if got := resolveBoard(""); got != "asrock-turin" {
		t.Errorf("resolveBoard = %q, want asrock-turin", got)
	}
}

func TestResolveBoardEmptyWhenUnset(t *testing.T) {
	t.Setenv("BENCHCTL_BOARD", "")
	if got := resolveBoard(""); got != "" {
		t.Errorf("resolveBoard = %q, want empty so the driver picks its default", got)
	}
}

func TestResolveBMCIPFlagWins(t *testing.T) {
	t.Setenv("BENCHCTL_BMC_IP", "from-env")
	if got := resolveBMCIP("from-flag"); got != "from-flag" {
		t.Errorf("resolveBMCIP = %q, want from-flag", got)
	}
}

func TestResolveBMCIPEnvFallback(t *testing.T) {
	t.Setenv("BENCHCTL_BMC_IP", "192.168.50.11")
	if got := resolveBMCIP(""); got != "192.168.50.11" {
		t.Errorf("resolveBMCIP = %q, want 192.168.50.11", got)
	}
}

func TestResolveBMCIPEmptyWhenUnset(t *testing.T) {
	t.Setenv("BENCHCTL_BMC_IP", "")
	if got := resolveBMCIP(""); got != "" {
		t.Errorf("resolveBMCIP = %q, want empty when neither flag nor env is set", got)
	}
}

func TestBuildUnknownPlatformErrors(t *testing.T) {
	_, err := build(options{host: "bench.local", platform: "no-such"})
	if err == nil || !strings.Contains(err.Error(), "no-such") {
		t.Fatalf("build with unknown platform should error, got %v", err)
	}
}

func TestBuildNoPlatformSelectedErrors(t *testing.T) {
	t.Setenv("BENCHCTL_PLATFORM", "")
	platform.SetDefault("")
	_, err := build(options{host: "bench.local"})
	if err == nil || !strings.Contains(err.Error(), "--platform") {
		t.Fatalf("build with no platform selected should point at --platform, got %v", err)
	}
}

func TestBuildBenchrackSucceeds(t *testing.T) {
	driver, err := build(options{host: "bench.local", platform: "benchrack"})
	if err != nil {
		t.Fatal(err)
	}
	if driver == nil {
		t.Fatal("build returned a nil platform")
	}
}
