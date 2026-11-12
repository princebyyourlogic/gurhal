// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"
	"fmt"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// Counters and missions (the disc's offline free missions, PS2 layouts):
//
//	C>S 0x0811 enter a counter (+0x24 type, -, zone, map, entry we came from, counter id)
//	C>S 0x1710 options -> 0x1711; 0x0C05 files -> 0x0C06 table; 0x0C0F -> 0x0C10 options
//	C>S 0x170B background locations -> 0x170C (a captured table); 0x0812 leave the counter
//	C>S 0x0C01 accept (+0x24 quest) -> 0x1020, 0x1015 (banner + quest NBL), 0x0C02 0
//	C>S 0x0807 quest, zone ffff -> the mission's enter warp (handlers_area.go)
//	C>S 0x0808 after a mission load -> the join snapshot, then 0x0C09 start
//	C>S 0x0C0E cancel -> 0x1006 12 and back

func init() {
	handle(0x0811, 0x30, (*session).counterEnter)
	handle(0x0812, 0, (*session).counterLeave)
	handle(0x1710, 0x28, (*session).counterOptions)
	handle(0x0C05, 0x28, (*session).counterFiles)
	handle(0x0C0F, 0x28, (*session).counterFiles)
	handle(0x170B, 0, (*session).counterLocations)
	handle(0x0C01, 0x28, (*session).missionAccept)
	handle(0x0C0E, 0, (*session).missionCancel)
	handle(0x0C11, 0x28, (*session).seqQuery)
	handle(0x0C13, 0x28, (*session).seqQuery)
	handle(0x0808, 0, (*session).zoneLoaded)
	handle(0x0110, 0x30, (*session).charEvent)
	handle(0x0402, 0x3C, (*session).condition)
	handle(0x0403, 0x2C, (*session).condition)
	handle(0x0404, 0x2C, (*session).condition)
	handle(0x0E00, 0x30, (*session).hits)
	handle(0x0F0A, 0x64, (*session).objectEvent)
}

// counterEnter is 0x0811: quest 0 + an empty zone, the counter as the location (EGS
// counter_enter). Counters are private: the player leaves its zone.
func (s *session) counterEnter(b []byte) error {
	if s.char == nil || s.zone == nil {
		return nil
	}
	fz, fm := uint32(binary.LittleEndian.Uint16(b[0x26:])), uint32(binary.LittleEndian.Uint16(b[0x28:]))
	fe := binary.LittleEndian.Uint16(b[0x2A:])
	counter := binary.LittleEndian.Uint32(b[0x2C:])
	s.prev = &spot{rules.Area{Quest: s.area.Quest, Zone: fz, Map: fm}, fe}
	s.area = rules.Area{Quest: counterQuest}
	s.srv.world.leave(s.zone, s.gid)
	s.zone = nil
	s.me.where.Store(&place{area: s.area}) // party members at a counter take the leader's mission
	s.c.Log.Debug("counter enter", "counter", counter)
	s.char.mu.Lock()
	seq := s.counterSequence(counter, s.areanb+1)
	s.char.mu.Unlock()
	s.sendAll(seq)
	s.areanb += 2
	return nil
}

// counterLeave is 0x0812: back to where the counter was entered from.
func (s *session) counterLeave(_ []byte) error {
	if s.char == nil || s.me == nil || s.prev == nil || !s.available(s.prev.area.Quest, s.prev.area.Zone) {
		return nil
	}
	p := s.prev
	s.prev = nil
	s.goZone(p.area, p.entry, zoneKey{s.uni, p.area.Quest, p.area.Zone, 0}, lobbyZone, true)
	return nil
}

func (s *session) counterOptions(_ []byte) error { return s.c.Send(c1711CounterBG(s.gid)) }

// counterFiles answers 0x0C05 (the table) and 0x0C0F (its options) for a counter with a
// mission table.
func (s *session) counterFiles(b []byte) error {
	id := binary.LittleEndian.Uint32(b[0x24:])
	ct, ok := s.r.Counter(id)
	if !ok {
		return nil
	}
	opts := s.r.CounterOptions(ct)
	if s.lastCmd == 0x0C0F {
		return s.c.Send(c0c10Options(s.gid, opts))
	}
	nbl, ok := s.nbl(fmt.Sprintf("counter-table-%d.nbl", id))
	if !ok {
		return nil
	}
	return s.c.SendPaced(c0c06Table(s.gid, opts, nbl))
}

func (s *session) counterLocations(_ []byte) error {
	if t, ok := s.srv.Data.Capture("packet170c.bin"); ok {
		return s.c.Send(c170cLocations(s.gid, t))
	}
	return nil
}

// missionAccept is 0x0C01: the run starts over for its owner (the party leader); the
// leader's members at the same counter take it too.
func (s *session) missionAccept(b []byte) error {
	if s.me == nil {
		return nil
	}
	qid := binary.LittleEndian.Uint32(b[0x24:])
	if s.r.Mission(qid) == nil {
		return s.c.Send(c0c02(s.gid, 1))
	}
	w := s.srv.world
	owner := s.gid
	var others []*player
	w.partyMu.Lock()
	if pt := s.me.party; pt != nil {
		owner = pt.slots[pt.leader].gid
		if pt.slots[pt.leader] == s.me {
			pt.mission = &partyMission{qid, owner}
			for _, m := range pt.members() {
				if m != s.me {
					others = append(others, m)
				}
			}
		}
	}
	w.partyMu.Unlock()
	w.startRun(runKey{s.uni, qid, owner})
	s.mission = &missionState{quest: qid, owner: owner, origin: s.prev}
	s.c.Log.Info("mission accept", "quest", qid, "owner", fmt.Sprintf("%#08x", owner))
	nbl, _ := s.nbl(questNBL(qid))
	s.c.Send(c1020(s.gid))
	s.c.SendPaced(c1015(s.gid, qid, nbl))
	here := s.me.where.Load()
	for _, m := range others { // members where the leader is (the same counter, or zone)
		mw := m.where.Load()
		if here == nil || mw == nil || mw.area.Quest != here.area.Quest || mw.area.Zone != here.area.Zone {
			continue
		}
		if m.r.Mission(qid) == nil { // the other disc lacks it
			m.conn.Relay(c0c02(m.gid, 4))
			continue
		}
		mnbl, _ := s.srv.Data.NBL(m.disc, questNBL(qid))
		m.conn.Relay(c1020(m.gid))
		m.conn.Relay(c1015(m.gid, qid, mnbl))
		m.conn.Relay(c0c02(m.gid, 0))
	}
	return s.c.Send(c0c02(s.gid, 0))
}

func (s *session) missionCancel(_ []byte) error {
	if s.me == nil || s.mission == nil {
		return nil
	}
	s.endMission(12)
	return nil
}

// seqQuery answers AotI story scripts' item queries (0x0C11 / 0x0C13) with success.
func (s *session) seqQuery(b []byte) error {
	return s.c.Send(c0c12Seq(s.lastCmd+1, s.gid, binary.LittleEndian.Uint32(b[0x24:])))
}

// zoneLoaded is 0x0808: in a mission, the join snapshot and the start.
func (s *session) zoneLoaded(_ []byte) error {
	if s.mission == nil || !s.mission.in || s.zone == nil {
		return nil
	}
	pending := s.mission.startPending
	s.mission.startPending = false
	s.zone.loaded(s.gid, pending)
	return nil
}

// charEvent is 0x0110 (+0x2C event, +0x30 param): 2 the types menu, 3 change type, 7
// down (a Scape Doll revives), 8 back to the lobby after dying.
func (s *session) charEvent(b []byte) error {
	if s.char == nil {
		return nil
	}
	ev, param := binary.LittleEndian.Uint32(b[0x2C:]), binary.LittleEndian.Uint32(b[0x30:])
	c := s.char
	switch {
	case ev == 2:
		c.mu.Lock()
		table := s.r.TypesTable(c.raw, c.st)
		c.mu.Unlock()
		return s.c.Send(c0113Types(s.gid, table))
	case ev == 3 && s.me != nil:
		c.mu.Lock()
		if !s.r.CanChangeType(c.st, int(param)) {
			c.mu.Unlock()
			return nil
		}
		c.st.Cls = int(param)
		c.st.HP = min(c.st.HP, s.r.Derived(c.raw, c.st).MaxHP)
		s.saveLocked()
		user := s.ownUser()
		c.mu.Unlock()
		return s.c.Send(c010d(s.lid, s.gid, s.gid, s.lid, user, noSource, true))
	case ev == 7 && s.me != nil:
		return s.down()
	case ev == 8 && s.me != nil:
		c.mu.Lock()
		c.st.HP = s.r.Derived(c.raw, c.st).MaxHP
		s.saveLocked()
		c.mu.Unlock()
		back := s.prev
		if s.mission != nil {
			ms := s.mission
			back, s.mission = ms.origin, nil
			s.setInMission(false)
			s.c.Send(c1006(&member{p: s.me, lid: s.lid}, 10, 0))
		}
		if back != nil {
			s.goZone(back.area, back.entry, zoneKey{s.uni, back.area.Quest, back.area.Zone, 0}, lobbyZone, true)
		}
	}
	return nil
}

const scapeDoll = "03010900"

// down: a Scape Doll revives at once (the client uses it, 0x0105 event 8); without one
// the player is incapacitated and the mission counts a death.
func (s *session) down() error {
	c := s.char
	var doll rules.ItemID
	doll.UnmarshalText([]byte(scapeDoll))
	c.mu.Lock()
	var d *rules.Item
	for i := range c.st.Inv {
		if c.st.Inv[i].ID == doll {
			d = &c.st.Inv[i]
			break
		}
	}
	if d == nil {
		c.mu.Unlock()
		if s.zone != nil && s.mission != nil && s.mission.in {
			s.zone.death()
		}
		return s.c.Send(c0111(s.lid, s.gid, s.gid, s.lid, 3, 1))
	}
	mx := s.r.Derived(c.raw, c.st).MaxHP
	rec := s.r.VarRec(d, s.disc)
	uuid := d.UUID
	s.r.RemoveItem(c.st, uuid, 1)
	c.st.HP = mx
	s.saveLocked()
	se2 := c.status.SE2(monoNow())
	c.mu.Unlock()
	use := wire.Encode(0x0105, itemHead(s.gid, s.lid).U32(s.gid).U32(uint32(s.lid)).U8(0).U8(8).U8(rec[0x14]).U8(0).U32(uuid))
	s.c.Send(use)
	s.c.Send(c0117(&member{p: s.me, lid: s.lid}, s.gid, s.lid, se2, mx))
	s.srv.world.partyHP(s.me, mx)
	return nil
}

// --- forwarded to the zone ---------------------------------------------------------------------------

func (s *session) condition(b []byte) error {
	if s.zone != nil {
		s.zone.condition(s.gid, s.lastCmd, b)
	}
	return nil
}

func (s *session) hits(b []byte) error {
	if s.zone != nil {
		s.zone.hits(s.gid, b)
	}
	return nil
}

func (s *session) objectEvent(b []byte) error {
	if s.zone != nil && s.mission != nil && s.mission.in {
		s.zone.object(s.gid, b)
		return nil
	}
	return s.chair(b)
}

// counterSequence is EGS's counter_enter load: quest 0 + an empty zone, the counter as the
// location (0x100E with its id), 0x0215 x2, 0x020C, 0x1202 / 0x1204 / 0x1206 / 0x1207,
// 0x1212, 0x0201, 0x0A06, 0x0208, 0x0236. Hold the character lock.
func (s *session) counterSequence(counter, nb uint32) [][]byte {
	gid, lid := s.gid, s.lid
	quest, _ := s.nbl(questNBL(0))
	s.entry = 0
	user := s.ownUser()
	be := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
	hd := wire.Body{}.Head(wire.NoLID, gid)
	chunk := wire.Body{}.Bytes(fill(0xff, 20)).Zero(28).Bytes(be(0x0000ffff)).Bytes(be(0xff000000)).Bytes(be(0x64000a00))
	ctype := uint32(1)
	if counter == 0xFFFFFFFF {
		ctype = 2
	}
	var chunks wire.Body
	for range 6 {
		chunks = chunks.Bytes(chunk)
	}
	return [][]byte{
		c0c00Quest(lid, gid, counterQuest),
		c020eQuest(s.disc, quest),
		c0a05(lid, gid),
		c010d(lid, gid, gid, lid, user, noSource, true),
		c0200ZoneInit(lid, gid, 0, missionVar),
		wire.Encode(0x020F, wire.Body{}.U16(wire.NoLID).Zero(34).U8(0).U8(0xFF).Zero(2).U32(2000).Zero(2000)),
		c0205Map(lid, gid, rules.Area{}, 0, nb, lid, 0),
		wire.Encode(0x100E, wire.Body{}.Bytes(be(0xffffffbf)).Zero(16).Bytes(wire.ACC).Bytes(be(gid)).Zero(8).U8(1).U8(0).
			Zero(6).Bytes(be(0xffffff7f)).Bytes(utf16Fixed("Counter", 64)).Zero(4).U32(counter).U32(ctype)),
		c0215Host(lid, gid, 0),
		c0215Host(lid, gid, 0),
		wire.EncodeOn(wire.ChanClient, 0x020C, wire.Body{}.Bytes(be(0xffff0000)).Zero(32)),
		wire.Encode(0x1202, append(append([]byte(nil), hd...), wire.Body{}.Zero(4).Bytes(be(0x10000000)).Zero(8).
			Bytes(be(0x14000000)).Zero(4)...)),
		wire.Encode(0x1204, wire.Body{}.Head(lid, gid).Zero(4).Bytes(be(0x20000000)).Zero(32)),
		wire.Encode(0x1206, append(append([]byte(nil), hd...), wire.Body{}.Zero(4).Bytes(be(0x80020000)).Zero(640)...)),
		wire.Encode(0x1207, append(append([]byte(nil), hd...), chunks...)),
		c1212(gid),
		c0201(lid, gid, gid, user),
		c0a06(lid, gid, s.char.st.Inv),
		c0208Loaded(lid, gid, nb+1),
		c0236Start(lid, gid),
	}
}
