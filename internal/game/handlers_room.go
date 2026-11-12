// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"
	"math/rand/v2"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// My Room (the offline Extra lobby's room zone, served as quest 1120000 zone 1, one
// instance per owner):
//
//	0x0807 to the room (the elevators, the console's visit warp) / C>S 0x1304 visit (+0x24
//	       owner) -> the room zone, then 0x1327 room data
//	C>S 0x130b set object (+0x28 s16 spot, +0x2C UUID; 0xFFFFFFFF takes it back) -> 0x130c
//	C>S 0x130d console (+0x28 sub, +0x2C A, +0x30 B): 0 withdraw, production subs, menus
//	C>S 0x130f (+0x28 s16 sub, +0x2A P, +0x3C UUID): 0 deposit, 4 give to the Probo,
//	       production subs
//	S>C 0x130e notify: +0x28 type, +0x2C a1, +0x30 a2 (+0x34 a3, +0x38 a4)

func init() {
	handle(0x1304, 0x2C, (*session).roomVisit)
	handle(0x130B, 0x30, (*session).roomObject)
	handle(0x130D, 0x34, (*session).roomConsole)
	handle(0x130F, 0x40, (*session).roomItem)
}

var roomArea = rules.Area{Quest: myRoomQuest, Zone: 1, Map: 1}

// wantsRoom: the Colony elevators (1120000 zone 0/1/ffff) or the native room zone (lobby
// quest, zone 1, map 1).
func wantsRoom(q, z, m uint32) bool {
	return (q == myRoomQuest && (z == 0 || z == 1 || z == noZone)) || (lobbyQuests[q] && z == 1 && m == 1)
}

// enterRoom loads owner's room and its data; raw and st describe the owner.
func (s *session) enterRoom(owner uint32, raw []byte, st *rules.State, entrance byte) {
	s.goZone(roomArea, 0, zoneKey{s.uni, myRoomQuest, roomArea.Zone, owner}, roomZone, true)
	s.char.mu.Lock()
	ownerByte := s.ownUser()[0x15]
	if owner != s.gid {
		ownerByte = s.userOf(owner, raw, st)[0x15]
	}
	s.char.mu.Unlock()
	s.c.Send(s.c1327(owner, ownerByte, raw, st, entrance))
}

func roomHead(gid uint32, lid uint16) wire.Body { return wire.Body{}.Head(lid, gid) }

// c1327 is the room data: owner, owner byte (+ AotI's entrance, its "own room" test),
// name, 32 furniture slots, the Probo block.
func (s *session) c1327(owner uint32, ownerByte byte, raw []byte, st *rules.State, entrance byte) []byte {
	second := byte(0)
	if s.disc == rules.AotI {
		second = entrance
	}
	name := make([]byte, 0x40)
	if raw != nil {
		copy(name, raw[rules.RawName:rules.RawRace])
	}
	body := wire.Body{}.U32(owner).U8(ownerByte).U8(second).Zero(2).Bytes(name).Zero(16).Bytes(rules.RoomSlotsOf(st))
	body = append(body, make([]byte, 0x954-len(body))...)
	if pr := st.Probo; pr != nil {
		rank := 0
		for _, x := range pr.Stats[:4] {
			rank += max(0, x/100)
		}
		binary.LittleEndian.PutUint16(body[0x8E0:], uint16(int16(rank)))
		binary.LittleEndian.PutUint16(body[0x904:], uint16(int16(pr.Type)))
		for i, x := range pr.Stats {
			binary.LittleEndian.PutUint16(body[0x948+2*i:], uint16(int16(x)))
		}
		if len(pr.Name) > 0 { // named: +0x2E5A 1, the name from +0x2E5C (20 of its 32 characters fit)
			binary.LittleEndian.PutUint16(body[0x926:], 1)
			copy(body[0x928:], pr.Name)
		}
	}
	return wire.Encode(0x1327, roomHead(s.gid, s.lid).Bytes(body))
}

func (s *session) roomVisit(b []byte) error {
	if s.char == nil || s.me == nil {
		return nil
	}
	owner := binary.LittleEndian.Uint32(b[0x24:])
	if owner == s.gid {
		return nil
	}
	var raw []byte
	var st *rules.State
	if o := s.srv.world.find(owner); o != nil {
		if c := o.char.Load(); c != nil {
			c.mu.Lock()
			raw, st = c.raw, cloneState(c.st)
			c.mu.Unlock()
		}
	} else if cs, err := s.srv.Store.Characters(s.ctx, owner); err == nil && len(cs) > 0 && cs[0].Slot == 0 {
		raw, st = cs[0].Raw, s.stateOf(cs[0])
	}
	if raw == nil {
		return nil
	}
	s.enterRoom(owner, raw, st, 0)
	s.shopVisited(owner)
	return nil
}

// cloneState copies what a visit or a shop search reads of another character's state.
func cloneState(st *rules.State) *rules.State {
	c := *st
	c.Room = append([]byte(nil), st.Room...)
	if st.Probo != nil {
		p := *st.Probo
		c.Probo = &p
	}
	if st.Shop != nil {
		sh := *st.Shop
		sh.Items = append([]rules.ShopItem(nil), st.Shop.Items...)
		c.Shop = &sh
	}
	c.Visitors = append([]rules.Visitor(nil), st.Visitors...)
	return &c
}

func (s *session) roomObject(b []byte) error {
	if s.char == nil || s.zone == nil {
		return nil
	}
	slot := int(int16(binary.LittleEndian.Uint16(b[0x28:])))
	uuid := binary.LittleEndian.Uint32(b[0x2C:])
	c, r := s.char, s.r
	c.mu.Lock()
	goods, it, added, err := r.SetObject(c.st, slot, uuid)
	if err == nil {
		s.saveLocked()
	}
	c.mu.Unlock()
	if err != nil {
		s.c.Log.Debug("room object refused", "err", err)
		return nil
	}
	if added {
		s.c.Send(c0106Add(r, s.disc, s.gid, s.lid, it))
	} else {
		s.c.Send(c0106Remove(r, s.disc, s.gid, s.lid, it))
	}
	s.zone.roomGoods(slot, goods)
	return nil
}

// roomGoods shows a furniture change to everyone in the room (0x130c).
func (z *zone) roomGoods(slot, goods int) {
	z.do(func(zs *zoneState) {
		zs.broadcast(func(d *member) []byte {
			return wire.Encode(0x130C, head(d).U32(d.p.gid).U16(uint16(int16(slot))).U16(uint16(int16(goods))).Zero(4))
		})
	})
}

func c130eRoom(gid uint32, lid uint16, kind uint32, a ...int32) []byte {
	b := roomHead(gid, lid).U32(gid).U32(kind)
	for i := range 4 {
		v := int32(0)
		if i < len(a) {
			v = a[i]
		}
		b = b.U32(uint32(v))
	}
	return wire.Encode(0x130E, b)
}

func (s *session) c1324Stock() []byte {
	recs := s.r.StockRecords(s.char.st, s.disc)
	b := roomHead(s.gid, s.lid).U32(uint32(len(recs)))
	for _, x := range recs {
		b = b.Bytes(x)
	}
	return wire.Encode(0x1324, b)
}

func (s *session) roomConsole(b []byte) error {
	if s.char == nil || s.me == nil {
		return nil
	}
	if done, err := s.myShopConsole(b); done {
		return err
	}
	sub, bb := binary.LittleEndian.Uint32(b[0x28:]), binary.LittleEndian.Uint32(b[0x30:])
	c, r := s.char, s.r
	switch {
	case sub == 0: // withdraw from the room stock: +0x2C count, +0x30 index
		qty, index := int(binary.LittleEndian.Uint32(b[0x2C:])), int(bb)
		c.mu.Lock()
		added, err := r.Withdraw(c.st, index, qty)
		var stock []byte
		if err == nil {
			s.saveLocked()
			stock = s.c1324Stock()
		}
		c.mu.Unlock()
		if err == nil {
			s.c.Send(c0106Add(r, s.disc, s.gid, s.lid, added))
			s.c.SendPaced(stock)
		}
	case sub == 1 || sub == 4 || sub == 6 || sub == 7 || sub == 8:
		return s.synth(0x130D, b)
	case sub == 0x19:
		return s.c.Send(c130eRoom(s.gid, s.lid, 21, 0, 0x1A))
	case sub == 0x1E:
		return s.c.Send(c130eRoom(s.gid, s.lid, 23, 0, 0))
	case sub == 0x16:
		return s.c.Send(c130eRoom(s.gid, s.lid, 18))
	case sub == 0x15:
		c.mu.Lock()
		n := c.st.RoomGifts
		c.mu.Unlock()
		ok := int32(0)
		if n < rules.GiftLimit {
			ok = 1
		}
		return s.c.Send(c130eRoom(s.gid, s.lid, 17, ok, int32(n)))
	case sub == 0x0C && bb == 0:
		c.mu.Lock()
		ownerByte := s.ownUser()[0x15]
		pkt := s.c1327(s.gid, ownerByte, c.raw, c.st, s.entrance)
		c.mu.Unlock()
		return s.c.SendPaced(pkt)
	case sub == 0x0C && bb >= 1 && bb <= 3:
		c.mu.Lock()
		st, name := c.st, c.raw[rules.RawName:rules.RawRace]
		lists := r.ShopConsts(st)
		recs := r.StockRecords(st, s.disc)
		body := roomHead(s.gid, s.lid).U32(s.gid).Zero(4).Bytes(r.RoomRecord(st, name, s.disc)).Bytes(lists).Bytes(lists).
			U32(uint32(len(recs)))
		for _, x := range recs {
			body = body.Bytes(x)
		}
		c.mu.Unlock()
		return s.c.SendPaced(wire.Encode(0x130A, body))
	case sub == 0x0C && bb == 4:
		return s.c.Send(wire.Encode(0x1318, roomHead(s.gid, s.lid).U32(s.gid).Zero(4).Zero(0x404)))
	}
	return nil
}

func (s *session) roomItem(b []byte) error {
	if s.char == nil || s.me == nil {
		return nil
	}
	if done, err := s.myShopItem(b); done {
		return err
	}
	sub := int16(binary.LittleEndian.Uint16(b[0x28:]))
	uuid := binary.LittleEndian.Uint32(b[0x3C:])
	c, r := s.char, s.r
	switch sub {
	case 0: // deposit into the room stock: +0x2A count
		qty := int(int16(binary.LittleEndian.Uint16(b[0x2A:])))
		c.mu.Lock()
		removed, err := r.Deposit(c.st, uuid, qty)
		var stock []byte
		if err == nil {
			s.saveLocked()
			stock = s.c1324Stock()
		}
		c.mu.Unlock()
		if err == nil {
			s.c.Send(c0106Remove(r, s.disc, s.gid, s.lid, removed))
			s.c.SendPaced(stock)
		}
	case 4: // give to the Probo
		c.mu.Lock()
		removed, ok := r.GiveItem(c.st, uuid)
		if !ok {
			c.mu.Unlock()
			return nil
		}
		evolved, deltas := r.ProboGift(c.st, removed)
		pr := *c.st.Probo
		s.saveLocked()
		c.mu.Unlock()
		ev := int16(0)
		if evolved {
			ev = 1
		}
		body := roomHead(s.gid, s.lid).U32(s.gid).U16(uint16(ev)).U16(uint16(int16(pr.Type)))
		for i := range 5 {
			body = body.U16(uint16(int16(pr.Stats[i]))).U16(uint16(int16(deltas[i])))
		}
		s.c.Send(c0106Remove(r, s.disc, s.gid, s.lid, removed))
		s.c.Send(wire.Encode(0x1312, body))
		if s.zone != nil {
			s.zone.do(func(zs *zoneState) {
				zs.broadcast(func(d *member) []byte { return c130eRoom(d.p.gid, d.lid, 22) })
			})
		}
	default:
		return s.synth(0x130F, b)
	}
	return nil
}

// --- synthesis -----------------------------------------------------------------------------------------

// synthCursor is the production entry and variant the console is on.
type synthCursor struct {
	entry   int // -1: none
	variant int
	set     bool
}

func (s *session) c131b(kind, e, a, b int, id string, d, f int, rec []byte) []byte {
	if rec == nil {
		rec = s.r.Rec20(id, 1, 0, 0, 0, false)
	}
	return wire.Encode(0x131B, roomHead(s.gid, s.lid).U32(s.gid).U16(uint16(int16(kind))).U16(uint16(int16(e))).
		U16(uint16(int16(a))).U16(uint16(int16(b))).Zero(4).Bytes(rec).U16(uint16(int16(d))).U16(uint16(int16(f))).U16(0).
		Bytes(s.r.ItemName(id)))
}

func (s *session) c1310(kind, slot int, rec []byte) []byte {
	return wire.Encode(0x1310, roomHead(s.gid, s.lid).U32(s.gid).U16(uint16(int16(kind))).U16(uint16(int16(slot))).Bytes(rec))
}

func (s *session) c130eSynth(kind uint32, a ...int) []byte {
	v := make([]int32, len(a))
	for i, x := range a {
		v[i] = int32(x)
	}
	return c130eRoom(s.gid, s.lid, kind, v...)
}

// entryRows are an entry's rows of the production list (0x130d 7).
func (s *session) entryRows(st *rules.State, i int) [][]byte {
	r := s.r
	e := rules.SynthEntryList(st)[i]
	var b *rules.SynthBoard
	if e != nil {
		b = r.SynthBoard(e.Board)
	}
	if e == nil || b == nil {
		out := [][]byte{s.c131b(0, i, 0, 0, "", 0, 0, nil), s.c131b(2, i, 0, 0, "", 0, 0, nil), s.c130eSynth(0xB, i, 0, 0, 0)}
		for k := range 4 {
			out = append(out, s.c131b(1, i, k, 0, "", 0, 0, nil))
		}
		return out
	}
	v := e.Variant
	o := b.Outputs[max(0, v)]
	sr, ir := r.Rooms(st, o.ID)
	running := e.Running
	if b.Wildcard {
		running += 10
	}
	one := 1
	if b.VariantBoard {
		one = 0
	}
	out := [][]byte{
		s.c131b(0, i, running, one, e.Board, e.Used, b.FreqMax, nil),
		s.c131b(2, i, r.SynthRate(st, b, v), 0, o.ID, v, r.OutputMax(st, e, b), r.Rec20(o.ID, 1, 0, o.Element, 0, true)),
		s.c130eSynth(0xB, i, sr, 0, ir),
	}
	for k := range 4 {
		id, need := b.SynthMaterial(e, k, v)
		n := 0
		if id != "" {
			n = r.StockCount(st, id)
		}
		out = append(out, s.c131b(1, i, k, b.SlotTypes[k], id, need, n, nil))
	}
	return out
}

// materialRows are the output and the four material rows after a variant change.
func (s *session) materialRows(st *rules.State, e *rules.SynthEntry, b *rules.SynthBoard, v, shownVariant int) [][]byte {
	r := s.r
	o := b.Outputs[max(0, v)]
	out := [][]byte{s.c130eSynth(7, shownVariant),
		s.c131b(2, s.synthCur.entry, r.SynthRate(st, b, v), 0, o.ID, shownVariant, r.OutputMax(st, e, b), nil)}
	for k := range 4 {
		id, need := b.SynthMaterial(e, k, v)
		n := 0
		if id != "" {
			n = r.StockCount(st, id)
		}
		out = append(out, s.c1310(4, k, r.Rec20(id, 1, 0, 0, 0, false)), s.c131b(1, s.synthCur.entry, k, b.SlotTypes[k], id, need, n, nil))
	}
	return out
}

// synth is one production command (0x130d subs 1/4/6/7/8, 0x130f subs 2/3/5/6/7/0xB..0x10).
func (s *session) synth(cmd uint16, body []byte) error {
	c, r := s.char, s.r
	c.mu.Lock()
	st := c.st
	ents := rules.SynthEntryList(st)
	cur := &s.synthCur
	if !cur.set {
		cur.entry, cur.variant, cur.set = -1, -1, true
	}
	var out [][]byte
	var adds, removes []rules.Item
	changed := false
	have := func() (*rules.SynthEntry, *rules.SynthBoard) {
		if cur.entry < 0 || ents[cur.entry] == nil {
			return nil, nil
		}
		return ents[cur.entry], r.SynthBoard(ents[cur.entry].Board)
	}
	if cmd == 0x130D {
		switch binary.LittleEndian.Uint32(body[0x28:]) {
		case 7:
			for i := range rules.SynthEntries {
				out = append(out, s.entryRows(st, i)...)
			}
		case 8, 4:
			e, b := have()
			if e == nil || b == nil {
				break
			}
			if binary.LittleEndian.Uint32(body[0x28:]) == 4 {
				e.Running, e.Variant, e.Result = 0, -1, nil
				changed = true
			}
			cur.variant = -1
			e.Slots = b.Recipe(0)
			if binary.LittleEndian.Uint32(body[0x28:]) == 8 {
				out = append(out, s.materialRows(st, e, b, 0, -1)...)
			}
		case 6:
			e, b := have()
			if e == nil || b == nil {
				break
			}
			res := r.SynthRoll(s.srv.dice, st, e, b)
			changed = true
			sr, ir := r.Rooms(st, res.ID)
			out = append(out, s.c1310(0, 0, r.Rec20(res.ID, 1, 0, res.Element, res.Pct, true)), s.c130eSynth(0xB, cur.entry, sr, 0, ir),
				s.c131b(3, res.Fail, sr, ir, res.ID, 0, 0, nil),
				wire.Encode(0x131C, roomHead(s.gid, s.lid).U32(s.gid).Bytes(r.ConstRecID(res.ID))))
		}
	} else {
		sub := int16(binary.LittleEndian.Uint16(body[0x28:]))
		p := int(int16(binary.LittleEndian.Uint16(body[0x2A:])))
		q := int(binary.LittleEndian.Uint32(body[0x30:]))
		var rid string
		var ruuid uint32
		if len(body) >= 0x48 {
			rid = rules.ItemID(body[0x44:0x48]).String()
			ruuid = binary.LittleEndian.Uint32(body[0x40:])
		}
		switch {
		case sub == 2 || sub == 5: // register a board from the inventory
			b := r.SynthBoard(rid)
			if b == nil {
				break
			}
			i := -1
			if sub == 5 && p >= 0 && p < rules.SynthEntries {
				i = p
			} else {
				for k, x := range ents {
					if x == nil {
						i = k
						break
					}
				}
			}
			if i < 0 {
				break
			}
			removed, err := r.RemoveItem(st, ruuid, 1)
			if err != nil {
				break
			}
			removes = append(removes, removed)
			ents[i] = &rules.SynthEntry{Board: rid, FailID: -1, Variant: -1, Slots: b.Recipe(0)}
			changed = true
		case sub == 6 && q >= 0 && q < rules.SynthEntries && ents[q] != nil:
			cur.entry = q
			cur.variant = 0
			if b := r.SynthBoard(ents[q].Board); b != nil && b.VariantBoard {
				cur.variant = -1
			}
			out = append(out, s.c130eSynth(3, 0, 0), s.c130eSynth(4, ents[q].Running, 0))
		case sub == 7 && q >= 0 && q < rules.SynthEntries:
			cur.entry, cur.variant = q, -1
			if ents[q] != nil {
				cur.variant = ents[q].Variant
			}
		case sub == 0x10 && q >= 0 && q < rules.SynthEntries && ents[q] != nil:
			b := r.SynthBoard(ents[q].Board)
			if b == nil {
				break
			}
			for k := range 4 {
				for v := range 8 {
					m := b.Materials[k][v]
					id := ""
					if m.ID != "ffffffff" && m.ID != "00000000" {
						id = m.ID
					}
					n := 0
					if id != "" {
						n = r.StockCount(st, id)
					}
					out = append(out, s.c131b(4, q, k*10+v, m.Flag, id, m.Qty, n, nil))
				}
			}
		case sub == 3 && p >= 0 && p < 4:
			e, b := have()
			if e == nil || b == nil {
				break
			}
			v := -1
			for k := range 8 {
				if b.Materials[p][k].ID == rid {
					v = k
					break
				}
			}
			if v < 0 {
				v = 0
				for k := range 8 {
					if b.Materials[p][k].Flag == 1 {
						v = 7
					}
				}
			}
			e.Variant, cur.variant = v, v
			e.Slots = b.Recipe(v)
			e.Slots[p] = rid
			changed = true
			out = append(out, s.materialRows(st, e, b, v, v)...)
		case sub == 0xB: // start (variant boards: one product)
			e, b := have()
			if e == nil || b == nil {
				break
			}
			e.Running, e.Variant, e.Result = 1, max(0, p), nil
			r.SynthConsume(st, e, b, e.Variant, 1)
			changed = true
			out = append(out, s.c130eSynth(0xA, 0))
		case sub == 0xC:
			if e, _ := have(); e != nil {
				e.Running, e.Result = 1, nil
				e.Variant = max(0, e.Variant)
				changed = true
			}
			out = append(out, s.c130eSynth(0xC, 0))
		case sub >= 0xD && sub <= 0xF: // take out: inventory / stock / discard
			e, b := have()
			if e == nil || b == nil || e.Result == nil {
				break
			}
			n := 1
			if !b.VariantBoard {
				n = max(1, q)
				r.SynthConsume(st, e, b, 0, n)
			}
			res := e.Result
			var id rules.ItemID
			known := id.UnmarshalText([]byte(res.ID)) == nil
			if known {
				_, known = r.Item(id)
			}
			if sub == 0xD && known {
				if added, err := r.AddItem(st, id, n, 0); err == nil {
					adds = append(adds, added)
				}
			} else if sub == 0xE && known {
				st.RoomStock = append(st.RoomStock, rules.Item{UUID: 0x30000000 + uint32(rand.IntN(0x0FFFFFFF)), ID: id, Qty: n})
				out = append(out, s.c1310(2, 0, r.Rec20(res.ID, n, 0, 0, 0, false)))
			}
			e.Used += n
			e.Running, e.Result = 0, nil
			if e.Used >= b.FreqMax {
				ents[cur.entry] = nil
			}
			changed = true
		}
	}
	if changed || len(adds) > 0 || len(removes) > 0 {
		s.saveLocked()
	}
	c.mu.Unlock()
	for _, x := range adds {
		s.c.Send(c0106Add(r, s.disc, s.gid, s.lid, x))
	}
	for _, x := range removes {
		s.c.Send(c0106Remove(r, s.disc, s.gid, s.lid, x))
	}
	for _, pkt := range out {
		s.c.Send(pkt)
	}
	return nil
}
