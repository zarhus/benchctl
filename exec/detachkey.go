// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package exec

import (
	"fmt"
	"strings"
)

// DefaultDetachKey is Ctrl+], the key telnet also takes as its escape.
const DefaultDetachKey byte = 0x1D

// ParseDetachKey reads a detach key written as "ctrl+<key>" or "^<key>", where
// <key> is a letter or one of [ \ ] ^ _, in either case. "none" returns 0,
// which leaves detach to the program's own sequence.
func ParseDetachKey(spec string) (byte, error) {
	lower := strings.ToLower(strings.TrimSpace(spec))
	if lower == "none" {
		return 0, nil
	}
	var key string
	switch {
	case strings.HasPrefix(lower, "ctrl+"):
		key = lower[len("ctrl+"):]
	case strings.HasPrefix(lower, "^"):
		key = lower[1:]
	}
	if len(key) == 1 {
		char := strings.ToUpper(key)[0]
		if char >= 'A' && char <= '_' {
			return char & 0x1F, nil
		}
	}
	return 0, fmt.Errorf("unknown detach key %q (want ctrl+<letter or one of [\\]^_>, or none)", spec)
}

// DetachKeyName writes key the way ParseDetachKey reads it, as in "Ctrl+]".
func DetachKeyName(key byte) string {
	return "Ctrl+" + string(rune(key|0x40))
}
