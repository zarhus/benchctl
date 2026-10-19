// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"io"
	"strings"
)

// fakeRunner is a test Runner. handler decides each call's output; calls
// records every argv passed to Run.
type fakeRunner struct {
	handler        func(call int, argv []string) (string, error)
	calls          [][]string
	interactive    [][]string
	sequence       [][]string // every call in order, plain and interactive alike
	noEscape       []bool     // per interactive call, whether the ssh escape was disabled
	pushed         bool
	pulled         bool
	streamOut      string // canned stdout for Stream
	streamErr      error  // result of the Stream wait function
	host           string // reported by Host()
	detachHint     string // reported by DetachHint()
	interactiveErr error
	interactiveOut string // canned output written to RunInteractiveOut's out
	// interactiveOutHandler, when set, scripts RunInteractiveOut per call
	// instead of the fixed interactiveOut/interactiveErr above - for tests
	// that need a sequence of outcomes (e.g. Console's retry loop).
	interactiveOutHandler func(call int, out io.Writer, argv []string) error
}

func (fake *fakeRunner) Host() string { return fake.host }

func (fake *fakeRunner) DetachHint() string { return fake.detachHint }

func (fake *fakeRunner) Run(argv ...string) (string, error) {
	callIndex := len(fake.calls)
	fake.calls = append(fake.calls, argv)
	fake.sequence = append(fake.sequence, argv)
	if fake.handler != nil {
		return fake.handler(callIndex, argv)
	}
	return "", nil
}

func (fake *fakeRunner) RunInteractive(argv ...string) error {
	fake.interactive = append(fake.interactive, argv)
	fake.sequence = append(fake.sequence, argv)
	fake.noEscape = append(fake.noEscape, false)
	return fake.interactiveErr
}

func (fake *fakeRunner) RunInteractiveNoEscape(argv ...string) error {
	fake.interactive = append(fake.interactive, argv)
	fake.sequence = append(fake.sequence, argv)
	fake.noEscape = append(fake.noEscape, true)
	return fake.interactiveErr
}

func (fake *fakeRunner) RunInteractiveOut(out io.Writer, argv ...string) error {
	callIndex := len(fake.interactive)
	fake.interactive = append(fake.interactive, argv)
	fake.sequence = append(fake.sequence, argv)
	fake.noEscape = append(fake.noEscape, false)
	if fake.interactiveOutHandler != nil {
		return fake.interactiveOutHandler(callIndex, out, argv)
	}
	if fake.interactiveOut != "" {
		_, _ = io.WriteString(out, fake.interactiveOut)
	}
	return fake.interactiveErr
}

func (fake *fakeRunner) Stream(argv ...string) (io.ReadCloser, func() error, error) {
	fake.calls = append(fake.calls, argv)
	fake.sequence = append(fake.sequence, argv)
	return io.NopCloser(strings.NewReader(fake.streamOut)), func() error { return fake.streamErr }, nil
}

func (fake *fakeRunner) Push(localPath, remotePath string) (string, func() error, error) {
	fake.pushed = true
	return remotePath, func() error { return nil }, nil
}

func (fake *fakeRunner) Pull(localPath, remotePath string) (string, func() error, error) {
	fake.pulled = true
	return remotePath, func() error { return nil }, nil
}
