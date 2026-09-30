// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"strings"
	"testing"

	"github.com/zarhus/benchctl/platform"
)

func TestFormatVersion(t *testing.T) {
	cases := []struct {
		tag      string
		revision string
		modified bool
		want     string
	}{
		// No tag, not printed
		// No VCS stamp (e.g. `go run`) falls back to a fixed label.
		{"dev", "", false, "dev"},
		{"dev", "", true, "dev"},
		// A full commit SHA is abbreviated to its leading 12 characters.
		{"dev", "fef7778621288afe235c65ebab58a38bef566bba", false, "fef777862128"},
		{"dev", "fef7778621288afe235c65ebab58a38bef566bba", true, "fef777862128-dirty"},
		// A revision already shorter than the limit is left as is.
		{"dev", "5d4f57d", false, "5d4f57d"},
		// A dirty working tree at build time is marked so the build is traceable.
		{"dev", "5d4f57d", true, "5d4f57d-dirty"},
		// When tag is set it is printed with revision
		{"v1.0.0", "fef777862128", false, "v1.0.0 (fef777862128)"},
	}
	for _, tc := range cases {
		if got := formatVersion(tc.tag, tc.revision, tc.modified); got != tc.want {
			t.Errorf("formatVersion(%q, %v) = %q, want %q", tc.revision, tc.modified, got, tc.want)
		}
	}
}

func TestVersionFlag(t *testing.T) {
	out, err := run(t, "--version")
	if err != nil {
		t.Fatal(err)
	}
	// Cobra renders "<name> version <value>"; the value comes from version().
	if !strings.Contains(out, "benchctl version ") {
		t.Errorf("--version output = %q, want it to contain %q", out, "benchctl version ")
	}
}

func TestVersionLineNamesTheBuild(t *testing.T) {
	// The default platform tells the build flavors apart in --version.
	platform.SetDefault("flavor-test")
	defer platform.SetDefault("")
	if got := versionLine(); !strings.HasSuffix(got, " (flavor-test build)") {
		t.Errorf("versionLine() = %q, want a (flavor-test build) suffix", got)
	}
	platform.SetDefault("")
	if got := versionLine(); strings.Contains(got, "build") {
		t.Errorf("versionLine() = %q, want no build suffix without a default", got)
	}
}

func TestVerboseShorthandUnaffectedByVersion(t *testing.T) {
	// --version must not steal the -v shorthand, which belongs to --verbose.
	root := newRootCmd()
	root.InitDefaultVersionFlag() // adds --version, choosing a shorthand
	if shorthand := root.Flags().ShorthandLookup("v"); shorthand == nil || shorthand.Name != "verbose" {
		t.Errorf("-v shorthand = %v, want it bound to --verbose", shorthand)
	}
}
