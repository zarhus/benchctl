// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"os"
	"strconv"

	"github.com/zarhus/benchctl/exec"
	"github.com/zarhus/benchctl/platform"
)

// options holds the resolved global flags for a command invocation.
type options struct {
	host      string
	platform  string
	password  string
	board     string
	tasmotaIP string
	verbose   bool
	detachKey string
	bmc       bmcFlags
}

// resolveHost returns the bench host from the flag, then BENCHCTL_HOST, and
// errors if neither is set so benchctl never silently acts on localhost.
func resolveHost(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if val := os.Getenv("BENCHCTL_HOST"); val != "" {
		return val, nil
	}
	return "", fmt.Errorf("no target host: pass --host or set BENCHCTL_HOST (a bench sets it for you)")
}

// resolvePlatform returns the platform name from the flag, then
// BENCHCTL_PLATFORM, then the binary's registry default.
func resolvePlatform(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if val := os.Getenv("BENCHCTL_PLATFORM"); val != "" {
		return val
	}
	return platform.Default()
}

// resolveBoard returns the board name from the flag, then BENCHCTL_BOARD, then
// empty so the driver picks its own default. Drivers that ignore the board are
// unaffected.
func resolveBoard(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("BENCHCTL_BOARD")
}

// resolveTasmotaIP returns the AC-control Tasmota address from the flag, then
// BENCHCTL_TASMOTA_IP, then empty so the driver uses its board default.
func resolveTasmotaIP(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("BENCHCTL_TASMOTA_IP")
}

// resolveBMCIP returns the BMC address for --source sol from the flag, then
// BENCHCTL_BMC_IP, then empty so the caller reports it as missing.
func resolveBMCIP(flagVal string) string {
	return resolveSetting(flagVal, "BENCHCTL_BMC_IP")
}

// resolveSetting returns the flag's value, then the environment variable's.
func resolveSetting(flagVal, env string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(env)
}

// resolveSettingOr is resolveSetting with a default for when both are unset.
func resolveSettingOr(flagVal, env, def string) string {
	if val := resolveSetting(flagVal, env); val != "" {
		return val
	}
	return def
}

// resolveBMC returns the BMC settings from the flags, then their environment
// variables.
func resolveBMC(flags bmcFlags) (platform.BMCAccess, error) {
	access := platform.BMCAccess{
		IP:           resolveSetting(flags.ip, "BENCHCTL_BMC_IP"),
		Jump:         resolveSetting(flags.jump, "BENCHCTL_BMC_JUMP"),
		User:         resolveSetting(flags.user, "BENCHCTL_BMC_USER"),
		Password:     resolveSetting(flags.password, "BENCHCTL_BMC_PASSWORD"),
		System:       resolveSetting(flags.system, "BENCHCTL_BMC_SYSTEM"),
		Console:      resolveSetting(flags.console, "BENCHCTL_BMC_CONSOLE"),
		ConsolePort:  flags.consolePort,
		ConsoleEntry: resolveSetting(flags.consoleEntry, "BENCHCTL_BMC_CONSOLE_ENTRY"),
		CAFile:       resolveSetting(flags.caFile, "BENCHCTL_BMC_CA"),
		Insecure:     flags.insecure,
	}
	if access.ConsolePort == 0 {
		if val := os.Getenv("BENCHCTL_BMC_CONSOLE_PORT"); val != "" {
			port, err := strconv.Atoi(val)
			if err != nil || port <= 0 || port > 65535 {
				return access, fmt.Errorf("BENCHCTL_BMC_CONSOLE_PORT=%q is not a port number", val)
			}
			access.ConsolePort = port
		}
	}
	if !access.Insecure {
		if val := os.Getenv("BENCHCTL_BMC_INSECURE"); val != "" {
			insecure, err := strconv.ParseBool(val)
			if err != nil {
				return access, fmt.Errorf("BENCHCTL_BMC_INSECURE=%q is not a boolean", val)
			}
			access.Insecure = insecure
		}
	}
	return access, nil
}

// resolveDetachKey returns the console detach key from the flag, then
// BENCHCTL_DETACH_KEY, then Ctrl+]. "none" gives 0, which leaves detach to the
// console program's own sequence.
func resolveDetachKey(flagVal string) (byte, error) {
	spec := flagVal
	if spec == "" {
		spec = os.Getenv("BENCHCTL_DETACH_KEY")
	}
	if spec == "" {
		return exec.DefaultDetachKey, nil
	}
	return exec.ParseDetachKey(spec)
}

// resolvePassword returns the SSH password from the flag, then
// BENCHCTL_PASSWORD, then the driver's default.
func resolvePassword(flagVal, driverDefault string) string {
	if flagVal != "" {
		return flagVal
	}
	if val := os.Getenv("BENCHCTL_PASSWORD"); val != "" {
		return val
	}
	return driverDefault
}

// build resolves the options into a ready Platform.
func build(opts options) (platform.Platform, error) {
	name := resolvePlatform(opts.platform)
	if name == "" {
		return nil, fmt.Errorf("no platform selected: pass --platform or set BENCHCTL_PLATFORM (known: %v)", platform.Names())
	}
	spec, ok := platform.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown platform %q (known: %v)", name, platform.Names())
	}
	host, err := resolveHost(opts.host)
	if err != nil {
		return nil, err
	}
	target := exec.Target{
		Host:     host,
		User:     spec.DefaultUser,
		Password: resolvePassword(opts.password, spec.DefaultPassword),
	}
	if err := exec.CheckDependencies(target); err != nil {
		return nil, err
	}
	detachKey, err := resolveDetachKey(opts.detachKey)
	if err != nil {
		return nil, err
	}
	runner := &exec.CmdRunner{Target: target, Verbose: opts.verbose, DetachKey: detachKey}
	bmc, err := resolveBMC(opts.bmc)
	if err != nil {
		return nil, err
	}
	config := platform.Config{
		Board:     resolveBoard(opts.board),
		TasmotaIP: resolveTasmotaIP(opts.tasmotaIP),
		BMC:       bmc,
	}
	return spec.New(runner, config), nil
}
