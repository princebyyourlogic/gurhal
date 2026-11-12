// SPDX-License-Identifier: GPL-3.0-or-later

// Package patch answers the discs' patch servers (11110 PSU, 11140 AotI) by telling the
// client it is up to date. The framing is EGS's patch protocol, not the login/game one:
//
//	u32le size (header included) | u16le command | u16 0 | body
//
//	S>C 0x01  hello, encryption off
//	C>S 0x02  PSU's hello, 32 bytes (e44c0915 0 0 1 1 000100be)
//	    0x14  AotI's, the PC-sized 52 bytes: it sends that one because our hello has a
//	          non-zero word at +0x18
//	S>C 0x13  update complete
//
// Neither PS2 disc knows the PC file-list commands (AotI drops the connection on 0x06,
// PSU reports "Failed to download system file"), so 0x13 follows the hello at once.
package patch

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/princebyyourlogic/gurhal/internal/netx"
)

const (
	headerSize = 8
	maxPacket  = 0x1000

	cmdHello    = 0x01
	cmdHelloPSU = 0x02
	cmdHelloPC  = 0x14
	cmdComplete = 0x13
)

var (
	errSize = errors.New("patch: bad packet size")

	hello    = packet(cmdHello, helloBody())
	complete = packet(cmdComplete, nil)
)

func helloBody() []byte {
	b := make([]byte, 32)
	copy(b, []byte{0x8b, 0x9f, 0x2d, 0xfa})
	binary.LittleEndian.PutUint32(b[16:], 1)
	return b
}

func packet(cmd uint16, body []byte) []byte {
	out := make([]byte, headerSize+len(body))
	binary.LittleEndian.PutUint32(out, uint32(len(out)))
	binary.LittleEndian.PutUint16(out[4:], cmd)
	copy(out[headerSize:], body)
	return out
}

func read(r io.Reader) (uint16, []byte, error) {
	var h [headerSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.LittleEndian.Uint32(h[:]))
	if n < headerSize || n > maxPacket {
		return 0, nil, fmt.Errorf("%w: %d", errSize, n)
	}
	body := make([]byte, n-headerSize)
	_, err := io.ReadFull(r, body)
	return binary.LittleEndian.Uint16(h[4:]), body, err
}

type Service struct {
	Log *slog.Logger
}

func (s *Service) Handle(_ context.Context, c *netx.Conn) {
	if c.Send(hello) != nil {
		return
	}
	for {
		cmd, body, err := read(c.R)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.Log.Debug("patch closed", "err", err)
			}
			return
		}
		switch cmd {
		case cmdHelloPSU, cmdHelloPC:
			c.Log.Info("patch: client up to date", "hello", fmt.Sprintf("%#02x", cmd), "body", fmt.Sprintf("%x", body))
			if c.Send(complete) != nil {
				return
			}
		default:
			c.Log.Debug("patch: command ignored", "cmd", fmt.Sprintf("%#02x", cmd), "size", len(body))
		}
	}
}
