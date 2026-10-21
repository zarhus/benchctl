// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"
)

// NBD protocol constants, from the NBD protocol document (doc/proto.md in the
// nbd repository). Only the fixed newstyle handshake is served.
const (
	nbdMagic        = 0x4e42444d41474943 // "NBDMAGIC"
	nbdOptMagic     = 0x49484156454f5054 // "IHAVEOPT"
	nbdReplyMagic   = 0x0003e889045565a9 // option reply
	nbdRequestMagic = 0x25609513
	nbdSimpleReply  = 0x67446698

	nbdFlagFixedNewstyle = 1 << 0 // handshake flags
	nbdFlagNoZeroes      = 1 << 1
	nbdFlagCNoZeroes     = 1 << 1 // client flags

	nbdFlagHasFlags = 1 << 0 // transmission flags
	nbdFlagReadOnly = 1 << 1

	nbdOptExportName = 1
	nbdOptAbort      = 2
	nbdOptList       = 3
	nbdOptInfo       = 6
	nbdOptGo         = 7

	nbdRepAck        = 1
	nbdRepServer     = 2
	nbdRepInfo       = 3
	nbdRepErrUnsup   = 1<<31 | 1
	nbdRepErrInvalid = 1<<31 | 3

	nbdInfoExport = 0

	nbdCmdRead  = 0
	nbdCmdWrite = 1
	nbdCmdDisc  = 2

	nbdEPERM  = 1
	nbdEINVAL = 22

	nbdMaxOption  = 64 << 10 // longest option data read
	nbdMaxRequest = 32 << 20 // longest read or write payload
)

// nbdExport serves one read-only file to an NBD client over the fixed newstyle
// handshake. It takes any export name, answers reads, refuses writes with
// EPERM and other commands with EINVAL, and logs the handshake, the first read
// and a line every summaryEvery while reads arrive.
type nbdExport struct {
	file     io.ReaderAt
	size     int64
	progress io.Writer

	mu           sync.Mutex
	reads        int
	bytes        int64
	reported     int  // reads at the last summary
	disconnected bool // the client sent NBD_CMD_DISC
}

// nbdSummaryEvery is how often the export logs its read count.
const nbdSummaryEvery = 30 * time.Second

// serve runs the handshake and answers requests on conn until the client sends
// NBD_CMD_DISC or aborts the handshake, which return nil, or conn fails.
func (e *nbdExport) serve(conn io.ReadWriter) error {
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	ok, err := e.handshake(r, w)
	if err != nil || !ok {
		return err
	}
	fmt.Fprintf(e.progress, "NBD: the BMC's client finished the handshake, serving %d bytes read-only.\n", e.size)
	return e.transmit(r, w)
}

// handshake negotiates the export. It returns true once the client has chosen
// it, false when the client aborts.
func (e *nbdExport) handshake(r io.Reader, w *bufio.Writer) (bool, error) {
	put(w, uint64(nbdMagic), uint64(nbdOptMagic), uint16(nbdFlagFixedNewstyle|nbdFlagNoZeroes))
	if err := w.Flush(); err != nil {
		return false, err
	}
	var clientFlags uint32
	if err := binary.Read(r, binary.BigEndian, &clientFlags); err != nil {
		return false, fmt.Errorf("reading the NBD client flags: %w", err)
	}
	noZeroes := clientFlags&nbdFlagCNoZeroes != 0
	for {
		var head struct {
			Magic  uint64
			Option uint32
			Length uint32
		}
		if err := binary.Read(r, binary.BigEndian, &head); err != nil {
			return false, fmt.Errorf("reading an NBD option: %w", err)
		}
		if head.Magic != nbdOptMagic {
			return false, fmt.Errorf("NBD option with magic %#x, want %#x", head.Magic, uint64(nbdOptMagic))
		}
		if head.Length > nbdMaxOption {
			return false, fmt.Errorf("NBD option %d carries %d bytes, more than %d", head.Option, head.Length, nbdMaxOption)
		}
		data := make([]byte, head.Length)
		if _, err := io.ReadFull(r, data); err != nil {
			return false, fmt.Errorf("reading NBD option %d: %w", head.Option, err)
		}
		reply := func(kind uint32, payload []byte) {
			put(w, uint64(nbdReplyMagic), head.Option, kind, uint32(len(payload)))
			w.Write(payload)
		}
		done := false
		switch head.Option {
		case nbdOptExportName:
			put(w, uint64(e.size), uint16(nbdFlagHasFlags|nbdFlagReadOnly))
			if !noZeroes {
				w.Write(make([]byte, 124))
			}
			done = true
		case nbdOptAbort:
			reply(nbdRepAck, nil)
			return false, w.Flush()
		case nbdOptList:
			reply(nbdRepServer, []byte{0, 0, 0, 0}) // one export, named ""
			reply(nbdRepAck, nil)
		case nbdOptInfo, nbdOptGo:
			if len(data) < 6 || 4+int(binary.BigEndian.Uint32(data))+2 > len(data) {
				reply(nbdRepErrInvalid, nil)
				break
			}
			info := binary.BigEndian.AppendUint16(nil, nbdInfoExport)
			info = binary.BigEndian.AppendUint64(info, uint64(e.size))
			info = binary.BigEndian.AppendUint16(info, nbdFlagHasFlags|nbdFlagReadOnly)
			reply(nbdRepInfo, info)
			reply(nbdRepAck, nil)
			done = head.Option == nbdOptGo
		default:
			reply(nbdRepErrUnsup, nil)
		}
		if err := w.Flush(); err != nil {
			return false, err
		}
		if done {
			return true, nil
		}
	}
}

// transmit answers requests until NBD_CMD_DISC or an error. Each reply goes out
// in one write, which wsStream splits into websocket messages.
func (e *nbdExport) transmit(r io.Reader, w *bufio.Writer) error {
	buf := make([]byte, 0, 16+128<<10)
	for {
		var req struct {
			Magic  uint32
			Flags  uint16
			Type   uint16
			Cookie uint64
			Offset uint64
			Length uint32
		}
		if err := binary.Read(r, binary.BigEndian, &req); err != nil {
			return fmt.Errorf("reading an NBD request: %w", err)
		}
		if req.Magic != nbdRequestMagic {
			return fmt.Errorf("NBD request with magic %#x, want %#x", req.Magic, nbdRequestMagic)
		}
		errno := uint32(0)
		var data []byte
		switch req.Type {
		case nbdCmdDisc:
			e.mu.Lock()
			e.disconnected = true
			e.mu.Unlock()
			fmt.Fprintln(e.progress, "NBD: the BMC's client disconnected.")
			return nil
		case nbdCmdRead:
			if req.Length > nbdMaxRequest || req.Offset > uint64(e.size) || uint64(req.Length) > uint64(e.size)-req.Offset {
				errno = nbdEINVAL
				break
			}
			if cap(buf) < 16+int(req.Length) {
				buf = make([]byte, 0, 16+int(req.Length))
			}
			data = buf[16 : 16+int(req.Length)]
			if n, err := e.file.ReadAt(data, int64(req.Offset)); n < len(data) {
				return fmt.Errorf("reading the image at %d: %w", req.Offset, err)
			}
			e.countRead(req.Offset, req.Length)
		case nbdCmdWrite:
			if req.Length > nbdMaxRequest {
				return fmt.Errorf("NBD write of %d bytes, more than %d", req.Length, nbdMaxRequest)
			}
			if _, err := io.CopyN(io.Discard, r, int64(req.Length)); err != nil {
				return fmt.Errorf("reading an NBD write: %w", err)
			}
			errno = nbdEPERM
		default:
			errno = nbdEINVAL
		}
		reply := binary.BigEndian.AppendUint32(buf[:0], nbdSimpleReply)
		reply = binary.BigEndian.AppendUint32(reply, errno)
		reply = binary.BigEndian.AppendUint64(reply, req.Cookie)
		if errno == 0 {
			reply = reply[:16+len(data)]
		}
		w.Write(reply)
		if err := w.Flush(); err != nil {
			return err
		}
	}
}

// countRead counts a read and logs the first one.
func (e *nbdExport) countRead(offset uint64, length uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.reads == 0 {
		fmt.Fprintf(e.progress, "NBD: first read from the BMC: %d bytes at offset %d.\n", length, offset)
	}
	e.reads++
	e.bytes += int64(length)
}

// summarize logs the read count and bytes served every interval in which reads
// arrived, until done closes.
func (e *nbdExport) summarize(done <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			e.mu.Lock()
			if e.reads != e.reported {
				fmt.Fprintf(e.progress, "NBD: %d reads, %d bytes served.\n", e.reads, e.bytes)
				e.reported = e.reads
			}
			e.mu.Unlock()
		}
	}
}

// clientDisconnected reports whether the client sent NBD_CMD_DISC.
func (e *nbdExport) clientDisconnected() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.disconnected
}

// put writes values big-endian to w. A bufio.Writer keeps the first error for
// Flush to return.
func put(w *bufio.Writer, values ...any) {
	for _, v := range values {
		_ = binary.Write(w, binary.BigEndian, v)
	}
}
