// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"testing"

	"github.com/zarhus/benchctl/platform"
)

func TestFlashTargetFromName(t *testing.T) {
	cases := []struct {
		name    string
		want    platform.FlashTarget
		wantErr bool
	}{
		{"host", platform.FlashHost, false},
		{"bmc", platform.FlashBMC, false},
		{"", 0, true},
		{"wat", 0, true},
	}
	for _, tc := range cases {
		got, err := flashTargetFromName(tc.name)
		if tc.wantErr {
			if err == nil {
				t.Errorf("flashTargetFromName(%q) = nil error, want error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("flashTargetFromName(%q) error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("flashTargetFromName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseFlashTarget(t *testing.T) {
	cases := []struct {
		args    []string
		want    platform.FlashTarget
		wantErr bool
	}{
		{nil, platform.FlashHost, false},
		{[]string{"host"}, platform.FlashHost, false},
		{[]string{"bmc"}, platform.FlashBMC, false},
		{[]string{"wat"}, 0, true},
	}
	for _, tc := range cases {
		got, err := parseFlashTarget(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseFlashTarget(%q) = nil error, want error", tc.args)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseFlashTarget(%q) error: %v", tc.args, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseFlashTarget(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
