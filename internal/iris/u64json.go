// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package iris

import (
	"encoding/binary"
	"fmt"
	"math"
)

// This file decodes IrisU64Json, the binary sibling of the plain-text
// IrisJson format Client otherwise speaks - ported directly from Arm's own
// reference decoder (FVP_RD_V3/Iris/Python/iris/u64json.py's
// _decode_recursive, in the loopctl repository), not guessed. It exists
// only to work around a real server bug, not because benchctl wants the
// binary format's performance: confirmed empirically (2026-09-16, against a
// real FVP_RD_V3 11.29.35 model) that this model's Iris server never sends
// a response to anything - not even the handshake's own
// instanceRegistry_registerInstance, the first call any client must make -
// when a client's CONNECT handshake claims support for IrisJson alone,
// exactly what benchctl's handshake used to send. The moment IrisU64Json is
// also listed (as Arm's own reference client always does, and as
// handshake's request now does), the server answers immediately - but
// always in IrisU64Json, regardless of which format the request itself was
// framed in or which format is listed first. So benchctl must decode
// IrisU64Json responses even though it still only ever sends plain
// IrisJson requests (confirmed the server accepts that fine).
//
// Only decoding is implemented, not encoding: benchctl never needs to send
// IrisU64Json, only to read a response framed in it.

// string__u8a is the short-string encoding of "__u8a", the member name
// u64json.py's compatible byte-array-in-object encoding uses - see
// decodeValue's object case.
const string__u8a = 0x20006138755F5F05

// bytesToWords reinterprets data (whose length must be a multiple of 8) as
// a little-endian uint64 word stream, the unit IrisU64Json values are
// built from.
func bytesToWords(data []byte) ([]uint64, error) {
	if len(data)%8 != 0 {
		return nil, fmt.Errorf("u64json: message length %d is not a multiple of 8", len(data))
	}
	words := make([]uint64, len(data)/8)
	for i := range words {
		words[i] = binary.LittleEndian.Uint64(data[i*8:])
	}
	return words, nil
}

// wordsToBytes is bytesToWords' inverse: the little-endian byte
// representation of words, used to pull embedded string/byte-array content
// back out of a run of words.
func wordsToBytes(words []uint64) []byte {
	b := make([]byte, len(words)*8)
	for i, w := range words {
		binary.LittleEndian.PutUint64(b[i*8:], w)
	}
	return b
}

// decodeValue decodes one U64Json value starting at words[offset], mirroring
// u64json.py's _decode_recursive. It returns the value (using the same
// dynamic Go shapes json.Unmarshal would produce - map[string]any,
// []any, string, bool, nil, float64/int64/uint64 - so an IrisU64Json result
// can be re-marshalled through encoding/json for existing call sites to
// consume exactly like an IrisJson one) and how many words it consumed.
func decodeValue(words []uint64, offset int) (value any, consumed int, err error) {
	if offset >= len(words) {
		return nil, 0, fmt.Errorf("u64json: truncated value at word %d", offset)
	}
	head := words[offset]
	msb4 := head >> 60

	switch {
	case msb4 == 0x0 || msb4 == 0xF: // +ve NumberU64, inline
		return head, 1, nil

	case msb4 == 0x1: // -ve NumberS64, inline
		return int64(head | 0xf000000000000000), 1, nil

	case msb4 >= 0x2 && msb4 <= 0x7: // short string
		strLen := int(head & 0xff)
		containerLen := (strLen >> 3) + 1
		if offset+containerLen > len(words) {
			return nil, 0, fmt.Errorf("u64json: truncated short string at word %d", offset)
		}
		b := wordsToBytes(words[offset : offset+containerLen])
		if 1+strLen > len(b) {
			return nil, 0, fmt.Errorf("u64json: short string length %d exceeds its container", strLen)
		}
		return string(b[1 : 1+strLen]), containerLen, nil

	case msb4 == 0x8: // NumberU64[]
		arrLen := int(head & 0x0fffffffffffffff)
		if offset+1+arrLen > len(words) {
			return nil, 0, fmt.Errorf("u64json: truncated NumberU64 array at word %d", offset)
		}
		arr := make([]any, arrLen)
		for i := 0; i < arrLen; i++ {
			arr[i] = words[offset+1+i]
		}
		return arr, arrLen + 1, nil

	case msb4 == 0xA: // Value[] (generic array)
		containerLen := int(head & 0x0fffffffffffffff)
		if offset+1 >= len(words) {
			return nil, 0, fmt.Errorf("u64json: truncated array header at word %d", offset)
		}
		arrLen := int(words[offset+1])
		arr := make([]any, 0, arrLen)
		cur := offset + 2
		for i := 0; i < arrLen; i++ {
			v, n, err := decodeValue(words, cur)
			if err != nil {
				return nil, 0, err
			}
			arr = append(arr, v)
			cur += n
		}
		return arr, containerLen, nil

	case msb4 == 0xB: // Object
		containerLen := int(head & 0x0fffffffffffffff)
		if offset+1 >= len(words) {
			return nil, 0, fmt.Errorf("u64json: truncated object header at word %d", offset)
		}
		memberCount := int(words[offset+1])
		cur := offset + 2

		// The compatible byte-array-wrapped-in-object encoding: a single
		// "__u8a" member holding a NumberU64[] that is itself a native
		// ByteArray. See u64json.py's own matching check in
		// _decode_recursive's Object case.
		if memberCount == 1 && cur+2 < len(words) &&
			words[cur] == string__u8a &&
			words[cur+1] == 0x8000000000000000+uint64(containerLen)-4 &&
			((words[cur+2]+7)&^uint64(7)) == 0xcb00000000000000+uint64(containerLen-5)*8 {
			v, n, err := decodeValue(words, cur+2)
			if err != nil {
				return nil, 0, err
			}
			return v, n + 4, nil
		}

		obj := make(map[string]any, memberCount)
		for i := 0; i < memberCount; i++ {
			k, n, err := decodeValue(words, cur)
			if err != nil {
				return nil, 0, err
			}
			cur += n
			key, ok := k.(string)
			if !ok {
				return nil, 0, fmt.Errorf("u64json: object member name is not a string (%T)", k)
			}
			v, n2, err := decodeValue(words, cur)
			if err != nil {
				return nil, 0, err
			}
			cur += n2
			obj[key] = v
		}
		return obj, containerLen, nil

	case msb4 == 0xC:
		msb8 := head >> 56
		switch msb8 {
		case 0xc0: // NumberU64, explicit
			if offset+1 >= len(words) {
				return nil, 0, fmt.Errorf("u64json: truncated NumberU64 at word %d", offset)
			}
			return words[offset+1], 2, nil
		case 0xc1: // NumberS64, explicit
			if offset+1 >= len(words) {
				return nil, 0, fmt.Errorf("u64json: truncated NumberS64 at word %d", offset)
			}
			return int64(words[offset+1]), 2, nil
		case 0xca: // float64
			if offset+1 >= len(words) {
				return nil, 0, fmt.Errorf("u64json: truncated float at word %d", offset)
			}
			return math.Float64frombits(words[offset+1]), 2, nil
		case 0xcb: // native ByteArray
			arrayLength := int(head & 0x00ffffffffffffff)
			consumed := (arrayLength + 15) >> 3
			if offset+consumed > len(words) {
				return nil, 0, fmt.Errorf("u64json: truncated byte array at word %d", offset)
			}
			b := wordsToBytes(words[offset+1 : offset+consumed])
			if arrayLength > len(b) {
				return nil, 0, fmt.Errorf("u64json: byte array length %d exceeds its container", arrayLength)
			}
			return append([]byte(nil), b[:arrayLength]...), consumed, nil
		case 0xcc: // long string
			strLen := int(head & 0x00ffffffffffffff)
			consumed := (strLen + 15) >> 3
			if offset+consumed > len(words) {
				return nil, 0, fmt.Errorf("u64json: truncated long string at word %d", offset)
			}
			b := wordsToBytes(words[offset+1 : offset+consumed])
			if strLen > len(b) {
				return nil, 0, fmt.Errorf("u64json: long string length %d exceeds its container", strLen)
			}
			return string(b[:strLen]), consumed, nil
		case 0xcd: // null
			return nil, 1, nil
		case 0xce: // false
			return false, 1, nil
		case 0xcf: // true
			return true, 1, nil
		default:
			return nil, 0, fmt.Errorf("u64json: invalid MSB 0x%x at word %d", msb8, offset)
		}

	default:
		return nil, 0, fmt.Errorf("u64json: unexpected value tag 0x%x at word %d", msb4, offset)
	}
}

// decodeU64JSONResponse decodes one top-level IrisU64Json response message
// - the only message shape benchctl ever needs to read, since it never
// publishes functions of its own for the server to call back into (see
// Client's own doc comment) - mirroring u64json.py's _decode_recursive
// message case (msb4 0xE, msb8 0xe2).
func decodeU64JSONResponse(data []byte) (id int64, result any, isError bool, errCode int64, errMsg string, err error) {
	words, err := bytesToWords(data)
	if err != nil {
		return 0, nil, false, 0, "", err
	}
	if len(words) < 3 {
		return 0, nil, false, 0, "", fmt.Errorf("u64json: message too short (%d words)", len(words))
	}
	head := words[0]
	msb4 := head >> 60
	if msb4 != 0xE {
		return 0, nil, false, 0, "", fmt.Errorf("u64json: top-level value is not a message (tag 0x%x)", msb4)
	}
	msb8 := head >> 56
	version := (head >> 48) & 0xff
	if version != 0x20 {
		return 0, nil, false, 0, "", fmt.Errorf("u64json: unsupported message version 0x%x", version)
	}
	if msb8 != 0xe2 {
		return 0, nil, false, 0, "", fmt.Errorf("u64json: expected a response message, got tag 0x%x", msb8)
	}
	id = int64(words[1])
	// words[2] is "dest" (the destination instId the server encodes
	// responses with) - irrelevant here, a one-shot client has nowhere else
	// to route it.
	offset := 3
	if offset >= len(words) {
		return 0, nil, false, 0, "", fmt.Errorf("u64json: truncated response error code")
	}
	code := int64(words[offset])
	offset++
	if code == 0 {
		v, _, err := decodeValue(words, offset)
		if err != nil {
			return 0, nil, false, 0, "", err
		}
		return id, v, false, 0, "", nil
	}
	v, _, err := decodeValue(words, offset)
	if err != nil {
		return 0, nil, false, 0, "", err
	}
	msg, ok := v.(string)
	if !ok {
		return 0, nil, false, 0, "", fmt.Errorf("u64json: error message is not a string (%T)", v)
	}
	// A trailing error 'data' field, if present, is dropped here: rpcError
	// (shared with the plain IrisJson path) has nowhere to carry it either.
	return id, nil, true, code, msg, nil
}
