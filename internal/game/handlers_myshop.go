// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"
	"encoding/json"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// My Shop (PS2 layouts; the room record R is rules.RoomRecord):
//
//	C>S 0x130d (+0x28 sub, +0x2C A, +0x30 B): 0x10 shop menu -> 130e 2 (customers);
//	    0x11 take back slot A, B units; 0x12 open (A != 0) / close -> 130e 0xd to the room;
//	    0x0d B 4 visitor log -> 0x1320, B 3 visited shops -> 0x1321 (none);
//	    0x0c B 0..3 in someone else's room: its 0x1327 / 0x1323
//	C>S 0x130f (+0x28 s16 sub, +0x2A s16 P, +0x2C A, +0x30 B, +0x40 item record):
//	    0x11 put stock entry P at price A, B units; 0x12 slot P: price A (>= 0), B units back;
//	    0x14 buy slot P at price A, B units -> 0x0106, 0x0115, 130e 0x13 (1 bought)
//	C>S 0x1319 ad (+0x28 0xf8), 0x131a Partner Machine name (+0x28 0x40),
//	    0x1313 search (+0x28 s16 mode, +0x30 0x40 text) -> 0x1314
//
// After a change everyone in the owner's room gets 0x1323, the owner also 0x1324 when its
// stock changed. A guest buys from an owner online (both characters locked in GID order)
// or offline (the stored state, under shopMu).

func init() {
	handle(0x1319, 0x120, (*session).shopAd)
	handle(0x131A, 0x68, (*session).proboName)
	handle(0x1313, 0x30, (*session).shopSearch)
}

// roomOwner is the GID of the My Room s stands in (0: none).
func (s *session) roomOwner() uint32 {
	if s.zone == nil || s.zone.key.quest != myRoomQuest || s.zone.key.owner == 0 {
		return 0
	}
	return s.zone.key.owner
}

// roomBodies are R and its constant lists for both discs (0x1323 after its head).
func (s *session) roomBodies(st *rules.State, raw []byte) map[string][]byte {
	lists := s.r.ShopConsts(st)
	out := map[string][]byte{}
	for _, d := range []string{rules.PSU, rules.AotI} {
		out[d] = append(append(s.r.RoomRecord(st, raw[rules.RawName:rules.RawRace], d), lists...), lists...)
	}
	return out
}

// refreshRoom sends owner's room record to s and, when s stands in that room, the others
// there; stock adds s's own storage (owner == s).
func (s *session) refreshRoom(owner uint32, bodies map[string][]byte, stock []byte) {
	s.c.Send(wire.Encode(0x1323, roomHead(s.gid, s.lid).Bytes(bodies[s.disc])))
	if stock != nil {
		s.c.SendPaced(stock)
	}
	if s.roomOwner() == owner {
		s.zone.do(func(zs *zoneState) {
			for _, d := range zs.others(s.gid) {
				d.p.conn.Relay(wire.Encode(0x1323, head(d).Bytes(bodies[d.p.disc])))
			}
		})
	}
}

// withOwner runs f on a room owner's state and raw record: s's own, an online owner's or
// a stored one (slot 0, as a visit). Both characters are locked while f runs, in GID
// order; online is the owner's player when online. When f reports a change the owner
// (and s) are saved.
func (s *session) withOwner(owner uint32, f func(st *rules.State, raw []byte, online *player) bool) error {
	if owner == s.gid {
		s.char.mu.Lock()
		defer s.char.mu.Unlock()
		if f(s.char.st, s.char.raw, s.me) {
			s.saveLocked()
		}
		return nil
	}
	if o := s.srv.world.find(owner); o != nil {
		if c := o.char.Load(); c != nil {
			first, second := s.char, c
			if owner < s.gid {
				first, second = c, s.char
			}
			first.mu.Lock()
			second.mu.Lock()
			defer first.mu.Unlock()
			defer second.mu.Unlock()
			if f(c.st, c.raw, o) {
				s.saveLocked()
				c.bank(time.Now())
				s.srv.saver.queue(owner, c.slot, c.st)
				o.snap.Store(c.snapshot(o.r))
			}
			return nil
		}
	}
	s.srv.shopMu.Lock()
	defer s.srv.shopMu.Unlock()
	cs, err := s.srv.Store.Characters(s.ctx, owner)
	if err != nil || len(cs) == 0 || cs[0].Slot != 0 {
		return err
	}
	var st rules.State
	if json.Unmarshal(cs[0].State, &st) != nil || st.Level == 0 {
		return nil
	}
	s.char.mu.Lock()
	defer s.char.mu.Unlock()
	if f(&st, cs[0].Raw, nil) {
		s.saveLocked()
		s.srv.saver.queue(owner, 0, &st)
		s.srv.saver.flush(s.ctx, owner, 0)
	}
	return nil
}

// myShopConsole answers the shop's 0x130d subs; false leaves the packet to the room.
func (s *session) myShopConsole(b []byte) (bool, error) {
	sub, a, bb := binary.LittleEndian.Uint32(b[0x28:]), binary.LittleEndian.Uint32(b[0x2C:]), binary.LittleEndian.Uint32(b[0x30:])
	here := s.roomOwner()
	switch {
	case sub == 0x10:
		if here != s.gid {
			return true, s.c.Send(c130eRoom(s.gid, s.lid, 2, 0, 0))
		}
		gid, lid, conn := s.gid, s.lid, s.me.conn
		s.zone.do(func(zs *zoneState) { conn.Relay(c130eRoom(gid, lid, 2, 0, int32(len(zs.others(gid))))) })
		return true, nil
	case sub == 0x11:
		n := -1
		if bb > 0 && bb < 0x80000000 {
			n = int(bb)
		}
		var bodies map[string][]byte
		var stock []byte
		err := s.withOwner(s.gid, func(st *rules.State, raw []byte, _ *player) bool {
			err := s.r.ShopTakeBack(st, int(a), n)
			bodies, stock = s.roomBodies(st, raw), s.c1324Stock()
			return err == nil
		})
		s.refreshRoom(s.gid, bodies, stock)
		return true, err
	case sub == 0x12:
		var bodies map[string][]byte
		var changed, open bool
		err := s.withOwner(s.gid, func(st *rules.State, raw []byte, _ *player) bool {
			changed = st.ShopSetOpen(a != 0)
			open, bodies = st.Shop.Open, s.roomBodies(st, raw)
			return changed
		})
		if changed {
			flag := int32(0)
			if open {
				flag = 1
			}
			s.c.Send(c130eRoom(s.gid, s.lid, 0xD, flag))
			if here == s.gid {
				s.zone.do(func(zs *zoneState) {
					for _, d := range zs.others(s.gid) {
						d.p.conn.Relay(c130eRoom(d.p.gid, d.lid, 0xD, flag))
					}
				})
			}
		}
		s.refreshRoom(s.gid, bodies, nil)
		return true, err
	case sub == 0x0D && bb == 4:
		s.char.mu.Lock()
		log := s.char.st.Visitors[:min(len(s.char.st.Visitors), rules.VisitorMax)]
		body := roomHead(s.gid, s.lid).U32(0).U32(uint32(len(log)))
		for _, v := range log {
			body = body.Bytes(rules.VisitorRecord(v))
		}
		s.char.mu.Unlock()
		return true, s.c.Send(wire.Encode(0x1320, body))
	case sub == 0x0D && bb == 3:
		return true, s.c.Send(wire.Encode(0x1321, roomHead(s.gid, s.lid).U32(0).U32(0)))
	case sub == 0x0C && bb <= 3 && here != 0 && here != s.gid:
		var pkt []byte
		err := s.withOwner(here, func(st *rules.State, raw []byte, _ *player) bool {
			if bb == 0 {
				pkt = s.c1327(here, s.userOf(here, raw, st)[0x15], raw, st, 0)
			} else {
				pkt = wire.Encode(0x1323, roomHead(s.gid, s.lid).Bytes(s.roomBodies(st, raw)[s.disc]))
			}
			return false
		})
		if pkt != nil {
			s.c.Send(pkt)
		}
		return true, err
	}
	return false, nil
}

// myShopItem answers the shop's 0x130f subs; false leaves the packet to the room.
func (s *session) myShopItem(b []byte) (bool, error) {
	if len(b) < 0x60 {
		return false, nil
	}
	sub, p := int16(binary.LittleEndian.Uint16(b[0x28:])), int16(binary.LittleEndian.Uint16(b[0x2A:]))
	a, bb := int(int32(binary.LittleEndian.Uint32(b[0x2C:]))), int(int32(binary.LittleEndian.Uint32(b[0x30:])))
	var bodies map[string][]byte
	var stock []byte
	switch sub {
	case 0x11:
		err := s.withOwner(s.gid, func(st *rules.State, raw []byte, _ *player) bool {
			_, err := s.r.ShopPut(st, int(uint16(p)), a, bb)
			bodies, stock = s.roomBodies(st, raw), s.c1324Stock()
			return err == nil
		})
		s.refreshRoom(s.gid, bodies, stock)
		return true, err
	case 0x12:
		err := s.withOwner(s.gid, func(st *rules.State, raw []byte, _ *player) bool {
			ok := false
			if a >= 0 {
				ok = st.ShopPrice(int(p), a) == nil
			}
			if bb > 0 {
				ok = s.r.ShopTakeBack(st, int(p), bb) == nil || ok
				stock = s.c1324Stock()
			}
			bodies = s.roomBodies(st, raw)
			return ok
		})
		s.refreshRoom(s.gid, bodies, stock)
		return true, err
	case 0x14:
		owner := s.roomOwner()
		if owner == 0 || owner == s.gid {
			return true, s.c.Send(c130eRoom(s.gid, s.lid, 0x13, 0, 0))
		}
		var mine [][]byte
		var theirs []byte
		var seller *player
		bought := false
		err := s.withOwner(owner, func(st *rules.State, raw []byte, online *player) bool {
			added, _, _, err := s.r.ShopBuy(st, s.char.st, int(p), a, bb, b[0x40:0x60])
			bodies = s.roomBodies(st, raw)
			if err != nil {
				s.c.Log.Debug("shop buy refused", "owner", owner, "err", err)
				return false
			}
			bought = true
			mine = [][]byte{c0106Add(s.r, s.disc, s.gid, s.lid, added),
				c0115(s.gid, s.lid, noSource, rules.LevelBlock(s.char.st, s.disc))}
			if seller = online; seller != nil {
				theirs = c0115(seller.gid, seller.lid(), noSource, rules.LevelBlock(st, seller.disc))
			}
			return true
		})
		for _, pkt := range mine {
			s.c.Send(pkt)
		}
		if theirs != nil {
			seller.conn.Relay(theirs)
		}
		ok := int32(0)
		if bought {
			ok = 1
		}
		s.c.Send(c130eRoom(s.gid, s.lid, 0x13, 0, ok))
		if bodies != nil {
			s.refreshRoom(owner, bodies, nil)
		}
		return true, err
	}
	return false, nil
}

// userOf is another character's user record as s sees it (its owner byte at +0x15).
func (s *session) userOf(gid uint32, raw []byte, st *rules.State) []byte {
	return s.r.User(gid, 0, raw, st, s.area, 0, rules.Pos{}, s.disc, "")
}

// shopVisited notes s in the visitor log of the room it entered.
func (s *session) shopVisited(owner uint32) {
	s.char.mu.Lock()
	name := append([]byte(nil), s.char.raw[rules.RawName:rules.RawRace]...)
	slot := s.char.slot
	s.char.mu.Unlock()
	if owner == s.gid {
		return
	}
	s.withOwner(owner, func(st *rules.State, _ []byte, _ *player) bool {
		rules.NoteVisit(st, s.gid, name, slot, time.Now().Unix())
		return true
	})
}

func (s *session) shopAd(b []byte) error {
	if s.char == nil {
		return nil
	}
	return s.withOwner(s.gid, func(st *rules.State, _ []byte, _ *player) bool {
		if st.Shop == nil {
			st.Shop = &rules.RoomShop{}
		}
		st.Shop.Ad = append(rules.HexBytes(nil), b[0x28:0x120]...)
		return true
	})
}

func (s *session) proboName(b []byte) error {
	if s.char == nil {
		return nil
	}
	return s.withOwner(s.gid, func(st *rules.State, _ []byte, _ *player) bool {
		if st.Probo == nil {
			st.Probo = &rules.Probo{}
		}
		st.Probo.Name = append(rules.HexBytes(nil), b[0x28:0x68]...)
		return true
	})
}

func (s *session) shopSearch(b []byte) error {
	if s.char == nil || s.me == nil {
		return nil
	}
	mode := int(int16(binary.LittleEndian.Uint16(b[0x28:])))
	var text string
	if len(b) >= 0x70 {
		text = rules.UTF16Text(b[0x30:0x70])
	}
	stored, err := s.srv.Store.OpenShops(s.ctx)
	if err != nil {
		return err
	}
	// The online players' live states (their saves may still be queued), then the stored
	// shops of everyone else.
	var rows []rules.ShopRow
	live := map[uint32]bool{}
	for _, p := range s.srv.world.players() {
		c := p.char.Load()
		if c == nil {
			continue
		}
		live[p.gid] = true
		c.mu.Lock()
		if c.st.Shop != nil && c.st.Shop.Open {
			rows = append(rows, rules.ShopRow{GID: p.gid, Slot: c.slot, Name: c.raw[rules.RawName:rules.RawRace], St: cloneState(c.st)})
		}
		c.mu.Unlock()
	}
	for _, o := range stored {
		st := new(rules.State)
		if live[o.GID] || json.Unmarshal(o.State, st) != nil {
			continue
		}
		rows = append(rows, rules.ShopRow{GID: o.GID, Slot: o.Slot, Name: o.Raw[rules.RawName:rules.RawRace], St: st})
	}
	found := s.r.SearchShops(rows, mode, text)
	body := roomHead(s.gid, s.lid).U32(0).U16(0).U16(uint16(mode)).U32(uint32(len(found)))
	for _, e := range found {
		body = body.Bytes(e)
	}
	body = append(body, make([]byte, (rules.ShopResults-len(found))*0x148)...)
	return s.c.Send(wire.Encode(0x1314, body))
}
