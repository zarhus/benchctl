// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/zarhus/benchctl/platform"
)

// fakePlatform records calls and returns canned results.
type fakePlatform struct {
	state     platform.PowerStatus
	status    platform.Status
	flashErr  error
	softReset bool
	hardReset bool
	acState   platform.PowerStatus
	acErr     error
	acSet     []platform.Power
	acCycled  bool
}

func (fake *fakePlatform) PowerState() (platform.PowerStatus, error) { return fake.state, nil }
func (fake *fakePlatform) SetPower(platform.Power) error             { return nil }
func (fake *fakePlatform) PowerReset() error                         { fake.softReset = true; return nil }
func (fake *fakePlatform) HardReset() error                          { fake.hardReset = true; return nil }
func (fake *fakePlatform) ACPowerState() (platform.PowerStatus, error) {
	return fake.acState, fake.acErr
}
func (fake *fakePlatform) SetACPower(p platform.Power) error {
	fake.acSet = append(fake.acSet, p)
	return fake.acErr
}
func (fake *fakePlatform) ACPowerCycle() error                   { fake.acCycled = true; return fake.acErr }
func (fake *fakePlatform) Console() error                        { return nil }
func (fake *fakePlatform) FlashProbe(platform.FlashTarget) error { return fake.flashErr }
func (fake *fakePlatform) FlashRead(platform.FlashTarget, string) error {
	return fake.flashErr
}
func (fake *fakePlatform) FlashWrite(platform.FlashTarget, string, bool) error {
	return fake.flashErr
}
func (fake *fakePlatform) FlashStatus(platform.FlashTarget) (platform.Status, error) {
	return fake.status, nil
}
func (fake *fakePlatform) FlashAbort(platform.FlashTarget) error { return nil }

func withFakePlatform(t *testing.T, driver platform.Platform) {
	t.Helper()
	old := buildPlatform
	buildPlatform = func(options) (platform.Platform, error) { return driver, nil }
	t.Cleanup(func() { buildPlatform = old })
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestPowerStatusPrintsState(t *testing.T) {
	withFakePlatform(t, &fakePlatform{state: platform.PowerStatus{Power: platform.PowerOn, Detail: "sample-state"}})
	out, err := run(t, "power", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "on") || !strings.Contains(out, "sample-state") {
		t.Errorf("power status output = %q, want the readable state and platform detail", out)
	}
}

func TestFlashStatusPrintsStateNotID(t *testing.T) {
	withFakePlatform(t, &fakePlatform{status: platform.Status{State: "complete", ID: "abc"}})
	out, err := run(t, "flash", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "complete") {
		t.Errorf("flash status output = %q, want the state", out)
	}
	if strings.Contains(out, "abc") {
		t.Errorf("flash status output = %q, should not print the id", out)
	}
}

func TestFlashWriteBMCPropagatesNotImplemented(t *testing.T) {
	withFakePlatform(t, &fakePlatform{flashErr: platform.ErrNotImplemented})
	_, err := run(t, "flash", "write", "bmc", "fw.bin")
	if !errors.Is(err, platform.ErrNotImplemented) {
		t.Errorf("flash write bmc error = %v, want ErrNotImplemented", err)
	}
}

func TestPowerResetRoutesSoftVersusHard(t *testing.T) {
	soft := &fakePlatform{}
	withFakePlatform(t, soft)
	if _, err := run(t, "power", "reset"); err != nil {
		t.Fatal(err)
	}
	if !soft.softReset || soft.hardReset {
		t.Errorf("power reset = soft %v hard %v, want soft only", soft.softReset, soft.hardReset)
	}

	hard := &fakePlatform{}
	withFakePlatform(t, hard)
	if _, err := run(t, "power", "reset", "--hard"); err != nil {
		t.Fatal(err)
	}
	if !hard.hardReset || hard.softReset {
		t.Errorf("power reset --hard = soft %v hard %v, want hard only", hard.softReset, hard.hardReset)
	}
}

func TestPowerACStatusPrintsState(t *testing.T) {
	withFakePlatform(t, &fakePlatform{acState: platform.PowerStatus{Power: platform.PowerOn}})
	out, err := run(t, "power", "ac", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "on") {
		t.Errorf("power ac status output = %q, want the AC state", out)
	}
}

func TestPowerACOnOffRoute(t *testing.T) {
	on := &fakePlatform{}
	withFakePlatform(t, on)
	if _, err := run(t, "power", "ac", "on"); err != nil {
		t.Fatal(err)
	}
	if len(on.acSet) != 1 || on.acSet[0] != platform.PowerOn {
		t.Errorf("power ac on set = %v, want [on]", on.acSet)
	}

	off := &fakePlatform{}
	withFakePlatform(t, off)
	if _, err := run(t, "power", "ac", "off"); err != nil {
		t.Fatal(err)
	}
	if len(off.acSet) != 1 || off.acSet[0] != platform.PowerOff {
		t.Errorf("power ac off set = %v, want [off]", off.acSet)
	}
}

func TestPowerACCycleRoutes(t *testing.T) {
	f := &fakePlatform{}
	withFakePlatform(t, f)
	if _, err := run(t, "power", "ac", "cycle"); err != nil {
		t.Fatal(err)
	}
	if !f.acCycled {
		t.Error("power ac cycle did not call ACPowerCycle")
	}
}

func TestPowerACPropagatesNotImplemented(t *testing.T) {
	withFakePlatform(t, &fakePlatform{acErr: platform.ErrNotImplemented})
	_, err := run(t, "power", "ac", "on")
	if !errors.Is(err, platform.ErrNotImplemented) {
		t.Errorf("power ac on error = %v, want ErrNotImplemented", err)
	}
}

func TestTasmotaIPFlagScope(t *testing.T) {
	// The flag belongs to the commands that reach the plug, power ac and the flash
	// commands that drive the bus, rather than to the global set.
	withFakePlatform(t, &fakePlatform{})
	for _, args := range [][]string{
		{"power", "ac", "status"},
		{"flash", "probe", "bmc"},
		{"flash", "read", "bmc", "dump.bin"},
		{"flash", "write", "bmc", "fw.bin"},
	} {
		if _, err := run(t, append(args, "--tasmota-ip", "10.0.0.1")...); err != nil {
			t.Errorf("%v --tasmota-ip should be accepted, got %v", args, err)
		}
	}
	if _, err := run(t, "power", "on", "--tasmota-ip", "10.0.0.1"); err == nil {
		t.Error("power on --tasmota-ip should be rejected as an unknown flag")
	}
}

func TestFlashPassesTasmotaIP(t *testing.T) {
	var got options
	old := buildPlatform
	buildPlatform = func(opts options) (platform.Platform, error) { got = opts; return &fakePlatform{}, nil }
	t.Cleanup(func() { buildPlatform = old })

	if _, err := run(t, "flash", "write", "bmc", "fw.bin", "--tasmota-ip", "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if got.tasmotaIP != "10.0.0.1" {
		t.Errorf("flash write tasmotaIP = %q, want 10.0.0.1", got.tasmotaIP)
	}

	// Without the flag the driver falls back to the board's plug.
	if _, err := run(t, "flash", "write", "bmc", "fw.bin"); err != nil {
		t.Fatal(err)
	}
	if got.tasmotaIP != "" {
		t.Errorf("flash write tasmotaIP = %q, want empty so the board default applies", got.tasmotaIP)
	}
}

func TestCommandTreeWired(t *testing.T) {
	root := newRootCmd()
	want := map[string][]string{
		"power": {"on", "off", "status", "reset", "ac"},
		"flash": {"probe", "read", "write", "status", "abort"},
	}
	for parent, subs := range want {
		parentCmd, _, err := root.Find([]string{parent})
		if err != nil || parentCmd.Name() != parent {
			t.Errorf("missing top-level command %q", parent)
			continue
		}
		for _, sub := range subs {
			subCmd, _, err := root.Find([]string{parent, sub})
			if err != nil || subCmd.Name() != sub {
				t.Errorf("missing command %q %q", parent, sub)
			}
		}
	}
	if _, _, err := root.Find([]string{"console"}); err != nil {
		t.Error("missing console command")
	}
}
