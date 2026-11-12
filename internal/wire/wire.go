// SPDX-License-Identifier: GPL-3.0-or-later

// Package wire implements the login and game server framing both PS2 discs use (EGS
// docs/protocol):
//
//	u32le size (header included, padded to 4) | u16 BE command | u8 channel | u8 0 | body
//
// The client sends on channel 2 and the server answers on 3; channel 1 is a cast the
// server relays to the rest of the zone. A packet over FragmentLimit bytes goes out as
// 0x0B03 fragments, each acknowledged by the client with an empty 0x0B05.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	HeaderSize     = 8
	FragmentLimit  = 0x4000
	CmdFragment    = 0x0B03
	CmdFragmentAck = 0x0B05

	ChanCast   = 1
	ChanClient = 2
	ChanServer = 3
)

var ErrSize = errors.New("wire: bad packet size")

type Packet struct {
	Cmd     uint16
	Channel uint8
	Body    []byte // everything after the header, padding included
}

// Read reads one packet, refusing sizes below the header or above max.
func Read(r io.Reader, max int) (Packet, error) {
	var h [HeaderSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Packet{}, err
	}
	n := int(binary.LittleEndian.Uint32(h[:]))
	if n < HeaderSize || n > max {
		return Packet{}, fmt.Errorf("%w: %d", ErrSize, n)
	}
	body := make([]byte, n-HeaderSize)
	if _, err := io.ReadFull(r, body); err != nil {
		return Packet{}, err
	}
	return Packet{Cmd: binary.BigEndian.Uint16(h[4:]), Channel: h[6], Body: body}, nil
}

// IsFragmentAck reports whether p is the client's 0x0B05 for a fragment.
func (p Packet) IsFragmentAck() bool { return p.Cmd == CmdFragmentAck && len(p.Body) == 0 }

// Encode frames a server packet (channel 3).
func Encode(cmd uint16, body []byte) []byte { return EncodeOn(ChanServer, cmd, body) }

// EncodeOn frames a packet on the given channel, zero-padding it to 4 bytes.
func EncodeOn(channel uint8, cmd uint16, body []byte) []byte {
	n := (HeaderSize + len(body) + 3) &^ 3
	out := make([]byte, n)
	binary.LittleEndian.PutUint32(out, uint32(n))
	binary.BigEndian.PutUint16(out[4:], cmd)
	out[6] = channel
	copy(out[HeaderSize:], body)
	return out
}

// Fragments splits a framed packet into the pieces that go on the wire: the packet
// itself, or 0x0B03 fragments of at most FragmentLimit bytes each:
//
//	u32le 16 + chunk size | 0b 03 00 00 | u32le total size | u32le offset | chunk
func Fragments(pkt []byte) [][]byte {
	if len(pkt) <= FragmentLimit {
		return [][]byte{pkt}
	}
	out := make([][]byte, 0, (len(pkt)+FragmentLimit-1)/FragmentLimit)
	for o := 0; o < len(pkt); o += FragmentLimit {
		chunk := pkt[o:min(o+FragmentLimit, len(pkt))]
		f := make([]byte, 16+len(chunk))
		binary.LittleEndian.PutUint32(f, uint32(len(f)))
		binary.BigEndian.PutUint16(f[4:], CmdFragment)
		binary.LittleEndian.PutUint32(f[8:], uint32(len(pkt)))
		binary.LittleEndian.PutUint32(f[12:], uint32(o))
		copy(f[16:], chunk)
		out = append(out, f)
	}
	return out
}
