// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testPlatform sets up an fvpPlatform whose BENCHCTL_FVP_MODEL is
// modelScript, an executable file that stands in for FVP_RD_V3: it receives
// the same "-C key=value ... -I -R" argv the real model would, and is free
// to ignore all of it.
func testPlatform(t *testing.T, modelScript string) *fvpPlatform {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BENCHCTL_FVP_WORKDIR", filepath.Join(dir, "state"))
	t.Setenv("BENCHCTL_FVP_MODEL", modelScript)
	p := newFVP(nil, Config{})
	fv, ok := p.(*fvpPlatform)
	if !ok {
		t.Fatalf("newFVP returned %T, want *fvpPlatform", p)
	}
	// start() requires the full RESET_TO_BL31 firmware set for any image
	// that is not a FIP - stand in for a real firmware build so every test
	// that powers on does not have to set this up itself; the two tests that
	// specifically cover the missing-file case remove it again.
	populateOutputComponents(t, fv)
	// The fake models start, fail and stop in milliseconds, so the waits sized
	// for a real model only slow the tests down.
	fv.startGrace = 50 * time.Millisecond
	fv.shutdownGrace = 50 * time.Millisecond
	fv.pollEvery = 10 * time.Millisecond
	fv.norSize = 256 * 1024
	return fv
}

// populateOutputComponents writes stand-in files at every path
// resolveOutputComponents expects under fv.workDir's own output/<board>
// tree.
func populateOutputComponents(t *testing.T, fv *fvpPlatform) {
	t.Helper()
	comp := resolveOutputComponents(fv.workDir, fv.board)
	for _, f := range []string{
		comp.rseRom, comp.rseFlash, comp.rseVM0, comp.rseVM1,
		comp.scpRamfw, comp.mcpRamfw, comp.bl31, comp.rmm,
		comp.spmcManifest, comp.ntFwConfig, comp.stmm, comp.hafnium,
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("stand-in"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeImage(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("not a real BL33 image, just needs to exist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewFVPRequiresModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BENCHCTL_FVP_WORKDIR", dir)
	t.Setenv("BENCHCTL_FVP_MODEL", "")
	fv := newFVP(nil, Config{}).(*fvpPlatform)
	if fv.configErr == nil {
		t.Fatal("want an error when BENCHCTL_FVP_MODEL is unset")
	}
	if _, err := fv.PowerState(); err == nil {
		t.Fatal("PowerState should surface the config error")
	}
}

func TestNewFVPRequiresAnExistingModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BENCHCTL_FVP_WORKDIR", dir)
	t.Setenv("BENCHCTL_FVP_MODEL", filepath.Join(dir, "no-such-binary"))
	fv := newFVP(nil, Config{}).(*fvpPlatform)
	if fv.configErr == nil {
		t.Fatal("want an error when BENCHCTL_FVP_MODEL does not exist")
	}
}

func TestNewFVPRequiresAnExecutableModel(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(notExec, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BENCHCTL_FVP_WORKDIR", dir)
	t.Setenv("BENCHCTL_FVP_MODEL", notExec)
	fv := newFVP(nil, Config{}).(*fvpPlatform)
	if fv.configErr == nil {
		t.Fatal("want an error when BENCHCTL_FVP_MODEL is not executable")
	}
}

func TestUnknownBoard(t *testing.T) {
	fv := newFVP(nil, Config{Board: "nope"}).(*fvpPlatform)
	if fv.configErr == nil {
		t.Fatal("want an error for an unknown board")
	}
}

// TestPowerLifecycle covers the full AC/soft-power split: SetPower requires
// AC already on and never itself launches or kills the process (only
// starts/stops firmware execution, via Iris); SetACPower is what launches
// and kills it. A fake Iris server tracks "running" the same way the real
// simulationTime_run/stop/get calls would.
func TestPowerLifecycle(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "model.sh", `#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
	running := false
	runs := 0
	port := fakeIrisServer(t, func(method string, params json.RawMessage) (any, string) {
		switch method {
		case "instanceRegistry_registerInstance":
			return map[string]any{"instId": 1}, ""
		case "simulationTime_run":
			runs++
			running = true
			return nil, ""
		case "simulationTime_get":
			return map[string]any{"running": running}, ""
		default:
			return nil, "unexpected method " + method
		}
	})
	t.Setenv("BENCHCTL_FVP_IRIS_PORT", strconv.Itoa(port))
	fv := testPlatform(t, model)
	fv.runner = &fakeRunner{}
	image := writeImage(t, dir, "fip.bin")

	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}

	status, err := fv.PowerState()
	if err != nil || status.Power != PowerOff {
		t.Fatalf("PowerState = %v, %v, want off", status, err)
	}

	// SetPower alone must not launch anything: AC is still off.
	if err := fv.SetPower(PowerOn); err == nil {
		t.Fatal("SetPower(on) with AC off: want an error, got nil")
	} else if !strings.Contains(err.Error(), "power ac on") {
		t.Errorf("SetPower(on) with AC off error = %q, want it to point at 'power ac on'", err)
	}

	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetACPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	// AC on but not told to run yet: firmware execution is paused.
	status, err = fv.PowerState()
	if err != nil || status.Power != PowerOff {
		t.Fatalf("PowerState after AC on = %v, %v, want off (paused)", status, err)
	}
	pid, _ := readPID(fv.pidFile)

	if err := fv.SetPower(PowerOn); err != nil {
		t.Fatalf("SetPower(on): %v", err)
	}
	if runs != 1 {
		t.Fatalf("simulationTime_run calls after first SetPower(on) = %d, want 1", runs)
	}
	// startFirmware starts execution in place over Iris - it must NOT kill
	// and relaunch the process (see SetPower/PowerReset's own comments on
	// why: the model's own console-owning process must survive a `power
	// on` so an externally-held telnet session, like open-source-
	// firmware-validation's, is never broken by it).
	if pidAfterOn, ok := readPID(fv.pidFile); !ok || pidAfterOn != pid {
		t.Fatalf("pid after SetPower(on) = %v, want unchanged (%d): power on must not relaunch the process", pidAfterOn, pid)
	}
	status, err = fv.PowerState()
	if err != nil || status.Power != PowerOn {
		t.Fatalf("PowerState after on = %v, %v, want on", status, err)
	}

	// Idempotent: already on, so this must not error or run again.
	if err := fv.SetPower(PowerOn); err != nil {
		t.Fatalf("SetPower(on) again: %v", err)
	}
	if runs != 1 {
		t.Fatalf("simulationTime_run calls after redundant SetPower(on) = %d, want still 1", runs)
	}

	// PowerOff is not implemented (see SetPower's own doc comment) - the
	// only supported way to stop firmware execution is `power ac off`.
	if err := fv.SetPower(PowerOff); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("SetPower(off) = %v, want ErrNotImplemented", err)
	}
}

func TestPowerOnRequiresAFlashedImageFirst(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "model.sh", "#!/bin/sh\necho \"terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}\"\nwhile :; do sleep 0.05; done\n")
	fv := testPlatform(t, model)

	err := fv.SetACPower(PowerOn)
	if err == nil {
		t.Fatal("want an error powering on with nothing flashed")
	}
	if !strings.Contains(err.Error(), "flash write") {
		t.Errorf("error = %q, want it to point at 'flash write'", err)
	}
}

func TestStartFailureSurfacesProcessLogTail(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "crash.sh", `#!/bin/sh
echo "boom: bad image" >&2
echo "boom: bad image"
exit 1
`)
	fv := testPlatform(t, model)
	image := writeImage(t, dir, "fip.bin")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	err := fv.SetACPower(PowerOn)
	if err == nil {
		t.Fatal("want an error when the model exits immediately")
	}
	if !strings.Contains(err.Error(), "boom: bad image") {
		t.Errorf("error = %q, want it to include the model's own output", err)
	}
}

func TestStartCreatesTheDummyNORFlashImagesOnce(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "model.sh", `#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
	fv := testPlatform(t, model)
	image := writeImage(t, dir, "fip.bin")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	nor1 := filepath.Join(fv.workDir, "nor1_flash.img")
	nor2 := filepath.Join(fv.workDir, "nor2_flash.img")
	for _, path := range []string{nor1, nor2} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty, want a gzipped dummy image", path)
		}
	}
}

func TestStartPassesTheExpectedModelFlags(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "argv.txt")
	model := writeScript(t, dir, "model.sh", `#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}"
printf '%s\n' "$@" > "`+dump+`"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
	t.Setenv("BENCHCTL_FVP_CONSOLE_PORT", "5099") // set before construction: newFVP reads it once, at startup
	fv := testPlatform(t, model)
	image := writeImage(t, dir, "fip.bin")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	got, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	argv := string(got)
	for _, want := range []string{
		"css.ap_periph.terminal_ns_uart0.start_port=5099",
		"css.ap_periph.ns_uart0.out_file=" + fv.consoleLog,
		"ros.board.dram=" + image + "@0x60000000", // the BL33 preload - direct DRAM entry, not fetched from flashloader0
		"ros.board.flashloader1.fname=" + filepath.Join(fv.workDir, "nor1_flash.img"),
		"ros.board.flashloader2.fname=" + filepath.Join(fv.workDir, "nor2_flash.img"),
		"pcie_group_0.pcie4.hierarchy_file_name=<default>",
		"pcie_group_0.pcie4.pcie_rc.ahci0.endpoint.ats_supported=true",
		// Every UART terminal, ns_uart0 included, must be told not to
		// auto-open (an xterm, wherever a real display is attached, running
		// its own telnet client): confirmed against a real run, leaving
		// ns_uart0 at start_telnet=1 opens a competing xterm that claims the
		// terminal's single client slot before Console's own telnet can
		// attach - matches Arm's own reference launch script
		// (model-scripts/rdinfra/platforms/rdv3/run_model.sh), which sets
		// start_telnet=0 across the board, ns_uart0 included.
		"css.sysctrl.terminal_rse_uart.start_telnet=0",
		"css.sysctrl.scp.terminal_uart_scp.start_telnet=0",
		"css.sysctrl.scp.terminal_uart_lcp.start_telnet=0",
		"css.sysctrl.mcp.terminal_uart_mcp.start_telnet=0",
		"css.ap_periph.terminal_sec_uart.start_telnet=0",
		"css.ap_periph.terminal_ns_uart1.start_telnet=0",
		"css.ap_periph.terminal_ns_uart0.start_telnet=0",
		"-I",
		"--iris-port",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("model argv missing %q\ngot: %s", want, argv)
		}
	}
	// -R (--run) must be absent: SetACPower launches the model paused, not
	// running - see start's own doc comment. Checked as a whole line since
	// "-R" alone would also match inside "--iris-port" etc.
	for _, line := range strings.Split(strings.TrimRight(argv, "\n"), "\n") {
		if line == "-R" {
			t.Errorf("model argv includes -R, want the model launched paused (no auto-run)\ngot: %s", argv)
		}
	}
	// flashloader0 is the platform's own flash chip, not a FIP - it must
	// still be pointed at bootImage (a full flash-shaped payload like
	// coreboot.rom reads its own CBFS/FMAP back from there), see start's
	// own doc comment.
	if !strings.Contains(argv, "ros.board.flashloader0.fname="+image) {
		t.Errorf("model argv missing flashloader0 pointed at the boot image\ngot: %s", argv)
	}
}

// TestStartIncludesPCIeHierarchyFilesWhenPresentInWorkDir covers pcie_group_1
// through _3: unlike the RSE firmware, these are static example files that
// ship with model-scripts rather than anything a firmware build produces, so
// they are looked for in workDir - where an operator drops a copy, the same
// way the two NOR flash images live there - not beside the flashed image.
func TestStartIncludesPCIeHierarchyFilesWhenPresentInWorkDir(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "argv.txt")
	model := writeScript(t, dir, "model.sh", `#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}"
printf '%s\n' "$@" > "`+dump+`"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
	fv := testPlatform(t, model)
	image := writeImage(t, dir, "fip.bin")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	// FlashWrite (via SetPower(off), a no-op here) does not create workDir;
	// SetPower(on) below does, but only once start begins - write the
	// hierarchy files into it now, ahead of that, since MkdirAll is
	// idempotent and this only needs the directory to exist first.
	if err := os.MkdirAll(fv.workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		name := fmt.Sprintf("example_pcie_hierarchy_%d.json", n)
		if err := os.WriteFile(filepath.Join(fv.workDir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	got, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	argv := string(got)
	for n := 1; n <= 3; n++ {
		want := fmt.Sprintf("pcie_group_%d.pcie4.hierarchy_file_name=%s", n, filepath.Join(fv.workDir, fmt.Sprintf("example_pcie_hierarchy_%d.json", n)))
		if !strings.Contains(argv, want) {
			t.Errorf("model argv missing %q\ngot: %s", want, argv)
		}
	}
}

func TestStartOmitsPCIeHierarchyFilesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "argv.txt")
	model := writeScript(t, dir, "model.sh", `#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}"
printf '%s\n' "$@" > "`+dump+`"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
	fv := testPlatform(t, model)
	image := writeImage(t, dir, "fip.bin")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	got, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	if strings.Contains(string(got), "pcie_group_1.pcie4.hierarchy_file_name") {
		t.Errorf("model argv should not reference a pcie hierarchy file that does not exist, got: %s", got)
	}
}

// TestStartIncludesRequiredFirmwareAndBootImage covers the fix for the model
// staying alive and elaborating but never producing any console output:
// without RSE (Runtime Security Engine) - the root of the boot chain, ahead
// of everything else - and the RESET_TO_BL31 companions BL31 needs to reach
// BL33, there is no code to run at all. A real firmware build produces
// these files under its own output/<board> tree, which the operator mirrors
// under workDir (populateOutputComponents stands in for that here), so
// start looks for them there - never beside whatever `flash write host`
// recorded, which need not live under workDir at all. The BL33 image itself
// (that recorded path) is preloaded at the same fixed address regardless of
// whether it is a normal build's own uefi.bin or a coreboot.rom.
func TestStartIncludesRequiredFirmwareAndBootImage(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "argv.txt")
	model := writeScript(t, dir, "model.sh", `#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}"
printf '%s\n' "$@" > "`+dump+`"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
	fv := testPlatform(t, model) // populates the required firmware tree
	image := writeImage(t, dir, "coreboot.rom")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	got, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	argv := string(got)
	comp := resolveOutputComponents(fv.workDir, fv.board)
	for _, want := range []string{
		"css.sysctrl.rse.rom.raw_image=" + comp.rseRom,
		"css.sysctrl.rse.cpu=" + comp.rseFlash + "@0xB0000000",
		"css.sysctrl.rse.cpu=" + comp.rseVM0 + "@0x31000400",
		"css.sysctrl.rse.cpu=" + comp.rseVM1 + "@0x31080000",
		"css.sysctrl.scp.cpu=" + comp.scpRamfw + "@0x0",
		"css.sysctrl.mcp.cpu=" + comp.mcpRamfw + "@0x0",
		"css.ap_periph.arsm_sram=" + comp.bl31 + "@0x63000",
		"css.ap_periph.arsm_sram=" + comp.spmcManifest + "@0x1f500",
		"ros.board.dram=" + comp.ntFwConfig + "@0x73000000",
		"ros.board.dram=" + comp.hafnium + "@0x7a889000",
		"ros.board.dram=" + comp.stmm + "@0x7AA00000",
		"ros.board.dram=" + comp.rmm + "@0x7B200000",
		"ros.board.dram=" + image + "@0x60000000",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("model argv missing %q\ngot: %s", want, argv)
		}
	}
}

func TestParseFVPData(t *testing.T) {
	dir := t.TempDir()
	a := writeImage(t, dir, "a.bin")
	b := writeImage(t, dir, "b.bin")
	t.Chdir(dir)
	for _, tc := range []struct {
		name, value string
		want        []string
	}{
		{"empty", "", nil},
		{"one", "ros.board.dram=" + a + "@0x08000000", []string{"ros.board.dram=" + a + "@0x08000000"}},
		{"several", "ros.board.dram=" + a + "@0x1000,css.sysctrl.scp.cpu=" + b + "@16", []string{
			"ros.board.dram=" + a + "@0x1000",
			"css.sysctrl.scp.cpu=" + b + "@16",
		}},
		{"relative path", "ros.board.dram=b.bin@0x0", []string{"ros.board.dram=" + b + "@0x0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFVPData(tc.value)
			if err != nil {
				t.Fatalf("parseFVPData(%q): %v", tc.value, err)
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("parseFVPData(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestParseFVPDataSplitsAtTheLastAtAndTheFirstEquals(t *testing.T) {
	dir := t.TempDir()
	odd := writeImage(t, dir, "x=y@z.bin")
	got, err := parseFVPData("ros.board.dram=" + odd + "@0x10")
	if err != nil {
		t.Fatalf("parseFVPData: %v", err)
	}
	if want := "ros.board.dram=" + odd + "@0x10"; len(got) != 1 || got[0] != want {
		t.Errorf("got %q, want [%q]", got, want)
	}
}

func TestParseFVPDataRejectsMalformedEntries(t *testing.T) {
	dir := t.TempDir()
	a := writeImage(t, dir, "a.bin")
	for _, entry := range []string{
		"ros.board.dram=" + a,                     // no address
		"ros.board.dram" + a + "@0x0",             // no '='
		"=" + a + "@0x0",                          // empty target
		"ros.board.dram=@0x0",                     // empty file
		"ros.board.dram=" + a + "@",               // empty address
		"ros.board.dram=" + a + "@0xZZ",           // bad address
		"ros.board.dram=" + a + "@-1",             // negative address
		"ros.board.dram=" + dir + "/none.bin@0x0", // missing file
		"ros.board.dram=" + dir + "@0x0",          // directory
		"ros.board.dram=" + a + "@0x0,",           // trailing empty entry
	} {
		_, err := parseFVPData(entry)
		if err == nil {
			t.Errorf("parseFVPData(%q): want an error", entry)
			continue
		}
		if !strings.Contains(err.Error(), "BENCHCTL_FVP_DATA") {
			t.Errorf("parseFVPData(%q) error %q does not name BENCHCTL_FVP_DATA", entry, err)
		}
	}
}

func TestNewFVPRejectsBadFVPData(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "model.sh", "#!/bin/sh\n")
	t.Setenv("BENCHCTL_FVP_WORKDIR", dir)
	t.Setenv("BENCHCTL_FVP_MODEL", model)
	t.Setenv("BENCHCTL_FVP_DATA", "ros.board.dram="+filepath.Join(dir, "none.bin")+"@0x0")
	fv := newFVP(nil, Config{}).(*fvpPlatform)
	if fv.configErr == nil || !strings.Contains(fv.configErr.Error(), "BENCHCTL_FVP_DATA") {
		t.Fatalf("configErr = %v, want an error naming BENCHCTL_FVP_DATA", fv.configErr)
	}
	if _, err := fv.PowerState(); err == nil {
		t.Fatal("PowerState should surface the config error")
	}
}

// TestStartAppendsFVPDataAfterTheBuiltInPreloads covers both boot flows.
func TestStartAppendsFVPDataAfterTheBuiltInPreloads(t *testing.T) {
	for _, flow := range []string{"bl33", "fip"} {
		t.Run(flow, func(t *testing.T) {
			dir := t.TempDir()
			dump := filepath.Join(dir, "argv.txt")
			model := writeScript(t, dir, "model.sh", `#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}"
printf '%s\n' "$@" > "`+dump+`"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
			extra := writeImage(t, dir, "extra.bin")
			t.Setenv("BENCHCTL_FVP_DATA", "ros.board.dram="+extra+"@0x08000000")
			fv := testPlatform(t, model)
			var image, lastBuiltIn string
			if flow == "fip" {
				image = writeFIP(t, filepath.Join(dir, "build"))
				lastBuiltIn = "css.sysctrl.rse.cpu=" + resolveFIPComponents(filepath.Dir(image)).rseVM1 + "@0x31080000"
			} else {
				image = writeImage(t, dir, "uefi.bin")
				lastBuiltIn = "ros.board.dram=" + image + "@0x60000000"
			}
			if err := fv.FlashWrite(FlashHost, image, false); err != nil {
				t.Fatalf("FlashWrite: %v", err)
			}
			if err := fv.SetACPower(PowerOn); err != nil {
				t.Fatalf("SetACPower(on): %v", err)
			}
			t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

			got, err := os.ReadFile(dump)
			if err != nil {
				t.Fatalf("read recorded argv: %v", err)
			}
			args := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
			n := len(args)
			want := []string{lastBuiltIn, "--data", "ros.board.dram=" + extra + "@0x08000000"}
			if n < 3 || strings.Join(args[n-3:], "\n") != strings.Join(want, "\n") {
				t.Errorf("model argv ends with %q, want %q", args[max(n-3, 0):], want)
			}
		})
	}
}

// TestStartFailsWhenFirmwareMissing covers the other side of that fix: in
// the RESET_TO_BL31 flow a missing companion is a configuration error
// reported before the model is even launched, not a degraded-but-alive
// mode.
func TestStartFailsWhenFirmwareMissing(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "model.sh", `#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
	fv := testPlatform(t, model)
	if err := os.RemoveAll(filepath.Join(fv.workDir, "output")); err != nil {
		t.Fatal(err)
	}
	image := writeImage(t, dir, "coreboot.rom")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}

	err := fv.SetACPower(PowerOn)
	if err == nil {
		t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })
		t.Fatal("want an error powering on with the required firmware tree missing")
	}
	if !strings.Contains(err.Error(), "missing one or more required firmware files") {
		t.Errorf("error = %q, want it to name the missing firmware", err)
	}
	if status, _ := fv.ACPowerState(); status.Power != PowerOff {
		t.Errorf("ACPowerState = %v, want off: a failed start must not leave a model process running", status)
	}
}

// writeFIP writes a stand-in FIP (the ToC magic and some padding) and the
// four RSE images a build places next to it, and returns the FIP's path.
func writeFIP(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, "output", "rdv3", "rdv3")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	fip := filepath.Join(out, "fip-uefi.bin")
	if err := os.WriteFile(fip, append(append([]byte{}, fipTOCMagic...), make([]byte, 60)...), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tf_m_rom.bin", "tf_m_flash.bin", "tf_m_vm0_0.bin", "tf_m_vm1_0.bin"} {
		if err := os.WriteFile(filepath.Join(out, name), []byte("stand-in"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return fip
}

func TestIsFIP(t *testing.T) {
	dir := t.TempDir()
	fip := writeFIP(t, dir)
	short := filepath.Join(dir, "short.bin")
	if err := os.WriteFile(short, fipTOCMagic[:2], 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{fip, true},
		{writeImage(t, dir, "uefi.bin"), false},
		{short, false},
	} {
		got, err := isFIP(tc.path)
		if err != nil {
			t.Fatalf("isFIP(%s): %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("isFIP(%s) = %v, want %v", tc.path, got, tc.want)
		}
	}
	if _, err := isFIP(filepath.Join(dir, "absent.bin")); err == nil {
		t.Error("isFIP on a missing file: want an error")
	}
}

// TestStartBootsAFIPFromFlashloader0 covers the FIP flow: the FIP goes on
// flashloader0, the RSE images come from the FIP's own directory, and none
// of the RESET_TO_BL31 preloads are passed.
func TestStartBootsAFIPFromFlashloader0(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "argv.txt")
	model := writeScript(t, dir, "model.sh", `#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}"
printf '%s\n' "$@" > "`+dump+`"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
	fv := testPlatform(t, model)
	fip := writeFIP(t, filepath.Join(dir, "build"))
	if err := fv.FlashWrite(FlashHost, fip, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	got, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	argv := string(got)
	rse := resolveFIPComponents(filepath.Dir(fip))
	for _, want := range []string{
		"ros.board.flashloader0.fname=" + fip,
		"css.sysctrl.rse.rom.raw_image=" + rse.rseRom,
		"css.sysctrl.rse.cpu=" + rse.rseFlash + "@0xB0000000",
		"css.sysctrl.rse.cpu=" + rse.rseVM0 + "@0x31000400",
		"css.sysctrl.rse.cpu=" + rse.rseVM1 + "@0x31080000",
		"css.ap_periph.ns_uart0.out_file=" + fv.consoleLog,
		"-I",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("model argv missing %q\ngot: %s", want, argv)
		}
	}
	for _, unwanted := range []string{
		"css.sysctrl.scp.cpu=",
		"css.sysctrl.mcp.cpu=",
		"css.ap_periph.arsm_sram=",
		"ros.board.dram=",
		filepath.Join(fv.workDir, "output"),
	} {
		if strings.Contains(argv, unwanted) {
			t.Errorf("model argv contains %q, want no RESET_TO_BL31 preloads in the FIP flow\ngot: %s", unwanted, argv)
		}
	}
}

func TestStartFailsWhenRSEFirmwareIsMissingNextToTheFIP(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "model.sh", `#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
	fv := testPlatform(t, model)
	fip := writeFIP(t, filepath.Join(dir, "build"))
	vm1 := filepath.Join(filepath.Dir(fip), "tf_m_vm1_0.bin")
	if err := os.Remove(vm1); err != nil {
		t.Fatal(err)
	}
	if err := fv.FlashWrite(FlashHost, fip, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}

	err := fv.SetACPower(PowerOn)
	if err == nil {
		t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })
		t.Fatal("want an error powering on with an RSE image missing next to the FIP")
	}
	if !strings.Contains(err.Error(), vm1) {
		t.Errorf("error = %q, want it to name %s", err, vm1)
	}
	if status, _ := fv.ACPowerState(); status.Power != PowerOff {
		t.Errorf("ACPowerState = %v, want off: a failed start must not leave a model process running", status)
	}
}

func TestFlashTargetBMCNotImplemented(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "model.sh", "#!/bin/sh\nexit 0\n")
	fv := testPlatform(t, model)
	if err := fv.FlashProbe(FlashBMC); err != ErrNotImplemented {
		t.Fatalf("FlashProbe(bmc) = %v, want ErrNotImplemented", err)
	}
	if err := fv.FlashRead(FlashBMC, filepath.Join(dir, "out")); err != ErrNotImplemented {
		t.Fatalf("FlashRead(bmc) = %v, want ErrNotImplemented", err)
	}
}

func TestDiskMountDefaultsToVirtio(t *testing.T) {
	dir := t.TempDir()
	fv := testPlatform(t, writeScript(t, dir, "model.sh", "#!/bin/sh\nexit 0\n"))
	image := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(image, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fv.DiskMount(context.Background(), image, DiskOptions{}); err != nil {
		t.Fatal(err)
	}
	medium, path, ok := fv.currentDisk()
	if !ok || medium != DiskVirtio || path != image {
		t.Errorf("recorded disk = %v %q %v, want virtio %q", medium, path, ok, image)
	}
}

func TestDiskRefusesWhatTheModelCannotPresent(t *testing.T) {
	dir := t.TempDir()
	fv := testPlatform(t, writeScript(t, dir, "model.sh", "#!/bin/sh\nexit 0\n"))
	image := filepath.Join(dir, "debian.iso")
	if err := os.WriteFile(image, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]DiskOptions{
		"cd":        {Medium: DiskCD},
		"usb":       {Medium: DiskUSB},
		"boot once": {BootOnce: true},
		"power on":  {PowerOn: true},
		"BMC reset": {ResetBMC: true},
	} {
		if err := fv.DiskMount(context.Background(), image, opts); err == nil {
			t.Errorf("DiskMount with %s: want an error, got nil", name)
		}
	}
	if _, _, ok := fv.currentDisk(); ok {
		t.Error("a refused DiskMount recorded a disk")
	}
	if err := fv.DiskEject("CD1"); err == nil {
		t.Error("DiskEject(CD1): want an error, got nil")
	}
}

// TestConsoleTelnetsToTheRequestedPort covers Console trusting consolePort
// directly rather than reading it back out of the model's own process log:
// the profile's default (6000) is chosen clear of every other UART
// terminal's own default (5000, incrementing on collision - confirmed
// against a real run to land as high as 5012 with every terminal present),
// so the request is expected to reliably succeed.
// TestConsoleTelnetsToTheRequestedPort covers Console trusting consolePort
// directly. Console no longer probes the socket itself before attaching (a
// separate probe connection was found, via live testing, to consume the
// model's own console slot and starve the real telnet session right after
// it - see the comment in Console, in fvp.go, for the full story). It just
// retries handing off to the runner's RunInteractive, so no real listener
// is needed here.
func TestConsoleTelnetsToTheRequestedPort(t *testing.T) {
	dir := t.TempDir()
	port := 16000 // Console() no longer dials the port itself - see fakeRunner.RunInteractive.

	model := writeScript(t, dir, "model.sh", fmt.Sprintf(`#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port %d"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`, port))
	t.Setenv("BENCHCTL_FVP_CONSOLE_PORT", strconv.Itoa(port))
	fv := testPlatform(t, model)
	runner := &fakeRunner{}
	fv.runner = runner
	image := writeImage(t, dir, "fip.bin")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetACPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	if err := fv.Console(); err != nil {
		t.Fatal(err)
	}
	if len(runner.interactive) != 1 {
		t.Fatalf("Console interactive calls = %d, want 1", len(runner.interactive))
	}
	got := strings.Join(runner.interactive[0], " ")
	if !strings.Contains(got, "telnet") || !strings.Contains(got, strconv.Itoa(port)) {
		t.Errorf("console argv = %q, want telnet against the requested port %d", got, port)
	}
}

// TestConsoleSuppressesFastFailingRetriesButShowsTheRealSession reproduces
// what the operator reported after the retry loop was added: it fixed the
// connection (eventually), but spammed the terminal with every failed
// attempt's own "Connection closed by foreign host" along the way. Two fast
// fails (each writing distinctive output and failing well inside
// consoleFastFailThreshold) must not reach fv.progress at all, while the
// third, successful attempt's own output must - because a success is
// promoted to visible regardless of how quickly it returned (see the
// guard.promote() call right after session.Run in Console).
func TestConsoleSuppressesFastFailingRetriesButShowsTheRealSession(t *testing.T) {
	dir := t.TempDir()
	port := 16001

	model := writeScript(t, dir, "model.sh", fmt.Sprintf(`#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port %d"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`, port))
	t.Setenv("BENCHCTL_FVP_CONSOLE_PORT", strconv.Itoa(port))
	fv := testPlatform(t, model)
	var progress bytes.Buffer
	fv.progress = &progress
	runner := &fakeRunner{}
	runner.interactiveOutHandler = func(call int, out io.Writer, argv []string) error {
		if call < 2 {
			fmt.Fprintf(out, "noise from failed attempt %d\n", call)
			return fmt.Errorf("Connection closed by foreign host")
		}
		fmt.Fprintln(out, "real session output")
		return nil
	}
	fv.runner = runner
	image := writeImage(t, dir, "fip.bin")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetACPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	if err := fv.Console(); err != nil {
		t.Fatalf("Console: %v", err)
	}
	if len(runner.interactive) != 3 {
		t.Fatalf("interactive attempts = %d, want 3 (two silent failures then success)", len(runner.interactive))
	}
	got := progress.String()
	if strings.Contains(got, "noise from failed attempt") {
		t.Errorf("progress = %q, want the failed attempts' own output suppressed entirely", got)
	}
	if !strings.Contains(got, "real session output") {
		t.Errorf("progress = %q, want the successful attempt's own output shown", got)
	}
}

// TestConsoleGivesUpQuietlyShowingOnlyTheLastAttempt covers the other half
// of the operator's request: "print only the output from the last try
// (whether successful or not)" - when every attempt fails fast and the
// overall consoleAttachTimeout is exhausted, the final error report should
// carry the LAST attempt's own output, not an earlier one and not silence.
func TestConsoleGivesUpQuietlyShowingOnlyTheLastAttempt(t *testing.T) {
	dir := t.TempDir()
	port := 16002

	model := writeScript(t, dir, "model.sh", fmt.Sprintf(`#!/bin/sh
echo "terminal_ns_uart0: Listening for serial connection on port %d"
trap 'exit 0' TERM
while :; do sleep 0.05; done
`, port))
	t.Setenv("BENCHCTL_FVP_CONSOLE_PORT", strconv.Itoa(port))
	fv := testPlatform(t, model)
	fv.consoleAttachTimeout = 40 * time.Millisecond
	var progress bytes.Buffer
	fv.progress = &progress
	runner := &fakeRunner{}
	runner.interactiveOutHandler = func(call int, out io.Writer, argv []string) error {
		fmt.Fprintf(out, "noise from failed attempt %d\n", call)
		time.Sleep(2 * time.Millisecond) // paces the loop to a handful of attempts, not thousands
		return fmt.Errorf("Connection closed by foreign host")
	}
	fv.runner = runner
	image := writeImage(t, dir, "fip.bin")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetACPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	err := fv.Console()
	if err == nil {
		t.Fatal("want an error when every attempt fails fast within consoleAttachTimeout")
	}
	attempts := len(runner.interactive)
	if attempts < 2 {
		t.Fatalf("want at least 2 attempts before giving up, got %d", attempts)
	}
	got := progress.String()
	if n := strings.Count(got, "noise from failed attempt"); n != 1 {
		t.Fatalf("progress shows %d attempts' output, want exactly 1 (the last): %q", n, got)
	}
	want := fmt.Sprintf("noise from failed attempt %d", attempts-1)
	if !strings.Contains(got, want) {
		t.Errorf("progress = %q, want it to contain only the last attempt's own output (%q)", got, want)
	}
}

func TestConsoleRefusesWhenTheModelIsOff(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "model.sh", "#!/bin/sh\nexit 0\n")
	fv := testPlatform(t, model)
	runner := &fakeRunner{}
	fv.runner = runner

	err := fv.Console()
	if err == nil {
		t.Fatal("want an error when the model is off")
	}
	if !strings.Contains(err.Error(), "power ac on") {
		t.Errorf("error = %q, want it to point at 'power ac on'", err)
	}
	if len(runner.interactive) != 0 {
		t.Errorf("Console should not have attempted to attach, got %d interactive calls", len(runner.interactive))
	}
}

func TestConsolePortDefaultsClearOfEveryOtherTerminal(t *testing.T) {
	dir := t.TempDir()
	fv := testPlatform(t, writeScript(t, dir, "model.sh", "#!/bin/sh\nexit 0\n"))
	if fv.consolePort != 6000 {
		t.Fatalf("default consolePort = %d, want 6000, chosen clear of every other UART terminal's own default of 5000", fv.consolePort)
	}
}

// TestACPowerStateReflectsTheProcess covers ACPowerState directly: off with
// nothing launched, on once SetACPower(on) launches the (paused) process.
func TestACPowerStateReflectsTheProcess(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "model.sh", "#!/bin/sh\necho \"terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}\"\nwhile :; do sleep 0.05; done\n")
	fv := testPlatform(t, model)
	fv.runner = &fakeRunner{}
	image := writeImage(t, dir, "fip.bin")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}

	status, err := fv.ACPowerState()
	if err != nil || status.Power != PowerOff {
		t.Fatalf("ACPowerState = %v, %v, want off", status, err)
	}

	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetACPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	status, err = fv.ACPowerState()
	if err != nil || status.Power != PowerOn {
		t.Fatalf("ACPowerState after SetACPower(on) = %v, %v, want on", status, err)
	}
}

func TestSOLNotImplemented(t *testing.T) {
	dir := t.TempDir()
	fv := testPlatform(t, writeScript(t, dir, "model.sh", "#!/bin/sh\nexit 0\n"))
	if err := fv.ConsoleSOL(BMC{IP: "10.0.0.1"}); err != ErrNotImplemented {
		t.Fatalf("ConsoleSOL = %v, want ErrNotImplemented", err)
	}
}

// fakeIrisServer stands in for a real Iris server: enough of the
// IrisRpc/1.0 handshake and IrisJson framing to exercise HardReset/stop's
// opportunistic use of internal/iris against, dispatching each call to
// handler by method name. See internal/iris's own tests for coverage of
// the client side of this same protocol; this is deliberately a separate,
// minimal implementation rather than a shared test helper, since platform
// cannot import iris's unexported test internals across packages.
// fakeIrisServer accepts as many connections as a test throws at it, not
// just one: real usage here opens a fresh Iris connection per call
// (PowerState, startFirmware, ...), not one persistent connection reused
// throughout, so a test with more than one power operation needs more
// than one connection served.
// fakeIrisNoResponse is a sentinel a handler returns as errMsg to mean
// "never respond to this call at all" - see serveFakeIrisConn.
const fakeIrisNoResponse = "\x00 test sentinel: send no response at all"

func fakeIrisServer(t *testing.T, handler func(method string, params json.RawMessage) (result any, errMsg string)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	// Connections are served concurrently (one goroutine each, since a
	// test can open more than one at a time), but calls into handler are
	// serialized through this mutex:
	// most test handlers close over plain, unsynchronized state (a running
	// bool, a call counter, ...), which a real concurrent invocation would
	// race on. Serializing also better matches the real model, which is
	// itself single-threaded and only ever processes one Iris request at a
	// time regardless of how many connections are open.
	var mu sync.Mutex
	synchronized := func(method string, params json.RawMessage) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		return handler(method, params)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by test cleanup
			}
			go serveFakeIrisConn(conn, synchronized)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func serveFakeIrisConn(conn net.Conn, handler func(method string, params json.RawMessage) (result any, errMsg string)) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if _, err := conn.Write([]byte("IrisRpc/1.0 200 OK\r\nFormat: IrisJson\r\n\r\n")); err != nil {
		return
	}
	for {
		prefix, err := r.ReadString(':')
		if err != nil || prefix != "IrisJson:" {
			return
		}
		lenField, err := r.ReadString(':')
		if err != nil {
			return
		}
		n, err := strconv.Atoi(strings.TrimSuffix(lenField, ":"))
		if err != nil {
			return
		}
		data := make([]byte, n)
		total := 0
		for total < len(data) {
			m, err := r.Read(data[total:])
			total += m
			if err != nil {
				return
			}
		}
		if _, err := r.ReadByte(); err != nil { // trailing '\n'
			return
		}
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			return
		}
		result, errMsg := handler(req.Method, req.Params)
		if errMsg == fakeIrisNoResponse {
			// Simulates the model's single-threaded event loop being too
			// busy (e.g. mid-reset) to ack even a plain query: the request
			// was received, but nothing is ever written back for it -
			// unlike a real dropped connection, the loop carries on and
			// serves later requests normally.
			continue
		}
		resp := map[string]any{"id": req.ID, "jsonrpc": "2.0"}
		if errMsg != "" {
			resp["error"] = map[string]any{"code": 1, "message": errMsg}
		} else {
			resp["result"] = result
		}
		respData, err := json.Marshal(resp)
		if err != nil {
			return
		}
		if _, err := conn.Write([]byte(fmt.Sprintf("IrisJson:%d:%s\n", len(respData), respData))); err != nil {
			return
		}
	}
}

// TestResetNotImplemented covers PowerReset and HardReset both being
// unimplemented for FVP - see PowerReset's own doc comment for why: Iris's
// own in-place reset is unreliable (fails outright while running, and can
// segfault the model after a reset that reported success) and a
// relaunch-based one would break any console connection
// open-source-firmware-validation already holds directly, so neither is
// offered here at all, regardless of power state.
func TestResetNotImplemented(t *testing.T) {
	for _, reset := range []struct {
		name string
		call func(*fvpPlatform) error
	}{
		{"HardReset", (*fvpPlatform).HardReset},
		{"PowerReset", (*fvpPlatform).PowerReset},
	} {
		t.Run(reset.name, func(t *testing.T) {
			dir := t.TempDir()
			fv := testPlatform(t, writeScript(t, dir, "model.sh", "#!/bin/sh\nexit 0\n"))
			if err := reset.call(fv); !errors.Is(err, ErrNotImplemented) {
				t.Fatalf("%s = %v, want ErrNotImplemented", reset.name, err)
			}
		})
	}
}

// TestSetPowerOffNotImplemented covers SetPower(PowerOff) being
// unimplemented for FVP - see SetPower's own doc comment for why.
func TestSetPowerOffNotImplemented(t *testing.T) {
	dir := t.TempDir()
	fv := testPlatform(t, writeScript(t, dir, "model.sh", "#!/bin/sh\nexit 0\n"))
	if err := fv.SetPower(PowerOff); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("SetPower(off) = %v, want ErrNotImplemented", err)
	}
}

// TestPowerOnErrorsWhenItDoesNotActuallyTakeEffect reproduces the exact
// silent-failure bug reported against the real model: a server that
// accepts simulationTime_run (callTolerant does not error on a missing
// acknowledgment - see its own doc comment) but whose running state never
// actually changes - simulating a request that was received but not
// genuinely processed. Before waitForIrisRunState existed, this looked
// identical to success at the RPC level; it must now surface as a clear
// error instead.
func TestPowerOnErrorsWhenItDoesNotActuallyTakeEffect(t *testing.T) {
	dir := t.TempDir()
	model := writeScript(t, dir, "model.sh", "#!/bin/sh\necho \"terminal_ns_uart0: Listening for serial connection on port ${BENCHCTL_FVP_CONSOLE_PORT:-6000}\"\nwhile :; do sleep 0.05; done\n")
	// running is left permanently false and never actually updated by
	// Run, the same shape as a request that gets accepted (no error, ack
	// or not) but silently has no effect.
	port := fakeIrisServer(t, func(method string, params json.RawMessage) (any, string) {
		if method == "simulationTime_get" {
			return map[string]any{"running": false}, ""
		}
		return nil, ""
	})
	t.Setenv("BENCHCTL_FVP_IRIS_PORT", strconv.Itoa(port))
	fv := testPlatform(t, model)
	fv.runner = &fakeRunner{}
	fv.irisEffectTimeout = 300 * time.Millisecond
	fv.pollEvery = 20 * time.Millisecond
	image := writeImage(t, dir, "fip.bin")
	if err := fv.FlashWrite(FlashHost, image, false); err != nil {
		t.Fatalf("FlashWrite: %v", err)
	}
	if err := fv.SetACPower(PowerOn); err != nil {
		t.Fatalf("SetACPower(on): %v", err)
	}
	t.Cleanup(func() { _ = fv.SetACPower(PowerOff) })

	err := fv.SetPower(PowerOn)
	if err == nil {
		t.Fatal("want an error when the model's running state never actually changes after Run")
	}
	if !strings.Contains(err.Error(), "may not have taken effect") {
		t.Errorf("error = %q, want it to say the request may not have taken effect", err)
	}
}
