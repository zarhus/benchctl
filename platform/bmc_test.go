// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zarhus/benchctl/internal/redfish"
)

// fakeBMC is a Redfish service with one or more ComputerSystems. A reset moves
// the power state at once, as a BMC does once the host has followed.
type fakeBMC struct {
	mu            sync.Mutex
	systems       []string // IDs listed under /redfish/v1/Systems
	powerState    string
	allowed       []string // ResetType@Redfish.AllowableValues, nil to omit
	serialConsole map[string]any
	resets        []string // ResetType of every reset posted, in order
	requests      int
}

func (bmc *fakeBMC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bmc.mu.Lock()
	defer bmc.mu.Unlock()
	bmc.requests++
	if user, password, ok := r.BasicAuth(); !ok || user != "root" || password != "0penBmc" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == redfish.SystemsPath:
		var members []map[string]string
		for _, id := range bmc.systems {
			members = append(members, map[string]string{"@odata.id": redfish.SystemsPath + "/" + id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Members": members})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, redfish.SystemsPath+"/"):
		id := strings.TrimPrefix(r.URL.Path, redfish.SystemsPath+"/")
		if !slices.Contains(bmc.systems, id) {
			http.NotFound(w, r)
			return
		}
		reset := map[string]any{"target": r.URL.Path + "/Actions/ComputerSystem.Reset"}
		if bmc.allowed != nil {
			reset["ResetType@Redfish.AllowableValues"] = bmc.allowed
		}
		system := map[string]any{
			"Id":         id,
			"PowerState": bmc.powerState,
			"Actions":    map[string]any{"#ComputerSystem.Reset": reset},
		}
		if bmc.serialConsole != nil {
			system["SerialConsole"] = bmc.serialConsole
		}
		_ = json.NewEncoder(w).Encode(system)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Actions/ComputerSystem.Reset"):
		var body struct{ ResetType string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		bmc.resets = append(bmc.resets, body.ResetType)
		switch body.ResetType {
		case "On", "ForceRestart", "PowerCycle":
			bmc.powerState = "On"
		case "ForceOff":
			bmc.powerState = "Off"
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// newTestBMC starts bmc over TLS and returns a driver that reaches it.
func newTestBMC(t *testing.T, bmc *fakeBMC, access BMCAccess, runner *fakeRunner) (*bmcPlatform, *strings.Builder) {
	t.Helper()
	server := httptest.NewTLSServer(bmc)
	t.Cleanup(server.Close)
	if access.IP == "" {
		access.IP = strings.TrimPrefix(server.URL, "https://")
	}
	if access.User == "" {
		access.User = "root"
	}
	if access.Password == "" {
		access.Password = "0penBmc"
	}
	driver := newBMC(runner, Config{BMC: access}).(*bmcPlatform)
	if driver.configErr != nil {
		t.Fatal(driver.configErr)
	}
	driver.redfish.BaseURL = server.URL
	driver.redfish.HTTP = server.Client()
	driver.pollEvery = 0
	progress := &strings.Builder{}
	driver.progress = progress
	driver.setenv = func(string, string) error { return nil }
	driver.probeSSH = func(int) error { return nil }
	return driver, progress
}

// bmcwebSystem is an OpenBMC bmcweb service: system ID "system" and the
// SerialConsole bmcweb reports.
func bmcwebSystem(power string) *fakeBMC {
	return &fakeBMC{
		systems:    []string{"system"},
		powerState: power,
		allowed:    []string{"On", "ForceOff", "ForceOn", "ForceRestart", "GracefulRestart", "GracefulShutdown", "PowerCycle", "Nmi"},
		serialConsole: map[string]any{
			"MaxConcurrentSessions": 15,
			"SSH": map[string]any{
				"ServiceEnabled":        true,
				"Port":                  2200,
				"HotKeySequenceDisplay": "Press ~. to exit console",
			},
			"IPMI": map[string]any{"ServiceEnabled": true},
		},
	}
}

// plainSystem is a BMC with system ID "System_0", no ForceRestart among its
// reset types, and no SerialConsole.
func plainSystem(power string) *fakeBMC {
	return &fakeBMC{
		systems:    []string{"System_0"},
		powerState: power,
		allowed:    []string{"On", "ForceOff", "GracefulShutdown", "PowerCycle"},
	}
}

func TestBMCNeedsAddressAndCredentials(t *testing.T) {
	driver := newBMC(&fakeRunner{}, Config{})
	_, err := driver.PowerState()
	if err == nil {
		t.Fatal("PowerState with no BMC settings succeeded")
	}
	for _, want := range []string{"BENCHCTL_BMC_IP", "BENCHCTL_BMC_USER", "BENCHCTL_BMC_PASSWORD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestBMCRejectsAnUnknownConsoleMethod(t *testing.T) {
	driver := newBMC(&fakeRunner{}, Config{BMC: BMCAccess{IP: "10.0.0.5", User: "root", Password: "pw", Console: "telnet"}})
	if err := driver.Console(); err == nil || !strings.Contains(err.Error(), "telnet") {
		t.Errorf("Console = %v, want an error naming the unknown method", err)
	}
}

func TestBMCPowerStateFindsTheOnlySystem(t *testing.T) {
	for _, bmc := range []*fakeBMC{bmcwebSystem("On"), plainSystem("On")} {
		driver, _ := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
		status, err := driver.PowerState()
		if err != nil {
			t.Fatalf("system %s: %v", bmc.systems[0], err)
		}
		if status.Power != PowerOn || status.Detail != "" {
			t.Errorf("system %s: PowerState = %v, want on", bmc.systems[0], status)
		}
	}
}

func TestBMCPowerStateKeepsTransitionsInTheDetail(t *testing.T) {
	driver, _ := newTestBMC(t, plainSystem("PoweringOn"), BMCAccess{}, &fakeRunner{})
	status, err := driver.PowerState()
	if err != nil {
		t.Fatal(err)
	}
	if status.Power != PowerOff || status.Detail != "PoweringOn" {
		t.Errorf("PowerState = %+v, want off with detail PoweringOn", status)
	}
}

func TestBMCSeveralSystemsNeedASetting(t *testing.T) {
	bmc := plainSystem("On")
	bmc.systems = []string{"System_0", "System_1"}
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	_, err := driver.PowerState()
	if err == nil || !strings.Contains(err.Error(), "System_0, System_1") {
		t.Fatalf("PowerState = %v, want an error naming both systems", err)
	}

	driver, _ = newTestBMC(t, bmc, BMCAccess{System: "System_1"}, &fakeRunner{})
	if _, err := driver.PowerState(); err != nil {
		t.Errorf("PowerState with the system set = %v", err)
	}
}

func TestBMCSetPowerSendsOnAndForceOff(t *testing.T) {
	bmc := plainSystem("Off")
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	if err := driver.SetPower(PowerOn); err != nil {
		t.Fatal(err)
	}
	if err := driver.SetPower(PowerOff); err != nil {
		t.Fatal(err)
	}
	if want := []string{"On", "ForceOff"}; !slices.Equal(bmc.resets, want) {
		t.Errorf("resets = %q, want %q", bmc.resets, want)
	}
}

func TestBMCSetPowerLeavesAHostAlreadyThere(t *testing.T) {
	bmc := plainSystem("On")
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	if err := driver.SetPower(PowerOn); err != nil {
		t.Fatal(err)
	}
	if len(bmc.resets) != 0 {
		t.Errorf("resets = %q, want none for a host already on", bmc.resets)
	}
}

func TestBMCSetPowerTimesOut(t *testing.T) {
	bmc := plainSystem("Off")
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	driver.powerTimeout = 0
	// The host never follows: the BMC keeps reporting it off.
	stuck := &stuckBMC{fakeBMC: bmc}
	server := httptest.NewTLSServer(stuck)
	t.Cleanup(server.Close)
	driver.redfish.BaseURL, driver.redfish.HTTP = server.URL, server.Client()
	if err := driver.SetPower(PowerOn); err == nil || !strings.Contains(err.Error(), "still off") {
		t.Errorf("SetPower = %v, want a timeout naming the state", err)
	}
}

// stuckBMC accepts resets and leaves the power state as it was.
type stuckBMC struct{ *fakeBMC }

func (bmc *stuckBMC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	state := bmc.powerState
	bmc.fakeBMC.ServeHTTP(w, r)
	bmc.mu.Lock()
	bmc.powerState = state
	bmc.mu.Unlock()
}

func TestBMCPowerResetPrefersForceRestart(t *testing.T) {
	bmc := bmcwebSystem("On")
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	if err := driver.PowerReset(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ForceRestart"}; !slices.Equal(bmc.resets, want) {
		t.Errorf("resets = %q, want %q", bmc.resets, want)
	}
}

func TestBMCPowerResetFallsBackToPowerCycle(t *testing.T) {
	bmc := plainSystem("On")
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	if err := driver.PowerReset(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"PowerCycle"}; !slices.Equal(bmc.resets, want) {
		t.Errorf("resets = %q, want %q", bmc.resets, want)
	}
}

func TestBMCPowerResetPowersOnAHostThatIsOff(t *testing.T) {
	bmc := plainSystem("Off")
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	if err := driver.PowerReset(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"On"}; !slices.Equal(bmc.resets, want) {
		t.Errorf("resets = %q, want %q", bmc.resets, want)
	}
}

func TestBMCResetWithoutAllowableValuesTakesTheFirstChoice(t *testing.T) {
	bmc := plainSystem("On")
	bmc.allowed = nil
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	if err := driver.PowerReset(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ForceRestart"}; !slices.Equal(bmc.resets, want) {
		t.Errorf("resets = %q, want %q", bmc.resets, want)
	}
}

func TestBMCHardResetNeedsForceRestart(t *testing.T) {
	bmc := plainSystem("On")
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	if err := driver.HardReset(); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("HardReset = %v, want ErrNotImplemented on a BMC without ForceRestart", err)
	}

	bmc = bmcwebSystem("On")
	driver, _ = newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	if err := driver.HardReset(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ForceRestart"}; !slices.Equal(bmc.resets, want) {
		t.Errorf("resets = %q, want %q", bmc.resets, want)
	}
}

func TestBMCLeavesACAndFlashAlone(t *testing.T) {
	driver := newBMC(&fakeRunner{}, Config{})
	for name, err := range map[string]error{
		"SetACPower":   driver.SetACPower(PowerOff),
		"ACPowerCycle": driver.ACPowerCycle(),
		"FlashWrite":   driver.FlashWrite(FlashHost, "fw.bin", false),
		"ConsoleUART1": driver.ConsoleUART1(),
		"DiskMount":    driver.DiskMount("disk.img", DiskAHCI),
	} {
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s = %v, want ErrNotImplemented", name, err)
		}
	}
}

func TestBMCConsoleUsesTheSSHConsoleRedfishReports(t *testing.T) {
	runner := &fakeRunner{detachHint: "Ctrl+]"}
	driver, progress := newTestBMC(t, bmcwebSystem("On"), BMCAccess{Jump: "jumphost"}, runner)
	if err := driver.Console(); err != nil {
		t.Fatal(err)
	}
	if len(runner.interactive) != 1 {
		t.Fatalf("interactive = %q, want one attach", runner.interactive)
	}
	want := []string{
		"sshpass", "-e", "ssh", "-tt", "-e", "none",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10",
		"-J", "jumphost", "-p", "2200", "-l", "root", driver.access.IP,
	}
	if got := runner.interactive[0]; !slices.Equal(got, want) {
		t.Errorf("attach = %q, want %q", got, want)
	}
	if len(runner.calls) != 0 {
		t.Errorf("calls = %q, want no release around an SSH console", runner.calls)
	}
	if !strings.Contains(progress.String(), "Detach with Ctrl+].") {
		t.Errorf("progress = %q, want the runner's detach key", progress)
	}
}

func TestBMCConsoleKeepsSSHEscapeWithoutADetachKey(t *testing.T) {
	runner := &fakeRunner{}
	driver, progress := newTestBMC(t, bmcwebSystem("On"), BMCAccess{}, runner)
	if err := driver.Console(); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(runner.interactive[0], "none") {
		t.Errorf("attach = %q, want ssh's escape left on", runner.interactive[0])
	}
	if !strings.Contains(progress.String(), "Press ~. to exit console") {
		t.Errorf("progress = %q, want the BMC's own hot key", progress)
	}
}

func TestBMCConsolePassesThePasswordInTheEnvironment(t *testing.T) {
	runner := &fakeRunner{}
	driver, _ := newTestBMC(t, bmcwebSystem("On"), BMCAccess{}, runner)
	env := map[string]string{}
	driver.setenv = func(key, value string) error { env[key] = value; return nil }
	if err := driver.Console(); err != nil {
		t.Fatal(err)
	}
	if env["SSHPASS"] != "0penBmc" {
		t.Errorf("SSHPASS = %q, want the BMC password", env["SSHPASS"])
	}
	if slices.Contains(runner.interactive[0], "0penBmc") {
		t.Errorf("attach = %q, want the password off the command line", runner.interactive[0])
	}
}

func TestBMCConsoleDefaultsToSSHOnPort2200(t *testing.T) {
	runner := &fakeRunner{}
	driver, _ := newTestBMC(t, plainSystem("On"), BMCAccess{}, runner)
	if err := driver.Console(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.interactive[0], " "); !strings.Contains(got, "-p 2200 -l root") {
		t.Errorf("attach = %q, want ssh to port 2200", got)
	}
}

func TestBMCConsoleSendsTheEntryCommandOfASharedCLI(t *testing.T) {
	bmc := plainSystem("On")
	bmc.serialConsole = map[string]any{
		"SSH": map[string]any{
			"ServiceEnabled":       true,
			"Port":                 22,
			"SharedWithManagerCLI": true,
			"ConsoleEntryCommand":  "console com2",
		},
	}
	runner := &fakeRunner{}
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, runner)
	if err := driver.Console(); err != nil {
		t.Fatal(err)
	}
	got := runner.interactive[0]
	if got[len(got)-1] != "console com2" || !strings.Contains(strings.Join(got, " "), "-p 22 ") {
		t.Errorf("attach = %q, want port 22 and the entry command", got)
	}
}

func TestBMCConsoleFallsBackToSOL(t *testing.T) {
	bmc := bmcwebSystem("On")
	bmc.serialConsole["SSH"] = map[string]any{"ServiceEnabled": false, "Port": 2200}
	runner := &fakeRunner{}
	driver, _ := newTestBMC(t, bmc, BMCAccess{Jump: "jumphost"}, runner)
	if err := driver.Console(); err != nil {
		t.Fatal(err)
	}
	attach := runner.interactive[0]
	if !slices.Equal(attach[:6], []string{"ssh", "-tt", "-e", "none", "-o", "ConnectTimeout=10"}) || attach[6] != "jumphost" {
		t.Errorf("attach = %q, want ssh to the jump host", attach)
	}
	remote := attach[len(attach)-1]
	for _, want := range []string{"env IPMI_PASSWORD=0penBmc ipmitool", "-E", "sol activate"} {
		if !strings.Contains(remote, want) {
			t.Errorf("remote command %q, want it to contain %q", remote, want)
		}
	}
	if len(runner.calls) != 2 || !strings.Contains(strings.Join(runner.calls[0], " "), "sol deactivate") {
		t.Errorf("calls = %q, want sol deactivate before and after", runner.calls)
	}
}

func TestBMCConsoleRunsSOLLocallyWithoutAJumpHost(t *testing.T) {
	runner := &fakeRunner{}
	driver, _ := newTestBMC(t, bmcwebSystem("On"), BMCAccess{Console: "sol"}, runner)
	if err := driver.Console(); err != nil {
		t.Fatal(err)
	}
	attach := runner.interactive[0]
	if attach[0] != "env" || attach[2] != "ipmitool" {
		t.Errorf("attach = %q, want ipmitool run locally", attach)
	}
}

func TestBMCConsoleSettingsSkipRedfish(t *testing.T) {
	for _, access := range []BMCAccess{{Console: "sol"}, {Console: "ssh", ConsolePort: 2200}} {
		bmc := plainSystem("On")
		driver, _ := newTestBMC(t, bmc, access, &fakeRunner{})
		if err := driver.Console(); err != nil {
			t.Fatal(err)
		}
		if bmc.requests != 0 {
			t.Errorf("console %s made %d Redfish requests, want none", access.Console, bmc.requests)
		}
	}
}

func TestBMCConsoleWithoutRedfishTriesSSH(t *testing.T) {
	runner := &fakeRunner{}
	driver, progress := newTestBMC(t, plainSystem("On"), BMCAccess{}, runner)
	driver.redfish.BaseURL = "https://127.0.0.1:1"
	if err := driver.Console(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.interactive[0], " "); !strings.Contains(got, "-p 2200") {
		t.Errorf("attach = %q, want the default SSH port", got)
	}
	if !strings.Contains(progress.String(), "Cannot read the BMC's SerialConsole") {
		t.Errorf("progress = %q, want the Redfish failure reported", progress)
	}
}

func TestBMCConsoleWithoutSSHFallsBackToSOL(t *testing.T) {
	// A BMC that advertises no SerialConsole and has nothing on port 2200.
	runner := &fakeRunner{}
	driver, progress := newTestBMC(t, plainSystem("On"), BMCAccess{}, runner)
	var probed []int
	driver.probeSSH = func(port int) error {
		probed = append(probed, port)
		return errors.New("connection refused")
	}
	if err := driver.Console(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(probed, []int{2200}) {
		t.Errorf("probed %v, want port 2200", probed)
	}
	if got := strings.Join(runner.interactive[0], " "); !strings.Contains(got, "sol activate") {
		t.Errorf("attach = %q, want SOL", got)
	}
	if !strings.Contains(progress.String(), "using IPMI SOL") {
		t.Errorf("progress = %q, want the fallback reported", progress)
	}
}

func TestBMCConsoleProbesOnlyWhenNothingSaysWhich(t *testing.T) {
	for name, bmc := range map[string]*fakeBMC{"advertised": bmcwebSystem("On"), "set": plainSystem("On")} {
		access := BMCAccess{}
		if name == "set" {
			access.Console = "ssh"
		}
		driver, _ := newTestBMC(t, bmc, access, &fakeRunner{})
		driver.probeSSH = func(int) error {
			t.Errorf("%s: probed SSH, want the console taken as given", name)
			return nil
		}
		if err := driver.Console(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSSHBannerTellsSSHFromOtherServers(t *testing.T) {
	for greeting, wantErr := range map[string]bool{"SSH-2.0-dropbear\r\n": false, "HTTP/1.1 400\r\n": true} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				_, _ = conn.Write([]byte(greeting))
				conn.Close()
			}
		}()
		host, port, _ := net.SplitHostPort(listener.Addr().String())
		driver := newBMC(&fakeRunner{}, Config{BMC: BMCAccess{IP: host, User: "root", Password: "pw"}}).(*bmcPlatform)
		portNum, _ := strconv.Atoi(port)
		if err := driver.sshBanner(portNum); (err != nil) != wantErr {
			t.Errorf("greeting %q: sshBanner = %v, want error %v", greeting, err, wantErr)
		}
		listener.Close()
	}
}

func TestBMCConsoleRefusesWhenBothConsolesAreOff(t *testing.T) {
	bmc := bmcwebSystem("On")
	bmc.serialConsole = map[string]any{
		"SSH":  map[string]any{"ServiceEnabled": false},
		"IPMI": map[string]any{"ServiceEnabled": false},
	}
	runner := &fakeRunner{}
	driver, _ := newTestBMC(t, bmc, BMCAccess{}, runner)
	if err := driver.Console(); err == nil || !strings.Contains(err.Error(), "BENCHCTL_BMC_CONSOLE") {
		t.Errorf("Console = %v, want an error pointing at the setting", err)
	}
	if len(runner.interactive) != 0 {
		t.Errorf("interactive = %q, want no attach", runner.interactive)
	}
}

func TestBMCConsoleSOLForcesSOL(t *testing.T) {
	runner := &fakeRunner{}
	driver, _ := newTestBMC(t, bmcwebSystem("On"), BMCAccess{}, runner)
	if err := driver.ConsoleSOL(BMC{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.interactive[0], " "); !strings.Contains(got, "sol activate") {
		t.Errorf("attach = %q, want SOL", got)
	}
}
