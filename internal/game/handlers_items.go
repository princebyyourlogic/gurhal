// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/rules"
)

// Inventory events (PS2 layouts):
//
//	C>S 0x0105 item event (+0x2C index, +0x2D event 1 equip, 2 unequip, 3 link, 4 unlink, 8
//	           use; +0x30 UUID) -> confirmed with 0x0105 (an equip carries the weapon block)
//	C>S 0x0109 palette (weapon palette slots, item palette) -> kept, sent back in 0x0A0A
//	C>S 0x0106 event 6 pick up a ground item -> the zone
//	S>C 0x0105 event 0x0A a weapon's PP (the server spends and regenerates it)

func init() {
	handle(0x0105, 0x34, (*session).itemEvent)
	handle(0x0109, 0x74, (*session).palette)
	handle(0x0106, 0x54, (*session).pickupCmd)
}

func (s *session) itemEvent(b []byte) error {
	if s.char == nil {
		return nil
	}
	c, r, lid := s.char, s.r, s.lid
	uuid := binary.LittleEndian.Uint32(b[0x30:])
	if s.me != nil && b[0x2D] == 7 { // learn a PA disk
		return s.learnDisk(b)
	}
	if s.me != nil && b[0x2D] == 8 { // a grinder used on a weapon: grind
		c.mu.Lock()
		grinder := false
		for _, x := range c.st.Inv {
			grinder = grinder || (x.UUID == uuid && x.ID[0] == 0x0F && x.ID[1] == 0x01)
		}
		c.mu.Unlock()
		if grinder {
			return s.grind(b)
		}
	}
	c.mu.Lock()
	var pp [][]byte
	if b[0x2D] == 8 && s.me != nil { // a used item may still be the attack id of hits
		for _, x := range c.st.Inv {
			if x.UUID == uuid {
				c.recentUse = map[uint32]rules.Item{uuid: x}
				for _, w := range r.PPItem(c.st, x, int(b[0x2F])) { // Photon Charge: before the echo
					pp = append(pp, c0105PP(s.gid, lid, w.UUID, r.CurPP(w)))
				}
				break
			}
		}
	}
	ev, slot := r.ItemEvent(c.st, b)
	s.saveLocked()
	var extra []byte
	if ev == 1 && slot != nil { // equip a weapon: its hitbox block (+0x3C)
		extra = r.EquipBlock(slot)
	}
	out := append(pp, c0105Reply(s.gid, lid, b, extra))
	if ev == 1 && slot != nil {
		if it, ok := r.Item(slot.ID); ok && (it.Cat == 2 || it.Cat == 8) {
			// the weapons' reach was computed before this reach factor arrived: confirm them again
			for wi, w := range c.st.Inv {
				if it, ok := r.Item(w.ID); ok && w.Active != 0 && it.Cat == 1 {
					req := make([]byte, 0x34)
					binary.LittleEndian.PutUint32(req[0x24:], s.gid)
					binary.LittleEndian.PutUint32(req[0x28:], uint32(lid))
					req[0x2C], req[0x2D] = byte(wi), 1
					binary.LittleEndian.PutUint32(req[0x30:], w.UUID)
					out = append(out, c0105Reply(s.gid, lid, req, r.EquipBlock(&c.st.Inv[wi])))
				}
			}
		}
	}
	c.mu.Unlock()
	for _, p := range out {
		s.c.Send(p)
	}
	return nil
}

// palette is 0x0109: +0x2C 6 right-hand weapon UUIDs, +0x44 6 left-hand, +0x5C 6 item
// palette ids. The client rebuilds its palette from the records on every load.
func (s *session) palette(b []byte) error {
	if s.char == nil {
		return nil
	}
	c := s.char
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.st.Inv {
		var bits uint8
		for k := range 6 {
			if u := c.st.Inv[i].UUID; u == binary.LittleEndian.Uint32(b[0x2C+4*k:]) || u == binary.LittleEndian.Uint32(b[0x44+4*k:]) {
				bits |= 1 << k
			}
		}
		c.st.Inv[i].Slots = bits
	}
	c.st.Palette = append(rules.HexBytes(nil), b[0x5C:0x74]...)
	s.saveLocked()
	return nil
}

func (s *session) pickupCmd(b []byte) error {
	if b[0x2D] == 6 && s.zone != nil && s.char != nil {
		s.zone.pickup(s.gid, binary.LittleEndian.Uint32(b[0x34:]))
	}
	return nil
}

// ppCast charges an attack's PP (the casts are relayed as they are).
func (s *session) ppCast(cmd uint16, body []byte) {
	if s.char == nil || (cmd != 0x0102 && cmd != 0x0104 && cmd != 0x010F) {
		return
	}
	c := s.char
	c.mu.Lock()
	c.ppReady = true
	w := s.r.PPCast(c.st, &c.ppLeft, cmd, body)
	var pkt []byte
	if w != nil {
		s.saveLocked()
		pkt = c0105PP(s.gid, s.lid, w.UUID, s.r.CurPP(w))
	}
	c.mu.Unlock()
	if pkt != nil {
		s.c.Send(pkt)
	}
}

// ppRegen is one regeneration tick in a mission, once the player has moved there.
func (s *session) ppRegen() {
	c := s.me
	if c == nil {
		return
	}
	ch := c.char.Load()
	if ch == nil {
		return
	}
	ch.mu.Lock()
	if !ch.inMission || !ch.ppReady {
		ch.mu.Unlock()
		return
	}
	var out [][]byte
	lid := c.lid()
	for _, w := range s.r.PPRegen(ch.st) {
		out = append(out, c0105PP(s.gid, lid, w.UUID, s.r.CurPP(w)))
	}
	if len(out) > 0 {
		ch.bank(time.Now())
		s.srv.saver.queue(s.gid, ch.slot, ch.st)
	}
	ch.mu.Unlock()
	for _, p := range out {
		s.c.Send(p)
	}
}
