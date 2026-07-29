// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/zarhus/benchctl/exec"
	"github.com/zarhus/benchctl/internal/rte"
	"github.com/zarhus/benchctl/internal/tasmota"
)

// BenchRack is the RTE-based bench. The driver controls GPIO, the SPI mux, and
// power over the RTE REST API, and runs flashrom over SSH. flashrom runs
// synchronously within a flash command, so there is no update state to query or
// abort between invocations. FlashStatus and FlashAbort report ErrNotImplemented.

func init() {
	Register(Spec{
		Name:            "benchrack",
		DefaultUser:     "root",
		DefaultPassword: "meta-rte",
		New:             newBenchRack,
	})
}

// RTE logical GPIO ids. Ids 1-3 and 8-9 are open-collector. Ids 13-16 are
// push-pull J10 (expander) pins, and 17-19 push-pull native pins on J1. See
// docs/projects/benchrack-dual-spi for the wiring.
const (
	gpioSpiLines   = 1  // SPI CS/SCLK/MISO/MOSI enable: low on, high-z off
	gpioSpiVoltage = 2  // SPI Vcc level: low 3.3V, high-z 1.8V
	gpioSpiVcc     = 3  // SPI Vcc enable: low on, high-z off
	gpioResetBtn   = 8  // DUT reset button
	gpioPowerBtn   = 9  // DUT power button
	gpioMuxEnable  = 13 // GPIO400, 2:1 mux enable (active-low): high isolates both flashes, low routes the selected branch
	gpioEnHost     = 14 // E_GPA1, host load-switch enable
	gpioEnBMC      = 15 // E_GPA2, BMC load-switch enable: high on, low off
	gpioMuxSelect  = 16 // 2:1 mux select
	gpioPowerLED   = 17 // DUT power LED readback (led1, J1 pin 1 / GPIO12): high on, low off
)

// gpioController is the subset of the RTE REST client the driver uses, kept as
// an interface so tests can substitute a fake.
type gpioController interface {
	Get(id int) (rte.Pin, error)
	Set(id int, state string, holdSecs int) error
}

// targetCfg describes one flash on a board.
type targetCfg struct {
	chip      string // flashrom -c value, empty means auto-detect
	voltage   string // "3.3V" or "1.8V"
	sizeBytes int64  // expected firmware size
	muxSelect string // mux-select level ("high"/"low") that routes to this flash
	enable    int    // load-switch enable pin id for this flash
}

// consoleCfg is where the DUT serial console is reached. The RTE runs ser2net,
// which exports the console over telnet on a TCP port.
type consoleCfg struct {
	port int
}

// board is the per-board configuration.
type board struct {
	host          targetCfg
	bmc           targetCfg
	powerSwitches bool // enable the selected branch's load switch during a flash
	console       consoleCfg
	tasmotaIP     string // AC-control Tasmota plug address; empty means the board has no AC control
}

const defaultBoard = "asrock-turin"

// defaultTasmotaIP is where the plug sits on the RTE's isolated wifi AP: the
// single-address DHCP pool always hands it this address.
const defaultTasmotaIP = "192.168.66.50"

// remoteFirmware is where FlashWrite stages the image on the RTE. It lives on
// /data (persistent storage) rather than /var/tmp (tmpfs): the RTE has little
// free RAM, and flashrom already holds roughly twice the flash size in memory to
// write and verify, so staging the image in RAM as well can exhaust it on a
// 64 MB flash. The name is fixed, so each flash overwrites the last.
const remoteFirmware = "/data/rom.bin"

// remoteReadback is where FlashRead has flashrom write the chip image on the RTE
// before it is copied back to the caller. It lives on /data for the same reason
// as remoteFirmware, and its fixed name means each read overwrites the last.
const remoteReadback = "/data/readback.bin"

var boards = map[string]board{
	"asrock-turin": {
		host:          targetCfg{chip: "W25Q256JV_Q", voltage: "3.3V", sizeBytes: 32 * 1024 * 1024, muxSelect: "low", enable: gpioEnHost},
		bmc:           targetCfg{chip: "", voltage: "3.3V", sizeBytes: 64 * 1024 * 1024, muxSelect: "high", enable: gpioEnBMC},
		powerSwitches: true,
		// TODO(bring-up): confirm the mux-select polarity against the hardware.
		// ser2net.yaml maps /dev/ttyS1 (115200n81) to telnet port 13541.
		console:   consoleCfg{port: 13541},
		tasmotaIP: defaultTasmotaIP,
	},
}

func boardNames() string {
	names := make([]string, 0, len(boards))
	for name := range boards {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("%v", names)
}

type benchRack struct {
	runner   exec.Runner
	gpio     gpioController
	tasmota  *tasmota.Client // AC power control; nil when the board declares no Tasmota
	board    board
	boardErr error // set when the requested board is unknown
	idled    bool  // parkOff has driven the bus to its off state, see ensureIdle

	spiDev   string
	spiSpeed int
	settle   time.Duration // delay after energizing SPI Vcc and lines

	pollInterval time.Duration
	pollTimeout  time.Duration
	powerOnHold  int           // power-button pulse, seconds, to power on
	powerOffHold int           // power-button hold, seconds, to force S5 off
	resetHold    int           // reset-button pulse, seconds
	acCycleDelay time.Duration // off-to-on gap during an AC power-cycle
	// powerCycleWait is the gap a reset leaves between releasing the force-off
	// press and the power-on press, so the platform settles in S5 and reads the
	// on-press as a distinct press rather than a continuation of the hold.
	powerCycleWait time.Duration

	progress io.Writer
}

func newBenchRack(runner exec.Runner, cfg Config) Platform {
	name := cfg.Board
	if name == "" {
		name = defaultBoard
	}
	bench := &benchRack{
		runner:         runner,
		gpio:           rte.New(runner.Host()),
		spiDev:         "/dev/spidev1.0",
		spiSpeed:       16000,
		settle:         2 * time.Second,
		pollInterval:   time.Second,
		pollTimeout:    60 * time.Second,
		powerOnHold:    1,
		powerOffHold:   6,
		resetHold:      1,
		acCycleDelay:   2 * time.Second,
		powerCycleWait: 3 * time.Second,
		progress:       os.Stderr,
	}
	brd, ok := boards[name]
	if !ok {
		bench.boardErr = fmt.Errorf("unknown board %q (known: %s)", name, boardNames())
		return bench
	}
	bench.board = brd
	// A Tasmota address from the config overrides the board default, and enables
	// AC control on a board that declares none. When neither supplies one, the
	// board has no AC control and the AC methods report ErrNotImplemented.
	ip := cfg.TasmotaIP
	if ip == "" {
		ip = brd.tasmotaIP
	}
	if ip != "" {
		bench.tasmota = tasmota.New(runner, ip)
	}
	return bench
}

func (bench *benchRack) target(t FlashTarget) (targetCfg, error) {
	switch t {
	case FlashHost:
		return bench.board.host, nil
	case FlashBMC:
		return bench.board.bmc, nil
	default:
		return targetCfg{}, fmt.Errorf("unknown flash target %v", t)
	}
}

// ensureIdle brings the bench to its resting state before an operation touches
// it, and does so once per driver instance. Parking writes to the mux, so
// repeating it inside the power poll would rewrite the mux on every reading.
// Because it is a no-op after the first call, an operation can park up front and
// still delegate to another exported one.
//
// Every exported operation calls it first. deenergize runs only as a defer, so a
// flash killed mid-run or interrupted by an RTE reboot leaves the SPI rail up and
// a load switch closed. Parking here means a later command cannot energize the
// board while the RTE still drives a flash.
func (bench *benchRack) ensureIdle() error {
	if bench.boardErr != nil {
		return bench.boardErr
	}
	if bench.idled {
		return nil
	}
	return bench.parkOff()
}

func (bench *benchRack) PowerState() (PowerStatus, error) {
	if err := bench.ensureIdle(); err != nil {
		return PowerStatus{}, err
	}
	pin, err := bench.gpio.Get(gpioPowerLED)
	if err != nil {
		return PowerStatus{}, err
	}
	power := PowerOff
	if pin.Asserted() {
		power = PowerOn
	}
	return PowerStatus{Power: power}, nil
}

func (bench *benchRack) SetPower(target Power) error {
	if err := bench.ensureIdle(); err != nil {
		return err
	}
	cur, err := bench.PowerState()
	if err != nil {
		return err
	}
	if cur.Power == target {
		return nil
	}
	fmt.Fprintf(bench.progress, "Powering host %s...\n", target)
	hold := bench.powerOnHold
	if target == PowerOff {
		hold = bench.powerOffHold
	}
	if err := bench.gpio.Set(gpioPowerBtn, "low", hold); err != nil {
		return err
	}
	return bench.pollPower(target)
}

func (bench *benchRack) pollPower(target Power) error {
	start := time.Now()
	for {
		cur, err := bench.PowerState()
		if err != nil {
			return err
		}
		if cur.Power == target {
			return nil
		}
		if time.Since(start) >= bench.pollTimeout {
			return fmt.Errorf("timed out after %s waiting for host %s", bench.pollTimeout, target)
		}
		time.Sleep(bench.pollInterval)
	}
}

func (bench *benchRack) PowerReset() error {
	if err := bench.ensureIdle(); err != nil {
		return err
	}
	fmt.Fprintln(bench.progress, "Power-cycling host...")
	if err := bench.SetPower(PowerOff); err != nil {
		return err
	}
	// The force-off holds the power button down until the RTE releases it. Wait
	// for the button to release and the platform to settle in S5 before pressing
	// again, so the on-press is a distinct press the platform acts on rather than
	// a continuation of the hold, which would keep it off.
	time.Sleep(bench.powerCycleWait)
	return bench.SetPower(PowerOn)
}

func (bench *benchRack) HardReset() error {
	if err := bench.ensureIdle(); err != nil {
		return err
	}
	status, err := bench.PowerState()
	if err != nil {
		return err
	}
	if status.Power != PowerOn {
		return fmt.Errorf("host is %s, use a hard reset only when the host is powered on and a normal reset does not work", status)
	}
	fmt.Fprintln(bench.progress, "Resetting host via reset button...")
	return bench.gpio.Set(gpioResetBtn, "low", bench.resetHold)
}

// acClient returns the Tasmota client, or ErrNotImplemented when the board has
// no AC control configured. It reports an unsupported board before parking, so
// an operation the platform cannot perform leaves the bench untouched.
func (bench *benchRack) acClient() (*tasmota.Client, error) {
	if bench.boardErr != nil {
		return nil, bench.boardErr
	}
	if bench.tasmota == nil {
		return nil, ErrNotImplemented
	}
	if err := bench.ensureIdle(); err != nil {
		return nil, err
	}
	return bench.tasmota, nil
}

// ACPowerState reports whether mains is applied to the PSU. It says nothing
// about whether the host booted: reading the power LED (PowerState) remains the
// real host-power signal.
func (bench *benchRack) ACPowerState() (PowerStatus, error) {
	client, err := bench.acClient()
	if err != nil {
		return PowerStatus{}, err
	}
	on, err := client.Power()
	if err != nil {
		return PowerStatus{}, err
	}
	return PowerStatus{Power: acPower(on)}, nil
}

// SetACPower switches the mains feed and confirms the Tasmota reached the
// requested state. It does not press the power button or wait on the host: with
// AC-recovery set to power on after loss, applying mains boots the DUT on its
// own, otherwise the host stays in S5 and a separate power on is needed.
func (bench *benchRack) SetACPower(target Power) error {
	client, err := bench.acClient()
	if err != nil {
		return err
	}
	fmt.Fprintf(bench.progress, "Switching AC %s...\n", target)
	on, err := client.SetPower(target == PowerOn)
	if err != nil {
		return err
	}
	if got := acPower(on); got != target {
		return fmt.Errorf("AC did not switch %s, Tasmota reports %s", target, got)
	}
	return nil
}

// ACPowerCycle drops mains and reapplies it after a short delay, forcing a cold
// AC kill the front-panel button cannot.
func (bench *benchRack) ACPowerCycle() error {
	if _, err := bench.acClient(); err != nil {
		return err
	}
	fmt.Fprintln(bench.progress, "Power-cycling AC...")
	if err := bench.SetACPower(PowerOff); err != nil {
		return err
	}
	time.Sleep(bench.acCycleDelay)
	return bench.SetACPower(PowerOn)
}

// acPower maps the Tasmota on/off reading to a Power value.
func acPower(on bool) Power {
	if on {
		return PowerOn
	}
	return PowerOff
}

func (bench *benchRack) Console() error {
	if err := bench.ensureIdle(); err != nil {
		return err
	}
	port := strconv.Itoa(bench.board.console.port)
	fmt.Fprintf(bench.progress, "Attaching to host console via ser2net on port %s. Detach with CTRL+] then \"quit\".\n", port)
	return bench.runner.RunInteractive("telnet", "localhost", port)
}

// withFlashBus parks the bus, powers the board off so the RTE drives the flash,
// energizes the selected branch, and runs fn with that flash on the SPI bus. It
// always de-energizes the bus afterward, even when fn fails.
func (bench *benchRack) withFlashBus(target FlashTarget, fn func(tgt targetCfg) error) error {
	if bench.boardErr != nil {
		return bench.boardErr
	}
	tgt, err := bench.target(target)
	if err != nil {
		return err
	}

	// Park the load switches and the bus off before touching voltage or the mux,
	// calling parkOff rather than ensureIdle so a flash never trusts the state an
	// earlier one left behind.
	if err := bench.parkOff(); err != nil {
		return err
	}

	// The board is off, so the RTE powers the flash.
	if err := bench.SetPower(PowerOff); err != nil {
		return err
	}

	// Energize the selected branch, and always return to idle afterward.
	defer bench.deenergize()

	voltage := "low" // 3.3V
	if tgt.voltage == "1.8V" {
		voltage = "high-z"
	}
	if err := bench.gpio.Set(gpioSpiVoltage, voltage, 0); err != nil {
		return err
	}
	if err := bench.gpio.Set(gpioMuxSelect, tgt.muxSelect, 0); err != nil {
		return err
	}
	// Enable the mux (active-low) once the branch is selected, routing the
	// selected flash onto the bus. parkOff and deenergize leave it disabled.
	if err := bench.gpio.Set(gpioMuxEnable, "low", 0); err != nil {
		return err
	}
	// Bring up the SPI Vcc rail before closing the branch load switch, so the
	// switch never ties a de-energized rail to a flash that another supply (the
	// motherboard) may still hold at voltage and back-drive the rail.
	if err := bench.gpio.Set(gpioSpiVcc, "low", 0); err != nil {
		return err
	}
	if bench.board.powerSwitches {
		if err := bench.gpio.Set(tgt.enable, "high", 0); err != nil {
			return err
		}
	}
	time.Sleep(bench.settle)
	if err := bench.gpio.Set(gpioSpiLines, "low", 0); err != nil {
		return err
	}
	time.Sleep(bench.settle)

	return fn(tgt)
}

func (bench *benchRack) FlashProbe(target FlashTarget) error {
	return bench.withFlashBus(target, func(tgt targetCfg) error {
		return bench.flashrom(flashProbe, tgt.chip, "")
	})
}

func (bench *benchRack) FlashRead(target FlashTarget, outPath string) error {
	return bench.withFlashBus(target, func(tgt targetCfg) error {
		remote, fetch, err := bench.runner.Pull(outPath, remoteReadback)
		if err != nil {
			return err
		}
		if err := bench.flashrom(flashRead, tgt.chip, remote); err != nil {
			return err
		}
		return fetch()
	})
}

func (bench *benchRack) FlashWrite(target FlashTarget, fw string, force bool) error {
	if bench.boardErr != nil {
		return bench.boardErr
	}
	tgt, err := bench.target(target)
	if err != nil {
		return err
	}
	info, err := os.Stat(fw)
	if err != nil {
		return err
	}
	if info.Size() != tgt.sizeBytes && !force {
		return fmt.Errorf("%s is %d bytes, expected %d. Use --force to override", fw, info.Size(), tgt.sizeBytes)
	}
	return bench.withFlashBus(target, func(tgt targetCfg) error {
		remote, cleanup, err := bench.runner.Push(fw, remoteFirmware)
		if err != nil {
			return err
		}
		defer func() { _ = cleanup() }()
		return bench.flashrom(flashWrite, tgt.chip, remote)
	})
}

// parkOff drives the load-switch enables and the SPI bus to a known-off state.
// It exports the E_GPA pins as outputs when they are still high-Z inputs from
// boot, and turns off SPI Vcc or lines if a previous run left them on. On success
// it records the bench as idle, which is what lets ensureIdle skip a second park.
func (bench *benchRack) parkOff() error {
	for _, id := range []int{gpioEnBMC, gpioEnHost} {
		pin, err := bench.gpio.Get(id)
		if err != nil {
			return err
		}
		if pin.Direction != "out" || pin.Asserted() {
			if err := bench.gpio.Set(id, "low", 0); err != nil {
				return err
			}
		}
	}
	for _, id := range []int{gpioSpiVcc, gpioSpiLines} {
		pin, err := bench.gpio.Get(id)
		if err != nil {
			return err
		}
		if pin.Asserted() {
			if err := bench.gpio.Set(id, "high-z", 0); err != nil {
				return err
			}
		}
	}
	// Point the mux select at the host branch, the idle default. Resting the
	// select on the BMC branch freezes the BMC even with the mux disabled, so the
	// select must never sit there while idle.
	if err := bench.gpio.Set(gpioMuxSelect, bench.board.host.muxSelect, 0); err != nil {
		return err
	}
	// Drive the mux enable high (active-low, so disabled) to isolate both flashes
	// while idle, which is the default state whenever a flash is not in progress.
	if err := bench.gpio.Set(gpioMuxEnable, "high", 0); err != nil {
		return err
	}
	bench.idled = true
	return nil
}

// deenergize returns the bus and switches to idle. It runs in a defer, so it is
// best-effort: the flash result is what the caller reports.
func (bench *benchRack) deenergize() {
	_ = bench.gpio.Set(gpioSpiLines, "high-z", 0)
	// Open the load switches before dropping the SPI Vcc rail, isolating the
	// flash from the rail before it de-energizes so no external supply can drive
	// current back into it.
	_ = bench.gpio.Set(gpioEnBMC, "low", 0)
	_ = bench.gpio.Set(gpioEnHost, "low", 0)
	_ = bench.gpio.Set(gpioSpiVcc, "high-z", 0)
	_ = bench.gpio.Set(gpioMuxEnable, "high", 0)
	// Return the mux select to the host branch, the idle default, so a finished
	// BMC flash never leaves the select resting on the BMC and freezing it.
	_ = bench.gpio.Set(gpioMuxSelect, bench.board.host.muxSelect, 0)
	_ = bench.gpio.Set(gpioSpiVoltage, "high-z", 0)
}

// flashOp is a flashrom operation: probe (detect the chip only), read, or write.
type flashOp int

const (
	flashProbe flashOp = iota
	flashRead
	flashWrite
)

// flashrom runs one flashrom operation over SSH, echoing output as it streams,
// and fails if flashrom exits non-zero. file is the bench-side image path for a
// read or a write, and is ignored for a probe.
func (bench *benchRack) flashrom(op flashOp, chip, file string) error {
	argv := []string{"flashrom", "-p", fmt.Sprintf("linux_spi:dev=%s,spispeed=%d", bench.spiDev, bench.spiSpeed)}
	if chip != "" {
		argv = append(argv, "-c", chip)
	}
	var start, done string
	switch op {
	case flashProbe:
		start, done = "Probing flash...", "Probe complete."
	case flashRead:
		argv = append(argv, "-r", file)
		start, done = "Reading flash...", "Read complete."
	case flashWrite:
		argv = append(argv, "-w", file)
		start, done = "Flashing...", "Flash complete."
	}

	fmt.Fprintln(bench.progress, start)
	stdout, wait, err := bench.runner.Stream(argv...)
	if err != nil {
		return err
	}
	// Forward flashrom's output byte for byte rather than line by line, so an
	// in-place step (a "Reading old flash chip contents... " prefix that flashrom
	// completes with "done." only after the read) shows as it happens instead of
	// appearing whole once its closing newline arrives.
	if _, err := io.Copy(bench.progress, stdout); err != nil {
		return err
	}
	if err := wait(); err != nil {
		return fmt.Errorf("flashrom failed: %w", err)
	}
	fmt.Fprintln(bench.progress, done)
	return nil
}

func (bench *benchRack) FlashStatus(FlashTarget) (Status, error) {
	return Status{}, ErrNotImplemented
}

func (bench *benchRack) FlashAbort(FlashTarget) error {
	return ErrNotImplemented
}
