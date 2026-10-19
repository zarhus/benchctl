// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package exec

import (
	"os"
)

// NoEcho leaves the terminal as it is outside Linux.
func NoEcho(_ *os.File) func() { return func() {} }
