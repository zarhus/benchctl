// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stmcginnis/gofish"
	"github.com/zarhus/benchctl/exec"
	"github.com/zarhus/benchctl/internal/ipmi"
	"github.com/zarhus/benchctl/internal/redfish"
)

func init() {
	Register(Spec{
		Name: "bmc",
		New:  newBMC,
	})
}

// bmcPlatform drives a host through its BMC alone: power over Redfish, and the
// host console over the BMC's SSH console or IPMI SOL. It runs on the
// workstation and reaches the BMC directly or through an ssh jump host, with
// ProxyJump, keys and agent taken from the user's OpenSSH configuration.
//
// It also attaches disk images as the BMC's virtual media, in bmcmedia.go. AC
// power and flash writes stay with whoever owns the machine.
type bmcPlatform struct {
	runner    exec.Runner
	access    BMCAccess
	progress  io.Writer
	configErr error

	redfish    *redfish.Client
	systemPath string            // resolved on first use
	media      *gofish.APIClient // virtual media client, connected on first use
	out        io.Writer         // disk status output

	pollEvery       time.Duration
	powerTimeout    time.Duration
	slotsTimeout    time.Duration // remote media and its slots appearing
	insertTimeout   time.Duration // an InsertMedia or EjectMedia task
	busyTimeout     time.Duration // retrying a boot override the BMC refuses while the host boots
	bmcResetTimeout time.Duration // the BMC answering again after a reset
	localAddr       func(bmc string) (string, error)
	setenv          func(key, value string) error
	startPiped      func(argv []string) (consoleProcess, error)
	probeSSH        func(port int) error
}

// Defaults for a BMC that reports no SerialConsole. Port 2200 is OpenBMC's
// obmc-console-ssh.
const (
	bmcDefaultConsolePort = 2200
	bmcDefaultHotKey      = `"~." at the start of a line`
)

func newBMC(runner exec.Runner, cfg Config) Platform {
	b := &bmcPlatform{
		runner:          runner,
		access:          cfg.BMC,
		progress:        os.Stderr,
		out:             os.Stdout,
		pollEvery:       2 * time.Second,
		powerTimeout:    3 * time.Minute,
		slotsTimeout:    60 * time.Second,
		insertTimeout:   3 * time.Minute,
		busyTimeout:     60 * time.Second,
		bmcResetTimeout: 5 * time.Minute,
		localAddr:       localAddrToward,
		setenv:          os.Setenv,
		startPiped: func(argv []string) (consoleProcess, error) {
			return exec.StartPiped(argv)
		},
	}
	b.probeSSH = b.sshBanner
	b.configErr = b.configure()
	return b
}

// configure checks the settings and builds the Redfish client.
func (b *bmcPlatform) configure() error {
	var missing []string
	if b.access.IP == "" {
		missing = append(missing, "--bmc-ip or BENCHCTL_BMC_IP")
	}
	if b.access.User == "" {
		missing = append(missing, "--bmc-user or BENCHCTL_BMC_USER")
	}
	if b.access.Password == "" {
		missing = append(missing, "--bmc-password or BENCHCTL_BMC_PASSWORD")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the bmc platform needs %s", strings.Join(missing, ", "))
	}
	switch b.access.Console {
	case "", "ssh", "sol":
	default:
		return fmt.Errorf("unknown BMC console %q (want ssh or sol)", b.access.Console)
	}
	httpClient, err := redfish.NewHTTPClient(redfish.Transport{
		Jump:     b.access.Jump,
		CAFile:   b.access.CAFile,
		Insecure: b.access.Insecure,
	})
	if err != nil {
		return err
	}
	b.redfish = &redfish.Client{
		BaseURL:  (&url.URL{Scheme: "https", Host: b.access.IP}).String(),
		User:     b.access.User,
		Password: b.access.Password,
		HTTP:     httpClient,
	}
	return nil
}

// resolveSystem finds the path of the ComputerSystem the driver acts on, once.
func (b *bmcPlatform) resolveSystem() error {
	if b.configErr != nil {
		return b.configErr
	}
	if b.systemPath == "" {
		path, err := b.redfish.SystemPath(b.access.System)
		if err != nil {
			return err
		}
		b.systemPath = path
	}
	return nil
}

// system reads the ComputerSystem the driver acts on.
func (b *bmcPlatform) system() (redfish.System, error) {
	if err := b.resolveSystem(); err != nil {
		return redfish.System{}, err
	}
	return b.redfish.System(b.systemPath)
}

// redfishPower maps a Redfish PowerState onto a PowerStatus. Only "On" counts as
// on. The transitional states stay in the detail.
func redfishPower(state string) PowerStatus {
	switch state {
	case "On":
		return PowerStatus{Power: PowerOn}
	case "Off":
		return PowerStatus{Power: PowerOff}
	default:
		return PowerStatus{Power: PowerOff, Detail: state}
	}
}

func (b *bmcPlatform) PowerState() (PowerStatus, error) {
	system, err := b.system()
	if err != nil {
		return PowerStatus{}, err
	}
	return redfishPower(system.PowerState), nil
}

func (b *bmcPlatform) SetPower(target Power) error {
	return b.setPower(context.Background(), target)
}

// setPower is SetPower that stops waiting for the host when ctx ends.
func (b *bmcPlatform) setPower(ctx context.Context, target Power) error {
	system, err := b.system()
	if err != nil {
		return err
	}
	if redfishPower(system.PowerState).Power == target {
		return nil
	}
	resetType := "On"
	if target == PowerOff {
		resetType = "ForceOff"
	}
	if err := b.reset(system, resetType); err != nil {
		return err
	}
	return b.pollPower(ctx, target)
}

func (b *bmcPlatform) PowerReset() error {
	system, err := b.system()
	if err != nil {
		return err
	}
	if redfishPower(system.PowerState).Power != PowerOn {
		return b.SetPower(PowerOn)
	}
	if err := b.reset(system, "ForceRestart", "PowerCycle"); err != nil {
		return err
	}
	return b.pollPower(context.Background(), PowerOn)
}

func (b *bmcPlatform) HardReset() error {
	system, err := b.system()
	if err != nil {
		return err
	}
	status := redfishPower(system.PowerState)
	if status.Power != PowerOn {
		return fmt.Errorf("host is %s, use a hard reset only when the host is powered on and a normal reset does not work", status)
	}
	if pickReset(system.Actions.Reset.Allowed, "ForceRestart") == "" {
		return ErrNotImplemented
	}
	if err := b.reset(system, "ForceRestart"); err != nil {
		return err
	}
	return b.pollPower(context.Background(), PowerOn)
}

// pickReset returns the first of choices the BMC allows. A BMC that lists no
// allowable values gets the first choice. It returns empty when the BMC allows
// none of them.
func pickReset(allowed []string, choices ...string) string {
	if len(allowed) == 0 {
		return choices[0]
	}
	for _, choice := range choices {
		if slices.Contains(allowed, choice) {
			return choice
		}
	}
	return ""
}

// reset posts ComputerSystem.Reset with the first of choices the system allows.
func (b *bmcPlatform) reset(system redfish.System, choices ...string) error {
	action := system.Actions.Reset
	resetType := pickReset(action.Allowed, choices...)
	if resetType == "" {
		return fmt.Errorf("the BMC allows none of %s for ComputerSystem.Reset (it allows %s)", strings.Join(choices, ", "), strings.Join(action.Allowed, ", "))
	}
	target := action.Target
	if target == "" {
		target = b.systemPath + "/Actions/ComputerSystem.Reset"
	}
	fmt.Fprintf(b.progress, "Sending ComputerSystem.Reset %s to %s...\n", resetType, b.access.IP)
	return b.redfish.Reset(target, resetType)
}

// pollPower waits for the host to reach target, or until ctx ends.
func (b *bmcPlatform) pollPower(ctx context.Context, target Power) error {
	deadline := time.Now().Add(b.powerTimeout)
	for {
		status, err := b.PowerState()
		if err != nil {
			return err
		}
		if status.Power == target && status.Detail == "" {
			fmt.Fprintf(b.progress, "Host is %s.\n", target)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("host still %s after %s, want %s", status, b.powerTimeout, target)
		}
		if err := pause(ctx, b.pollEvery); err != nil {
			return err
		}
	}
}

// errInterrupted reports a wait that ended with its context.
var errInterrupted = errors.New("interrupted before the host booted the image")

// pause waits d, or returns errInterrupted once ctx has ended.
func pause(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return errInterrupted
	}
	select {
	case <-ctx.Done():
		return errInterrupted
	case <-time.After(d):
		return nil
	}
}

// bmcConsole is the console the driver attaches to.
type bmcConsole struct {
	method string // "ssh" or "sol"
	port   int    // SSH console port
	entry  string // command sent on login to reach the host console
	hotKey string // the BMC's own detach sequence, as it describes it
}

// console picks the console from the settings, then from the SerialConsole the
// system reports, preferring SSH, then from the defaults. Settings that decide
// everything skip the Redfish request.
func (b *bmcPlatform) console() (bmcConsole, error) {
	chosen := bmcConsole{
		method: b.access.Console,
		port:   b.access.ConsolePort,
		entry:  b.access.ConsoleEntry,
		hotKey: bmcDefaultHotKey,
	}
	if chosen.method == "sol" || (chosen.method == "ssh" && chosen.port != 0) {
		return chosen, nil
	}
	system, err := b.system()
	if err != nil {
		if b.configErr != nil {
			return bmcConsole{}, err
		}
		fmt.Fprintf(b.progress, "Cannot read the BMC's SerialConsole (%v).\n", err)
		system = redfish.System{}
	}
	ssh, sol := system.SerialConsole.SSH, system.SerialConsole.IPMI
	if chosen.method == "" {
		switch {
		case ssh != nil && ssh.ServiceEnabled:
			chosen.method = "ssh"
		case sol != nil && sol.ServiceEnabled:
			chosen.method = "sol"
		case ssh == nil && sol == nil:
			chosen.method = b.guessConsole(chosen.port)
		default:
			return bmcConsole{}, fmt.Errorf("the BMC reports its SSH and IPMI serial consoles disabled: pick one with --bmc-console or BENCHCTL_BMC_CONSOLE")
		}
	}
	if ssh != nil {
		if chosen.port == 0 {
			chosen.port = ssh.Port
		}
		if chosen.entry == "" && ssh.SharedWithManagerCLI {
			chosen.entry = ssh.ConsoleEntryCommand
		}
		if ssh.HotKeySequenceDisplay != "" {
			chosen.hotKey = ssh.HotKeySequenceDisplay
		}
	}
	if chosen.port == 0 {
		chosen.port = bmcDefaultConsolePort
	}
	return chosen, nil
}

// guessConsole picks the console of a BMC that advertises none: SSH when an
// SSH server answers on port, or on the default console port, and SOL
// otherwise.
func (b *bmcPlatform) guessConsole(port int) string {
	if port == 0 {
		port = bmcDefaultConsolePort
	}
	if err := b.probeSSH(port); err != nil {
		fmt.Fprintf(b.progress, "The BMC advertises no serial console and SSH on port %d does not answer (%v), using IPMI SOL.\n", port, err)
		return "sol"
	}
	return "ssh"
}

// sshBanner checks that an SSH server answers on port of the BMC, reached the
// same way as Redfish.
func (b *bmcPlatform) sshBanner(port int) error {
	conn, err := redfish.Dial(b.access.Jump, net.JoinHostPort(b.access.IP, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	result := make(chan error, 1)
	go func() {
		banner := make([]byte, 4)
		_, err := io.ReadFull(conn, banner)
		if err == nil && string(banner) != "SSH-" {
			err = fmt.Errorf("the server there is not SSH (it sent %q)", banner)
		}
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-time.After(sshBannerTimeout):
		return fmt.Errorf("no SSH banner within %s", sshBannerTimeout)
	}
}

// sshBannerTimeout bounds the wait for an SSH server's greeting, which it
// sends as soon as a client connects.
const sshBannerTimeout = 10 * time.Second

func (b *bmcPlatform) Console() error {
	if b.configErr != nil {
		return b.configErr
	}
	chosen, err := b.console()
	if err != nil {
		return err
	}
	if chosen.method == "sol" {
		return b.solSession().Run(b.runner, b.progress)
	}
	session, err := b.sshSession(chosen)
	if err != nil {
		return err
	}
	return session.Run(b.runner, b.progress)
}

// ConsoleServe relays a console on addr. com1 is the console the settings and
// Redfish pick, as Console attaches, and sol is IPMI SOL, as ConsoleSOL
// attaches. Over SOL it runs sol deactivate before each attach and on the way
// out, as the interactive console does, so a payload left open by a dropped
// session, its own included, never blocks the next attach.
func (b *bmcPlatform) ConsoleServe(ctx context.Context, addr string, source ConsoleSource) error {
	var session ConsoleSession
	switch source.Name {
	case "com1":
		if b.configErr != nil {
			return b.configErr
		}
		chosen, err := b.console()
		if err != nil {
			return err
		}
		if chosen.method == "sol" {
			session = b.solSession()
			break
		}
		if err := b.setenv("SSHPASS", b.access.Password); err != nil {
			return err
		}
		// The relay's client sends bytes rather than keys, so ssh's escape is off.
		session = ConsoleSession{
			What:   "host console over SSH at " + b.access.IP + " port " + strconv.Itoa(chosen.port),
			Attach: b.sshConsoleArgv(chosen, true),
		}
	case "sol":
		if err := b.useBMC(source.BMC); err != nil {
			return err
		}
		session = b.solSession()
	case "uart1":
		return ErrNotImplemented
	default:
		return unknownSource(source.Name)
	}
	return serveSession(ctx, addr, session, b.startPiped, b.runner, b.progress)
}

// ConsoleSOL attaches over IPMI SOL whatever the console setting says. The
// address and credentials come from the same settings as the rest of the
// driver, and bmc fills any the caller passes.
func (b *bmcPlatform) ConsoleSOL(bmc BMC) error {
	if err := b.useBMC(bmc); err != nil {
		return err
	}
	return b.solSession().Run(b.runner, b.progress)
}

// useBMC takes the address and credentials bmc sets in place of the settings,
// and checks the result.
func (b *bmcPlatform) useBMC(bmc BMC) error {
	if bmc.IP != "" {
		b.access.IP = bmc.IP
	}
	if bmc.User != "" {
		b.access.User = bmc.User
	}
	if bmc.Password != "" {
		b.access.Password = bmc.Password
	}
	b.configErr = b.configure()
	return b.configErr
}

// sshSession attaches to the BMC's SSH console. The password reaches sshpass in
// SSHPASS, which keeps it out of the process list.
func (b *bmcPlatform) sshSession(chosen bmcConsole) (ConsoleSession, error) {
	if err := b.setenv("SSHPASS", b.access.Password); err != nil {
		return ConsoleSession{}, err
	}
	port := strconv.Itoa(chosen.port)
	return ConsoleSession{
		What:   "host console over SSH at " + b.access.IP + " port " + port,
		Detach: chosen.hotKey,
		Attach: b.sshConsoleArgv(chosen, b.runner.DetachHint() != ""),
	}, nil
}

// sshConsoleArgv builds the ssh command for the BMC's console. noEscape
// disables ssh's escape, so "~" reaches the console: the runner's detach key or
// the relay ends the session instead. Otherwise ssh's "~." is the way out.
//
// The jump host is crossed with "ssh -W" as the ProxyCommand, the way Redfish
// requests cross it, so the connection to the jump host has the same
// ConnectTimeout as the one to the BMC, and only the BMC gets the password.
func (b *bmcPlatform) sshConsoleArgv(chosen bmcConsole, noEscape bool) []string {
	argv := []string{"sshpass", "-e", "ssh", "-tt"}
	if noEscape {
		argv = append(argv, "-e", "none")
	}
	argv = append(argv, "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10")
	if b.access.Jump != "" {
		// ssh expands %h and %p, and runs the command with the shell.
		jump := strings.ReplaceAll(exec.ShellJoin([]string{b.access.Jump}), "%", "%%")
		argv = append(argv, "-o", "ProxyCommand=ssh -o BatchMode=yes -o ConnectTimeout=10 -W '[%h]:%p' "+jump)
	}
	argv = append(argv, "-p", strconv.Itoa(chosen.port), "-l", b.access.User, b.access.IP)
	if chosen.entry != "" {
		argv = append(argv, chosen.entry)
	}
	return argv
}

// solSession attaches over IPMI SOL. ipmitool runs on the jump host, since IPMI
// is UDP and the jump host forwards TCP only. sol deactivate runs around the
// attach, which drops anyone else on the payload.
func (b *bmcPlatform) solSession() ConsoleSession {
	client := ipmi.NewEnvPassword(b.access.IP, b.access.User)
	session := ConsoleSession{
		What:        "host console over IPMI SOL at " + b.access.IP,
		Detach:      `"~." at the start of a line`,
		Attach:      b.ipmiArgv(client.SOLActivate(), true),
		Unreachable: client.Unreachable,
		NoEscape:    true,
		Release:     b.ipmiArgv(client.SOLDeactivate(), false),
	}
	return session
}

// ipmiArgv runs an ipmitool command with the password in its environment: on
// the jump host through ssh, or locally without one. tty allocates a terminal
// on the jump host for an interactive session.
func (b *bmcPlatform) ipmiArgv(argv []string, tty bool) []string {
	withPassword := append([]string{"env", ipmi.PasswordEnv + "=" + b.access.Password}, argv...)
	if b.access.Jump == "" {
		return withPassword
	}
	ssh := []string{"ssh"}
	if tty {
		// "~." belongs to ipmitool whichever way the session ends.
		ssh = append(ssh, "-tt", "-e", "none")
	}
	ssh = append(ssh, "-o", "ConnectTimeout=10", b.access.Jump, exec.ShellJoin(withPassword))
	return ssh
}

func (b *bmcPlatform) ConsoleUART1() error { return ErrNotImplemented }

func (b *bmcPlatform) ACPowerState() (PowerStatus, error) { return PowerStatus{}, ErrNotImplemented }
func (b *bmcPlatform) SetACPower(Power) error             { return ErrNotImplemented }
func (b *bmcPlatform) ACPowerCycle() error                { return ErrNotImplemented }

func (b *bmcPlatform) FlashProbe(FlashTarget) error               { return ErrNotImplemented }
func (b *bmcPlatform) FlashRead(FlashTarget, string) error        { return ErrNotImplemented }
func (b *bmcPlatform) FlashWrite(FlashTarget, string, bool) error { return ErrNotImplemented }
func (b *bmcPlatform) FlashStatus(FlashTarget) (Status, error) {
	return Status{}, ErrNotImplemented
}
func (b *bmcPlatform) FlashAbort(FlashTarget) error { return ErrNotImplemented }
