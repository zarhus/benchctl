// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// streamTestImage writes an image to a temporary file, long enough for reads
// whose replies take more than one websocket message.
func streamTestImage(t *testing.T, name string) (string, []byte) {
	t.Helper()
	data := bytes.Repeat([]byte("0123456789abcdef"), 20000)
	image := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(image, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return image, data
}

// startStream runs DiskServe on image in the background and returns a cancel
// for its context and its result.
func startStream(driver *bmcPlatform, image string, opts DiskServeOptions) (context.CancelFunc, <-chan error) {
	driver.pollEvery = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- driver.DiskServe(ctx, image, opts) }()
	return cancel, done
}

func TestBMCDiskServeStreamsOverTheNBDWebsocketWhereTheBMCOffersIt(t *testing.T) {
	bmc := openBMCMediaBMC()
	driver, progress, _ := newMediaTestBMC(t, bmc)
	image, data := streamTestImage(t, "disk.img")

	cancel, done := startStream(driver, image, DiskServeOptions{Transport: TransportAuto, DiskOptions: DiskOptions{PowerOn: true}})
	defer cancel()
	waitFor(t, func() bool { return strings.Contains(progress.String(), DiskReady) }, DiskReady)
	bmc.mu.Lock()
	streamed := bytes.Equal(bmc.streamed, data)
	inserts := len(bmc.inserts)
	target, enabled := bmc.boot["BootSourceOverrideTarget"], bmc.boot["BootSourceOverrideEnabled"]
	resets := slices.Clone(bmc.resets)
	bmc.mu.Unlock()
	if !streamed {
		t.Error("the BMC's NBD client read something other than the image")
	}
	if inserts != 0 {
		t.Errorf("InsertMedia posted %d times, want none on the websocket route", inserts)
	}
	if target != "Usb" || enabled != "Continuous" {
		t.Errorf("boot override = %v, %v, want Usb, Continuous", target, enabled)
	}
	if want := []string{"ForceOff", "On"}; !slices.Equal(resets, want) {
		t.Errorf("resets = %q, want %q", resets, want)
	}
	for _, want := range []string{"over its NBD websocket /nbd/0", "first read from the BMC: 262144 bytes at offset 0", "Slot_0 holds the image"} {
		if !strings.Contains(progress.String(), want) {
			t.Errorf("progress = %q, want %q in it", progress, want)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("%v\n%s", err, progress)
	}
	bmc.mu.Lock()
	defer bmc.mu.Unlock()
	if bmc.slot("Slot_0").inserted {
		t.Error("Slot_0 still inserted after the stream ended")
	}
	if got := bmc.boot["BootSourceOverrideEnabled"]; got != "Disabled" {
		t.Errorf("boot override = %v after the stream, want Disabled", got)
	}
}

func TestBMCDiskServeEndsCleanlyWhenDiskEjectDisconnectsTheStream(t *testing.T) {
	bmc := openBMCMediaBMC()
	driver, progress, _ := newMediaTestBMC(t, bmc)
	other, _, _ := newMediaTestBMC(t, bmc)
	image, _ := streamTestImage(t, "debian.iso")

	cancel, done := startStream(driver, image, DiskServeOptions{Transport: TransportWebsocket})
	defer cancel()
	waitFor(t, func() bool { return strings.Contains(progress.String(), DiskReady) }, DiskReady)
	bmc.mu.Lock()
	target := bmc.boot["BootSourceOverrideTarget"]
	bmc.mu.Unlock()
	if target != "Cd" {
		t.Errorf("boot target = %v, want Cd for an ISO", target)
	}

	if err := other.DiskEject(""); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%v\n%s", err, progress)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the mount still runs after disk eject")
	}
	if !strings.Contains(progress.String(), "The BMC disconnected") {
		t.Errorf("progress = %q, want the disconnect reported", progress)
	}
	if want := []string{"Slot_0"}; !slices.Equal(bmc.ejects, want) {
		t.Errorf("ejects = %q, want %q", bmc.ejects, want)
	}
}

func TestBMCDiskServeUndoesTheMountWhenTheBMCClosesTheStream(t *testing.T) {
	bmc := openBMCMediaBMC()
	bmc.dropStream = true
	driver, progress, _ := newMediaTestBMC(t, bmc)
	driver.insertTimeout = time.Second
	image, _ := streamTestImage(t, "disk.img")

	cancel, done := startStream(driver, image, DiskServeOptions{Transport: TransportAuto, DiskOptions: DiskOptions{PowerOn: true}})
	defer cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the mount still runs after the BMC closed the stream\n%s", progress)
	}
	if err == nil || !strings.Contains(err.Error(), "Internal Error") {
		t.Fatalf("DiskServe = %v, want the BMC's close reason\n%s", err, progress)
	}
	if strings.Contains(progress.String(), DiskReady) {
		t.Errorf("progress = %q, want no %s", progress, DiskReady)
	}
	if len(bmc.patches) != 0 || slices.Contains(bmc.resets, "On") {
		t.Errorf("patches = %v, resets = %q, want no override and no power on", bmc.patches, bmc.resets)
	}
}

func TestBMCDiskServePicksTheRouteByTransportAndSlot(t *testing.T) {
	image, _ := streamTestImage(t, "disk.img")

	ami := amiMediaBMC()
	driver, _, _ := newMediaTestBMC(t, ami)
	err := driver.DiskServe(context.Background(), image, DiskServeOptions{Transport: TransportWebsocket, Listen: "127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "--transport websocket") || !strings.Contains(err.Error(), "404") {
		t.Errorf("forced websocket on a BMC without Slot_0 = %v, want a refusal with the 404", err)
	}
	if len(ami.resets) != 0 {
		t.Errorf("resets = %q, want the host left alone", ami.resets)
	}

	withInsert := openBMCMediaBMC()
	withInsert.slot("Slot_0").insert = true
	driver, _, _ = newMediaTestBMC(t, withInsert)
	err = driver.DiskServe(context.Background(), image, DiskServeOptions{Transport: TransportWebsocket, Listen: "127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "has InsertMedia") {
		t.Errorf("forced websocket on a Slot_0 with InsertMedia = %v, want a refusal", err)
	}

	bmc := openBMCMediaBMC()
	driver, progress, _ := newMediaTestBMC(t, bmc)
	cancel, done := startStream(driver, image, DiskServeOptions{Transport: TransportHTTPS, Listen: "127.0.0.1:0"})
	defer cancel()
	waitFor(t, func() bool { return strings.Contains(progress.String(), DiskReady) }, DiskReady)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("%v\n%s", err, progress)
	}
	if len(bmc.inserts) != 1 || bmc.inserts[0]["slot"] != "USB1" || bmc.streamed != nil {
		t.Errorf("inserts = %v, streamed %d bytes, want an InsertMedia into USB1 with --transport https", bmc.inserts, len(bmc.streamed))
	}
}

func TestBMCDiskStatusListsTheStreamSlot(t *testing.T) {
	bmc := openBMCMediaBMC()
	driver, _, out := newMediaTestBMC(t, bmc)

	if err := driver.DiskStatus(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Slot_0") || !strings.Contains(out.String(), "USB1") {
		t.Errorf("status = %q, want Slot_0 next to the collection's slots", out)
	}
}
