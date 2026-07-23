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
}

func (fake *fakePlatform) PowerState() (platform.PowerStatus, error) { return fake.state, nil }
func (fake *fakePlatform) SetPower(platform.Power) error             { return nil }
func (fake *fakePlatform) PowerReset() error                         { fake.softReset = true; return nil }
func (fake *fakePlatform) HardReset() error                          { fake.hardReset = true; return nil }
func (fake *fakePlatform) Console() error                            { return nil }
func (fake *fakePlatform) FlashProbe(platform.FlashTarget) error     { return fake.flashErr }
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

func TestCommandTreeWired(t *testing.T) {
	root := newRootCmd()
	want := map[string][]string{
		"power": {"on", "off", "status", "reset"},
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
