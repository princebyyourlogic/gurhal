// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/rules"
)

func init() {
	handle(0x021C, 0, (*session).enterLobby)
	handle(0x0807, 0x34, (*session).areaChange)
	handle(0x0304, 0x3C, (*session).chat)
	handle(0x021D, 0, (*session).uniCube)
	handle(0x021F, 0x2C, (*session).uniSelect)
}

const (
	colonyQuest    = 1100000
	spaceportQuest = 1104000
	offlineLobby   = 200000 // the disc's offline lobby: AotI story bases warp home to it
	myRoomQuest    = 1120000
	counterQuest   = 0x7FFFFFFF // EGS priv/quests/0: the quest every counter loads (quest-0.nbl)
	noParty        = 0xFFFFFFFF
	noZone         = 0xFFFF // 0x0807 zone ffff: depart into the accepted mission
)

var lobby = rules.Area{Quest: colonyQuest, Zone: 0, Map: 4}

// The native lobby zones (the disc's offline Extra lobby) warp with their own zone
// numbers; a map that zone has is served from the online lobby quest's zone 0.
var nativeZones = map[uint32]struct {
	quest uint32
	maps  map[uint32]bool
}{
	2:  {colonyQuest, set(1, 2, 3, 4, 5, 100, 101, 102, 103, 301)},
	3:  {1101000, set(1, 2, 3, 4, 100, 200, 400)},
	4:  {1102000, set(1, 3, 100, 200)},
	5:  {1103000, set(1, 2, 100, 101, 102, 200)},
	10: {spaceportQuest, set(900, 901)},
}

var lobbyQuests = set(colonyQuest, 1101000, 1102000, 1103000, spaceportQuest)

func set(vs ...uint32) map[uint32]bool {
	m := map[uint32]bool{}
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// resolve is the online area for a request, translating native lobby zone numbers.
func resolve(disc string, q, z, m uint32) rules.Area {
	if q == spaceportQuest && m == 900 && disc == rules.AotI {
		m = 901 // AotI's Spaceport only has map 901's entries
	}
	if n, ok := nativeZones[z]; ok && n.maps[m] && (lobbyQuests[q] || q == offlineLobby || q == myRoomQuest) {
		return rules.Area{Quest: n.quest, Map: m}
	}
	if z >= 11 && z <= 13 { // shop and side doors: the native zone 0 holds these maps
		for _, n := range nativeZones {
			if n.quest == q && n.maps[m] {
				return rules.Area{Quest: q, Map: m}
			}
		}
	}
	return rules.Area{Quest: q, Zone: z, Map: m}
}

func (s *session) nbl(name string) ([]byte, bool) { return s.srv.Data.NBL(s.disc, name) }

func questNBL(q uint32) string   { return fmt.Sprintf("quest-%d.nbl", q) }
func zoneNBL(q, z uint32) string { return fmt.Sprintf("zone-%d-%d.nbl", q, z) }

func (s *session) available(q, z uint32) bool {
	_, a := s.nbl(questNBL(q))
	_, b := s.nbl(zoneNBL(q, z))
	return a && b
}

// enterLobby puts the selected character into the Colony (0x021C after a load).
func (s *session) enterLobby(_ []byte) error {
	if s.char == nil {
		return nil
	}
	if s.me == nil {
		s.me = &player{gid: s.gid, disc: s.disc, r: s.r, conn: s.c, jobs: s.jobs, user: s.user}
		if old := s.srv.world.claim(s.me); old != nil {
			old.conn.Close() // the same account logged in again: the newest session wins
		}
		me := s.me
		go s.srv.guard("presence", func() { s.srv.world.presence(me) }) // card holders: now online
		s.srv.welcome(s.me)                                             // the current notice, then staff answers
	}
	s.me.char.Store(s.char)
	if _, ok := s.srv.universe(s.uni); !ok {
		s.uni = s.srv.Universes[0].ID
	}
	s.me.uni.Store(s.uni)
	s.refresh()
	s.goZone(lobby, 0, zoneKey{s.uni, lobby.Quest, lobby.Zone, 0}, lobbyZone, true)
	s.c.Log.Info("lobby", "lid", s.lid)
	return nil
}

// goZone is a zone change: join the zone instance, send its load (a new quest when
// questChange), then spawn for the others once it is out. In a mission the 0x0200
// mission variant and the host (the party leader if here) go along.
func (s *session) goZone(a rules.Area, entry uint16, key zoneKey, mode zoneMode, questChange bool) {
	s.area, s.entry = a, entry
	others := s.enterZone(key)
	host := uint32(0)
	inMission := mode == missionZone
	if inMission {
		host = uint32(s.lid)
		if pt, _ := s.srv.world.partyOf(s.me); pt != nil {
			s.srv.world.partyMu.Lock()
			leader := pt.slots[pt.leader]
			s.srv.world.partyMu.Unlock()
			if leader != nil {
				if w := leader.where.Load(); w != nil && w.area.Quest == a.Quest && w.area.Zone == a.Zone {
					host = uint32(w.lid)
				}
			}
		}
	}
	s.char.mu.Lock()
	seq := s.zoneSequence(s.areanb+1, questChange, others, mode, host)
	s.char.mu.Unlock()
	s.sendAll(seq)
	s.areanb += 2
	s.shown()
}

// enterZone joins a zone instance, leaving the one before.
func (s *session) enterZone(key zoneKey) [][]byte {
	if s.zone != nil {
		s.srv.world.leave(s.zone, s.gid)
	}
	m := &member{p: s.me, area: s.area, entry: s.entry, areanb: s.areanb}
	var others [][]byte
	s.zone, s.lid, others = s.srv.world.join(key, m)
	s.me.where.Store(&place{s.area, s.entry, s.lid})
	return others
}

// shown tells the zone where the player now stands, and once its load is written,
// spawns it for the others; the party learns the new location.
func (s *session) shown() {
	z, gid := s.zone, s.gid
	s.me.where.Store(&place{s.area, s.entry, s.lid})
	z.locate(gid, s.area, s.entry, s.areanb)
	s.c.After(func() { z.show(gid) })
	s.srv.world.partyLocation(s.me)
	me := s.me
	go s.srv.guard("card viewers", func() { s.srv.world.cardViewers(me) })
}

// zoneMode picks a zone load's 0x0200 variant: a lobby, a mission (with the host), a
// My Room.
type zoneMode int

const (
	lobbyZone zoneMode = iota
	missionZone
	roomZone
)

// zoneKeyFor is the instance an area belongs to: a mission's are per owner.
func (s *session) zoneKeyFor(a rules.Area) zoneKey {
	if s.mission != nil && s.mission.in && a.Quest == s.mission.quest {
		return zoneKey{s.uni, a.Quest, a.Zone, s.mission.owner}
	}
	return zoneKey{s.uni, a.Quest, a.Zone, 0}
}

// areaChange is 0x0807: +0x24 quest, zone u16, map u16, entry u16, area counter u16,
// party position (0xFFFFFFFF solo). Zone ffff departs into the accepted mission; leaving
// a mission's maps ends it. A map in the current zone gets the location load, another
// zone or quest with archives the zone load, anything else sends the player back to entry
// 0 where they stand.
func (s *session) areaChange(b []byte) error {
	if s.char == nil || s.me == nil {
		return nil
	}
	q := binary.LittleEndian.Uint32(b[0x24:])
	z, m := uint32(binary.LittleEndian.Uint16(b[0x28:])), uint32(binary.LittleEndian.Uint16(b[0x2A:]))
	entry := binary.LittleEndian.Uint16(b[0x2C:])
	solo := binary.LittleEndian.Uint32(b[0x30:]) == noParty
	s.char.mu.Lock()
	s.char.ppReady = false // no PP regeneration during a load
	s.char.mu.Unlock()

	if s.mission == nil && z == noZone { // a member follows the leader into the party's mission
		if pt, _ := s.srv.world.partyOf(s.me); pt != nil {
			s.srv.world.partyMu.Lock()
			pm := pt.mission
			s.srv.world.partyMu.Unlock()
			if pm != nil && pm.quest == q && s.r.Mission(q) != nil {
				s.mission = &missionState{quest: q, owner: pm.owner, origin: s.prev}
			}
		}
	}
	if ms := s.mission; ms != nil {
		mm := s.r.Mission(ms.quest)
		if q == ms.quest && z == noZone { // departing into the mission
			ez, em, ee := mm.Enter[0], mm.Enter[1], mm.Enter[2]
			ms.in, ms.startPending = true, true
			s.setInMission(true)
			s.c.Log.Info("mission enter", "quest", ms.quest, "to", fmt.Sprintf("%d/%d/%d", ez, em, ee))
			a := rules.Area{Quest: ms.quest, Zone: uint32(ez), Map: uint32(em)}
			s.goZone(a, uint16(ee), zoneKey{s.uni, ms.quest, uint32(ez), ms.owner}, missionZone, true)
			return nil
		}
		if ms.in && !(q == ms.quest && mm.HasMap(z, m)) { // leaving the mission
			s.endMission(10)
			return nil
		}
	}
	if wantsRoom(q, z, m) { // own room
		s.char.mu.Lock()
		raw, st := s.char.raw, cloneState(s.char.st)
		s.char.mu.Unlock()
		s.enterRoom(s.gid, raw, st, s.entrance)
		return nil
	}
	if s.zone == nil {
		return nil
	}
	to := resolve(s.disc, q, z, m)
	sameZone := solo && to.Quest == s.area.Quest && to.Zone == s.area.Zone
	otherZone := solo && !sameZone && s.available(to.Quest, to.Zone)
	s.c.Log.Debug("area change", "asked", fmt.Sprintf("%d/%d/%d", q, z, m), "to", to, "entry", entry,
		"same_zone", sameZone, "other_zone", otherZone)
	switch {
	case otherZone:
		origin := s.area.Quest
		var pre [][]byte
		if to.Quest == spaceportQuest { // the lobby counter reads SAVE_LC00..05 from 0x0D01
			s.char.mu.Lock()
			pre = [][]byte{c0d01(s.gid, s.r.CharBin(s.char.raw, s.char.st, s.disc), s.options,
				s.chrWork(s.lobbyCounterWork(origin)))}
			s.char.mu.Unlock()
		}
		s.sendAll(pre)
		// another zone of a mission keeps its instance but loads like a lobby zone (no host)
		s.goZone(to, entry, s.zoneKeyFor(to), lobbyZone, to.Quest != origin)
		return nil
	case sameZone:
		s.area, s.entry = to, entry
	default:
		s.entry = 0
	}
	_, _, _, seasonal := s.staged(s.area.Quest, s.area.Zone)
	s.char.mu.Lock()
	seq := s.locationSequence(s.areanb+1, false, nil, seasonal)
	s.char.mu.Unlock()
	s.sendAll(seq)
	s.areanb += 2
	s.me.where.Store(&place{s.area, s.entry, s.lid})
	s.zone.locate(s.gid, s.area, s.entry, s.areanb)
	s.srv.world.partyLocation(s.me)
	return nil
}

// setInMission marks the character for PP regeneration (missions only).
func (s *session) setInMission(in bool) {
	s.char.mu.Lock()
	s.char.inMission = in
	s.char.mu.Unlock()
}

// endMission leaves the mission (0x1006 event 10 end, 12 cancelled), refills HP and goes
// back to where the counter was entered from.
func (s *session) endMission(event uint8) {
	ms := s.mission
	s.mission = nil
	s.c.Log.Info("mission leave", "quest", ms.quest, "event", event)
	s.c.Send(c1006(&member{p: s.me, lid: s.lid}, event, 0))
	if pt, _ := s.srv.world.partyOf(s.me); pt != nil {
		s.srv.world.partyMu.Lock()
		if pt.slots[pt.leader] == s.me {
			pt.mission = nil
		}
		s.srv.world.partyMu.Unlock()
	}
	s.char.mu.Lock()
	s.char.inMission = false
	s.char.st.HP = s.r.Derived(s.char.raw, s.char.st).MaxHP
	s.char.mu.Unlock()
	s.save()
	if ms.in && ms.origin != nil {
		s.goZone(ms.origin.area, ms.origin.entry, zoneKey{s.uni, ms.origin.area.Quest, ms.origin.area.Zone, 0}, lobbyZone, true)
	}
}

// lobbyCounterWork is SAVE_LC00..05 for the Spaceport's lobby counter: LC00..03 offer
// the four lobbies, LC04 is where the player came from, LC05 != 0 skips the district
// submenu. The script resets them on every visit, so they go out before each entry.
func (s *session) lobbyCounterWork(origin uint32) map[int]int32 {
	w := s.lcWork
	if s.disc == rules.AotI && origin == colonyQuest && len(w) >= 6 {
		// AotI preloads only the shuttle movie of the gate's planet and hangs on any other:
		// keep what its own gate wrote, except LC05 = 1
		out := map[int]int32{}
		for i := range 6 {
			out[saveLC+i] = w[i]
		}
		out[saveLC+5] = 1
		return out
	}
	gate, ok := map[uint32]int32{1101000: 3, 1102000: 4, 1103000: 5}[origin]
	if !ok {
		if gate = w[4]; gate < 0 || gate > 2 {
			gate = 0
		}
	}
	return map[int]int32{saveLC: 1, saveLC + 1: 1, saveLC + 2: 1, saveLC + 3: 1, saveLC + 4: gate, saveLC + 5: 1}
}

// zoneSequence is EGS's area change into a zone: [0x0C00 + 0x020E for a new quest],
// 0x0A05, 0x0111 spawn, 0x010D, 0x0200 zone init, 0x020F zone NBL, then the location
// load with 0x1212 and 0x0A06, [0x0215 host in a mission], 0x0233 (the players already
// there) before 0x0208. Hold the character lock.
func (s *session) zoneSequence(nb uint32, questChange bool, others [][]byte, mode zoneMode, host uint32) [][]byte {
	a, gid, lid := s.area, s.gid, s.lid
	var seq [][]byte
	season, squest, zone, seasonal := s.zoneSeason(a, questChange)
	if !seasonal {
		zone, _ = s.nbl(zoneNBL(a.Quest, a.Zone))
	}
	if questChange {
		quest, _ := s.nbl(questNBL(a.Quest))
		if seasonal && s.disc != rules.AotI {
			quest = squest
		}
		seq = append(seq, c0c00Quest(lid, gid, a.Quest), c020eQuest(s.disc, quest))
	}
	v, inMission := lobbyVar, mode == missionZone
	switch mode {
	case missionZone:
		v = missionVar
	case roomZone:
		v = roomVar
	}
	seq = append(seq,
		c0a05(lid, gid),
		c0111(lid, gid, gid, lid, 6, 0),
		c010d(lid, gid, gid, lid, s.ownUser(), noSource, true),
		c0200ZoneInit(lid, gid, a.Zone, v),
		c020fZone(zone, season))
	loc := s.locationSequence(nb, true, others, seasonal)
	if inMission { // the session's host id, before 0x0233 / 0x0208
		at := len(loc) - 2
		if len(others) > 0 {
			at--
		}
		loc = append(loc[:at], append([][]byte{c0215Host(lid, gid, host)}, loc[at:]...)...)
	}
	return append(seq, loc...)
}

// locationSequence is the load from "the player's location" on: 0x0205 (with the
// seasonal flag), 0x100E, [0x1212], 0x0201, [0x0A06], [0x0233], 0x0208, 0x0236. A map
// change is only this. Hold the character lock.
func (s *session) locationSequence(nb uint32, zoneChange bool, others [][]byte, seasonal bool) [][]byte {
	a, gid, lid := s.area, s.gid, s.lid
	flag := uint8(0)
	if seasonal {
		flag = 1
	}
	seq := [][]byte{c0205Map(lid, gid, a, s.entry, nb, lid, flag), c100eLocation(gid, a, s.entry)}
	if zoneChange {
		seq = append(seq, c1212(gid))
	}
	seq = append(seq, c0201(lid, gid, gid, s.ownUser()))
	if zoneChange {
		seq = append(seq, c0a06(lid, gid, s.char.st.Inv))
	}
	if len(others) > 0 {
		seq = append(seq, c0233Players(lid, gid, s.disc, others))
	}
	return append(seq, c0208Loaded(lid, gid, nb+1), c0236Start(lid, gid))
}

// ownUser is the player's own user record (no position: the client places itself). Hold
// the character lock.
func (s *session) ownUser() []byte {
	s.char.bank(time.Now())
	return s.r.User(s.gid, s.lid, s.char.raw, s.char.st, s.area, s.entry, rules.Pos{}, s.disc, "")
}

func (s *session) chat(b []byte) error {
	if s.zone == nil {
		return nil
	}
	if !s.chatRate.allow(time.Now()) {
		return nil // flooding: dropped
	}
	text := rules.UTF16Text(b[0x3C:])
	s.c.Log.Debug("chat", "text", text)
	if strings.HasPrefix(strings.TrimSpace(text), "!") && s.me != nil && s.char != nil {
		s.gmCommand(text)
		return nil
	}
	if m, ok := s.srv.muted(s.gid); ok && s.me != nil { // not relayed
		say(s.me, mutedText(m), 4)
		return nil
	}
	s.zone.chat(s.gid, b)
	return nil
}

func (s *session) uniCube(_ []byte) error {
	return s.c.Send(c021eUniverses(s.srv.Universes, s.srv.world.population()))
}

// uniSelect is 0x021F: +0x24 universe (0 = cancel). The character loads again; the
// client's 0x021C then enters the new universe's lobby.
func (s *session) uniSelect(b []byte) error {
	uni := binary.LittleEndian.Uint32(b[0x24:])
	if _, ok := s.srv.universe(uni); !ok || s.char == nil {
		return nil
	}
	if s.zone != nil {
		s.srv.world.leave(s.zone, s.gid)
		s.zone = nil
	}
	s.uni = uni
	if s.me != nil {
		s.me.uni.Store(uni)
	}
	s.c.Send(c0230LeaveArea(s.gid))
	s.sendAll(s.loadSequence())
	return nil
}
