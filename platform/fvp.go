// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zarhus/benchctl/exec"
	"github.com/zarhus/benchctl/internal/iris"
)

// FVP is a platform driver for the real Arm RD-V3 Fixed Virtual Platform
// (FVP_RD_V3): "power" is the model process's lifetime, "console" is a
// telnet connection straight to its own console UART terminal (ns_uart0),
// and "flash" records which image the next power-on boots. There is no
// external launcher script: this driver knows how to invoke the model
// itself, the same way benchrack.go knows how to drive BenchRack's GPIO and
// SPI mux directly rather than shelling out to a board-specific script.
//
// It exists so a port can be driven through the same hardware-gated path a
// real bench uses (confirmation before a hardware milestone, one benchctl
// command per step) before there is a real bench to run it on.
func init() {
	Register(Spec{
		Name: "fvp",
		New:  newFVP,
	})
}

// profile is the per-board configuration. A second FVP-modelled reference
// design would add an entry here rather than a new driver.
type profile struct {
	bootTimeout          time.Duration // how long a power on/off waits before giving up
	pollEvery            time.Duration
	consolePort          int           // default telnet port for the console UART (start_port on the real model)
	irisPort             int           // default port for the Iris debug/control server (--iris-port on the real model)
	irisEffectTimeout    time.Duration // how long waitForIrisRunState waits for a Stop/Run request to actually take effect
	consoleAttachTimeout time.Duration // how long Console silently retries connecting before reporting failure
}

var profiles = map[string]profile{
	// Deliberately not 5000, the RD-V3 FVP's own default for every UART
	// terminal's start_port: every other terminal also defaults to wanting
	// 5000 and auto-increments from there on collision (confirmed against the
	// real model, this happens regardless of start_telnet), so requesting
	// 5000 for ns_uart0 too risks landing on whatever port the others left
	// free rather than the one actually requested. 6000 sits well clear of
	// that whole cluster (observed occupying 5000-5012 with every terminal
	// present), so the request reliably succeeds and Console can just trust
	// it, with no need to read the model's own announcement back out of its
	// process log to find out where it actually landed. 7300 for Iris is
	// clear of that same cluster and also clear of 7100-7109, the range
	// Iris/Python/iris/iris.py's own client scans by default when asked to
	// autodetect a port - pinning our own request avoids a coincidental
	// collision with anyone using that default on the same machine.
	"rdv3": {bootTimeout: 20 * time.Second, pollEvery: 200 * time.Millisecond, consolePort: 6000, irisPort: 7300, irisEffectTimeout: 10 * time.Second, consoleAttachTimeout: 5 * time.Second},
}

const defaultProfile = "rdv3"

// norFlashSize is the size of the two dummy NOR flash images the platform
// expects present at ros.board.flashloader{1,2}: 64MiB, matching
// model-scripts/sgi/sgi_common_util.sh's create_nor_flash_image. Their
// content does not matter - confirmed empirically, a model launched without
// them exits shortly after entering run mode regardless of what firmware
// image is flashed, and one launched with these (even blank) stays up.
const norFlashSize = 64 * 1024 * 1024

// fvpPlatform is the driver instance. There is exactly one control surface -
// the local model process - so unlike benchrack there is no separate REST or
// IPMI client to hold, and no board-specific GPIO wiring.
type fvpPlatform struct {
	runner exec.Runner

	modelBin string // path to the FVP_RD_V3 binary (BENCHCTL_FVP_MODEL)
	board    string // selected board profile (e.g. "rdv3") - also the name a real build's own output/<board> tree uses, see resolveOutputComponents

	workDir         string // where state lives: pidfile, logs, the recorded boot image
	pidFile         string
	processLog      string // the launched process's own stdout/stderr
	consoleLog      string // the console UART's own transcript (ns_uart0.out_file), written by the model itself
	bootImageRecord string // holds the path of the image (`flash write host`) the next power-on boots: a FIP, or a BL33 image preloaded directly into DRAM
	diskRecord      string // holds the medium+path of the disk image the next power-on attaches, like bootImageRecord
	consolePort     int    // the port requested via terminal_ns_uart0.start_port, and what Console trusts the model actually opened
	irisPort        int    // the port requested via --iris-port for the model's Iris debug/control server

	bootTimeout          time.Duration
	pollEvery            time.Duration
	irisEffectTimeout    time.Duration
	consoleAttachTimeout time.Duration

	// configErr is set at construction when the board, BENCHCTL_FVP_MODEL or
	// BENCHCTL_FVP_DATA is missing or unusable, and returned by every method
	// rather than only the ones that happen to touch the bad field: acting on
	// half-resolved configuration would fail confusingly deeper in, e.g.
	// inside exec.Command with an empty argv.
	configErr error
	progress  io.Writer

	// extraData holds the BENCHCTL_FVP_DATA entries, each a
	// <target>=<file>@<address> value for one more --data option, with the
	// file path made absolute. start appends them after the built-in preloads.
	extraData []string

	startGrace    time.Duration // see the startGrace constant
	shutdownGrace time.Duration // see the irisShutdownGrace constant
	norSize       int           // see the norFlashSize constant
}

func newFVP(runner exec.Runner, cfg Config) Platform {
	name := cfg.Board
	if name == "" {
		name = defaultProfile
	}
	fv := &fvpPlatform{runner: runner, progress: os.Stderr, startGrace: startGrace, shutdownGrace: irisShutdownGrace, norSize: norFlashSize}

	prof, ok := profiles[name]
	if !ok {
		fv.configErr = fmt.Errorf("unknown fvp board %q (known: [%s])", name, defaultProfile)
		return fv
	}
	fv.board = name
	fv.bootTimeout = prof.bootTimeout
	fv.pollEvery = prof.pollEvery
	fv.consolePort = envInt("BENCHCTL_FVP_CONSOLE_PORT", prof.consolePort)
	fv.irisPort = envInt("BENCHCTL_FVP_IRIS_PORT", prof.irisPort)
	fv.irisEffectTimeout = prof.irisEffectTimeout
	fv.consoleAttachTimeout = prof.consoleAttachTimeout

	fv.workDir = envOr("BENCHCTL_FVP_WORKDIR", ".fvp")
	fv.pidFile = filepath.Join(fv.workDir, "model.pid")
	fv.processLog = filepath.Join(fv.workDir, "model-stdout.log")
	fv.consoleLog = filepath.Join(fv.workDir, "console.log")
	fv.bootImageRecord = filepath.Join(fv.workDir, "boot-image")
	fv.diskRecord = filepath.Join(fv.workDir, "disk-image")

	model := os.Getenv("BENCHCTL_FVP_MODEL")
	if model == "" {
		fv.configErr = fmt.Errorf("BENCHCTL_FVP_MODEL is not set: give it the path to the FVP_RD_V3 model binary")
		return fv
	}
	info, err := os.Stat(model)
	if err != nil {
		fv.configErr = fmt.Errorf("BENCHCTL_FVP_MODEL=%s: %w", model, err)
		return fv
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		fv.configErr = fmt.Errorf("BENCHCTL_FVP_MODEL=%s is not an executable file", model)
		return fv
	}
	fv.modelBin = model

	extra, err := parseFVPData(os.Getenv("BENCHCTL_FVP_DATA"))
	if err != nil {
		fv.configErr = err
		return fv
	}
	fv.extraData = extra
	return fv
}

// parseFVPData parses BENCHCTL_FVP_DATA: comma-separated entries in the
// model's own --data form, <target>=<file>@<address>. The entry splits at its
// last '@' and then at the first '='. The target must be non-empty, the
// address must parse as an unsigned integer in any base strconv.ParseUint
// accepts, and the file must exist. A relative file path resolves against the
// current directory. The result holds one --data value per entry, with the
// path made absolute. An empty value yields no entries.
func parseFVPData(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	var out []string
	for _, entry := range strings.Split(value, ",") {
		at := strings.LastIndex(entry, "@")
		if at < 0 {
			return nil, fmt.Errorf("BENCHCTL_FVP_DATA entry %q: want <target>=<file>@<address>", entry)
		}
		head, addr := entry[:at], entry[at+1:]
		target, file, ok := strings.Cut(head, "=")
		if !ok || target == "" || file == "" {
			return nil, fmt.Errorf("BENCHCTL_FVP_DATA entry %q: want <target>=<file>@<address>", entry)
		}
		if _, err := strconv.ParseUint(addr, 0, 64); err != nil {
			return nil, fmt.Errorf("BENCHCTL_FVP_DATA entry %q: bad address %q: %w", entry, addr, err)
		}
		abs, err := filepath.Abs(file)
		if err != nil {
			return nil, fmt.Errorf("BENCHCTL_FVP_DATA entry %q: %w", entry, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("BENCHCTL_FVP_DATA entry %q: %w", entry, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("BENCHCTL_FVP_DATA entry %q: %s is a directory", entry, abs)
		}
		out = append(out, target+"="+abs+"@"+addr)
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func readPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return pid, true
}

func writePID(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// isAlive reports whether pid is running, by sending it the null signal. A
// pid that has been reused by an unrelated process would read as alive too;
// acceptable for a bench standing in for hardware, not for driving real one.
//
// It also matters that this is not fooled by a zombie: kill(pid, 0) reports a
// zombie as alive, since its pid is still allocated. That only misleads this
// driver when it is also the model's parent process - normally not the case,
// since each benchctl invocation is its own process and the model outlives
// it, but true within a single test binary that both starts and stops the
// model. Attempting a non-blocking reap first resolves that: it reaps the
// model itself where this process is its parent, and otherwise defers to the
// plain kill(pid, 0) probe, which is what a separate `power off` invocation
// always falls to.
func isAlive(pid int) bool {
	var status syscall.WaitStatus
	switch wpid, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); {
	case err == nil && wpid == pid:
		return false // just reaped it as our own child: definitely gone
	case err == nil:
		return true // our own child, and still running
	default:
		return syscall.Kill(pid, 0) == nil // not our child: the ordinary probe
	}
}

// PowerState reports whether the model's firmware is currently executing -
// not merely whether the process exists (see ACPowerState for that). AC
// off always reads as power off, since nothing can be executing without a
// process to execute it; AC on defers to Iris (simulationTime_get) to tell
// paused from running, defaulting to "off" (with a Detail explaining why)
// if Iris cannot be reached or queried, rather than erroring outright -
// callers like SetPower's own no-op check need a status even when Iris is
// having a bad moment.
func (fv *fvpPlatform) PowerState() (PowerStatus, error) {
	if fv.configErr != nil {
		return PowerStatus{}, fv.configErr
	}
	acStatus, err := fv.ACPowerState()
	if err != nil {
		return PowerStatus{}, err
	}
	if acStatus.Power != PowerOn {
		return PowerStatus{Power: PowerOff, Detail: "AC off"}, nil
	}
	client, err := iris.Dial(fv.irisAddr(), irisTimeout)
	if err != nil {
		return PowerStatus{Power: PowerOff, Detail: fmt.Sprintf("AC on, iris unreachable: %s", err)}, nil
	}
	defer client.Close()
	running, err := client.IsRunning(irisInstID)
	if err != nil {
		return PowerStatus{Power: PowerOff, Detail: fmt.Sprintf("AC on, iris query failed: %s", err)}, nil
	}
	if !running {
		return PowerStatus{Power: PowerOff, Detail: "AC on, firmware paused"}, nil
	}
	return PowerStatus{Power: PowerOn, Detail: acStatus.Detail}, nil
}

// SetPower starts firmware execution on an already-launched model (see
// SetACPower for launching/killing the process itself) - it never
// launches or kills the process on its own, and PowerOff is not
// implemented (see PowerReset's own doc comment for why, which applies
// here too: pausing and later resuming would need the same in-place Iris
// path that turned out unable to satisfy both "never crashes" and "never
// drops an already-open console connection" at once). PowerOn requires AC
// already on; an AC-off model reports the same "AC is off" error Console
// does, rather than silently launching one, since only `power ac on` is
// supposed to do that.
func (fv *fvpPlatform) SetPower(target Power) error {
	if fv.configErr != nil {
		return fv.configErr
	}
	if target == PowerOff {
		return ErrNotImplemented
	}
	status, err := fv.PowerState()
	if err != nil {
		return err
	}
	if status.Power == target {
		return nil
	}
	return fv.startFirmware()
}

// PowerReset and HardReset are not implemented for FVP.
//
// Restarting firmware execution from its reset vector needs one of two
// mechanisms, and neither can be offered here: Iris's own in-place
// simulation_reset was confirmed live (2026-09-16/17, against a real
// FVP_RD_V3 11.29.35 model) to fail outright ("Failed to reset
// simulation.") while firmware is actively executing (also confirmed
// against Arm's own reference client, so this is the model's own
// constraint, not a benchctl bug) and, less predictably, to go on to
// segfault the model process itself a few seconds after a reset that
// reported success (reproduced twice via journalctl -k, identical crash
// address both times). Killing and relaunching the whole process instead
// (like `power ac cycle`) was tried and confirmed reliable - but
// open-source-firmware-validation opens its own raw telnet connection
// directly (Serial Setup in keywords.robot, via Robot Framework's own
// Telnet library) once, before the first Power On, and keeps it open
// across every later power transition for the rest of the suite; it never
// goes through benchctl's own `console` command, so nothing in this
// driver can reattach it after a relaunch. Since the model always starts
// paused, every single `power on` would have needed that same relaunch,
// breaking that connection on literally the first power-on of every
// suite - not an acceptable trade for a "reset" command.
//
// `power ac off` then `power ac on` (a full relaunch) remains the one
// reliable way to restart firmware from its reset vector on this
// platform - it just cannot be offered under the `reset` name without
// also silently dropping any existing console connection, which is the
// one thing this platform must never do.
func (fv *fvpPlatform) PowerReset() error {
	if fv.configErr != nil {
		return fv.configErr
	}
	return ErrNotImplemented
}

// HardReset is PowerReset on this platform - see its doc comment for why
// neither is implemented.
func (fv *fvpPlatform) HardReset() error {
	if fv.configErr != nil {
		return fv.configErr
	}
	return ErrNotImplemented
}

// startFirmware starts firmware execution over Iris (simulationTime_run)
// on an already-elaborated model - it does not reset first. A model
// launched by SetACPower's own start() is already sitting unexecuted at
// its reset vector (see start's own doc comment: elaborated but paused,
// same as any Iris-server-started model without -R), so there is nothing
// to discard yet; Run alone is a genuine "power on from cold" here, not a
// resume. Since PowerOff and PowerReset are not implemented (see
// PowerReset's own doc comment), this is now the only way firmware
// execution ever starts on this platform, and it never needs
// simulation_reset at all - confirmed live to be fast and to never touch
// the process, so any existing console connection is untouched by it.
func (fv *fvpPlatform) startFirmware() error {
	acStatus, err := fv.ACPowerState()
	if err != nil {
		return err
	}
	if acStatus.Power != PowerOn {
		return fmt.Errorf("AC is off: run 'power ac on' first")
	}
	client, err := iris.Dial(fv.irisAddr(), irisTimeout)
	if err != nil {
		return fmt.Errorf("connect to iris server: %w", err)
	}
	defer client.Close()
	fmt.Fprintln(fv.progress, "Starting firmware execution (Iris simulationTime_run)...")
	if err := client.Run(irisInstID); err != nil {
		return err
	}
	return waitForIrisRunState(true, fv.irisEffectTimeout, fv.pollEvery, fv.progress, func() (bool, error) {
		return client.IsRunning(irisInstID)
	})
}

// waitForIrisRunState polls query (typically a client's IsRunning) until it
// reports want or timeout elapses (irisEffectTimeout for real use - longer
// than callTolerant's own short ackTimeout, since the whole reason that
// exists is that these operations can take a while to actually happen or
// ack on a busy model, so confirming the *effect* needs more patience than
// confirming the *acknowledgment* did). Its whole purpose is to turn a
// Stop/Run request that was accepted but never actually took effect -
// indistinguishable from a successful one at the RPC level once
// callTolerant has decided not to error on a missing acknowledgment - into
// an honest, actionable failure instead of a silent no-op.
//
// query is a closure rather than a fixed *iris.Client so a caller can poll
// over whichever connection makes sense for it, rather than this function
// assuming one particular shape.
//
// Reports progress on progress (typically fv.progress) roughly every
// irisWaitProgressEvery: a wait long enough to need this function at all is
// long enough that silence looks indistinguishable from a hang, and the
// interesting diagnostic question if it never reaches want is whether it
// was still making progress at all (i.e. does the error count keep
// climbing at a steady one-per-irisTimeout rate, meaning the server is
// simply still busy, or does something else change) rather than only the
// final failure.
func waitForIrisRunState(want bool, timeout, pollEvery time.Duration, progress io.Writer, query func() (running bool, err error)) error {
	start := time.Now()
	deadline := start.Add(timeout)
	var lastErr error
	var attempts int
	nextReport := start.Add(irisWaitProgressEvery)
	for {
		attempts++
		running, err := query()
		if err == nil {
			if running == want {
				return nil
			}
		} else {
			lastErr = err
		}
		now := time.Now()
		if progress != nil && !now.Before(nextReport) {
			if lastErr != nil {
				fmt.Fprintf(progress, "... still waiting for running=%v (attempt %d, %s elapsed) - last query error: %v\n", want, attempts, now.Sub(start).Round(time.Second), lastErr)
			} else {
				fmt.Fprintf(progress, "... still waiting for running=%v (attempt %d, %s elapsed) - last query reported running=%v\n", want, attempts, now.Sub(start).Round(time.Second), !want)
			}
			nextReport = now.Add(irisWaitProgressEvery)
		}
		if now.After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("could not confirm the model actually reached running=%v within %s: %w", want, timeout, lastErr)
			}
			return fmt.Errorf("requested but the model still reports running=%v after %s - the request may not have taken effect", !want, timeout)
		}
		time.Sleep(pollEvery)
	}
}

// irisWaitProgressEvery paces waitForIrisRunState's own status lines - often
// enough that a long wait (a real reset in particular) does not look
// indistinguishable from a hang, rare enough not to spam a short one.
const irisWaitProgressEvery = 5 * time.Second

// irisInstID is the instance id Arm's own reference client
// (Iris/Python/iris/debug/Model.py) always uses for the top-level
// simulation object - not something a caller picks per platform.
const irisInstID = 1

// irisAddr is where this model's Iris debug/control server listens, once
// running - see the profile's irisPort comment for why the port is pinned
// rather than autodetected.
func (fv *fvpPlatform) irisAddr() string {
	return "localhost:" + strconv.Itoa(fv.irisPort)
}

// irisTimeout bounds connecting to the Iris server (handshake plus
// registration) and any plain query made on it (IsRunning) - short,
// because those are all lightweight and expected to ack promptly; a
// genuinely unreachable Iris server (model not launched with
// --iris-port, or one whose server for any other reason never answers)
// should fail promptly too, not hang. It does NOT bound
// Run/Stop/Reset/RequestShutdown's wait for their own acknowledgment -
// those are tolerant of never getting one at all, for a real,
// empirically-confirmed reason; see internal/iris's own Client.callTolerant
// doc comment before changing either timeout.
const irisTimeout = 3 * time.Second

// start launches the model as a detached process: its own session, so that
// stop can signal the whole process group rather than only the model itself,
// and released from this process so a short-lived `benchctl power ac on`
// invocation exiting does not take the model down with it. It is
// SetACPower's implementation, not SetPower's: the model starts elaborated
// (every peripheral/UART/PCIe device up, Iris server listening) but with
// firmware execution paused, same as any Iris-server-started model without
// -R - `power on` (startFirmware) is what actually starts it running. This
// mirrors real hardware's mains/soft-power split: applying AC does not by
// itself turn the host on.
//
// The image `flash write host` recorded selects one of two boot flows. A
// FIP (fip-uefi.bin, see isFIP) boots the way a default build does
// (PLAT_RESET_TO_BL31=0): the FIP goes on ros.board.flashloader0, only the
// RSE images are preloaded, and BL1/BL2 load the rest from the FIP. The
// RSE images come from the FIP's own directory (see resolveFIPComponents).
//
// Any other image is BL33 for the RESET_TO_BL31 flow. That firmware is
// built with PLAT_RESET_TO_BL31=1 (see output/rdv3/boot_info.sh), which
// means BL1/BL2/BL32 are never fetched from a FIP at all, and TF-A's own
// reference build for this mode does not stop there - its own
// model-scripts/rdinfra/platforms/rdv3/run_model.sh preloads RSE, the eight
// RESET_TO_BL31 companions, and even BL33 (uefi.bin) directly. This driver
// does the same: the only image `flash write host` ever names is the BL33
// payload itself - a normal build's own uefi.bin, or a coreboot.rom built
// for the same entry point - preloaded straight into DRAM at BL31's
// normal-world entry. ros.board.flashloader0 (the platform's actual NOR
// flash chip, not a FIP-specific concept) is also still pointed at it, not
// because BL31/BL33 ever reads a FIP from there in this flow, but because a
// full flash-shaped payload like coreboot.rom reads its own CBFS/FMAP back
// from that flash device regardless of where its own code happens to be
// executing from - see this function's own flashloader0 comment below.
//
// The parameters below (names, defaults, the port announcement format, and
// the full set needed for the console UART to actually produce boot output
// rather than just an idle, elaborated-but-silent process) come from
// `$BENCHCTL_FVP_MODEL --list-params` and from empirically running the real
// FVP_RD_V3 binary against a real reference-firmware build - not from
// guesswork; see examples/rdv3-fvp/run.md in the loopctl repository for how
// that was confirmed, cross-checked against
// model-scripts/rdinfra/platforms/rdv3/run_model.sh's own invocation.
func (fv *fvpPlatform) start() error {
	bootImage, err := fv.currentBootImage()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(fv.workDir, 0o755); err != nil {
		return err
	}
	fip, err := isFIP(bootImage)
	if err != nil {
		return err
	}
	// The images preloaded for the selected boot flow. Every file is
	// mandatory, so a missing one is reported before the model is launched.
	var preload []string
	if fip {
		comp := resolveFIPComponents(filepath.Dir(bootImage))
		if missing := comp.missing(); len(missing) > 0 {
			return fmt.Errorf("missing RSE firmware next to the FIP %s: %s", bootImage, strings.Join(missing, ", "))
		}
		preload = comp.args()
	} else {
		// RSE (the root of the boot chain, ahead of everything else) and
		// the RESET_TO_BL31 companions BL31 itself needs.
		comp := resolveOutputComponents(fv.workDir, fv.board)
		if !comp.ok() {
			return fmt.Errorf("missing one or more required firmware files (tf_m_rom.bin, tf_m_flash.bin, tf_m_vm0_0.bin, tf_m_vm1_0.bin, scp_ramfw.bin, mcp_ramfw.bin, tf-bl31.bin, %s_spmc_sp_manifest.dtb, %s_nt_fw_config.dtb, hafnium.bin, stmm.pkg, rmm.img) under %s", fv.board, fv.board, filepath.Join(fv.workDir, "output", fv.board))
		}
		preload = comp.args(bootImage)
	}
	nor1, nor2, err := fv.norFlashImages()
	if err != nil {
		return err
	}
	logFile, err := os.Create(fv.processLog)
	if err != nil {
		return fmt.Errorf("create process log: %w", err)
	}
	defer logFile.Close()

	uartLog := func(name string) string { return filepath.Join(fv.workDir, name+".log") }
	port := strconv.Itoa(fv.consolePort)
	argv := []string{
		fv.modelBin,
		"-C", "ros.disable_visualisation=true",
		// ros.board.flashloader0 is the platform's own NOR flash chip
		// (V2M_FLASH0_BASE/RDV3_FLASH_BASE, 0x08000000 in both TF-A's and
		// coreboot's own headers). In the FIP flow BL1 and BL2 read the
		// FIP from it. In the RESET_TO_BL31 flow BL31/BL33 never read it
		// (confirmed live: a normal build's own uefi.bin boots fully with
		// nothing mapped here). It matters for a full flash-shaped image like
		// coreboot.rom though: bootblock itself runs fine from the DRAM
		// preload below, but coreboot's own CBFS/FMAP lookup afterward
		// reads from this flash device unconditionally, wherever bootblock
		// happens to be executing from - confirmed live, coreboot fails to
		// locate its CBFS otherwise. Mapping bootImage here too is a no-op
		// for a plain BL33 payload and required for a CBFS one, so it is
		// done unconditionally rather than trying to guess which kind
		// bootImage is.
		"-C", "ros.board.flashloader0.fname=" + bootImage,
		"-C", "ros.board.flashloader1.fname=" + nor1,
		"-C", "ros.board.flashloader1.fnameWrite=" + nor1,
		"-C", "ros.board.flashloader2.fname=" + nor2,
		"-C", "ros.board.flashloader2.fnameWrite=" + nor2,
		// -I with an explicit --iris-port (chosen clear of the UART cluster
		// and of the Python client's own 7100-7109 autodetect range, see
		// the profile's irisPort comment) starts the Iris debug/control
		// server. Deliberately no -R: the model elaborates fully (every
		// peripheral/UART/PCIe device up) but stays paused at reset,
		// waiting for a `power on` to actually start firmware execution -
		// see this function's own doc comment and PowerState/SetPower.
		"-I", "--iris-port", strconv.Itoa(fv.irisPort),

		// Every UART peripheral defaults to disabled (uart_enable=0), so each
		// one needs enabling even though only ns_uart0's transcript
		// (consoleLog) is this driver's actual console; the rest just get a
		// log file under workDir, matching the confirmed-working invocation.
		"-C", "css.sysctrl.rse_uart.out_file=" + uartLog("rse-uart"),
		"-C", "css.sysctrl.rse_uart.unbuffered_output=1",
		"-C", "css.sysctrl.rse_uart.uart_enable=true",
		"-C", "css.sysctrl.scp.pl011_uart_scp.out_file=" + uartLog("scp-uart"),
		"-C", "css.sysctrl.scp.pl011_uart_scp.unbuffered_output=1",
		"-C", "css.sysctrl.scp.pl011_uart_scp.uart_enable=true",
		"-C", "css.sysctrl.mcp.pl011_uart_mcp.out_file=" + uartLog("mcp-uart"),
		"-C", "css.sysctrl.mcp.pl011_uart_mcp.unbuffered_output=1",
		"-C", "css.sysctrl.mcp.pl011_uart_mcp.uart_enable=true",
		"-C", "css.sysctrl.mcp.pl011_uart_mcp.shutdown_tag=shutting",
		"-C", "css.ap_periph.sec_uart.out_file=" + uartLog("sec-uart"),
		"-C", "css.ap_periph.sec_uart.unbuffered_output=1",
		"-C", "css.ap_periph.sec_uart.uart_enable=true",
		"-C", "css.ap_periph.ns_uart1.out_file=" + uartLog("ns-uart1"),
		"-C", "css.ap_periph.ns_uart1.unbuffered_output=1",
		"-C", "css.ap_periph.ns_uart1.uart_enable=true",
		"-C", "css.sysctrl.scp.pl011_uart_lcp.out_file=" + uartLog("lcp-uart"),
		"-C", "css.sysctrl.scp.pl011_uart_lcp.unbuffered_output=1",
		"-C", "css.sysctrl.scp.pl011_uart_lcp.uart_enable=1",
		"-C", "css.sysctrl.scp.pl011_uart_lcp.baud_rate=115200",
		"-C", "css.sysctrl.scp.pl011_uart_lcp.clock_rate=24000000",
		"-C", "css.sysctrl.scp.pl011_uart_lcp.shutdown_on_eot=1",

		// Every UART's telnet terminal defaults to start_telnet=1, which -
		// where a real display is available, unlike the sandbox this was
		// first built against - makes the model auto-launch its own xterm
		// per terminal (terminal_command's default behaviour), each running
		// its own telnet client against the port. All disabled, including
		// ns_uart0: Arm's own reference launch script
		// (model-scripts/rdinfra/platforms/rdv3/run_model.sh) sets
		// start_telnet=0 on every terminal, ns_uart0 included, and never
		// launches a terminal of its own - it relies entirely on an
		// operator's own external telnet client reaching the port, exactly
		// what Console() does. An earlier version of this driver left
		// ns_uart0 at start_telnet=1 on the mistaken assumption that it was
		// needed for the port to exist at all - it is not, matching this
		// terminal to its reference config is still correct regardless.
		// NOTE: this alone was *not* confirmed to fix the "Connected...
		// Connection closed by foreign host" symptom Console() can hit -
		// the user reported it recurring even with this change in place, so
		// treat the xterm-collision theory as unconfirmed, not resolved.
		"-C", "css.sysctrl.terminal_rse_uart.start_telnet=0",
		"-C", "css.sysctrl.scp.terminal_uart_scp.start_telnet=0",
		"-C", "css.sysctrl.scp.terminal_uart_lcp.start_telnet=0",
		"-C", "css.sysctrl.mcp.terminal_uart_mcp.start_telnet=0",
		"-C", "css.ap_periph.terminal_sec_uart.start_telnet=0",
		"-C", "css.ap_periph.terminal_ns_uart1.start_telnet=0",

		// This driver's actual console: an explicit, requested port (chosen
		// clear of every other terminal's default so the request reliably
		// succeeds, see the profile's consolePort comment) and its own
		// transcript file for gates to grep.
		"-C", "css.ap_periph.terminal_ns_uart0.start_telnet=0",
		"-C", "css.ap_periph.terminal_ns_uart0.start_port=" + port,
		"-C", "css.ap_periph.ns_uart0.out_file=" + fv.consoleLog,
		"-C", "css.ap_periph.ns_uart0.unbuffered_output=1",
		"-C", "css.ap_periph.ns_uart0.uart_enable=true",

		// RSE/MHU channel and interconnect tuning the confirmed-working
		// invocation carries; without it RSE-AP/RSE-SCP handshakes over MHU
		// can misbehave even with the RSE firmware itself correctly loaded.
		"-C", "css.sysctrl.rse.DISABLE_GATING=true",
		"-C", "css.sysctrl.HAS_RSE_CPU_PRIVATE_REGION=1",
		"-C", "css.gic_distributor.ITS-device-bits=20",
		"-C", "css.sysctrl.rse.intchecker.ICBC_RESET_VALUE=0x0000011B",
		"-C", "css.sysctrl.rse.CMU4_NUM_DB_CH=5",
		"-C", "css.sysctrl.rse.CMU0_NUM_DB_CH=16",
		"-C", "css.sysctrl.scp.ap2scp_mhu_s.NUM_DB_CH=6",
		"-C", "css.sysctrl.scp.scp2ap_mhu_s.NUM_DB_CH=6",

		// PCIe: group 0 needs no hierarchy file of its own, just these two
		// static values from the confirmed-working invocation. Groups 1-3
		// each need one; see the loop below.
		"-C", "pcie_group_0.pcie4.hierarchy_file_name=<default>",
		"-C", "pcie_group_0.pcie4.pcie_rc.ahci0.endpoint.ats_supported=true",
	}

	// PCIe hierarchy files for groups 1-3: static example files that ship
	// with model-scripts (model-scripts/rdinfra/platforms/rdv3/example_pcie_hierarchy_N.json)
	// rather than anything a firmware build produces, so they are looked for
	// in workDir - the operator drops a copy there, the same way the two NOR
	// flash images live there - rather than beside the flashed image. Each
	// group is independent: a missing one is just omitted, with a note,
	// rather than failing the whole launch.
	for n := 1; n <= 3; n++ {
		path := filepath.Join(fv.workDir, fmt.Sprintf("example_pcie_hierarchy_%d.json", n))
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(fv.progress, "note: no %s in %s; pcie_group_%d keeps the model's own default hierarchy\n", filepath.Base(path), fv.workDir, n)
			continue
		}
		argv = append(argv, "-C", fmt.Sprintf("pcie_group_%d.pcie4.hierarchy_file_name=%s", n, path))
	}

	// A disk image recorded by `disk mount` (e.g. an OS installer ISO):
	// attached here, the same way the boot image is looked up via
	// currentBootImage, so it takes effect from this power-on onward until
	// `disk eject` or another `disk mount` changes the record. Parameter
	// names come from model-scripts/rdinfra/platforms/rdv3/run_model.sh's
	// own -v/-d flags (VIRTIO_IMAGE_PATH / SATADISK_IMAGE_PATH), not
	// guesswork.
	if medium, path, ok := fv.currentDisk(); ok {
		switch medium {
		case DiskVirtio:
			argv = append(argv, "-C", "ros.board.virtioblockdevice.image_path="+path)
		default: // DiskAHCI
			argv = append(argv, "-C", "pcie_group_0.pcie4.pcie_rc.ahci0.ahci.image_path="+path)
		}
		fmt.Fprintf(fv.progress, "Attaching disk image %s (%s)...\n", path, medium)
	}

	argv = append(argv, preload...)
	// BENCHCTL_FVP_DATA entries load after the built-in preloads, in both
	// boot flows.
	for _, d := range fv.extraData {
		argv = append(argv, "--data", d)
	}

	cmd := osexec.Command(argv[0], argv[1:]...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start model: %w", err)
	}
	pid := cmd.Process.Pid
	if fip {
		fmt.Fprintf(fv.progress, "Model starting (pid %d), booting the FIP %s from flashloader0, paused - run 'power on' to start execution...\n", pid, bootImage)
	} else {
		fmt.Fprintf(fv.progress, "Model starting (pid %d), preloading %s as BL33, paused - run 'power on' to start execution...\n", pid, bootImage)
	}

	// A model that fails fast (bad image, missing dependency) exits within a
	// beat. Catching that here turns a silent no-op boot into an error naming
	// what the model printed, rather than a gate failing downstream with
	// nothing in the process log to explain why.
	const probeEvery = 20 * time.Millisecond
	deadline := time.Now().Add(fv.startGrace)
	for isAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(probeEvery)
	}
	if !isAlive(pid) {
		return fmt.Errorf("model exited immediately: %s", tail(fv.processLog, 20))
	}
	if err := cmd.Process.Release(); err != nil {
		return err
	}
	if err := writePID(fv.pidFile, pid); err != nil {
		return err
	}

	// The process existing and surviving the fail-fast window above only
	// means elaboration started, not that it has reached the point of
	// actually opening ns_uart0's telnet port - PCIe/RSE/interconnect setup
	// ahead of it in the same elaboration sequence can take noticeably
	// longer. Without this, `console` (and anything else that telnets to
	// this port directly, e.g. a robot-framework suite) races that: AC
	// already reads as "on" well before the port is actually listening.
	// Confirmed via the model's own process log (model-stdout.log, not the
	// console transcript ns_uart0 itself writes) rather than a raw TCP
	// dial - an earlier version of this driver tried dialing the port
	// directly, but that was chasing a different bug at the time (an
	// xterm session collision, since fixed by ns_uart0's own
	// start_telnet=0 above) and was reverted when it didn't fix it; the
	// model's own log line is a more direct, purpose-built readiness
	// signal regardless of that history.
	return fv.waitForConsoleReady(pid)
}

// waitForConsoleReady blocks until the model's own process log reports
// ns_uart0's telnet terminal listening on the requested port, or
// bootTimeout passes. pid is checked throughout in case the model exits
// during elaboration rather than merely being slow to reach this point.
func (fv *fvpPlatform) waitForConsoleReady(pid int) error {
	want := fmt.Sprintf("terminal_ns_uart0: Listening for serial connection on port %d", fv.consolePort)
	deadline := time.Now().Add(fv.bootTimeout)
	for {
		if data, err := os.ReadFile(fv.processLog); err == nil && strings.Contains(string(data), want) {
			return nil
		}
		if !isAlive(pid) {
			return fmt.Errorf("model exited before its console came up: %s", tail(fv.processLog, 20))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("console (port %d) did not report ready within %s: %s", fv.consolePort, fv.bootTimeout, tail(fv.processLog, 20))
		}
		time.Sleep(fv.pollEvery)
	}
}

// outputComponents are every firmware file the RESET_TO_BL31 boot flow needs
// beyond the BL33 image itself (`flash write host`): the RSE boot chain and
// the eight companions BL31 needs to reach BL33 at all - all of it
// required in that flow. Their paths
// are resolved from workDir's own copy of a real build's own output/<board>
// tree, mirroring exactly where that build places them (confirmed against
// model-scripts/rdinfra/platforms/rdv3/run_model.sh's own $OUTDIR/$ROOTDIR
// variables) rather than a flat layout of benchctl's own invention that a
// real build's output does not match. The operator sets this up once, e.g.
// by symlinking $BENCHCTL_FVP_WORKDIR/output to the build's own output
// directory:
//
//   - output/<board>/<board>/            - the flat set a build also
//     stages beside the FIP: the RSE boot chain, plus four of the eight
//     companions (scp_ramfw.bin, mcp_ramfw.bin, tf-bl31.bin, rmm.img).
//   - output/<board>/components/<board>/ - the remaining three companions
//     (<board>_spmc_sp_manifest.dtb, <board>_nt_fw_config.dtb, stmm.pkg),
//     which a build never flattens next to the FIP.
//   - output/<board>/components/         - hafnium.bin, one level higher
//     than the rest since a real build shares it across boards.
type outputComponents struct {
	rseRom, rseFlash, rseVM0, rseVM1                                       string
	scpRamfw, mcpRamfw, bl31, spmcManifest, ntFwConfig, hafnium, stmm, rmm string
}

func resolveOutputComponents(workDir, board string) outputComponents {
	flat := filepath.Join(workDir, "output", board, board)
	nested := filepath.Join(workDir, "output", board, "components", board)
	root := filepath.Join(workDir, "output", board, "components")
	return outputComponents{
		rseRom:   filepath.Join(flat, "tf_m_rom.bin"),
		rseFlash: filepath.Join(flat, "tf_m_flash.bin"),
		rseVM0:   filepath.Join(flat, "tf_m_vm0_0.bin"),
		rseVM1:   filepath.Join(flat, "tf_m_vm1_0.bin"),

		scpRamfw:     filepath.Join(flat, "scp_ramfw.bin"),
		mcpRamfw:     filepath.Join(flat, "mcp_ramfw.bin"),
		bl31:         filepath.Join(flat, "tf-bl31.bin"),
		rmm:          filepath.Join(flat, "rmm.img"),
		spmcManifest: filepath.Join(nested, board+"_spmc_sp_manifest.dtb"),
		ntFwConfig:   filepath.Join(nested, board+"_nt_fw_config.dtb"),
		stmm:         filepath.Join(nested, "stmm.pkg"),
		hafnium:      filepath.Join(root, "hafnium.bin"),
	}
}

// ok reports whether every file outputComponents names is present. None of
// this depends on what `flash write` recorded - like the two NOR flash
// images and the PCIe hierarchy files, it is state the operator sets up
// once for the board, so swapping which BL33 image is under test never
// requires re-supplying it.
func (c outputComponents) ok() bool {
	for _, f := range []string{
		c.rseRom, c.rseFlash, c.rseVM0, c.rseVM1,
		c.scpRamfw, c.mcpRamfw, c.bl31, c.spmcManifest, c.ntFwConfig, c.hafnium, c.stmm, c.rmm,
	} {
		if _, err := os.Stat(f); err != nil {
			return false
		}
	}
	return true
}

// args returns the model arguments that load the RSE boot chain, the
// companions and bootImage as BL33. Addresses and the component set come
// from model-scripts/rdinfra/platforms/rdv3/run_model.sh's own
// RESET_TO_BL31_PRELOAD_PARAMS, including PRELOADED_BL33_BASE=0xE0000000
// (trusted-firmware-a's own
// plat/arm/board/neoverse_rd/platform/rdv3/platform.mk), which resolves to
// ros.board.dram's 0x60000000 offset below: ros.board.dram itself is
// addressed relative to DRAM_START=0x80000000, and BL31 jumps there
// unconditionally regardless of which image was preloaded, so a
// coreboot.rom built to run from that same address boots exactly the way
// uefi.bin does, console output included.
func (c outputComponents) args(bootImage string) []string {
	return []string{
		"-C", "css.sysctrl.rse.rom.raw_image=" + c.rseRom,
		"--data", "css.sysctrl.rse.cpu=" + c.rseFlash + "@0xB0000000",
		"--data", "css.sysctrl.rse.cpu=" + c.rseVM0 + "@0x31000400",
		"--data", "css.sysctrl.rse.cpu=" + c.rseVM1 + "@0x31080000",
		"--data", "css.sysctrl.scp.cpu=" + c.scpRamfw + "@0x0",
		"--data", "css.sysctrl.mcp.cpu=" + c.mcpRamfw + "@0x0",
		"--data", "css.ap_periph.arsm_sram=" + c.bl31 + "@0x63000",
		"--data", "css.ap_periph.arsm_sram=" + c.spmcManifest + "@0x1f500",
		"--data", "ros.board.dram=" + c.ntFwConfig + "@0x73000000",
		"--data", "ros.board.dram=" + c.hafnium + "@0x7a889000",
		"--data", "ros.board.dram=" + c.stmm + "@0x7AA00000",
		"--data", "ros.board.dram=" + c.rmm + "@0x7B200000",
		"--data", "ros.board.dram=" + bootImage + "@0x60000000",
	}
}

// fipTOCMagic is the first four bytes of every FIP: TOC_HEADER_NAME
// (0xAA640001) from trusted-firmware-a's
// include/tools_share/firmware_image_package.h, stored little-endian.
var fipTOCMagic = []byte{0x01, 0x00, 0x64, 0xaa}

// isFIP reports whether the image at path starts with the FIP table of
// contents header. A file shorter than the header is not a FIP.
func isFIP(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, len(fipTOCMagic))
	if _, err := io.ReadFull(f, head); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return bytes.Equal(head, fipTOCMagic), nil
}

// fipComponents are the RSE images the FIP flow preloads: the RSE boot ROM
// and the three images loaded into the RSE CPU's memory. A build places
// them next to the FIP in output/<board>/<board>/, and in this flow
// tf_m_flash.bin also carries BL1, so they are taken from the FIP's own
// directory to keep them from the same build.
type fipComponents struct {
	rseRom, rseFlash, rseVM0, rseVM1 string
}

func resolveFIPComponents(dir string) fipComponents {
	return fipComponents{
		rseRom:   filepath.Join(dir, "tf_m_rom.bin"),
		rseFlash: filepath.Join(dir, "tf_m_flash.bin"),
		rseVM0:   filepath.Join(dir, "tf_m_vm0_0.bin"),
		rseVM1:   filepath.Join(dir, "tf_m_vm1_0.bin"),
	}
}

// missing returns the paths of the RSE images that are not present.
func (c fipComponents) missing() []string {
	var out []string
	for _, f := range []string{c.rseRom, c.rseFlash, c.rseVM0, c.rseVM1} {
		if _, err := os.Stat(f); err != nil {
			out = append(out, f)
		}
	}
	return out
}

// args returns the model arguments that preload the RSE images, at the
// addresses model-scripts/rdinfra/platforms/rdv3/run_model.sh uses.
func (c fipComponents) args() []string {
	return []string{
		"-C", "css.sysctrl.rse.rom.raw_image=" + c.rseRom,
		"--data", "css.sysctrl.rse.cpu=" + c.rseFlash + "@0xB0000000",
		"--data", "css.sysctrl.rse.cpu=" + c.rseVM0 + "@0x31000400",
		"--data", "css.sysctrl.rse.cpu=" + c.rseVM1 + "@0x31080000",
	}
}

// norFlashImages returns the paths of the two dummy NOR flash images the
// platform expects at ros.board.flashloader{1,2}, creating them (gzipped,
// all-zero, fv.norSize) if they are not already there. They live in
// workDir rather than beside the flashed image, since they are state this
// driver owns, not something `flash write` records.
func (fv *fvpPlatform) norFlashImages() (nor1, nor2 string, err error) {
	nor1 = filepath.Join(fv.workDir, "nor1_flash.img")
	nor2 = filepath.Join(fv.workDir, "nor2_flash.img")
	for _, path := range []string{nor1, nor2} {
		if err := createDummyNORImage(path, fv.norSize); err != nil {
			return "", "", fmt.Errorf("create %s: %w", path, err)
		}
	}
	return nor1, nor2, nil
}

func createDummyNORImage(path string, size int) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	zero := make([]byte, 256*1024)
	for written := 0; written < size; written += len(zero) {
		if _, err := gz.Write(zero); err != nil {
			return err
		}
	}
	return gz.Close()
}

// startGrace is how long start watches a freshly launched model for an
// immediate exit.
const startGrace = 300 * time.Millisecond

// irisShutdownGrace bounds how long stop waits for a requested Iris
// graceful shutdown to actually take effect before falling back to a
// signal - short relative to bootTimeout, since a shutdown that hasn't
// happened within a few seconds of being asked for is not going to, and
// there is no reason to make every power off wait out the full timeout
// twice (once for the graceful attempt, again for the SIGKILL fallback).
const irisShutdownGrace = 5 * time.Second

// stop signals the model's process group (it is its own session leader, so
// its pid is also its process group id) and waits for it to exit, resending
// SIGKILL once bootTimeout has passed in case the first one raced a slow
// exit. Sending to the group rather than the single pid reaches whatever the
// model itself spawns, not only the process the pidfile names.
//
// It tries an Iris graceful shutdown (simulation_requestShutdown) first,
// when reachable: a real alternative to a bare signal, now that Iris
// support exists, though the reference model has no meaningful "graceful"
// state to reach the way a booted guest OS would (there is no init system
// inside the FVP itself) - falls straight through to the signal otherwise,
// so this is a strict improvement, never a slower path when Iris is not
// available.
func (fv *fvpPlatform) stop(sig syscall.Signal) error {
	pid, ok := readPID(fv.pidFile)
	if !ok {
		return nil
	}
	if isAlive(pid) {
		if client, err := iris.Dial(fv.irisAddr(), irisTimeout); err == nil {
			requestErr := client.RequestShutdown(irisInstID)
			client.Close()
			if requestErr == nil {
				fmt.Fprintln(fv.progress, "Stopping the model (Iris graceful shutdown requested)...")
				deadline := time.Now().Add(fv.shutdownGrace)
				for isAlive(pid) && time.Now().Before(deadline) {
					time.Sleep(fv.pollEvery)
				}
			}
		}
	}
	if isAlive(pid) {
		fmt.Fprintln(fv.progress, "Stopping the model...")
		_ = syscall.Kill(-pid, sig)
		deadline := time.Now().Add(fv.bootTimeout)
		for isAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(fv.pollEvery)
		}
		if isAlive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			time.Sleep(fv.pollEvery)
		}
	}
	_ = os.Remove(fv.pidFile)
	return nil
}

// ACPowerState reports whether the model process exists at all - the
// mains-level state SetPower's PowerOn requires already applied (see
// startFirmware). A process that exists but has its firmware execution
// paused still reads as AC on here: AC tracks the process, not whether it
// is currently executing.
func (fv *fvpPlatform) ACPowerState() (PowerStatus, error) {
	if fv.configErr != nil {
		return PowerStatus{}, fv.configErr
	}
	pid, ok := readPID(fv.pidFile)
	if !ok || !isAlive(pid) {
		return PowerStatus{Power: PowerOff}, nil
	}
	return PowerStatus{Power: PowerOn, Detail: fmt.Sprintf("model pid %d", pid)}, nil
}

// SetACPower launches or kills the model process itself, mirroring a real
// bench's mains feed: applying AC (start) makes the model exist - fully
// elaborated, Iris server up - but does not itself start firmware
// execution (see start's own doc comment); removing AC (stop) kills the
// process outright. `power on`/`power off` (SetPower) only work while AC
// is already on; they never launch or kill the process themselves.
func (fv *fvpPlatform) SetACPower(target Power) error {
	if fv.configErr != nil {
		return fv.configErr
	}
	status, err := fv.ACPowerState()
	if err != nil {
		return err
	}
	if status.Power == target {
		return nil
	}
	if target == PowerOff {
		return fv.stop(syscall.SIGKILL)
	}
	return fv.start()
}

func (fv *fvpPlatform) ACPowerCycle() error {
	if fv.configErr != nil {
		return fv.configErr
	}
	fmt.Fprintln(fv.progress, "AC-power-cycling the model...")
	if err := fv.SetACPower(PowerOff); err != nil {
		return err
	}
	return fv.SetACPower(PowerOn)
}

// Console attaches to the console UART's telnet terminal directly, the same
// way BenchRack's does: the model opens this port itself -
// css.ap_periph.terminal_ns_uart0.start_port - so there is no bridging
// process of ours in between. It trusts the requested port rather than
// reading the model's own announcement back out of its process log: the
// profile's consolePort is chosen clear of every other terminal's default,
// so the request reliably succeeds.
func (fv *fvpPlatform) Console() error {
	if fv.configErr != nil {
		return fv.configErr
	}
	// AC on, not PowerState's firmware-execution state: the console UART's
	// port exists as soon as the process is launched (see start), whether
	// or not firmware execution has actually been started with `power on`
	// yet - attaching before it has is normal (and useful, to not miss the
	// earliest boot output once it does start).
	status, err := fv.ACPowerState()
	if err != nil {
		return err
	}
	if status.Power != PowerOn {
		// Without this, an off model still gets a telnet attempt against the
		// requested port, which just fails with a bare "connection refused"
		// - true, but not why.
		return fmt.Errorf("model is off: run 'power ac on' first")
	}
	// waitForConsoleReady (in start, at power-ac-on time) confirms the model
	// itself reports the port open, but that alone was not enough in
	// practice - a real telnet attempt right after could still fail (seen
	// as a bare "exit status 1" from the real telnet binary).
	//
	// A separate silent probe connection was tried here first (connect
	// directly, no telnet subprocess, hold briefly to catch a connection
	// that gets accepted and then immediately dropped) and confirmed live
	// against the real model NOT to work: the probe itself would hold a
	// connection open for its own full hold window - genuinely stable, not
	// a fluke - and the very next, separate connection (the real telnet)
	// would still be dropped immediately regardless. That is strong
	// evidence the console only tolerates one connection at a time (maybe
	// one, ever, per launch) - so a probe connection does not just fail to
	// help, it actively consumes the one slot the real session needed.
	//
	// So this retries the real attach itself instead of anything separate:
	// no extra connection is ever made. There is still no way to test
	// connectivity without a real connection, and no way to "upgrade" a
	// non-interactive probe into the interactive session once connected
	// without that itself being a second connection - so every attempt has
	// to be the real, interactive telnet subprocess. What retrying adds is
	// not needing the operator to notice a failure and re-run `console` by
	// hand: an attempt that fails within consoleFastFailThreshold is
	// retried automatically, and only an attempt that never manages a
	// stable connection within consoleAttachTimeout is reported as this
	// command's own failure.
	//
	// Each retried attempt's own output (telnet's "Trying...", "Connection
	// closed by foreign host", and this function's own "Attaching to..."
	// announcement) is withheld via consoleSpamGuard rather than printed
	// live, since most attempts are expected to be fast, doomed failures
	// whose output is pure noise (this is what the operator originally
	// reported: retrying fixed the connection but spammed the terminal with
	// every failed attempt along the way). A guard promotes itself to live,
	// real-time passthrough the moment its attempt survives past
	// consoleFastFailThreshold - strong evidence it is a real, held session,
	// which must be seen live from then on, not buffered until it ends.
	port := strconv.Itoa(fv.consolePort)
	session := ConsoleSession{
		What:   "model console (ns_uart0) via telnet on port " + port,
		Detach: `CTRL+] then "quit"`,
		Attach: []string{"telnet", "localhost", port},
	}
	deadline := time.Now().Add(fv.consoleAttachTimeout)
	var lastErr error
	for {
		guard := newConsoleSpamGuard(fv.progress)
		session.Out = guard
		timer := time.AfterFunc(consoleFastFailThreshold, guard.promote)
		start := time.Now()
		err := session.Run(fv.runner, fv.progress)
		timer.Stop()
		if err == nil {
			// A success is definitionally real, not spam, whether or not it
			// happened to take the full consoleFastFailThreshold for the
			// timer to have promoted the guard on its own - make sure
			// anything buffered so far is shown rather than lost.
			guard.promote()
			return nil
		}
		if time.Since(start) >= consoleFastFailThreshold {
			// Held on for a while before failing: a real session the
			// operator was using, not a doomed-from-the-start attempt -
			// the guard already promoted itself to live output, so there is
			// nothing left buffered to show. Report as-is rather than
			// silently retrying over it.
			return err
		}
		lastErr = err
		if time.Now().After(deadline) {
			// The last attempt: show what it actually said instead of
			// staying silent, per the operator's own request - a total
			// failure should report the real final reason, not nothing.
			guard.flush()
			return fmt.Errorf("could not hold a stable console connection within %s: %w", fv.consoleAttachTimeout, lastErr)
		}
		// Fast fail with time left to retry: the guard's buffered spam is
		// simply dropped by starting a fresh one next iteration.
	}
}

// consoleFastFailThreshold is how quickly an attach attempt has to fail to
// be treated as "never really connected, worth retrying automatically"
// rather than as a genuine session the operator was using and then
// disconnected from. Also how long consoleSpamGuard buffers an attempt's
// output before promoting it to live passthrough, on the same reasoning.
const consoleFastFailThreshold = 3 * time.Second

// consoleSpamGuard withholds a console attach attempt's own output
// (including the "Attaching to..." announcement, via ConsoleSession.Out)
// until it is known worth showing: most attempts are expected to be fast,
// doomed failures during Console's retry loop, and their own connection
// noise is spam an operator should not have to see for each one. It
// promotes itself to real-time passthrough the moment the attempt survives
// past consoleFastFailThreshold, since a session held open that long is
// good evidence it is real and must be seen live from then on - buffering a
// genuine interactive session until it ends would defeat the point of an
// interactive console entirely.
type consoleSpamGuard struct {
	mu   sync.Mutex
	real io.Writer
	buf  bytes.Buffer
	live bool
}

func newConsoleSpamGuard(real io.Writer) *consoleSpamGuard {
	return &consoleSpamGuard{real: real}
}

func (g *consoleSpamGuard) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.live {
		return g.real.Write(p)
	}
	return g.buf.Write(p)
}

// promote switches to real-time passthrough, flushing anything buffered so
// far first. Safe to call more than once (only the first call has any
// effect) and from the timer goroutine concurrently with Write.
func (g *consoleSpamGuard) promote() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.live {
		return
	}
	g.live = true
	g.dumpLocked()
}

// flush prints whatever is buffered without switching to live mode - used
// only for the retry loop's last attempt, when there is nothing left to
// silently retry into and a total failure should show its own real reason.
func (g *consoleSpamGuard) flush() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.live {
		return
	}
	g.dumpLocked()
}

// dumpLocked writes out whatever is buffered, called with mu held. It leads
// with a bare "\r" and normalizes every "\n" in the buffered bytes to
// "\r\n": unlike a single live Write (which reaches the terminal the moment
// the process emits it, in whatever line-buffered shape it chose), what
// ends up buffered here is an accumulation from an attempt's still-piped,
// non-interactive standard output, which can arrive de-synchronized from
// the terminal's own cursor position by the time this fires (confirmed
// live: a buffered dump rendered as an increasingly indented staircase, the
// terminal seemingly not advancing to a fresh line the way individual live
// writes to it otherwise do). Leading with "\r" and translating internal
// line breaks explicitly removes the terminal's own line-ending handling
// from the equation for exactly this one bulk write, regardless of
// whichever part of that handling the staircase actually traced back to.
func (g *consoleSpamGuard) dumpLocked() {
	if g.buf.Len() == 0 {
		return
	}
	normalized := make([]byte, 0, g.buf.Len()+2)
	normalized = append(normalized, '\r')
	var prev byte
	for _, b := range g.buf.Bytes() {
		if b == '\n' && prev != '\r' {
			normalized = append(normalized, '\r')
		}
		normalized = append(normalized, b)
		prev = b
	}
	g.buf.Reset()
	_, _ = g.real.Write(normalized)
}

// ConsoleSOL has no analogue here: a virtual platform has no separate BMC.
func (fv *fvpPlatform) ConsoleSOL(BMC) error {
	return ErrNotImplemented
}

// ConsoleUART1 is unimplemented: the driver attaches only the model's console
// UART.
func (fv *fvpPlatform) ConsoleUART1() error {
	return ErrNotImplemented
}

// currentBootImage returns the image path FlashWrite last recorded (a FIP
// or a BL33 image), so a power on without a prior `flash write host` fails
// clearly rather than launching the model with nothing to boot.
func (fv *fvpPlatform) currentBootImage() (string, error) {
	data, err := os.ReadFile(fv.bootImageRecord)
	if err != nil {
		return "", fmt.Errorf("no boot image recorded yet: run 'flash write host <fip-uefi.bin|uefi.bin|coreboot.rom>' first")
	}
	path := strings.TrimSpace(string(data))
	if path == "" {
		return "", fmt.Errorf("no boot image recorded yet: run 'flash write host <fip-uefi.bin|uefi.bin|coreboot.rom>' first")
	}
	return path, nil
}

// currentDisk returns the medium and path DiskMount last recorded, or
// ok=false if none is recorded (the common case: most milestones don't need
// attached media at all).
func (fv *fvpPlatform) currentDisk() (medium DiskMedium, path string, ok bool) {
	data, err := os.ReadFile(fv.diskRecord)
	if err != nil {
		return 0, "", false
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(lines) != 2 {
		return 0, "", false
	}
	m, err := ParseDiskMedium(lines[0])
	if err != nil {
		return 0, "", false
	}
	return m, lines[1], true
}

// DiskMount records image as the disk to attach on the next power-on - the
// same deferred-effect pattern as FlashWrite, for the same reason: the
// running model already has whatever was attached at its own launch, so a
// mount only takes effect from the next start(). It powers the model off
// first so that effect is unambiguous rather than silently deferred.
func (fv *fvpPlatform) DiskMount(_ context.Context, image string, opts DiskOptions) error {
	if fv.configErr != nil {
		return fv.configErr
	}
	medium := opts.Medium
	switch medium {
	case DiskDefault:
		// virtio is the default rather than ahci: on rdv3, mounting an image via
		// pcie_group_0.pcie4.pcie_rc.ahci0.ahci.image_path reproducibly hits a
		// Synchronous Exception in the reference UEFI's ArmCpuDxe (identical
		// PC/registers regardless of the attached image's own content - Ubuntu
		// Desktop and Alpine both crash at exactly the same instruction), which
		// points at the AHCI probe/enumeration path itself, not the guest image.
		// See examples/rdv3-coreboot-linuxboot/kb/rdv3.md's D6 section.
		medium = DiskVirtio
	case DiskAHCI, DiskVirtio:
	default:
		return fmt.Errorf("the fvp platform attaches disks as virtio or ahci, not %s", medium)
	}
	if opts.BootOnce || opts.ResetBMC || opts.PowerOn {
		return fmt.Errorf("the fvp platform has no boot override or BMC and starts the model on power on: --boot-once, --reset-bmc and --power-on do not apply")
	}
	abs, err := filepath.Abs(image)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("%s: %w", image, err)
	}
	// The image path is one of start's -C flags, baked into the process at
	// launch - pausing/resuming firmware execution (SetPower) cannot change
	// it, only relaunching the process (SetACPower) can, so that is what
	// must happen for this to take effect.
	if err := fv.SetACPower(PowerOff); err != nil {
		return err
	}
	if err := os.MkdirAll(fv.workDir, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(fv.progress, "Recording disk image %s (%s)...\n", abs, medium)
	if err := os.WriteFile(fv.diskRecord, []byte(medium.String()+"\n"+abs+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(fv.progress, "Disk image recorded; it will be attached from the next power ac on.")
	return nil
}

// DiskEject forgets the recorded disk image. Like DiskMount, it applies AC
// off first (see its comment on why SetPower cannot substitute here) so the
// removal takes effect unambiguously from the next launch rather than
// silently applying to an already-running model.
func (fv *fvpPlatform) DiskEject(slot string) error {
	if fv.configErr != nil {
		return fv.configErr
	}
	if slot != "" {
		return fmt.Errorf("the fvp platform has one disk and no slots: drop --slot")
	}
	if err := fv.SetACPower(PowerOff); err != nil {
		return err
	}
	if err := os.Remove(fv.diskRecord); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintln(fv.progress, "Disk image record removed.")
	return nil
}

func (fv *fvpPlatform) DiskStatus() error {
	if fv.configErr != nil {
		return fv.configErr
	}
	medium, path, ok := fv.currentDisk()
	if !ok {
		fmt.Fprintln(fv.progress, "no disk image recorded")
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("recorded disk image %s: %w", path, err)
	}
	fmt.Fprintf(fv.progress, "disk image: %s (%s, %d bytes)\n", path, medium, info.Size())
	return nil
}

func (fv *fvpPlatform) FlashProbe(target FlashTarget) error {
	if fv.configErr != nil {
		return fv.configErr
	}
	if target != FlashHost {
		return ErrNotImplemented
	}
	image, err := fv.currentBootImage()
	if err != nil {
		fmt.Fprintln(fv.progress, "no boot image recorded")
		return nil
	}
	info, err := os.Stat(image)
	if err != nil {
		return fmt.Errorf("recorded image %s: %w", image, err)
	}
	fmt.Fprintf(fv.progress, "boot image: %s (%d bytes)\n", image, info.Size())
	return nil
}

func (fv *fvpPlatform) FlashRead(target FlashTarget, outPath string) error {
	if fv.configErr != nil {
		return fv.configErr
	}
	if target != FlashHost {
		return ErrNotImplemented
	}
	image, err := fv.currentBootImage()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(image)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, data, 0o644)
}

// FlashWrite records fw as the BL33 image the next launch preloads directly
// into DRAM at BL31's normal-world entry point, bypassing the FIP entirely
// - there is no FIP anywhere in this driver's boot flow (see start's own
// doc comment for why); fw is also still pointed at ros.board.flashloader0,
// the platform's own flash chip, for a full flash-shaped fw like
// coreboot.rom that reads its own CBFS/FMAP back from there (again, see
// start's own comment). fw is a normal build's own uefi.bin, or a
// coreboot.rom built for the same entry point; either way it needs the
// RESET_TO_BL31 companion firmware present under workDir's own output/<board>
// tree regardless (see resolveOutputComponents), not something FlashWrite
// itself checks. It applies AC off first exactly as a real driver would
// need the bus idle: the image path is one of start's -C/--data flags baked
// into the process at launch, so the running process is still using the old
// image regardless of firmware execution state, and only relaunching it
// (SetACPower, not SetPower) picks up the new one. force is accepted for
// interface symmetry with other platforms but unused: fw is an opaque
// binary, so there is no cheap content check worth doing before recording
// it, the way a mocked text image could be checked for a recognised line.
func (fv *fvpPlatform) FlashWrite(target FlashTarget, fw string, force bool) error {
	if fv.configErr != nil {
		return fv.configErr
	}
	if target != FlashHost {
		return ErrNotImplemented
	}
	abs, err := filepath.Abs(fw)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("%s: %w", fw, err)
	}
	if err := fv.SetACPower(PowerOff); err != nil {
		return err
	}
	if err := os.MkdirAll(fv.workDir, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(fv.progress, "Recording boot image %s...\n", abs)
	if err := os.WriteFile(fv.bootImageRecord, []byte(abs+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(fv.progress, "Boot image recorded; the next power ac on boots it (a FIP from flashloader0, any other image preloaded as BL33).")
	return nil
}

// FlashStatus and FlashAbort report not implemented for the same reason
// benchrack's do: a write here runs to completion within one command and
// leaves no update state to query or abort between invocations.
func (fv *fvpPlatform) FlashStatus(FlashTarget) (Status, error) {
	return Status{}, ErrNotImplemented
}

func (fv *fvpPlatform) FlashAbort(FlashTarget) error {
	return ErrNotImplemented
}

// tail returns the last n lines of the file at path, or "" if it cannot be
// read, so an error message degrades gracefully instead of failing itself.
func tail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
