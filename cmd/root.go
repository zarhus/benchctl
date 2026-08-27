// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

// Package cmd defines the benchctl command-line interface. Commands resolve a
// platform driver and call its interface methods; they hold no platform logic.
package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/zarhus/benchctl/platform"
)

var (
	flagHost      string
	flagPlatform  string
	flagPassword  string
	flagBoard     string
	flagTasmotaIP string
	flagVerbose   bool
)

// buildPlatform is the constructor commands use to obtain a Platform. It is a
// variable so tests can substitute a fake.
var buildPlatform = build

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "benchctl",
		Short:         "Control test benches",
		Version:       versionLine(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.CompletionOptions.HiddenDefaultCmd = true

	platformHelp := "platform driver (or BENCHCTL_PLATFORM)"
	if def := platform.Default(); def != "" {
		platformHelp += "; default " + def
	}

	flags := root.PersistentFlags()
	flags.StringVarP(&flagHost, "host", "H", "", "bench host (or BENCHCTL_HOST; localhost on the bench itself)")
	flags.StringVar(&flagPlatform, "platform", "", platformHelp)
	flags.StringVar(&flagPassword, "password", "", "SSH root password override (or BENCHCTL_PASSWORD)")
	flags.StringVar(&flagBoard, "board", "", "board profile for multi-board platforms (or BENCHCTL_BOARD)")
	flags.BoolVarP(&flagVerbose, "verbose", "v", false, "trace bench commands")
	_ = flags.MarkHidden("platform")

	root.AddCommand(powerCmd(), consoleCmd(), flashCmd())
	return root
}

func opts() options {
	return options{
		host:      flagHost,
		platform:  flagPlatform,
		password:  flagPassword,
		board:     flagBoard,
		tasmotaIP: flagTasmotaIP,
		verbose:   flagVerbose,
	}
}

// withPlatform builds the platform from the resolved flags and runs fn against
// it.
func withPlatform(fn func(platform.Platform) error) error {
	p, err := buildPlatform(opts())
	if err != nil {
		return err
	}
	return fn(p)
}

// ResolvedPlatform reports the platform name in effect, for error messages.
func ResolvedPlatform() string {
	return resolvePlatform(flagPlatform)
}

// Execute runs the benchctl root command.
func Execute() error {
	return newRootCmd().Execute()
}

// Main runs benchctl and returns the process exit code: 0 on success, 3 when
// the operation is not implemented on the selected platform (so scripts can
// tell "unsupported" from "failed"), and 1 on any other error.
func Main() int {
	err := Execute()
	switch {
	case err == nil:
		return 0
	case errors.Is(err, platform.ErrNotImplemented):
		fmt.Fprintf(os.Stderr, "benchctl: not implemented on platform %q\n", ResolvedPlatform())
		return 3
	default:
		fmt.Fprintf(os.Stderr, "benchctl: %v\n", err)
		return 1
	}
}
