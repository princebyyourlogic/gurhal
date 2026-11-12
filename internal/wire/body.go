// SPDX-License-Identifier: GPL-3.0-or-later

package wire

import (
	"encoding/binary"
	"unicode/utf16"
)

// Body builds a packet body field by field, little-endian unless a method says otherwise:
//
//	wire.Encode(0x0223, wire.Body{}.Zero(20).Bytes(tag).U32(gid))
type Body []byte

func (b Body) U8(v uint8) Body     { return append(b, v) }
func (b Body) U16(v uint16) Body   { return binary.LittleEndian.AppendUint16(b, v) }
func (b Body) U32(v uint32) Body   { return binary.LittleEndian.AppendUint32(b, v) }
func (b Body) Bytes(p []byte) Body { return append(b, p...) }
func (b Body) Zero(n int) Body     { return append(b, make([]byte, n)...) }
func (b Body) UTF16(units []uint16) Body {
	for _, u := range units {
		b = b.U16(u)
	}
	return b
}

// UTF16 encodes s as the client's UTF-16 text (no terminator).
func UTF16(s string) []uint16 { return utf16.Encode([]rune(s)) }

// Latin1 decodes a fixed-size, NUL-padded Latin-1 field (names and passwords in the
// login packets).
func Latin1(field []byte) string {
	r := make([]rune, 0, len(field))
	for _, c := range field {
		if c == 0 {
			break
		}
		r = append(r, rune(c))
	}
	return string(r)
}
