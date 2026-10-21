// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
)

// nbdTestClient is the client side of the NBD protocol, as the BMC's nbd-client
// and kernel speak it.
type nbdTestClient struct {
	r      *bufio.Reader
	w      io.Writer
	cookie uint64
}

func newNBDTestClient(conn io.ReadWriter) *nbdTestClient {
	return &nbdTestClient{r: bufio.NewReader(conn), w: conn}
}

// greet reads the server greeting and sends the client flags.
func (c *nbdTestClient) greet(clientFlags uint32) error {
	var greeting struct {
		Magic, OptMagic uint64
		Flags           uint16
	}
	if err := binary.Read(c.r, binary.BigEndian, &greeting); err != nil {
		return err
	}
	if greeting.Magic != nbdMagic || greeting.OptMagic != nbdOptMagic || greeting.Flags&nbdFlagFixedNewstyle == 0 {
		return fmt.Errorf("greeting = %+v, want fixed newstyle", greeting)
	}
	return binary.Write(c.w, binary.BigEndian, clientFlags)
}

// option sends an option with data.
func (c *nbdTestClient) option(option uint32, data []byte) error {
	msg := binary.BigEndian.AppendUint64(nil, nbdOptMagic)
	msg = binary.BigEndian.AppendUint32(msg, option)
	msg = binary.BigEndian.AppendUint32(msg, uint32(len(data)))
	_, err := c.w.Write(append(msg, data...))
	return err
}

// optionReply reads one option reply.
func (c *nbdTestClient) optionReply() (uint32, []byte, error) {
	var head struct {
		Magic        uint64
		Option, Type uint32
		Length       uint32
	}
	if err := binary.Read(c.r, binary.BigEndian, &head); err != nil {
		return 0, nil, err
	}
	if head.Magic != nbdReplyMagic {
		return 0, nil, fmt.Errorf("option reply magic %#x", head.Magic)
	}
	data := make([]byte, head.Length)
	_, err := io.ReadFull(c.r, data)
	return head.Type, data, err
}

// handshake negotiates the default export with NBD_OPT_GO, as nbd-client does,
// and returns its size and transmission flags.
func (c *nbdTestClient) handshake() (uint64, uint16, error) {
	if err := c.greet(nbdFlagFixedNewstyle | nbdFlagCNoZeroes); err != nil {
		return 0, 0, err
	}
	// An empty name and no information requests.
	if err := c.option(nbdOptGo, make([]byte, 6)); err != nil {
		return 0, 0, err
	}
	var size uint64
	var flags uint16
	for {
		kind, data, err := c.optionReply()
		if err != nil {
			return 0, 0, err
		}
		switch kind {
		case nbdRepInfo:
			if len(data) == 12 && binary.BigEndian.Uint16(data) == nbdInfoExport {
				size = binary.BigEndian.Uint64(data[2:])
				flags = binary.BigEndian.Uint16(data[10:])
			}
		case nbdRepAck:
			return size, flags, nil
		default:
			return 0, 0, fmt.Errorf("NBD_OPT_GO reply %#x", kind)
		}
	}
}

// request sends a request and returns the reply's error and, for a read that
// succeeds, its data.
func (c *nbdTestClient) request(kind uint16, offset uint64, length uint32, payload []byte) (uint32, []byte, error) {
	c.cookie++
	msg := binary.BigEndian.AppendUint32(nil, nbdRequestMagic)
	msg = binary.BigEndian.AppendUint16(msg, 0)
	msg = binary.BigEndian.AppendUint16(msg, kind)
	msg = binary.BigEndian.AppendUint64(msg, c.cookie)
	msg = binary.BigEndian.AppendUint64(msg, offset)
	msg = binary.BigEndian.AppendUint32(msg, length)
	if _, err := c.w.Write(append(msg, payload...)); err != nil {
		return 0, nil, err
	}
	if kind == nbdCmdDisc {
		return 0, nil, nil
	}
	var reply struct {
		Magic, Errno uint32
		Cookie       uint64
	}
	if err := binary.Read(c.r, binary.BigEndian, &reply); err != nil {
		return 0, nil, err
	}
	if reply.Magic != nbdSimpleReply || reply.Cookie != c.cookie {
		return 0, nil, fmt.Errorf("reply %+v, want a simple reply with cookie %d", reply, c.cookie)
	}
	if reply.Errno != 0 || kind != nbdCmdRead {
		return reply.Errno, nil, nil
	}
	data := make([]byte, length)
	_, err := io.ReadFull(c.r, data)
	return 0, data, err
}

// readAll reads the export in chunks of chunk bytes.
func (c *nbdTestClient) readAll(size uint64, chunk uint32) ([]byte, error) {
	var all []byte
	for offset := uint64(0); offset < size; offset += uint64(chunk) {
		length := uint64(chunk)
		if size-offset < length {
			length = size - offset
		}
		errno, data, err := c.request(nbdCmdRead, offset, uint32(length), nil)
		if err != nil {
			return nil, err
		}
		if errno != 0 {
			return nil, fmt.Errorf("read at %d: errno %d", offset, errno)
		}
		all = append(all, data...)
	}
	return all, nil
}

// startNBDExport serves image on one end of a pipe and returns a client on the
// other, with the export and serve's result.
func startNBDExport(t *testing.T, image []byte) (*nbdTestClient, *nbdExport, <-chan error, *syncWriter) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	progress := &syncWriter{}
	export := &nbdExport{file: bytes.NewReader(image), size: int64(len(image)), progress: progress}
	served := make(chan error, 1)
	go func() { served <- export.serve(server) }()
	return newNBDTestClient(client), export, served, progress
}

func TestNBDExportServesReadsAndRefusesWrites(t *testing.T) {
	image := []byte("0123456789abcdef")
	client, export, served, progress := startNBDExport(t, image)

	size, flags, err := client.handshake()
	if err != nil {
		t.Fatal(err)
	}
	if size != uint64(len(image)) || flags != nbdFlagHasFlags|nbdFlagReadOnly {
		t.Errorf("export size %d, flags %#x, want %d and read-only", size, flags, len(image))
	}
	if errno, data, err := client.request(nbdCmdRead, 2, 5, nil); err != nil || errno != 0 || string(data) != "23456" {
		t.Errorf("read 5 at 2 = %d %q %v, want 23456", errno, data, err)
	}
	if errno, _, err := client.request(nbdCmdRead, 12, 8, nil); err != nil || errno != nbdEINVAL {
		t.Errorf("read past the end = %d %v, want EINVAL", errno, err)
	}
	if errno, _, err := client.request(nbdCmdWrite, 0, 4, []byte("xxxx")); err != nil || errno != nbdEPERM {
		t.Errorf("write = %d %v, want EPERM", errno, err)
	}
	if errno, _, err := client.request(3, 0, 0, nil); err != nil || errno != nbdEINVAL {
		t.Errorf("flush = %d %v, want EINVAL", errno, err)
	}
	if all, err := client.readAll(size, 5); err != nil || !bytes.Equal(all, image) {
		t.Errorf("read all = %q %v, want the image", all, err)
	}
	if _, _, err := client.request(nbdCmdDisc, 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Errorf("serve = %v, want nil after NBD_CMD_DISC", err)
	}
	if !export.clientDisconnected() {
		t.Error("clientDisconnected = false after NBD_CMD_DISC")
	}
	for _, want := range []string{"serving 16 bytes read-only", "first read from the BMC: 5 bytes at offset 2", "disconnected"} {
		if !bytes.Contains([]byte(progress.String()), []byte(want)) {
			t.Errorf("progress = %q, want %q in it", progress, want)
		}
	}
}

func TestNBDExportAnswersExportNameAfterAnUnknownOption(t *testing.T) {
	image := []byte("abc")
	client, _, served, _ := startNBDExport(t, image)

	if err := client.greet(nbdFlagFixedNewstyle); err != nil {
		t.Fatal(err)
	}
	if err := client.option(5, nil); err != nil { // NBD_OPT_STARTTLS
		t.Fatal(err)
	}
	if kind, _, err := client.optionReply(); err != nil || kind != nbdRepErrUnsup {
		t.Fatalf("STARTTLS reply = %#x %v, want NBD_REP_ERR_UNSUP", kind, err)
	}
	if err := client.option(nbdOptExportName, nil); err != nil {
		t.Fatal(err)
	}
	var export struct {
		Size  uint64
		Flags uint16
		Zero  [124]byte
	}
	if err := binary.Read(client.r, binary.BigEndian, &export); err != nil {
		t.Fatal(err)
	}
	if export.Size != 3 || export.Flags != nbdFlagHasFlags|nbdFlagReadOnly {
		t.Errorf("export = %d %#x, want 3 bytes, read-only", export.Size, export.Flags)
	}
	if errno, data, err := client.request(nbdCmdRead, 0, 3, nil); err != nil || errno != 0 || string(data) != "abc" {
		t.Errorf("read = %d %q %v, want abc", errno, data, err)
	}
	client.w.(net.Conn).Close()
	if err := <-served; err == nil {
		t.Error("serve = nil after the connection closed, want an error")
	}
}
