// SPDX-License-Identifier: GPL-3.0-or-later

package bot

import (
	"encoding/binary"
	"math"

	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// Mission flows as the PS2 client runs them (testplay's).

// Inventory parses a 0x0A0A: (UUID, item id) per slot.
func Inventory(p wire.Packet) [][2]uint32 {
	b := p.Body
	if len(b) < 0x168 {
		return nil
	}
	var out [][2]uint32
	for i := range int(b[0x24]) {
		o := 0x168 + 0x24*i // variable records (0x0A0A +0x170 counting the frame header)
		out = append(out, [2]uint32{binary.LittleEndian.Uint32(b[o+4:]), binary.BigEndian.Uint32(b[o+8:])})
	}
	return out
}

// Equip sends 0x0105 event 1 for inventory slot i and waits for the confirmation.
func (p *Player) Equip(i int, uuid uint32) error {
	m := p.Game.Mark()
	p.Game.Send(0x0105, append(append(append(u32(p.GID), u32(uint32(p.LID))...), byte(i), 1, 0, 0), append(u32(uuid), 0, 0, 0, 0)...))
	_, err := p.Game.Wait(0x0105, m, step)
	return err
}

// Counter enters counter id (0x0811) and asks for its options, table and mission options.
func (p *Player) Counter(id uint32) (int, error) {
	g := p.Game
	m := g.Mark()
	g.Send(0x0811, append([]byte{1, 0, 0, 0, 4, 0, 6, 0}, append(u32(id), make([]byte, 12)...)...))
	if _, err := g.Wait(0x0236, m, step); err != nil {
		return m, err
	}
	p.PostLoad(0x7FFFFFFF, 0, 0)
	for _, x := range []struct {
		cmd, want uint16
	}{{0x1710, 0x1711}, {0x0C05, 0x0C06}, {0x0C0F, 0x0C10}} {
		k := g.Mark()
		g.Send(x.cmd, u32(id))
		if x.cmd == 0x0C05 {
			defer g.Send(0x170B, nil)
		}
		if _, err := g.Wait(x.want, k, step); err != nil {
			return m, err
		}
	}
	return m, nil
}

// Accept takes mission qid and waits for 0x0C02.
func (p *Player) Accept(qid uint32) (wire.Packet, error) {
	g := p.Game
	m := g.Mark()
	g.Send(0x1705, nil)
	g.Send(0x1709, nil)
	g.Send(0x1707, u32(qid))
	g.Send(0x0C01, u32(qid))
	return g.Wait(0x0C02, m, step)
}

// Depart warps into the accepted mission (0x0807 zone ffff), reports in (0x0814, 0x0808)
// and returns the mark before the load.
func (p *Player) Depart(qid uint32) (int, error) {
	g := p.Game
	m := g.Mark()
	b := append(u32(qid), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 7, 0)
	g.Send(0x0807, append(b, u32(0xFFFFFFFF)...))
	if _, err := g.Wait(0x0236, m, step); err != nil {
		return m, err
	}
	p.nb++
	g.Send(0x0806, u32(p.nb))
	g.Send(0x0814, nil)
	p.nb++
	g.Send(0x0808, u32(p.nb))
	return m, nil
}

// ReportEnemy is an enemy's 0x0402 as its client sends it (condition id = target id - 150).
func (p *Player) ReportEnemy(id uint32) {
	b := append(u32(id-150), 0x54, 0xfe, 0, 0, 0xe1, 1, 0, 0x20)
	p.Game.Send(0x0402, append(b, 2, 0, 0x20, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff))
}

// Hits sends one 0x0E00 batch hitting each target with weapon uuid.
func (p *Player) Hits(seq uint32, targets []uint32, uuid uint32) {
	b := append(append(u32(uint32(len(targets))), u32(uint32(p.LID))...), u32(seq)...)
	for _, t := range targets {
		h := make([]byte, 0x50)
		binary.LittleEndian.PutUint32(h[0:], math.Float32bits(1))
		binary.LittleEndian.PutUint32(h[8:], math.Float32bits(2))
		binary.LittleEndian.PutUint32(h[0xC:], uint32(p.LID))
		binary.LittleEndian.PutUint32(h[0x10:], t)
		binary.LittleEndian.PutUint32(h[0x14:], uuid)
		b = append(b, h...)
	}
	p.Game.Send(0x0E00, b)
}

// Flag sends 0x0404 (set flag fid).
func (p *Player) Flag(fid uint32) { p.Game.Send(0x0404, append(u32(fid), u32(0)...)) }

// Pickup picks up ground item uuid (0x0106 event 6).
func (p *Player) Pickup(uuid uint32) {
	b := append(append(u32(p.GID), u32(uint32(p.LID))...), 0, 6, 0, 0, 0xff, 0xff, 0xff, 0xff)
	p.Game.Send(0x0106, append(append(b, u32(uuid)...), make([]byte, 0x1C)...))
}

// Leave warps out of a mission to the Colony exit (0x0807).
func (p *Player) Leave(z, m, e uint16) (int, error) {
	g := p.Game
	b := append(u32(1100000), u16(z)...)
	b = append(append(append(append(b, u16(m)...), u16(e)...), u16(9)...), u32(0xFFFFFFFF)...)
	mark := g.Mark()
	g.Send(0x0807, b)
	if _, err := g.Wait(0x0236, mark, step); err != nil {
		return mark, err
	}
	p.PostLoad(1100000, uint32(z), uint32(m))
	return mark, nil
}

// Invite asks player gid into a party (0x100A).
func (p *Player) Invite(gid uint32) { p.Game.Send(0x100A, append(u32(gid), make([]byte, 0x40)...)) }

// Answer answers an invite from inviter (0x100C result 1 = yes).
func (p *Player) Answer(inviter uint32, yes bool) {
	b := make([]byte, 0x54)
	copy(b, []byte{0, 1, 0x12, 0})
	binary.LittleEndian.PutUint32(b[4:], inviter)
	if yes {
		b[0x10] = 1
	}
	p.Game.Send(0x100C, b)
}
