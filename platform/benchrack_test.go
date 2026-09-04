// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zarhus/benchctl/internal/rte"
)

// gpioSet records one Set call on the fake controller.
type gpioSet struct {
	id    int
	state string
	hold  int
}

// fakeGPIO is a test gpioController. Get returns canned pins; Set records each
// call. A power-button press (gpioPowerBtn) toggles the power LED
// (gpioPowerLED), modelling how a press flips host power so SetPower's poll can
// settle.
//
// It also models the RTE's auto-release, against a virtual clock the driver's
// sleep advances: a press with a hold keeps its pin down until the clock passes
// the hold, and a press that lands while the pin is still down is recorded as an
// overlap. An overlap is the fault a released button rules out - the platform
// sees one continuous assertion rather than a new press.
type fakeGPIO struct {
	pins   map[int]rte.Pin
	sets   []gpioSet
	getErr error
	setErr error

	clock     time.Duration         // virtual now, moved on by advance
	heldUntil map[int]time.Duration // per pin, when the RTE lets it back up
	overlaps  []int                 // pins pressed again while still held
}

// advance moves the virtual clock on, standing in for the driver's sleep.
func (f *fakeGPIO) advance(d time.Duration) { f.clock += d }

// held reports whether the RTE is still holding pin id down.
func (f *fakeGPIO) held(id int) bool { return f.clock < f.heldUntil[id] }

func (f *fakeGPIO) Get(id int) (rte.Pin, error) {
	if f.getErr != nil {
		return rte.Pin{}, f.getErr
	}
	return f.pins[id], nil
}

func (f *fakeGPIO) Set(id int, state string, hold int) error {
	f.sets = append(f.sets, gpioSet{id, state, hold})
	if f.setErr != nil {
		return f.setErr
	}
	if hold > 0 {
		if f.held(id) {
			f.overlaps = append(f.overlaps, id)
		}
		if f.heldUntil == nil {
			f.heldUntil = map[int]time.Duration{}
		}
		f.heldUntil[id] = f.clock + time.Duration(hold)*time.Second
	}
	if id == gpioPowerBtn {
		led := f.pins[gpioPowerLED]
		led.State = 1 - led.State%2
		if f.pins == nil {
			f.pins = map[int]rte.Pin{}
		}
		f.pins[gpioPowerLED] = led
	}
	return nil
}

// setIDs returns just the pin ids of the recorded sets, in order.
func (f *fakeGPIO) setIDs() []int {
	ids := make([]int, len(f.sets))
	for i, s := range f.sets {
		ids[i] = s.id
	}
	return ids
}

// parkedBus reports whether the recorded sets contain an idle park: the mux
// select resting on hostSelect and the mux enable driven high (disabled).
func (f *fakeGPIO) parkedBus(hostSelect string) bool {
	var selected, disabled bool
	for _, s := range f.sets {
		if s.id == gpioMuxSelect && s.state == hostSelect {
			selected = true
		}
		if s.id == gpioMuxEnable && s.state == "high" {
			disabled = true
		}
	}
	return selected && disabled
}

// countSets returns how many recorded sets target id.
func (f *fakeGPIO) countSets(id int) int {
	n := 0
	for _, s := range f.sets {
		if s.id == id {
			n++
		}
	}
	return n
}

// firstSet returns the first recorded set for id, or false if none.
func (f *fakeGPIO) firstSet(id int) (gpioSet, bool) {
	for _, s := range f.sets {
		if s.id == id {
			return s, true
		}
	}
	return gpioSet{}, false
}

func testBenchRack(t *testing.T, runner *fakeRunner, g *fakeGPIO) *benchRack {
	t.Helper()
	b := newBenchRack(runner, Config{}).(*benchRack)
	b.gpio = g
	b.settle = 0
	b.powerSettle = 0
	// Point the driver's waits at the fake's clock, so a button-release wait is
	// observed in the fake rather than waited out in the test.
	b.sleep = g.advance
	b.pollInterval = time.Microsecond
	b.pollTimeout = 100 * time.Millisecond
	b.acCycleDelay = 0
	b.acDrainWait = 0
	b.progress = io.Discard
	return b
}

// writeSized writes a file of n bytes and returns its path.
func writeSized(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fw.rom")
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBenchRackRegistered(t *testing.T) {
	spec, ok := Get("benchrack")
	if !ok {
		t.Fatal("benchrack driver not registered")
	}
	if spec.DefaultPassword != "meta-rte" {
		t.Errorf("benchrack default password = %q, want meta-rte", spec.DefaultPassword)
	}
}

func TestBenchRackPowerStateReadsLED(t *testing.T) {
	cases := []struct {
		ledState uint
		want     Power
	}{
		{1, PowerOn},
		{0, PowerOff},
	}
	for _, tc := range cases {
		g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {ID: gpioPowerLED, State: tc.ledState}}}
		b := testBenchRack(t, &fakeRunner{}, g)
		got, err := b.PowerState()
		if err != nil {
			t.Fatal(err)
		}
		if got.Power != tc.want {
			t.Errorf("LED state %d: PowerState = %v, want %v", tc.ledState, got.Power, tc.want)
		}
	}
}

func TestBenchRackSetPowerOnPressesShort(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: 0}}}
	b := testBenchRack(t, &fakeRunner{}, g)
	if err := b.SetPower(PowerOn); err != nil {
		t.Fatal(err)
	}
	set, ok := g.firstSet(gpioPowerBtn)
	if !ok {
		t.Fatal("SetPower(on) did not press the power button")
	}
	if set.state != "low" || set.hold != b.powerOnHold {
		t.Errorf("power press = %+v, want state low hold %d", set, b.powerOnHold)
	}
}

func TestBenchRackSetPowerOffHoldsLong(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: 1}}}
	b := testBenchRack(t, &fakeRunner{}, g)
	if err := b.SetPower(PowerOff); err != nil {
		t.Fatal(err)
	}
	set, _ := g.firstSet(gpioPowerBtn)
	if set.hold != b.powerOffHold {
		t.Errorf("power-off hold = %d, want %d (force S5)", set.hold, b.powerOffHold)
	}
}

func TestBenchRackSetPowerReturnsWithTheButtonReleased(t *testing.T) {
	// A force-off latches S5 before its hold is up, so the power state settles
	// mid-press. Returning there leaves the button down for the next command,
	// whose press continues the hold instead of starting one.
	g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: 1}}}
	b := testBenchRack(t, &fakeRunner{}, g)
	if err := b.SetPower(PowerOff); err != nil {
		t.Fatal(err)
	}
	if g.held(gpioPowerBtn) {
		t.Error("SetPower(off) returned while the RTE still held the power button")
	}
}

func TestBenchRackPowerResetPressesOnAfterTheForceOffReleases(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: 1}}}
	b := testBenchRack(t, &fakeRunner{}, g)
	if err := b.PowerReset(); err != nil {
		t.Fatal(err)
	}
	if len(g.overlaps) != 0 {
		t.Errorf("power-reset pressed a button that was still held: pins %v", g.overlaps)
	}
}

func TestBenchRackHardResetReturnsWithTheButtonReleased(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: 1}}}
	b := testBenchRack(t, &fakeRunner{}, g)
	if err := b.HardReset(); err != nil {
		t.Fatal(err)
	}
	if g.held(gpioResetBtn) {
		t.Error("HardReset returned while the RTE still held the reset button")
	}
}

func TestBenchRackSetPowerIdempotent(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: 1}}}
	b := testBenchRack(t, &fakeRunner{}, g)
	if err := b.SetPower(PowerOn); err != nil {
		t.Fatal(err)
	}
	if _, pressed := g.firstSet(gpioPowerBtn); pressed {
		t.Error("SetPower(on) pressed the button though the host was already on")
	}
}

func TestBenchRackPowerResetCycles(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: 1}}}
	b := testBenchRack(t, &fakeRunner{}, g)
	if err := b.PowerReset(); err != nil {
		t.Fatal(err)
	}
	var holds []int
	for _, s := range g.sets {
		if s.id == gpioPowerBtn {
			holds = append(holds, s.hold)
		}
	}
	if len(holds) != 2 || holds[0] != b.powerOffHold || holds[1] != b.powerOnHold {
		t.Errorf("power-reset presses = %v, want [off-hold on-hold] = [%d %d]", holds, b.powerOffHold, b.powerOnHold)
	}
	// The reset ends with the platform powered back on.
	if st, _ := b.PowerState(); st.Power != PowerOn {
		t.Errorf("after reset power = %v, want on", st.Power)
	}
}

func TestBenchRackHardResetPulsesResetWhenOn(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: 1}}}
	b := testBenchRack(t, &fakeRunner{}, g)
	if err := b.HardReset(); err != nil {
		t.Fatal(err)
	}
	set, ok := g.firstSet(gpioResetBtn)
	if !ok || set.state != "low" || set.hold != b.resetHold {
		t.Errorf("hard reset = %+v ok=%v, want a reset-button pulse", set, ok)
	}
}

func TestBenchRackHardResetErrorsWhenOff(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: 0}}}
	b := testBenchRack(t, &fakeRunner{}, g)
	if err := b.HardReset(); err == nil {
		t.Fatal("HardReset should error when the host is off")
	}
	if _, pulsed := g.firstSet(gpioResetBtn); pulsed {
		t.Error("HardReset pulsed reset though the host was off")
	}
}

func TestBenchRackConsoleTelnetsToSer2net(t *testing.T) {
	runner := &fakeRunner{}
	b := testBenchRack(t, runner, &fakeGPIO{})
	if err := b.Console(); err != nil {
		t.Fatal(err)
	}
	if len(runner.interactive) != 1 {
		t.Fatalf("Console interactive calls = %d, want 1", len(runner.interactive))
	}
	got := strings.Join(runner.interactive[0], " ")
	if !strings.Contains(got, "telnet") || !strings.Contains(got, strconv.Itoa(b.board.console.port)) {
		t.Errorf("console argv = %q, want telnet against the ser2net port", got)
	}
}

func TestBenchRackConsoleSOLClearsThenActivates(t *testing.T) {
	runner := &fakeRunner{}
	b := testBenchRack(t, runner, &fakeGPIO{})
	bmc := BMC{IP: "192.168.50.11", User: "admin", Password: "Administrator"}

	if err := b.ConsoleSOL(bmc); err != nil {
		t.Fatal(err)
	}
	// A session that ended without "~." leaves the payload open, so the deactivate
	// must precede the activate.
	if len(runner.sequence) != 3 {
		t.Fatalf("ConsoleSOL calls = %d, want 3 (deactivate, activate, deactivate)", len(runner.sequence))
	}
	for i, want := range []string{"sol deactivate", "sol activate", "sol deactivate"} {
		got := strings.Join(runner.sequence[i], " ")
		if !strings.Contains(got, want) || !strings.Contains(got, bmc.IP) {
			t.Errorf("call %d = %q, want %s against %s", i, got, want, bmc.IP)
		}
	}
	if !runner.noEscape[0] {
		t.Error("ConsoleSOL attached with the ssh escape character live, so \"~.\" would not reach ipmitool")
	}
}

func TestBenchRackConsoleSOLStopsWhenTheBMCDoesNotAnswer(t *testing.T) {
	// The deactivate opens a session of its own, so a BMC that never answers is
	// known there, and attaching would only wait out the same timeout again.
	runner := &fakeRunner{handler: func(int, []string) (string, error) {
		return "", errors.New("exit status 1: Error: Unable to establish IPMI v2 / RMCP+ session")
	}}
	b := testBenchRack(t, runner, &fakeGPIO{})
	err := b.ConsoleSOL(BMC{IP: "192.168.10.192", User: "admin", Password: "Administrator"})
	if err == nil {
		t.Fatal("ConsoleSOL should report a BMC that did not answer")
	}
	if !strings.Contains(err.Error(), "192.168.10.192") {
		t.Errorf("error %q should name the BMC", err)
	}
	if len(runner.interactive) != 0 {
		t.Error("ConsoleSOL attached over a BMC session it could not open")
	}
}

func TestBenchRackConsoleSOLRequiresAddress(t *testing.T) {
	runner := &fakeRunner{}
	b := testBenchRack(t, runner, &fakeGPIO{})
	if err := b.ConsoleSOL(BMC{User: "admin", Password: "Administrator"}); err == nil {
		t.Fatal("ConsoleSOL should error without a BMC address")
	}
	if len(runner.calls) != 0 || len(runner.interactive) != 0 {
		t.Error("ConsoleSOL ran ipmitool without a BMC address")
	}
	if g := b.gpio.(*fakeGPIO); len(g.sets) != 0 {
		t.Errorf("ConsoleSOL without a BMC address set %v, want no GPIO writes", g.sets)
	}
}

func TestBenchRackConsoleSOLReportsActivateFailure(t *testing.T) {
	runner := &fakeRunner{interactiveErr: errors.New("SOL payload already active")}
	b := testBenchRack(t, runner, &fakeGPIO{})
	if err := b.ConsoleSOL(BMC{IP: "192.168.50.11", User: "admin", Password: "Administrator"}); err == nil {
		t.Fatal("ConsoleSOL should report an activate failure")
	}
}

func TestBenchRackConsoleUART1ReadsRawSerial(t *testing.T) {
	runner := &fakeRunner{}
	b := testBenchRack(t, runner, &fakeGPIO{})
	if err := b.ConsoleUART1(); err != nil {
		t.Fatal(err)
	}
	if len(runner.interactive) != 1 {
		t.Fatalf("ConsoleUART1 interactive calls = %d, want 1", len(runner.interactive))
	}
	got := strings.Join(runner.interactive[0], " ")
	for _, want := range []string{b.board.console.uart1Device, strconv.Itoa(b.board.console.uart1Baud), "stty", "cat"} {
		if !strings.Contains(got, want) {
			t.Errorf("console argv = %q, want it to mention %q", got, want)
		}
	}
}

func TestBenchRackConsoleUART1IgnoresModemControlLines(t *testing.T) {
	// Without clocal, opening a real UART blocks in open(2) until the line
	// asserts carrier detect, which a debug header never does, so cat would hang
	// with no output and no error rather than actually failing.
	runner := &fakeRunner{}
	b := testBenchRack(t, runner, &fakeGPIO{})
	if err := b.ConsoleUART1(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.interactive[0], " ")
	if !strings.Contains(got, "clocal") {
		t.Errorf("console argv = %q, want it to set clocal so open() does not block on carrier detect", got)
	}
}

func TestBenchRackConsoleUART1SuppressesKeystrokeEcho(t *testing.T) {
	// cat never reads stdin, but the ssh session's own pty (distinct from the
	// serial device) still echoes typed characters by default, so it needs its
	// echo turned off too, not just the device's.
	runner := &fakeRunner{}
	b := testBenchRack(t, runner, &fakeGPIO{})
	if err := b.ConsoleUART1(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.interactive[0], " ")
	if !strings.Contains(got, "stty -echo") {
		t.Errorf("console argv = %q, want it to disable echo on the controlling terminal", got)
	}
}

func TestBenchRackConsoleUART1NotImplementedWithoutDevice(t *testing.T) {
	// A board with no wired UART1 reports ErrNotImplemented without touching the
	// bench, the same shape as an AC operation on a board with no Tasmota.
	g := &fakeGPIO{}
	b := testBenchRack(t, &fakeRunner{}, g)
	b.board.console.uart1Device = ""
	if err := b.ConsoleUART1(); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("ConsoleUART1 = %v, want ErrNotImplemented", err)
	}
	if len(g.sets) != 0 {
		t.Errorf("ConsoleUART1 without a device set %v, want no GPIO writes", g.sets)
	}
}

func TestBenchRackFlashHostSequence(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC:  {Direction: "out", State: 0},
		gpioEnHost: {Direction: "out", State: 0},
		// host off, so SetPower(off) is a no-op and does not add presses
		gpioPowerLED: {State: 0},
	}}
	runner := &fakeRunner{streamOut: "flashrom output\n"}
	b := testBenchRack(t, runner, g)
	fw := writeSized(t, int(b.board.host.sizeBytes))

	if err := b.FlashWrite(FlashHost, fw, false); err != nil {
		t.Fatal(err)
	}

	// Energize order: voltage, mux select, mux enable, Vcc, host load switch,
	// lines.
	want := []int{gpioSpiVoltage, gpioMuxSelect, gpioMuxEnable, gpioSpiVcc, gpioEnHost, gpioSpiLines}
	var got []int
	// Reconstruct the energize prefix: take sets until SPI lines is first turned on.
	for _, s := range g.sets {
		got = append(got, s.id)
		if s.id == gpioSpiLines && s.state == "low" {
			break
		}
	}
	if len(got) < len(want) {
		t.Fatalf("energize sets = %v, want to include %v", got, want)
	}
	// The last six before (and including) lines-on must be the energize order.
	tail := got[len(got)-len(want):]
	for i := range want {
		if tail[i] != want[i] {
			t.Errorf("energize order = %v, want %v", tail, want)
			break
		}
	}

	// Mux select routed to the host branch.
	if set, _ := g.firstSet(gpioMuxSelect); set.state != b.board.host.muxSelect {
		t.Errorf("mux select = %q, want %q for host", set.state, b.board.host.muxSelect)
	}

	// flashrom ran over SSH with the host chip and a write.
	var flashArgv []string
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "flashrom" {
			flashArgv = call
		}
	}
	if flashArgv == nil {
		t.Fatal("flashrom was not run")
	}
	joined := strings.Join(flashArgv, " ")
	if !strings.Contains(joined, "-c "+b.board.host.chip) || !strings.Contains(joined, "-w ") {
		t.Errorf("flashrom argv = %q, want -c %s and -w", joined, b.board.host.chip)
	}
	if !runner.pushed {
		t.Error("firmware was not pushed to the bench")
	}
}

func TestBenchRackFlashMuxEnableInterlock(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	b := testBenchRack(t, &fakeRunner{}, g)
	fw := writeSized(t, int(b.board.host.sizeBytes))
	if err := b.FlashWrite(FlashHost, fw, false); err != nil {
		t.Fatal(err)
	}

	// The mux is enabled (driven low) only after the branch is selected.
	enableLowIdx := -1
	for i, s := range g.sets {
		if s.id == gpioMuxEnable && s.state == "low" {
			enableLowIdx = i
			break
		}
	}
	if enableLowIdx < 0 {
		t.Fatal("mux enable was never driven low to route the flash for writing")
	}
	if selIdx := indexOf(g.setIDs(), gpioMuxSelect); selIdx > enableLowIdx {
		t.Errorf("mux enabled (low) at %d before the select was set at %d", enableLowIdx, selIdx)
	}

	// Idle default once the flash is done: mux disabled (enable high).
	last := ""
	for _, s := range g.sets {
		if s.id == gpioMuxEnable {
			last = s.state
		}
	}
	if last != "high" {
		t.Errorf("mux enable final state = %q, want high (mux disabled when flash not in use)", last)
	}
}

func TestBenchRackFlashMuxSelectDefaultsToHost(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	b := testBenchRack(t, &fakeRunner{handler: tasmotaReplies()}, g)
	fw := writeSized(t, int(b.board.bmc.sizeBytes))
	if err := b.FlashWrite(FlashBMC, fw, false); err != nil {
		t.Fatal(err)
	}
	// The flash routes the mux to the BMC, but the bus must return to the host
	// branch afterward: resting the select on the BMC freezes it even with the
	// mux disabled.
	lastMuxSelect := ""
	for _, s := range g.sets {
		if s.id == gpioMuxSelect {
			lastMuxSelect = s.state
		}
	}
	if lastMuxSelect != b.board.host.muxSelect {
		t.Errorf("mux select final state = %q, want %q (host is the idle default)", lastMuxSelect, b.board.host.muxSelect)
	}
}

func TestBenchRackFlashBMCOmitsChip(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	runner := &fakeRunner{handler: tasmotaReplies()}
	b := testBenchRack(t, runner, g)
	fw := writeSized(t, int(b.board.bmc.sizeBytes))

	if err := b.FlashWrite(FlashBMC, fw, false); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "flashrom" && strings.Contains(strings.Join(call, " "), "-c ") {
			t.Errorf("BMC flashrom argv = %q, should not pass -c (auto-detect)", strings.Join(call, " "))
		}
	}
}

func TestBenchRackFlashBMCRemovesMainsFirst(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	// Record the AC switches and how far the GPIO sequence had got when mains was
	// dropped, so the energize steps can be placed relative to it.
	var switches []string
	setsAtACOff := -1
	reply := tasmotaReplies()
	runner := &fakeRunner{}
	runner.handler = func(call int, argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "Power%20OFF") {
			switches = append(switches, "off")
			setsAtACOff = len(g.sets)
		} else {
			switches = append(switches, "on")
		}
		return reply(call, argv)
	}
	b := testBenchRack(t, runner, g)
	fw := writeSized(t, int(b.board.bmc.sizeBytes))

	if err := b.FlashWrite(FlashBMC, fw, false); err != nil {
		t.Fatal(err)
	}

	// Mains goes off once and stays off: the BMC comes back only on `power ac on`.
	if len(switches) != 1 || switches[0] != "off" {
		t.Fatalf("BMC flash AC switches = %v, want a single off", switches)
	}
	// Nothing energizes the BMC branch before mains is gone.
	for i, s := range g.sets[:setsAtACOff] {
		energized := (s.id == gpioMuxEnable && s.state == "low") ||
			(s.id == gpioSpiLines && s.state == "low") ||
			(s.id == gpioSpiVcc && s.state == "low") ||
			(s.id == gpioEnBMC && s.state == "high")
		if energized {
			t.Errorf("set %+v at %d energized the BMC branch before mains was removed", s, i)
		}
	}
}

func TestBenchRackFlashBMCWaitsAfterMainsOff(t *testing.T) {
	// The board holds the bus for a moment after mains goes away, so the flash
	// waits before it energizes the branch.
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	b := testBenchRack(t, &fakeRunner{handler: tasmotaReplies()}, g)
	b.acDrainWait = 20 * time.Millisecond
	fw := writeSized(t, int(b.board.bmc.sizeBytes))

	start := time.Now()
	if err := b.FlashWrite(FlashBMC, fw, false); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < b.acDrainWait {
		t.Errorf("BMC flash took %s, want at least the %s drain wait", elapsed, b.acDrainWait)
	}
}

func TestBenchRackFlashHostLeavesMainsAlone(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	runner := &fakeRunner{handler: tasmotaReplies()}
	b := testBenchRack(t, runner, g)
	fw := writeSized(t, int(b.board.host.sizeBytes))

	if err := b.FlashWrite(FlashHost, fw, false); err != nil {
		t.Fatal(err)
	}
	if calls := curlCalls(runner); len(calls) != 0 {
		t.Errorf("host flash curl calls = %v, want none (the host flash does not touch mains)", calls)
	}
}

func TestBenchRackFlashBMCNeedsACControl(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	b := testBenchRack(t, &fakeRunner{}, g)
	b.tasmota = nil

	err := b.FlashProbe(FlashBMC)
	if err == nil || !strings.Contains(err.Error(), "mains") {
		t.Fatalf("BMC flash without AC control = %v, want an error about removing mains", err)
	}
	if len(g.sets) != 0 {
		t.Errorf("refused BMC flash drove %v, want the bench left untouched", g.setIDs())
	}
}

func TestBenchRackFlashProbeDetectsWithoutReadOrWrite(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	runner := &fakeRunner{streamOut: "Found chip\n"}
	b := testBenchRack(t, runner, g)

	if err := b.FlashProbe(FlashHost); err != nil {
		t.Fatal(err)
	}

	var flashArgv []string
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "flashrom" {
			flashArgv = call
		}
	}
	if flashArgv == nil {
		t.Fatal("flashrom was not run")
	}
	joined := strings.Join(flashArgv, " ")
	if strings.Contains(joined, "-r ") || strings.Contains(joined, "-w ") {
		t.Errorf("probe flashrom argv = %q, should neither read nor write", joined)
	}
	if !strings.Contains(joined, "-c "+b.board.host.chip) {
		t.Errorf("probe flashrom argv = %q, want -c %s for host", joined, b.board.host.chip)
	}
	if runner.pushed || runner.pulled {
		t.Errorf("probe pushed=%v pulled=%v, want neither (no image transfer)", runner.pushed, runner.pulled)
	}
}

func TestBenchRackFlashReadPullsImageBack(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	runner := &fakeRunner{}
	b := testBenchRack(t, runner, g)
	out := filepath.Join(t.TempDir(), "dump.bin")

	if err := b.FlashRead(FlashHost, out); err != nil {
		t.Fatal(err)
	}

	var flashArgv []string
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "flashrom" {
			flashArgv = call
		}
	}
	if flashArgv == nil {
		t.Fatal("flashrom was not run")
	}
	joined := strings.Join(flashArgv, " ")
	if !strings.Contains(joined, "-r ") || strings.Contains(joined, "-w ") {
		t.Errorf("read flashrom argv = %q, want -r and not -w", joined)
	}
	if !runner.pulled {
		t.Error("read did not pull the image back from the bench")
	}

	// The bus is de-energized afterward: lines high-z and the mux disabled.
	lastState := func(id int) string {
		state := ""
		for _, s := range g.sets {
			if s.id == id {
				state = s.state
			}
		}
		return state
	}
	if lastState(gpioSpiLines) != "high-z" || lastState(gpioMuxEnable) != "high" {
		t.Errorf("after read lines=%q mux=%q, want lines high-z and mux disabled", lastState(gpioSpiLines), lastState(gpioMuxEnable))
	}
}

func TestBenchRackFlashExportsEGPAOnFirstRun(t *testing.T) {
	// E_GPA pins boot as high-Z inputs; the flash must drive them to a defined
	// off (output low) before energizing anything.
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC:    {Direction: "in"},
		gpioEnHost:   {Direction: "in"},
		gpioPowerLED: {State: 0},
	}}
	b := testBenchRack(t, &fakeRunner{}, g)
	fw := writeSized(t, int(b.board.host.sizeBytes))
	if err := b.FlashWrite(FlashHost, fw, false); err != nil {
		t.Fatal(err)
	}

	enBMC, okB := g.firstSet(gpioEnBMC)
	enHost, okH := g.firstSet(gpioEnHost)
	if !okB || enBMC.state != "low" || !okH || enHost.state != "low" {
		t.Fatalf("E_GPA sets = bmc %+v(%v) host %+v(%v), want both driven low", enBMC, okB, enHost, okH)
	}
	// Both E_GPA parked before the voltage change.
	ids := g.setIDs()
	voltIdx := indexOf(ids, gpioSpiVoltage)
	if indexOf(ids, gpioEnBMC) > voltIdx || indexOf(ids, gpioEnHost) > voltIdx {
		t.Errorf("E_GPA parked after voltage change; order = %v", ids)
	}
}

func TestBenchRackFlashParksLiveBusOff(t *testing.T) {
	// SPI Vcc and lines are found on; the flash must turn them off before the
	// mux or voltage change.
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioSpiVcc:   {State: 1},
		gpioSpiLines: {State: 1},
		gpioEnBMC:    {Direction: "out"},
		gpioEnHost:   {Direction: "out"},
		gpioPowerLED: {State: 0},
	}}
	b := testBenchRack(t, &fakeRunner{}, g)
	fw := writeSized(t, int(b.board.host.sizeBytes))
	if err := b.FlashWrite(FlashHost, fw, false); err != nil {
		t.Fatal(err)
	}
	// First action on Vcc and lines must be an off (high-z), before mux select.
	first := func(id int) int {
		for i, s := range g.sets {
			if s.id == id {
				if s.state != "high-z" {
					t.Errorf("first set of %d = %q, want high-z (parked off)", id, s.state)
				}
				return i
			}
		}
		t.Fatalf("pin %d never set", id)
		return -1
	}
	muxIdx := indexOf(g.setIDs(), gpioMuxSelect)
	if first(gpioSpiVcc) > muxIdx || first(gpioSpiLines) > muxIdx {
		t.Errorf("bus parked after mux change; order = %v", g.setIDs())
	}
}

func TestBenchRackFlashEnablesSwitchWhenConfigured(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	b := testBenchRack(t, &fakeRunner{}, g)
	b.board.powerSwitches = true
	fw := writeSized(t, int(b.board.host.sizeBytes))
	if err := b.FlashWrite(FlashHost, fw, false); err != nil {
		t.Fatal(err)
	}
	// The host branch switch is enabled high at some point during the flash.
	enabled := false
	for _, s := range g.sets {
		if s.id == gpioEnHost && s.state == "high" {
			enabled = true
		}
	}
	if !enabled {
		t.Error("powerSwitches on: host branch E_GPA was never enabled high")
	}
}

func TestBenchRackFlashPowersRailBeforeSwitch(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	b := testBenchRack(t, &fakeRunner{}, g)
	fw := writeSized(t, int(b.board.host.sizeBytes))
	if err := b.FlashWrite(FlashHost, fw, false); err != nil {
		t.Fatal(err)
	}

	// idx returns the position of the first set matching (id, state), or -1.
	idx := func(id int, state string) int {
		for i, s := range g.sets {
			if s.id == id && s.state == state {
				return i
			}
		}
		return -1
	}
	vccOn := idx(gpioSpiVcc, "low")
	switchOn := idx(gpioEnHost, "high")
	switchOff := idx(gpioEnHost, "low")
	vccOff := idx(gpioSpiVcc, "high-z")
	if vccOn < 0 || switchOn < 0 || switchOff < 0 || vccOff < 0 {
		t.Fatalf("missing a power set: vccOn=%d switchOn=%d switchOff=%d vccOff=%d", vccOn, switchOn, switchOff, vccOff)
	}
	// Power up: the RTE Vcc rail comes on before the load switch closes.
	if vccOn > switchOn {
		t.Errorf("Vcc on at %d, after the load switch at %d, want the rail up first", vccOn, switchOn)
	}
	// Power down: the load switch opens before the RTE Vcc rail drops.
	if switchOff > vccOff {
		t.Errorf("load switch off at %d, after Vcc at %d, want the switch open first", switchOff, vccOff)
	}
}

func TestBenchRackFlashSizeCheck(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	b := testBenchRack(t, &fakeRunner{}, g)
	fw := writeSized(t, int(b.board.host.sizeBytes)-1)

	if err := b.FlashWrite(FlashHost, fw, false); err == nil {
		t.Fatal("FlashWrite should reject a wrong-sized image without --force")
	}
	if err := b.FlashWrite(FlashHost, fw, true); err != nil {
		t.Errorf("FlashWrite --force should skip the size check, got %v", err)
	}
}

func TestBenchRackFlashRestoresOnError(t *testing.T) {
	g := &fakeGPIO{pins: map[int]rte.Pin{
		gpioEnBMC: {Direction: "out"}, gpioEnHost: {Direction: "out"}, gpioPowerLED: {State: 0},
	}}
	runner := &fakeRunner{streamErr: errors.New("flashrom exploded")}
	b := testBenchRack(t, runner, g)
	fw := writeSized(t, int(b.board.host.sizeBytes))

	if err := b.FlashWrite(FlashHost, fw, false); err == nil {
		t.Fatal("FlashWrite should surface a flashrom failure")
	}
	// The bus must be de-energized even on failure: last Vcc and lines sets off.
	lastState := func(id int) string {
		state := ""
		for _, s := range g.sets {
			if s.id == id {
				state = s.state
			}
		}
		return state
	}
	if lastState(gpioSpiLines) != "high-z" || lastState(gpioSpiVcc) != "high-z" {
		t.Errorf("after failure lines=%q vcc=%q, want both high-z", lastState(gpioSpiLines), lastState(gpioSpiVcc))
	}
	// The mux must be disabled (enable high) again even after a failed flash.
	if lastState(gpioMuxEnable) != "high" {
		t.Errorf("after failure mux enable=%q, want high (mux disabled)", lastState(gpioMuxEnable))
	}
}

func TestBenchRackFlashStatusAndAbortNotImplemented(t *testing.T) {
	b := testBenchRack(t, &fakeRunner{}, &fakeGPIO{})
	if _, err := b.FlashStatus(FlashHost); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("FlashStatus = %v, want ErrNotImplemented", err)
	}
	if err := b.FlashAbort(FlashHost); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("FlashAbort = %v, want ErrNotImplemented", err)
	}
}

func TestBenchRackUnknownBoardErrors(t *testing.T) {
	driver := newBenchRack(&fakeRunner{}, Config{Board: "no-such-board"})
	if _, err := driver.PowerState(); err == nil || !strings.Contains(err.Error(), "no-such-board") {
		t.Errorf("PowerState with unknown board = %v, want an error naming the board", err)
	}
}

// curlCalls returns the joined argv of every recorded curl call, in order.
func curlCalls(runner *fakeRunner) []string {
	var out []string
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "curl" {
			out = append(out, strings.Join(call, " "))
		}
	}
	return out
}

func TestBenchRackACStateReadsTasmota(t *testing.T) {
	for _, tc := range []struct {
		body string
		want Power
	}{
		{`{"POWER":"ON"}`, PowerOn},
		{`{"POWER":"OFF"}`, PowerOff},
	} {
		runner := &fakeRunner{handler: func(int, []string) (string, error) { return tc.body, nil }}
		b := testBenchRack(t, runner, &fakeGPIO{})
		got, err := b.ACPowerState()
		if err != nil {
			t.Fatal(err)
		}
		if got.Power != tc.want {
			t.Errorf("ACPowerState from %s = %v, want %v", tc.body, got.Power, tc.want)
		}
		calls := curlCalls(runner)
		if len(calls) != 1 || !strings.Contains(calls[0], defaultTasmotaIP) || !strings.Contains(calls[0], "cmnd=Power") {
			t.Errorf("ACPowerState curl calls = %v, want one read of the default Tasmota", calls)
		}
	}
}

func TestBenchRackSetACPowerSwitchesAndConfirms(t *testing.T) {
	runner := &fakeRunner{handler: func(_ int, argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "Power%20ON") {
			return `{"POWER":"ON"}`, nil
		}
		return `{"POWER":"OFF"}`, nil
	}}
	b := testBenchRack(t, runner, &fakeGPIO{})
	if err := b.SetACPower(PowerOn); err != nil {
		t.Fatal(err)
	}
	calls := curlCalls(runner)
	if len(calls) != 1 || !strings.Contains(calls[0], "Power%20ON") {
		t.Errorf("SetACPower(on) curl calls = %v, want one Power ON switch", calls)
	}
}

func TestBenchRackSetACPowerErrorsOnMismatch(t *testing.T) {
	// The plug reports the opposite of what was requested (e.g. relay stuck).
	runner := &fakeRunner{handler: func(int, []string) (string, error) { return `{"POWER":"OFF"}`, nil }}
	b := testBenchRack(t, runner, &fakeGPIO{})
	if err := b.SetACPower(PowerOn); err == nil {
		t.Fatal("SetACPower should error when the Tasmota does not reach the requested state")
	}
}

func TestBenchRackACPowerCycleOffThenOn(t *testing.T) {
	var order []string
	runner := &fakeRunner{handler: func(_ int, argv []string) (string, error) {
		j := strings.Join(argv, " ")
		switch {
		case strings.Contains(j, "Power%20OFF"):
			order = append(order, "off")
			return `{"POWER":"OFF"}`, nil
		case strings.Contains(j, "Power%20ON"):
			order = append(order, "on")
			return `{"POWER":"ON"}`, nil
		}
		return `{"POWER":"OFF"}`, nil
	}}
	b := testBenchRack(t, runner, &fakeGPIO{})
	if err := b.ACPowerCycle(); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "off" || order[1] != "on" {
		t.Errorf("AC cycle order = %v, want [off on]", order)
	}
}

func TestBenchRackACNotImplementedWithoutTasmota(t *testing.T) {
	b := testBenchRack(t, &fakeRunner{}, &fakeGPIO{})
	b.tasmota = nil
	if _, err := b.ACPowerState(); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("ACPowerState = %v, want ErrNotImplemented", err)
	}
	if err := b.SetACPower(PowerOn); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("SetACPower = %v, want ErrNotImplemented", err)
	}
	if err := b.ACPowerCycle(); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("ACPowerCycle = %v, want ErrNotImplemented", err)
	}
}

func TestBenchRackTasmotaIPOverride(t *testing.T) {
	runner := &fakeRunner{handler: func(int, []string) (string, error) { return `{"POWER":"OFF"}`, nil }}
	b := newBenchRack(runner, Config{TasmotaIP: "10.1.2.3"}).(*benchRack)
	b.gpio = &fakeGPIO{}
	if _, err := b.ACPowerState(); err != nil {
		t.Fatal(err)
	}
	calls := curlCalls(runner)
	if len(calls) != 1 || !strings.Contains(calls[0], "10.1.2.3") {
		t.Errorf("override curl calls = %v, want the configured IP 10.1.2.3", calls)
	}
}

// tasmotaReplies answers a Tasmota read or switch with the state the command
// asked for, so an AC operation confirms and returns.
func tasmotaReplies() func(int, []string) (string, error) {
	return func(_ int, argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "Power%20OFF") {
			return `{"POWER":"OFF"}`, nil
		}
		return `{"POWER":"ON"}`, nil
	}
}

// benchCommand is one exported driver operation, with the power-LED state it
// needs to run without erroring.
type benchCommand struct {
	name string
	led  uint
	run  func(*benchRack) error
}

// powerCommands are the operations that do not drive the flash bus themselves.
var powerCommands = []benchCommand{
	{"PowerState", 1, func(b *benchRack) error { _, err := b.PowerState(); return err }},
	{"SetPower", 0, func(b *benchRack) error { return b.SetPower(PowerOn) }},
	{"PowerReset", 1, func(b *benchRack) error { return b.PowerReset() }},
	{"HardReset", 1, func(b *benchRack) error { return b.HardReset() }},
	{"Console", 1, func(b *benchRack) error { return b.Console() }},
	{"ConsoleSOL", 1, func(b *benchRack) error {
		return b.ConsoleSOL(BMC{IP: "192.168.50.11", User: "admin", Password: "Administrator"})
	}},
	{"ConsoleUART1", 1, func(b *benchRack) error { return b.ConsoleUART1() }},
	{"ACPowerState", 1, func(b *benchRack) error { _, err := b.ACPowerState(); return err }},
	{"SetACPower", 1, func(b *benchRack) error { return b.SetACPower(PowerOn) }},
	{"ACPowerCycle", 1, func(b *benchRack) error { return b.ACPowerCycle() }},
}

func TestBenchRackPowerCommandsParkFlashBus(t *testing.T) {
	// A flash left energized by a killed run or an RTE reboot must not survive
	// into the next command: every operation parks the bus and the load switches
	// before it touches the bench.
	for _, tc := range powerCommands {
		g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: tc.led}}}
		b := testBenchRack(t, &fakeRunner{handler: tasmotaReplies()}, g)
		if err := tc.run(b); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !g.parkedBus(b.board.host.muxSelect) {
			t.Errorf("%s did not park the flash bus, sets = %v", tc.name, g.sets)
		}
	}
}

func TestBenchRackParksOncePerCommand(t *testing.T) {
	// Parking writes to the mux, so it belongs once at the start of a command.
	// A command that parks per power reading would rewrite the mux throughout
	// the poll loop.
	for _, tc := range powerCommands {
		g := &fakeGPIO{pins: map[int]rte.Pin{gpioPowerLED: {State: tc.led}}}
		b := testBenchRack(t, &fakeRunner{handler: tasmotaReplies()}, g)
		if err := tc.run(b); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := g.countSets(gpioMuxEnable); got != 1 {
			t.Errorf("%s parked %d times, want 1", tc.name, got)
		}
	}
}

func TestBenchRackACWithoutTasmotaLeavesGPIOUntouched(t *testing.T) {
	// An unsupported operation reports ErrNotImplemented without parking, so it
	// leaves the bench exactly as it found it.
	for _, tc := range []benchCommand{
		{"ACPowerState", 1, func(b *benchRack) error { _, err := b.ACPowerState(); return err }},
		{"SetACPower", 1, func(b *benchRack) error { return b.SetACPower(PowerOn) }},
		{"ACPowerCycle", 1, func(b *benchRack) error { return b.ACPowerCycle() }},
	} {
		g := &fakeGPIO{}
		b := testBenchRack(t, &fakeRunner{}, g)
		b.tasmota = nil
		if err := tc.run(b); !errors.Is(err, ErrNotImplemented) {
			t.Fatalf("%s = %v, want ErrNotImplemented", tc.name, err)
		}
		if len(g.sets) != 0 {
			t.Errorf("%s on a board without AC control set %v, want no GPIO writes", tc.name, g.sets)
		}
	}
}

// indexOf returns the index of v in s, or len(s) if absent (so absent sorts last).
func indexOf(s []int, v int) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return len(s)
}
