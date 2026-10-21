// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/stmcginnis/gofish/schemas"
)

// The websocket route: benchctl connects to the BMC's NBD websocket and serves
// the image's blocks as an NBD server, and the BMC's NBD client presents them
// to the host as a USB device. bmcweb offers it with its NBD proxy built in,
// as /nbd/<n> for the slot Slot_<n>. The slot answers a GET by name and has
// EjectMedia and no InsertMedia, and the VirtualMedia collection may leave it
// out.

// streamSlotID is the slot the websocket route serves, the one bmcweb links to
// /nbd/0.
const streamSlotID = "Slot_0"

// streamSlot returns the slot a manager offers for the websocket route, or nil
// with what each manager answered when none does.
func (b *bmcPlatform) streamSlot() (*schemas.VirtualMedia, []string, error) {
	_, managers, err := b.mediaManagers()
	if err != nil {
		return nil, nil, err
	}
	var answers []string
	for _, manager := range managers {
		uri := manager.ODataID + "/VirtualMedia/" + streamSlotID
		slot, err := schemas.GetVirtualMedia(b.media, uri)
		var redfishErr *schemas.Error
		switch {
		case errors.As(err, &redfishErr):
			answers = append(answers, fmt.Sprintf("GET %s: %v", uri, redfishError(err)))
		case err != nil:
			return nil, nil, fmt.Errorf("GET %s: %w", uri, err)
		case slot.SupportsMediaInsert:
			answers = append(answers, fmt.Sprintf("%s has InsertMedia", uri))
		default:
			return slot, nil, nil
		}
	}
	return nil, answers, nil
}

// routeSlot picks the route for a local image: the stream slot for websocket,
// or for auto when the BMC offers one, and nil for HTTPS, which an empty
// transport means too. A forced websocket fails on a BMC without the slot.
func (b *bmcPlatform) routeSlot(transport MediaTransport) (*schemas.VirtualMedia, error) {
	if transport != TransportAuto && transport != TransportWebsocket {
		return nil, nil
	}
	if _, err := b.mediaClient(); err != nil {
		return nil, err
	}
	slot, answers, err := b.streamSlot()
	if err != nil || slot != nil || transport != TransportWebsocket {
		return slot, err
	}
	return nil, fmt.Errorf("--transport websocket: the BMC offers no %s for an NBD websocket (%s): use --transport https or auto", streamSlotID, strings.Join(answers, ", "))
}

// allSlots returns the slots listSlots finds and the stream slot when the
// collection leaves it out.
func (b *bmcPlatform) allSlots() ([]*schemas.VirtualMedia, error) {
	slots, err := b.listSlots()
	if err != nil {
		return nil, err
	}
	stream, _, err := b.streamSlot()
	if err != nil || stream == nil {
		return slots, err
	}
	for _, slot := range slots {
		if slot.ODataID == stream.ODataID {
			return slots, nil
		}
	}
	return append(slots, stream), nil
}

// streamEndpoint is the websocket path for slot: the slot's
// Oem.OpenBMC.WebSocketEndpoint, or /nbd/<n> for Slot_<n>.
func streamEndpoint(slot *schemas.VirtualMedia) string {
	var oem struct {
		OpenBMC struct {
			WebSocketEndpoint string
		}
	}
	if json.Unmarshal(slot.OEM, &oem) == nil && oem.OpenBMC.WebSocketEndpoint != "" {
		return oem.OpenBMC.WebSocketEndpoint
	}
	return "/nbd/" + strings.TrimPrefix(slot.ID, "Slot_")
}

// streamDialTimeout bounds the websocket handshake.
const streamDialTimeout = 30 * time.Second

// dialStream opens the NBD websocket for slot with basic auth, over the
// driver's HTTP client, so the jump host and the certificate settings apply.
func (b *bmcPlatform) dialStream(slot *schemas.VirtualMedia) (*websocket.Conn, error) {
	u := url.URL{Scheme: "wss", Host: b.access.IP, Path: streamEndpoint(slot)}
	if base, err := url.Parse(b.redfish.BaseURL); err == nil {
		u.Host = base.Host
	}
	header := http.Header{}
	header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(b.access.User+":"+b.access.Password)))
	ctx, cancel := context.WithTimeout(context.Background(), streamDialTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient:      b.redfish.HTTP,
		HTTPHeader:      header,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("opening the NBD websocket %s: %s: %w", u.Path, resp.Status, err)
		}
		return nil, fmt.Errorf("opening the NBD websocket %s: %w", u.Path, err)
	}
	// bmcweb sends what the NBD client wrote as one message, up to about
	// 512 KiB.
	conn.SetReadLimit(-1)
	return conn, nil
}

// wsStream is a websocket as a byte stream: reads run through the binary
// messages in turn, and writes go out as binary messages of at most
// streamMessageMax bytes. A read returns the websocket.CloseError that ends the
// connection, reason included.
type wsStream struct {
	conn   *websocket.Conn
	reader io.Reader
}

func (s *wsStream) Read(p []byte) (int, error) {
	for {
		if s.reader == nil {
			_, reader, err := s.conn.Reader(context.Background())
			if err != nil {
				return 0, err
			}
			s.reader = reader
		}
		n, err := s.reader.Read(p)
		if errors.Is(err, io.EOF) {
			s.reader = nil
			if n == 0 {
				continue
			}
			err = nil
		}
		return n, err
	}
}

// streamMessageMax is the longest message benchctl sends. bmcweb reads each
// message into a buffer of 131088 bytes, 128 KiB and an NBD reply header, and
// closes the websocket on a longer one. It copies the bytes to the NBD client in
// order, so message boundaries carry no meaning.
const streamMessageMax = 128 << 10

func (s *wsStream) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n := min(len(p)-written, streamMessageMax)
		if err := s.conn.Write(context.Background(), websocket.MessageBinary, p[written:written+n]); err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

// streamClosed describes why the stream ended, with the BMC's close reason.
func streamClosed(err error) error {
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		reason := closeErr.Reason
		if reason == "" {
			reason = "no reason given"
		}
		return fmt.Errorf("the BMC closed the NBD websocket: %s (status %d)", reason, closeErr.Code)
	}
	return fmt.Errorf("the NBD stream ended: %w", err)
}

// streamMount streams image to slot over the NBD websocket for the host to
// boot: remote media on, host off, inserted media ejected, the websocket open,
// the slot reporting the image inserted, the boot override set, and the host on
// with opts.PowerOn. It then prints DiskReady and serves until ctx ends, which
// closes the websocket and clears the boot override.
//
// When ctx ends before DiskReady, streamMount stops after the step it is in.
// When that or a failure comes after the websocket opened, it closes the
// websocket, powers the host off, ejects the slots and clears the boot
// override. When the BMC's NBD client disconnects, as disk eject makes it,
// streamMount clears the boot override and returns. Any other end of the
// websocket clears the override and fails with the BMC's close reason.
func (b *bmcPlatform) streamMount(ctx context.Context, image string, opts DiskOptions, slot *schemas.VirtualMedia) error {
	interrupted := func() error { return pause(ctx, 0) }
	stopNotice := context.AfterFunc(ctx, func() {
		fmt.Fprintln(b.progress, "Interrupted: stopping after the current step and undoing the mount. Interrupt again to exit without undoing it.")
	})
	defer stopNotice()
	file, err := os.Open(image)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", image)
	}
	kind, err := mediaKindFor(image, opts.Medium)
	if err != nil {
		return err
	}
	_, managers, err := b.mediaManagers()
	if err == nil {
		err = b.enableRemoteMedia(ctx, managers)
	}
	if err == nil {
		err = interrupted()
	}
	if err != nil {
		return err
	}
	if err := b.setPower(ctx, PowerOff); err != nil {
		return err
	}
	slots, err := b.allSlots()
	if err == nil {
		err = b.ejectSlots(slots)
	}
	if err == nil {
		err = interrupted()
	}
	if err != nil {
		return err
	}

	fmt.Fprintf(b.progress, "Streaming %s to the BMC over its NBD websocket %s...\n", image, streamEndpoint(slot))
	conn, err := b.dialStream(slot)
	if err != nil {
		return err
	}
	export := &nbdExport{file: file, size: info.Size(), progress: b.progress}
	served := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		err := export.serve(&wsStream{conn: conn})
		close(done)
		served <- err
	}()
	go export.summarize(done, nbdSummaryEvery)
	closeStream := func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		<-done
	}
	defer closeStream()
	undo := func(err error) error {
		closeStream()
		// The BMC tears the slot down after the websocket closes. The eject in
		// undoMount reports a slot that stays inserted.
		_ = b.waitStreamEjected(slot)
		return b.undoMount(err)
	}

	if err := b.waitStreamInserted(ctx, slot, served); err != nil {
		return undo(err)
	}
	enabled := schemas.ContinuousBootSourceOverrideEnabled
	if opts.BootOnce {
		enabled = schemas.OnceBootSourceOverrideEnabled
	}
	steps := []func() error{
		interrupted,
		func() error { return b.setBootOverride(ctx, kind.target, enabled) },
		interrupted,
	}
	if opts.PowerOn {
		steps = append(steps, func() error { return b.setPower(ctx, PowerOn) }, interrupted)
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return undo(err)
		}
	}
	stopNotice()
	fmt.Fprintln(b.progress, DiskReady)

	select {
	case <-ctx.Done():
		fmt.Fprintln(b.progress, "Stopping: closing the NBD websocket.")
		closeStream()
		return errors.Join(b.waitStreamEjected(slot), b.clearBootOverride())
	case err := <-served:
		if err == nil && export.clientDisconnected() {
			fmt.Fprintf(b.progress, "The BMC disconnected %s from %s.\n", image, slot.ID)
			return b.clearBootOverride()
		}
		if err == nil {
			err = errors.New("the BMC's NBD client ended the handshake")
		}
		return errors.Join(streamClosed(err), b.clearBootOverride())
	}
}

// streamDiedHint follows a stream that ends before the slot reports the image
// inserted. An OpenBMC virtual media service crashed on the first Redfish read of
// an active proxy slot, which left the BMC's NBD device busy until a reboot.
const streamDiedHint = "the BMC's virtual media service may have stopped, some crash when Redfish reads an active proxy slot, " +
	"and its journal says why. A crashed service can hold the BMC's NBD device until the BMC reboots"

// waitStreamInserted waits up to insertTimeout for slot to report the image
// inserted, which the BMC reports once the host's USB device is up. It fails
// early when the stream ends.
func (b *bmcPlatform) waitStreamInserted(ctx context.Context, slot *schemas.VirtualMedia, served <-chan error) error {
	fmt.Fprintf(b.progress, "Waiting for %s to report the image inserted...\n", slot.ID)
	deadline := time.Now().Add(b.insertTimeout)
	for {
		current, err := schemas.GetVirtualMedia(b.media, slot.ODataID)
		if err != nil {
			return fmt.Errorf("GET %s: %w", slot.ODataID, redfishError(err))
		}
		if inserted(current) {
			fmt.Fprintf(b.progress, "%s holds the image.\n", slot.ID)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s does not report the image inserted after %s (ConnectedVia %s): the BMC may have failed to present it to the host, its journal for the virtual media service may say why",
				slot.ID, b.insertTimeout, orDash(string(current.ConnectedVia)))
		}
		select {
		case err := <-served:
			if err == nil {
				err = errors.New("the BMC's NBD client disconnected")
			}
			return fmt.Errorf("before %s reported the image inserted: %w: %s", slot.ID, streamClosed(err), streamDiedHint)
		case <-ctx.Done():
			return errInterrupted
		case <-time.After(b.pollEvery):
		}
	}
}

// waitStreamEjected waits up to insertTimeout for slot to report no image once
// the websocket is closed.
func (b *bmcPlatform) waitStreamEjected(slot *schemas.VirtualMedia) error {
	deadline := time.Now().Add(b.insertTimeout)
	for {
		current, err := schemas.GetVirtualMedia(b.media, slot.ODataID)
		if err != nil {
			return fmt.Errorf("GET %s: %w", slot.ODataID, redfishError(err))
		}
		if !inserted(current) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s still reports the image inserted %s after the websocket closed: run disk eject", slot.ID, b.insertTimeout)
		}
		time.Sleep(b.pollEvery)
	}
}
