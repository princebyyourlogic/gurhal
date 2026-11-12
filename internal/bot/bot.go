// SPDX-License-Identifier: GPL-3.0-or-later

// Package bot is a protocol client that plays the PS2 discs' side over plain TCP (psud's
// PSU_PLAIN_PORTS, or the Python server's plain ports): login, character creation, the
// lobby. It records every packet the server sends, fragments reassembled and acked.
package bot

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// SampleCharacter is a 0x0D02 body as the PSU client sends it (testplay's): slot 0,
// name, race, gender, type, appearance. Name, race, gender and type are set per bot.
var SampleCharacter, _ = hex.DecodeString("0000000000000000000000000000000000000000000000000000000000000000000000000000000054006500730074000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000017f000000000600000006000100060002000000030101000400280005000000000000000000000000ff7f00003cb60200ff7f0000ff7f000000000000ef8d0000ffff020079d400009a3f0100da1d0100ffff0000ffff00000f070000136a000001000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001000000010000000100000001000000010000000100000001000000010000000100000001000000010000000100000001000000010000000100000001000000")

const maxPacket = 0x40000

var ErrTimeout = errors.New("bot: timed out")

// Client is one connection to a login or game server.
type Client struct {
	conn net.Conn
	mu   sync.Mutex
	cond *sync.Cond
	rx   []wire.Packet
	err  error // set when the connection ends
}

// Dialer opens connections; set it to dial SSLv3 for a server's real ports.
var Dialer = func(addr string) (net.Conn, error) { return net.DialTimeout("tcp", addr, 5*time.Second) }

func Dial(addr string) (*Client, error) {
	conn, err := Dialer(addr)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn}
	c.cond = sync.NewCond(&c.mu)
	go c.read()
	return c, nil
}

func (c *Client) read() {
	frags := map[uint32][]byte{}
	var err error
	for {
		var p wire.Packet
		if p, err = wire.Read(c.conn, maxPacket); err != nil {
			break
		}
		if p.Cmd == wire.CmdFragment && len(p.Body) >= 8 { // total, offset, chunk: ack each one
			total, off := binary.LittleEndian.Uint32(p.Body), binary.LittleEndian.Uint32(p.Body[4:])
			buf := frags[total]
			if buf == nil {
				buf = make([]byte, total)
				frags[total] = buf
			}
			copy(buf[off:], p.Body[8:])
			c.conn.Write(wire.EncodeOn(wire.ChanClient, wire.CmdFragmentAck, nil))
			if int(off)+len(p.Body)-8 < int(total) {
				continue
			}
			delete(frags, total)
			if p, err = wire.Read(bytesReader(buf), maxPacket); err != nil {
				break
			}
		}
		if p.Cmd == 0 && len(p.Body) == 0 { // keepalive
			continue
		}
		c.mu.Lock()
		c.rx = append(c.rx, p)
		c.cond.Broadcast()
		c.mu.Unlock()
	}
	c.mu.Lock()
	if c.err = err; errors.Is(err, io.EOF) {
		c.err = io.EOF
	}
	c.cond.Broadcast()
	c.mu.Unlock()
}

// Send writes a client packet: a 0x24-byte head (zero unless given) and payload.
func (c *Client) Send(cmd uint16, payload []byte) error {
	return c.SendHead(cmd, make([]byte, 0x24), payload)
}

func (c *Client) SendHead(cmd uint16, head, payload []byte) error {
	return c.SendRaw(wire.EncodeOn(wire.ChanClient, cmd, append(append([]byte(nil), head...), payload...)))
}

func (c *Client) SendRaw(pkt []byte) error {
	_, err := c.conn.Write(pkt)
	return err
}

// Mark is the index of the next packet to arrive.
func (c *Client) Mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.rx)
}

// Since returns the packets received from mark on.
func (c *Client) Since(mark int) []wire.Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]wire.Packet(nil), c.rx[min(mark, len(c.rx)):]...)
}

// Clear forgets the packets received so far (a long-running bot's memory).
func (c *Client) Clear() {
	c.mu.Lock()
	c.rx = c.rx[:0]
	c.mu.Unlock()
}

// Wait returns the first packet cmd received at or after mark.
func (c *Client) Wait(cmd uint16, mark int, timeout time.Duration) (wire.Packet, error) {
	return c.WaitFor(cmd, mark, timeout, nil)
}

// WaitFor is Wait for the first cmd that pred accepts (nil: any).
func (c *Client) WaitFor(cmd uint16, mark int, timeout time.Duration, pred func(wire.Packet) bool) (wire.Packet, error) {
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() { c.mu.Lock(); c.cond.Broadcast(); c.mu.Unlock() })
	defer timer.Stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		for _, p := range c.rx[min(mark, len(c.rx)):] {
			if p.Cmd == cmd && (pred == nil || pred(p)) {
				return p, nil
			}
		}
		if c.err != nil {
			return wire.Packet{}, fmt.Errorf("bot: waiting for %#04x: %w", cmd, c.err)
		}
		if time.Now().After(deadline) {
			return wire.Packet{}, fmt.Errorf("%w waiting for %#04x", ErrTimeout, cmd)
		}
		c.cond.Wait()
	}
}

// Closed reports whether the server ended the connection.
func (c *Client) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err != nil
}

func (c *Client) Close() error { return c.conn.Close() }

// UTF16 encodes s for a packet field.
func UTF16(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

type byteReader struct {
	b []byte
}

func bytesReader(b []byte) *byteReader { return &byteReader{b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
