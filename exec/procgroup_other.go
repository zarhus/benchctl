// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package exec

import (
	"os"
)

// stopProcessGroup kills process alone where there are no Unix process groups.
func stopProcessGroup(process *os.Process) {
	_ = process.Kill()
}

// followResize does nothing where there is no SIGWINCH.
func followResize(_, _ *os.File) func() { return func() {} }
