// SPDX-License-Identifier: GPL-3.0-or-later

package bot

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/wire"
)

const step = 10 * time.Second // how long one step of a flow may take

// Player is a bot that logs in and plays: what the PS2 sends, in its order.
type Player struct {
	Name      string // account and character name
	Disc      string // "psu" or "aoti"
	LoginAddr string
	GameAddr  string

	GID  uint32
	LID  uint16
	Game *Client
	nb   uint32 // the client's 0x0806 / 0x0808 counter
	key  []byte
}

// Login signs in on the login server and takes the game key (0x0223).
func (p *Player) Login(password string) error {
	c, err := Dial(p.LoginAddr)
	if err != nil {
		return err
	}
	defer c.Close()
	platform := byte(0)
	if p.Disc == "aoti" {
		platform = 1
	}
	c.Send(0x080E, append([]byte{0, 1, 0, platform}, make([]byte, 16)...))
	creds := make([]byte, 48)
	copy(creds, p.Name)
	copy(creds[24:], password)
	m := c.Mark()
	c.Send(0x0219, creds)
	r, err := c.Wait(0x0223, m, step)
	if err != nil {
		return err
	}
	if len(r.Body) != 0x2C {
		return fmt.Errorf("bot %s: login refused", p.Name)
	}
	p.GID, p.key = binary.LittleEndian.Uint32(r.Body[0x24:]), append([]byte(nil), r.Body[0x28:0x2C]...)
	return c.Send(0x0217, nil)
}

// Enter runs the client's way from the game server's hello to the lobby: key auth,
// character list, create (slot 0) when create, select, load, lobby, post-load reports.
func (p *Player) Enter(create bool, race, gender, class byte) error {
	g, err := Dial(p.GameAddr)
	if err != nil {
		return err
	}
	p.Game = g
	if _, err := g.Wait(0x0202, 0, step); err != nil {
		return err
	}
	disc := byte(0)
	if p.Disc == "aoti" {
		disc = 1
	}
	g.Send(0x080E, append([]byte{0, 1, disc, disc}, make([]byte, 20)...))
	head := append([]byte{0xff, 0xff}, make([]byte, 0x22)...)
	steps := []struct {
		send func()
		want uint16
	}{
		{func() { g.SendHead(0x020D, head, append(append(u32(p.GID), p.key...), make([]byte, 8)...)) }, 0x0D05},
		{func() { g.Send(0x0D06, nil) }, 0x0D03},
	}
	for _, s := range steps {
		m := g.Mark()
		s.send()
		if _, err := g.Wait(s.want, m, step); err != nil {
			return err
		}
	}
	if create {
		raw := append([]byte(nil), SampleCharacter...)
		name := make([]byte, 64)
		copy(name, UTF16(p.Name))
		copy(raw[0x28:0x68], name)
		raw[0x68], raw[0x69], raw[0x6A] = race, gender, class
		g.Send(0x0D02, raw[0x24:])
		time.Sleep(200 * time.Millisecond) // no reply: the client waits too
	}
	m := g.Mark()
	g.Send(0x020B, append(u32(0), make([]byte, 8)...))
	if _, err := g.Wait(0x021B, m, step); err != nil {
		return err
	}
	m = g.Mark()
	g.Send(0x021C, append(u32(0), make([]byte, 8)...))
	if _, err := g.Wait(0x0236, m, step); err != nil {
		return err
	}
	if loaded, err := g.Wait(0x0208, m, 0); err == nil {
		p.LID = binary.LittleEndian.Uint16(loaded.Body)
	}
	p.PostLoad(1100000, 0, 4)
	return nil
}

// PostLoad is what the client reports after each 0x0236: 0x0806 n, 0x0815, 0x010B
// with where it stands, 0x0808 n+1.
func (p *Player) PostLoad(q, z, m uint32) {
	g := p.Game
	p.nb++
	g.Send(0x0806, u32(p.nb))
	g.Send(0x0815, nil)
	b := append(u32(p.GID), make([]byte, 24)...)
	b = append(b, make([]byte, 8)...)
	b = append(append(append(b, u32(q)...), u32(z)...), u32(m)...)
	g.Send(0x010B, append(b, make([]byte, 8)...))
	p.nb++
	g.Send(0x0808, u32(p.nb))
}

// Warp asks for an area change (0x0807) and waits for the load's 0x0236.
func (p *Player) Warp(q uint32, z, m, entry uint16) (int, error) {
	g := p.Game
	b := append(u32(q), u16(z)...)
	b = append(append(append(append(b, u16(m)...), u16(entry)...), u16(1)...), u32(0xFFFFFFFF)...)
	mark := g.Mark()
	g.Send(0x0807, b)
	if _, err := g.Wait(0x0236, mark, step); err != nil {
		return mark, err
	}
	p.PostLoad(q, uint32(z), uint32(m))
	return mark, nil
}

// Move is a channel-1 0x0102 position update.
func (p *Player) Move(x, y, z float32, dir uint16) error {
	body := make([]byte, 0x4C)
	binary.LittleEndian.PutUint32(body[0x24:], p.GID)
	binary.LittleEndian.PutUint16(body[0x2E:], dir)
	for i, f := range []float32{x, y, z} {
		binary.LittleEndian.PutUint32(body[0x34+4*i:], math.Float32bits(f))
	}
	return p.Game.SendRaw(encodeCast(0x0102, body))
}

// Chat says text in the lobby (0x0304).
func (p *Player) Chat(text string) error {
	msg := append(UTF16(text), 0, 0)
	b := append([]byte{0, 0, 0x12, 0}, u32(p.GID)...)
	b = append(b, make([]byte, 8)...)
	b = append(b, 0, 0, 0, byte(len(msg)/2), 0, 0, 0, 0)
	return p.Game.Send(0x0304, append(b, msg...))
}

func u16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func encodeCast(cmd uint16, body []byte) []byte { return wire.EncodeOn(wire.ChanCast, cmd, body) }
