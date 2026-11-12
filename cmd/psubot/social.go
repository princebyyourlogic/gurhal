// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/binary"
	"fmt"

	"github.com/princebyyourlogic/gurhal/internal/bot"
)

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

// social: A gives B a card and mails B, shops, trades B meseta, asks a grind quote and
// visits the own room. snap records both players' packets after each step.
func social(pa, pb *bot.Player, snap func(string, bool)) error {
	pa.Game.Send(0x1514, append(le32(pb.GID), 0, 0, 0, 0))
	snap("A offers B a card", true)
	pb.Game.Send(0x1504, []byte{0xFF, 0xFF, 1})
	snap("B takes the card", true)
	pb.Game.Send(0x1506, nil)
	snap("B's card flags", true)

	mail := make([]byte, 0x1AC+4)
	copy(mail, le32(pa.GID))
	copy(mail[8:], bot.UTF16("hello"))
	binary.LittleEndian.PutUint32(mail[0x1AC:], 1)
	pa.Game.Send(0x150B, append(mail, le32(pb.GID)...))
	snap("A mails B", true)

	shop := func(ev, qty byte, param uint32) {
		b := append(append(le32(pa.GID), le32(uint32(pa.LID))...), ev, 0, qty, 0)
		pa.Game.Send(0x010A, append(append(b, le32(param)...), make([]byte, 8)...))
	}
	pa.Game.Send(0x1A01, append(make([]byte, 4), append(le32(90), le32(0)...)...))
	snap("A talks to shop 90", true)
	shop(1, 0, 0)
	snap("A opens shop 0", true)
	mark := pa.Game.Mark()
	shop(2, 1, 0)
	snap("A buys", true)
	if p, err := pa.Game.Wait(0x010A, mark, 0); err == nil && len(p.Body) >= 0x34 && p.Body[0x2D] == 0 {
		shop(3, 1, binary.LittleEndian.Uint32(p.Body[0x30:]))
		snap("A sells it back", true)
	}

	act := func(p *bot.Player, partner uint32, a byte) { p.Game.Send(0x1901, append(le32(partner), a, 0)) }
	act(pa, pb.GID, 1)
	snap("A asks B to trade", true)
	act(pb, pa.GID, 2)
	snap("B accepts", true)
	act(pa, pb.GID, 2)
	snap("A's window", true)
	offer := append(append(append(le32(pa.GID), le32(pb.GID)...), le32(0xFFFFFFFF)...), le32(10)...)
	pa.Game.Send(0x1903, append(offer, 3, 0, 0, 0))
	snap("A offers 10 meseta", true)
	act(pa, pb.GID, 4)
	snap("A confirms", true)
	act(pb, pa.GID, 4)
	snap("B confirms", true)
	act(pa, pb.GID, 6)
	snap("A OKs", true)
	act(pb, pa.GID, 6)
	snap("B OKs", true)

	pa.Game.Send(0x0A09, []byte{1, 0, 0, 0})
	snap("A grind quote", true)
	pb.Game.Send(0x0A07, le32(pa.GID))
	snap("B views A", true)

	if _, err := pa.Warp(1120000, 0, 1, 0); err != nil {
		return fmt.Errorf("A own room: %w", err)
	}
	snap("A enters the own room", true)
	pa.Move(-2, 0, 3.5, 8192)
	snap("A moves in the room", true)
	return myShop(pa, pb, snap)
}

// myShop: A stocks a weapon, opens the shop; B searches, visits and buys it; A reads
// the visitor log.
func myShop(pa, pb *bot.Player, snap func(string, bool)) error {
	inv, err := pa.Game.Wait(0x0A0A, 0, 0)
	if err != nil {
		return err
	}
	var uuid uint32
	for _, it := range bot.Inventory(inv) {
		if it[1]>>24 == 1 {
			uuid = it[0]
			break
		}
	}
	item := func(p *bot.Player, sub, x int16, a, b uint32, rec []byte) {
		body := append(le32(p.GID), byte(sub), byte(sub>>8), byte(x), byte(x>>8))
		body = append(append(append(body, le32(a)...), le32(b)...), make([]byte, 8)...)
		body = append(append(body, le32(uuid)...), rec...)
		p.Game.Send(0x130F, append(body, make([]byte, 0x60-0x24-len(body))...))
	}
	console := func(p *bot.Player, sub, a, b uint32) {
		p.Game.Send(0x130D, append(append(append(le32(p.GID), le32(sub)...), le32(a)...), le32(b)...))
	}
	mark := pa.Game.Mark()
	item(pa, 0, 1, 0, 0, nil)
	snap("A stocks a weapon", true)
	item(pa, 0x11, int16(uuid), 250, 1, nil)
	snap("A puts it in the shop", true)
	p, err := pa.Game.Wait(0x1323, mark, 0)
	if err != nil {
		return fmt.Errorf("A's shop record: %w", err)
	}
	seen := append([]byte(nil), p.Body[0x24+0x144:0x24+0x164]...)
	console(pa, 0x12, 1, 0)
	snap("A opens the shop", true)
	pb.Game.Send(0x1313, append([]byte{0, 0, 0, 0, 5, 0, 0, 0}, le32(1)...))
	snap("B searches the shops", true)
	pb.Game.Send(0x1304, append(le32(pa.GID), 0, 0, 0, 0))
	snap("B visits A's room", true)
	console(pa, 0x10, 0, 0)
	snap("A's shop menu", true)
	item(pb, 0x14, 0, 250, 1, seen)
	snap("B buys", true)
	console(pa, 0x0D, 0, 4)
	snap("A's visitor log", true)
	return nil
}
