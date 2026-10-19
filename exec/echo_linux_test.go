// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package exec

import (
	"os"
	"testing"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestNoEchoTurnsEchoOffAndBack(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()
	lflag := func() uint32 {
		termios, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		return termios.Lflag
	}
	before := lflag()
	if before&unix.ECHO == 0 {
		t.Fatal("a new pty has echo off")
	}
	restore := NoEcho(tty)
	if during := lflag(); during&unix.ECHO != 0 || during&unix.ICANON == 0 || during&unix.ISIG == 0 {
		t.Errorf("lflag = %#x, want echo off with line editing and signals on", during)
	}
	restore()
	if after := lflag(); after != before {
		t.Errorf("lflag = %#x after restore, want %#x", after, before)
	}
}

func TestNoEchoLeavesAFileAlone(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "noecho")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	NoEcho(f)()
}
