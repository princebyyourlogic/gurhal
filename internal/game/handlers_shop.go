// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// NPCs and shops (PS2 layouts):
//
//	C>S 0x1A01 dialog (+0x28 shop id, +0x2C event: 0 talk/shop, 2 Lumilass, 3 PP cube,
//	           9 types menu) -> 0x1A02 / 0x1A03 / 0x1A04 / 0x1A07
//	C>S 0x1A05 Lumilass commit (no reply; the zone sees the new look)
//	C>S 0x010A shop (+0x2C event 1 enter (param NPC), 2 buy (param list index, A qty), 3 sell
//	           (param UUID, A qty)) -> 0x010A; meseta goes out as 0x0115
//	C>S 0x0A09 grind quote (+0x24 1, +0x25 grinder slot, +0x26 weapon slot) -> 0x0A09
//	C>S 0x0F0A outside missions: chairs (type 56, event 25 sit / 26 stand) -> 0x1211

func init() {
	handle(0x1A01, 0x30, (*session).dialog)
	handle(0x1A05, 0x59, (*session).lumilass)
	handle(0x010A, 0x34, (*session).shop)
	handle(0x0A09, 0x28, (*session).grindQuote)
}

// talk / shop NPCs: their 0x1A02 (a, b, c, d); anything lets the dialog go on.
var shopDialogs = map[uint32][4]uint16{80: {17, 17, 3, 9}, 90: {5, 1, 4, 5}, 91: {5, 5, 4, 7}, 92: {5, 8, 4, 0}, 93: {5, 18, 4, 0}}

func (s *session) dialog(b []byte) error {
	shop, ev := binary.LittleEndian.Uint32(b[0x28:]), binary.LittleEndian.Uint32(b[0x2C:])
	switch ev {
	case 0:
		d, ok := shopDialogs[shop]
		if !ok {
			d = [4]uint16{0, 1, 0, 0}
		}
		return s.c.Send(wire.Encode(0x1A02, wire.Body{}.Head(0, s.gid).Zero(4).U16(d[0]).U16(d[1]).U16(d[2]).U16(d[3])))
	case 2:
		if s.char == nil {
			return nil
		}
		s.char.mu.Lock()
		words := s.r.HairList(s.char.raw)
		s.char.mu.Unlock()
		body := wire.Body{}.Head(s.lid, s.gid).U32(0).U32(uint32(len(words)))
		if s.disc == rules.AotI { // AotI: two counts and 13 prices before the words
			body = wire.Body{}.Head(s.lid, s.gid).U32(0).U32(uint32(len(words))).U32(0).Zero(13 * 4)
		} else {
			body = body.Zero(8 * 4)
		}
		for _, w := range words {
			body = body.U32(w)
		}
		return s.c.Send(wire.Encode(0x1A03, body))
	case 3:
		if t, ok := s.srv.Data.Capture("ppcube.bin"); ok {
			return s.c.Send(wire.Encode(0x1A04, wire.Body{}.Head(wire.NoLID, s.gid).Zero(4).Bytes(t)))
		}
	case 9:
		menu := append(make([]byte, 0, 16), 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0)
		if s.char != nil {
			s.char.mu.Lock()
			menu = s.r.TypeMenu(s.char.st)
			s.char.mu.Unlock()
		}
		return s.c.Send(wire.Encode(0x1A07, wire.Body{}.Head(0, s.gid).Zero(4).U32(0).U32(0).Bytes(menu)))
	}
	return nil
}

// lumilass commits a new look: the raw record changes (state kept) and the zone rebuilds
// the model from a fresh 0x010D.
func (s *session) lumilass(b []byte) error {
	if s.char == nil {
		return nil
	}
	c := s.char
	c.mu.Lock()
	c.raw = s.r.ApplyLumilass(c.raw, b)
	raw := c.raw
	if s.me != nil {
		s.me.snap.Store(c.snapshot(s.r))
	}
	c.mu.Unlock()
	if err := s.srv.Store.SetRaw(s.ctx, s.gid, c.slot, raw); err != nil {
		return err
	}
	if s.zone != nil {
		s.zone.appearance(s.gid)
	}
	return nil
}

// appearance sends gid's fresh record to the others (after a Lumilass change).
func (z *zone) appearance(gid uint32) {
	z.do(func(zs *zoneState) {
		p := zs.members[gid]
		if p == nil {
			return
		}
		for _, d := range zs.others(gid) {
			d.p.conn.Relay(c010d(d.lid, d.p.gid, gid, p.lid, p.user(d.p.disc), noSource, false))
		}
	})
}

func shopHead(gid uint32, lid uint16, event, flag uint8) wire.Body {
	return wire.Body{}.Head(lid, gid).U32(gid).U32(uint32(lid)).U8(event).U8(flag)
}

func (s *session) shop(b []byte) error {
	if s.char == nil || s.me == nil {
		return nil
	}
	ev, qty := b[0x2C], int(b[0x2E])
	param := binary.LittleEndian.Uint32(b[0x30:])
	c, r, gid, lid := s.char, s.r, s.gid, s.lid
	fail := func(ev uint8) error { return s.c.Send(wire.Encode(0x010A, shopHead(gid, lid, ev, 1).Zero(6))) }
	switch ev {
	case 1:
		c.mu.Lock()
		lst, cat, ok := r.ShopList(int(param), c.st.Level)
		c.mu.Unlock()
		s.shopList = lst
		if !ok {
			return fail(1)
		}
		lst = lst[:min(len(lst), 255)]
		body := shopHead(gid, lid, 1, 0).U8(uint8(len(lst))).U8(uint8(cat)).Zero(4)
		for _, e := range lst {
			body = body.Bytes(r.ShopRecord(e))
		}
		return s.c.Send(wire.Encode(0x010A, body))
	case 2:
		if int(param) >= len(s.shopList) {
			return fail(2)
		}
		c.mu.Lock()
		slot, err := r.Buy(c.st, s.shopList[param].Key, qty)
		if err != nil {
			c.mu.Unlock()
			return fail(2)
		}
		s.saveLocked()
		block := rules.LevelBlock(c.st, s.disc)
		c.mu.Unlock()
		s.c.Send(c0115(gid, lid, noSource, block))
		return s.c.Send(wire.Encode(0x010A, shopHead(gid, lid, 2, 0).Zero(2).Bytes(r.VarRec(&slot, s.disc)).Bytes(r.ConstRec(&slot))))
	case 3:
		c.mu.Lock()
		n, _ := r.Sell(c.st, param, qty)
		if n == 0 {
			c.mu.Unlock()
			return fail(3)
		}
		s.saveLocked()
		block := rules.LevelBlock(c.st, s.disc)
		c.mu.Unlock()
		s.c.Send(c0115(gid, lid, noSource, block))
		return s.c.Send(wire.Encode(0x010A, shopHead(gid, lid, 3, 0).U8(uint8(n)).U8(0).U32(param)))
	}
	return nil
}

// grindQuote is 0x0A09 sub 1: the chance in 1/100 % (0xffff refused) for a grinder and a
// weapon slot.
func (s *session) grindQuote(b []byte) error {
	if s.char == nil || b[0x24] != 1 {
		return nil
	}
	gs, ws := int(b[0x25]), int(binary.LittleEndian.Uint16(b[0x26:]))
	chance := uint16(0xFFFF)
	s.char.mu.Lock()
	if inv := s.char.st.Inv; gs < len(inv) && ws < len(inv) {
		if c, ok := s.r.GrindChance(&inv[gs], &inv[ws]); ok {
			chance = uint16(max(0, min(c, 10000)))
		}
	}
	s.char.mu.Unlock()
	return s.c.Send(wire.Encode(0x0A09, wire.Body{}.Head(s.lid, s.gid).Bytes(b[0x24:0x26]).U16(chance)))
}

// grind commits a grinder on a weapon (0x0105 event 8 with a grinder; +0x2F weapon slot):
// 0x0106 event 0x0D with the weapon's record, then the grinder goes (0x0106 0x0C).
func (s *session) grind(b []byte) error {
	c, r := s.char, s.r
	guuid := binary.LittleEndian.Uint32(b[0x30:])
	wi := int(b[0x2F])
	c.mu.Lock()
	gi, ok, valid := r.Grind(s.srv.dice, c.st, guuid, wi)
	if !valid {
		c.mu.Unlock()
		return nil
	}
	w := c.st.Inv[wi]
	rec := r.VarRec(&w, s.disc)[4:]
	if !ok && s.disc != rules.AotI { // PSU: the record only on success
		rec = binary.LittleEndian.AppendUint32(nil, w.UUID)
		rec = append(rec, make([]byte, 0x1C)...)
	}
	okb := uint8(0)
	if ok {
		okb = 1
	}
	res := wire.Encode(0x0106, wire.Body{}.Head(s.lid, s.gid).U32(s.gid).U32(uint32(s.lid)).
		U8(uint8(gi)).U8(0x0D).U8(okb).U8(uint8(wi)).U32(guuid).Bytes(rec))
	removed, err := r.RemoveItem(c.st, guuid, 1)
	s.saveLocked()
	c.mu.Unlock()
	s.c.Send(res)
	if err == nil {
		s.c.Send(c0106Remove(r, s.disc, s.gid, s.lid, removed))
	}
	return nil
}

// learnDisk is 0x0105 event 7: a PA disk becomes a Lv1 PA (refused: the request echoed).
func (s *session) learnDisk(b []byte) error {
	c, r := s.char, s.r
	c.mu.Lock()
	pos, entry, cat, err := r.LearnDisk(c.st, b)
	if err != nil {
		c.mu.Unlock()
		s.c.Log.Debug("learn refused", "err", err)
		return s.c.Send(c0105Reply(s.gid, s.lid, b, nil))
	}
	s.saveLocked()
	c.mu.Unlock()
	req := append(append(append([]byte(nil), b[:0x2E]...), byte(pos), byte(cat)), b[0x30:0x34]...)
	pid := rules.PAID(entry.ID, s.disc)
	extra := append(r.PAParam(entry), binary.LittleEndian.AppendUint32(nil, entry.EXP)...)
	return s.c.Send(c0105Reply(s.gid, s.lid, req, append(extra, pid[:]...)))
}

// chair answers 0x0F0A outside missions: chairs (type 56) sit (25) / stand (26).
func (s *session) chair(b []byte) error {
	target := binary.LittleEndian.Uint32(b[0x30:])
	otype, ev := binary.LittleEndian.Uint16(b[0x34:]), b[0x62]
	if otype != 56 || (ev != 25 && ev != 26) {
		return nil
	}
	pose := uint32(0)
	if ev == 26 {
		pose = 2
	}
	return s.c.Send(wire.Encode(0x1211, wire.Body{}.Head(wire.NoLID, s.gid).U32(target).U32(0).U32(8).U32(pose)))
}
