// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package exec

import "testing"

func TestParseDetachKey(t *testing.T) {
	cases := []struct {
		spec string
		want byte
	}{
		{"ctrl+]", 0x1D},
		{"Ctrl+]", 0x1D},
		{"^]", 0x1D},
		{"ctrl+a", 0x01},
		{"CTRL+B", 0x02},
		{"ctrl+\\", 0x1C},
		{"none", 0},
		{"NONE", 0},
	}
	for _, c := range cases {
		got, err := ParseDetachKey(c.spec)
		if err != nil {
			t.Errorf("ParseDetachKey(%q) = %v", c.spec, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDetachKey(%q) = %#x, want %#x", c.spec, got, c.want)
		}
	}
}

func TestParseDetachKeyRejectsOthers(t *testing.T) {
	for _, spec := range []string{"", "]", "ctrl+", "ctrl+ab", "ctrl+1", "alt+x", "^~", "ctrl+@"} {
		if _, err := ParseDetachKey(spec); err == nil {
			t.Errorf("ParseDetachKey(%q) succeeded, want an error", spec)
		}
	}
}

func TestDetachKeyNameRoundTrips(t *testing.T) {
	for _, spec := range []string{"Ctrl+]", "Ctrl+A", "Ctrl+_", "Ctrl+\\"} {
		key, err := ParseDetachKey(spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := DetachKeyName(key); got != spec {
			t.Errorf("DetachKeyName(%#x) = %q, want %q", key, got, spec)
		}
	}
}
