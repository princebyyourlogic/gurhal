// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"
	"math"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/store"
)

// room is what a zone instance owns besides its members, as the client's offline room
// (TGameRoom) did: event flags and object states, enemies, ground items, and in a
// mission the judge (start, clear, rank, rewards). Only the zone's goroutine touches it.
//
//	C>S 0x0402 condition   -> 0x1201 to the zone; an enemy reporting in gets its 0x0E08
//	C>S 0x0403 / 0x0404    -> 0x1203 / 0x1205 flag changes
//	C>S 0x0E00 hits        -> 0x0E07 results, kills: EXP, drops, wave clears
//	C>S 0x0F0A objects     -> key consoles, gates, keys, 0x1213 states
//	C>S 0x0106 6 pickup    -> 0x0106 to the zone
type room struct {
	w        *world
	key      zoneKey
	r        *rules.Rules
	mission  *rules.Mission // nil outside missions
	run      *missionRun    // nil outside missions
	flags    *flagSet
	maps     []uint32
	roster   map[uint32]*rules.EnemySpec
	gates    []rules.Gateway
	gateDone map[uint32]bool
	enemies  map[uint32]*rules.Enemy
	pos      map[uint32][]byte // an enemy's last reported position (where its drop lands)
	set      map[[3]int]rules.SetObject
	objects  map[int]uint32
	boxes    map[uint32]*rules.Box
	ground   []rules.Ground // oldest first, at most groundMax
	rng      rules.Rand
}

const groundMax = 50

func (rm *room) init(w *world, key zoneKey, r *rules.Rules, run *missionRun) {
	*rm = room{w: w, key: key, r: r, flags: &flagSet{}, gateDone: map[uint32]bool{}, enemies: map[uint32]*rules.Enemy{},
		pos: map[uint32][]byte{}, set: map[[3]int]rules.SetObject{}, objects: map[int]uint32{},
		boxes: map[uint32]*rules.Box{}, rng: w.srv.dice}
	if r != nil {
		rm.mission = r.Mission(key.quest)
	}
	if rm.mission == nil {
		return
	}
	rm.maps = rm.mission.Maps(key.zone)
	rm.roster, rm.gates = rm.mission.Roster(key.zone)
	for _, o := range rm.mission.SetObjects(key.zone) {
		rm.set[[3]int{o.MapIndex, o.Group, o.Object}] = o
		if o.Type == 12 && o.Target >= 0 {
			rm.boxes[uint32(o.Target)] = &rules.Box{MapIndex: o.MapIndex, Params: o.Params, OID: o.OID, Pos: o.Pos}
		}
	}
	if run != nil {
		rm.run = run
		rm.run.mu.Lock()
		rm.run.flags[key.zone] = rm.flags
		rm.run.mu.Unlock()
	}
}

func (rm *room) close() {
	if rm.run != nil {
		rm.run.mu.Lock()
		if rm.run.flags[rm.key.zone] == rm.flags {
			delete(rm.run.flags, rm.key.zone)
		}
		rm.run.mu.Unlock()
	}
}

func (rm *room) mapIndex(mapID uint32) int {
	for i, m := range rm.maps {
		if m == mapID {
			return i
		}
	}
	return 0
}

// flagFn reads a mission flag in any zone of the run: zone, map id, event.
func (rm *room) flagFn() func(z, m, e int) bool {
	return func(z, mapID, ev int) bool {
		maps := rm.mission.Maps(uint32(z))
		idx := -1
		for i, x := range maps {
			if int(x) == mapID {
				idx = i
			}
		}
		if idx < 0 {
			return false
		}
		rm.run.mu.Lock()
		fs := rm.run.flags[uint32(z)]
		rm.run.mu.Unlock()
		return fs != nil && fs.isSet(uint32(idx*256+ev))
	}
}

// lock and unlock take a member's character lock on the zone's goroutine, keeping track
// so that a panic recovered there (safely) releases what it held.
func (zs *zoneState) lock(c *character) {
	c.mu.Lock()
	zs.held = append(zs.held, c)
}

func (zs *zoneState) unlock(c *character) {
	for i := len(zs.held) - 1; i >= 0; i-- {
		if zs.held[i] == c {
			zs.held = append(zs.held[:i], zs.held[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
}

// safely runs f on the zone's goroutine: a panic is logged and the character locks f
// held are released, so the zone and its players go on.
func (zs *zoneState) safely(what string, f func()) {
	defer func() {
		for i := len(zs.held) - 1; i >= 0; i-- {
			zs.held[i].mu.Unlock()
		}
		zs.held = zs.held[:0]
	}()
	zs.w.srv.guard(what, f)
}

// lockAll locks every member's character in GID order; the result maps GID to it.
func (zs *zoneState) lockAll() (map[uint32]*character, func()) {
	chars := map[uint32]*character{}
	var held []*character
	for _, m := range zs.sorted() {
		if c := m.p.char.Load(); c != nil {
			zs.lock(c)
			chars[m.p.gid] = c
			held = append(held, c)
		}
	}
	return chars, func() {
		for i := len(held) - 1; i >= 0; i-- {
			zs.unlock(held[i])
		}
	}
}

// saveLocked queues a member's state and refreshes what others see of it; hold its lock.
func (zs *zoneState) saveLocked(m *member, c *character) {
	c.bank(time.Now())
	zs.w.srv.saver.queue(m.p.gid, c.slot, c.st)
	m.p.snap.Store(c.snapshot(m.p.r))
}

// --- conditions and flags ----------------------------------------------------------------------------

// condition is C>S 0x0402 (+0x24 id, +0x28 4 x s16, +0x30 type) or 0x0403 / 0x0404 (+0x24
// flag id, +0x28 mode 0 set, 1 clear, 2 toggle).
func (z *zone) condition(gid uint32, cmd uint16, body []byte) {
	z.do(func(zs *zoneState) {
		rep := zs.members[gid]
		if rep == nil {
			return
		}
		rm := &zs.room
		cid, mode := binary.LittleEndian.Uint32(body[0x24:]), binary.LittleEndian.Uint32(body[0x28:])
		if cmd == 0x0402 {
			zs.broadcast(func(d *member) []byte { return c1201(d, body) })
		} else {
			b := cmd == 0x0403
			rm.flags.set(cid, mode, b)
			zs.broadcast(func(d *member) []byte { return c1205Flag(d, cid, mode, b) })
		}
		if cmd == 0x0402 && len(body) >= 0x34 { // an enemy reporting in: its status now
			eid := cid + 150 // condition id + 150 = the enemy's target id
			ex, ey, ez := int16(binary.LittleEndian.Uint16(body[0x28:])), int16(binary.LittleEndian.Uint16(body[0x2A:])),
				int16(binary.LittleEndian.Uint16(body[0x2C:]))
			if ex != 0 || ez != 0 {
				rm.pos[eid] = xyz(float32(ex), float32(ey), float32(ez))
			}
			if spec := rm.roster[eid]; spec != nil && rm.enemies[eid] == nil {
				e := rep.p.r.NewEnemy(eid, spec, zs.level(rep))
				rm.enemies[eid] = e
				zs.broadcast(func(d *member) []byte { return c0e08Enemy(d, e) })
			}
			if binary.LittleEndian.Uint32(body[0x30:])&3 == 3 { // gateway condition id = gateway id - 150
				for _, g := range rm.gates {
					if g.ID-150 == cid && len(g.Flags) > 0 && g.Flags[0] >= 0 && !rm.gateDone[g.ID] {
						rm.gateDone[g.ID] = true
						zs.setFlag(uint32(g.MapIndex*256 + g.Flags[0]))
						break
					}
				}
			}
		}
		zs.missionCheck(rep)
	})
}

// setFlag sets a set-A flag and tells the zone (0x1205 mode 0).
func (zs *zoneState) setFlag(fid uint32) {
	zs.room.flags.set(fid, 0, false)
	zs.broadcast(func(d *member) []byte { return c1205Flag(d, fid, 0, false) })
}

func (zs *zoneState) level(m *member) int {
	if c := m.p.char.Load(); c != nil {
		zs.lock(c)
		defer zs.unlock(c)
		return max(1, c.st.Level)
	}
	return 1
}

func xyz(x, y, z float32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, math.Float32bits(x))
	b = binary.LittleEndian.AppendUint32(b, math.Float32bits(y))
	return binary.LittleEndian.AppendUint32(b, math.Float32bits(z))
}

// --- hits --------------------------------------------------------------------------------------------

// hits resolves a C>S 0x0E00 from gid.
func (z *zone) hits(gid uint32, body []byte) {
	z.do(func(zs *zoneState) {
		rep := zs.members[gid]
		if rep == nil {
			return
		}
		rm := &zs.room
		chars, unlock := zs.lockAll()
		repChar := chars[gid]
		if repChar == nil {
			unlock()
			return
		}
		now := monoNow()
		fighters := map[uint16]*rules.Fighter{}
		byGID := map[uint32]*rules.Fighter{}
		for g, c := range chars {
			m := zs.members[g]
			f := c.fighter(m.p.r, m.lid)
			fighters[m.lid], byGID[g] = f, f
		}
		b := rules.Battle{R: rep.p.r, Rng: rm.rng, Now: now, Players: fighters, Enemies: rm.enemies, Roster: rm.roster,
			Boxes: rm.boxes, Level: max(1, repChar.st.Level), Self: gid}
		out := b.Resolve(body)

		for _, d := range zs.members {
			for _, e := range out.Fresh {
				d.p.conn.Relay(c0e08Enemy(d, e))
			}
			if len(out.Entries) > 0 {
				d.p.conn.Relay(c0e07Hits(d, out.Entries))
			}
		}
		hpNow := map[*player]int{}
		saved := map[uint32]bool{}
		save := func(g uint32) {
			if !saved[g] {
				saved[g] = true
				zs.saveLocked(zs.members[g], chars[g])
			}
		}
		for _, f := range out.Hurt {
			save(f.GID)
			hpNow[zs.members[f.GID].p] = f.St.HP
		}
		for _, g := range out.Grown {
			if g.Up {
				save(g.F.GID)
			}
			src := zs.members[g.F.GID]
			zs.broadcast(func(d *member) []byte { return c0108(d, g.F.GID, src.lid, g.PA, g.Up) })
		}
		if len(out.Kills) > 0 {
			zs.waveClears()
			if rm.run != nil {
				rm.run.mu.Lock()
				rm.run.run.Kills += len(out.Kills)
				rm.run.mu.Unlock()
			}
		}
		for _, br := range out.Broken {
			zs.boxBroken(br)
		}
		wall := time.Now()
		for _, e := range out.Kills {
			zs.dropFor(e, rep.p.disc, wall)
		}
		for _, e := range out.Kills {
			for pg, amount := range rules.KillEXP(e) {
				m, c, f := zs.members[pg], chars[pg], byGID[pg]
				if m == nil || c == nil || f.St.HP == 0 {
					continue
				}
				amount = int(float64(amount) * zs.w.srv.calendar().Effects(wall, m.p.disc).EXP)
				up := m.p.r.AddEXP(c.raw, c.st, amount)
				save(pg)
				if up { // 0x010D alone, naming the enemy: the client's level-up
					user := m.p.r.User(pg, m.lid, c.raw, c.st, m.area, m.entry, rules.Pos{}, m.p.disc, "")
					m.p.conn.Relay(c010d(m.lid, pg, pg, m.lid, user, e.ID, true))
					hpNow[m.p] = c.st.HP
				} else {
					m.p.conn.Relay(c0115(pg, m.lid, e.ID, rules.LevelBlock(c.st, m.p.disc)))
				}
			}
		}
		unlock()
		for p, hp := range hpNow {
			zs.w.partyHP(p, hp)
		}
		if len(out.Kills) > 0 {
			zs.missionCheck(rep)
		}
	})
}

// waveClears sets the clear flag of each gateway whose enemies are all dead, once.
func (zs *zoneState) waveClears() {
	rm := &zs.room
	for _, g := range rm.gates {
		if rm.gateDone[g.ID] || len(g.Enemies) == 0 || len(g.Flags) == 0 || g.Flags[0] < 0 {
			continue
		}
		all := true
		for _, e := range g.Enemies {
			if en := rm.enemies[e.ID]; en == nil || !en.Dead {
				all = false
				break
			}
		}
		if all {
			rm.gateDone[g.ID] = true
			zs.setFlag(uint32(g.MapIndex*256 + g.Flags[0]))
		}
	}
}

// dropFor rolls a dead enemy's drop (a running event's extra rolls when one gives
// nothing, then the season's event items) and puts it on the ground; disc is the
// reporting player's, whose events apply.
func (zs *zoneState) dropFor(e *rules.Enemy, disc string, now time.Time) {
	rm := &zs.room
	if e.Spec == nil {
		return
	}
	srv := zs.w.srv
	fx := srv.calendar().Effects(now, disc)
	area, _, _ := rm.r.ZoneArea(rm.key.quest, rm.key.zone)
	d, ok := rm.r.RollEnemy(rm.rng, e.Spec.Variant, e.Level, area)
	for i := 1; i < fx.DropRolls && !ok; i++ {
		d, ok = rm.r.RollEnemy(rm.rng, e.Spec.Variant, e.Level, area)
	}
	if !ok {
		d, ok = srv.seasonDrop(rm.rng, rm.r, now, fx.EventDrop)
	}
	if !ok {
		return
	}
	if d.Meseta > 0 {
		d.Meseta = int(float64(d.Meseta) * fx.Meseta)
	}
	where := rm.pos[e.ID]
	if where == nil {
		where = e.KillPos
	}
	zs.drop(d, e.Spec.Map, where)
}

func (zs *zoneState) drop(d rules.Drop, mapID uint32, where []byte) {
	rm := &zs.room
	g, ok := rm.r.GroundItem(zs.w.srv.nextGroundUUID(), d, mapID, fixed(where, 12))
	if !ok {
		return
	}
	if len(rm.ground) >= groundMax { // the client recycles the oldest too
		rm.ground = rm.ground[1:]
	}
	rm.ground = append(rm.ground, g)
	zs.broadcast(func(m *member) []byte { return c0a02Ground(m, rm.r, g) })
}

// boxBroken breaks a box: object state 3, its drop, and its event flag (params +0x20).
func (zs *zoneState) boxBroken(br rules.Broken) {
	rm := &zs.room
	box := br.Box
	state := rm.objects[box.OID] | 3
	rm.objects[box.OID] = state
	var mapID uint32
	if box.MapIndex < len(rm.maps) {
		mapID = rm.maps[box.MapIndex]
	}
	zs.broadcast(func(d *member) []byte { return c1213Object(d, box.OID, state) })
	if len(box.Params) >= 0x22 {
		if ev := int16(binary.LittleEndian.Uint16(box.Params[0x20:])); ev >= 0 {
			zs.setFlag(uint32(box.MapIndex*256 + int(ev)))
		}
	}
	area, level, _ := rm.r.ZoneArea(rm.key.quest, rm.key.zone)
	if len(box.Params) > 3 && box.Params[2] < 0x80 {
		if d, ok := rm.r.RollObject(rm.rng, int(box.Params[2]), int(box.Params[3]), level, area); ok {
			where := br.Pos
			if len(box.Pos) == 3 {
				where = xyz(float32(box.Pos[0]), float32(box.Pos[1]), float32(box.Pos[2]))
			}
			zs.drop(d, mapID, where)
		}
	}
}

// --- statuses ----------------------------------------------------------------------------------------

// statusTick runs due status ticks and expiries of members and enemies.
func (zs *zoneState) statusTick() {
	now := monoNow()
	for _, m := range zs.members {
		c := m.p.char.Load()
		if c == nil {
			continue
		}
		zs.lock(c)
		next, ok := c.status.NextEvent(now)
		if !ok || next > 0 {
			zs.unlock(c)
			continue
		}
		f := c.fighter(m.p.r, m.lid)
		before := c.st.HP
		ticks, hp := c.status.Step(c.st.HP, f.MaxHP(), now)
		c.st.HP = hp
		entries := rules.TickEntries(uint32(m.lid), &c.status, ticks, now)
		se2 := c.status.SE2(now)
		if hp != before {
			zs.saveLocked(m, c)
		}
		zs.unlock(c)
		for _, d := range zs.members {
			if len(entries) > 0 {
				d.p.conn.Relay(c0e07Hits(d, entries))
			}
			d.p.conn.Relay(c0117(d, m.p.gid, m.lid, se2, hp))
		}
		if len(entries) > 0 {
			zs.w.partyHP(m.p, hp)
		}
	}
	for _, e := range zs.room.enemies {
		if e.Dead {
			continue
		}
		if next, ok := e.Status.NextEvent(now); !ok || next > 0 {
			continue
		}
		var ticks []rules.Tick
		ticks, e.HP = e.Status.Step(e.HP, e.MaxHP, now)
		if entries := rules.TickEntries(e.ID, &e.Status, ticks, now); len(entries) > 0 {
			zs.broadcast(func(d *member) []byte { return c0e07Hits(d, entries) })
		}
	}
}

// --- mission joins, objects, pickups -----------------------------------------------------------------

// loaded is C>S 0x0808 in a mission: the join snapshot (conditions ready, the flag sets),
// the ground items, then the start once (startPending).
func (z *zone) loaded(gid uint32, startPending bool) {
	z.do(func(zs *zoneState) {
		m := zs.members[gid]
		if m == nil {
			return
		}
		rm := &zs.room
		a, b := rm.flags.copyAll()
		for _, pkt := range [][]byte{c1202Ready(m), c1204FlagsB(m, b[:]), c1206FlagsA(m, a[:])} {
			m.p.conn.Relay(pkt)
		}
		if len(rm.ground) > 0 {
			m.p.conn.Relay(c0a03Ground(m, rm.r, rm.ground))
		}
		if !startPending || rm.run == nil {
			return
		}
		rm.run.mu.Lock()
		started, t0 := rm.run.started, rm.run.t0
		rm.run.mu.Unlock()
		if started { // a member joining a running mission
			m.p.conn.Relay(c0c09(gid, m.lid, 0, uint32(time.Since(t0).Seconds())))
			return
		}
		zs.missionCheck(m)
	})
}

// object is C>S 0x0F0A in a mission: +0x24 s16 block, group, object, map id, object id;
// +0x34 type; +0x62 event. Key consoles (type 22) set their flags, keys (31) theirs,
// event 12 activates (state bit 0), a healing pad (50, event 9) heals 10 %.
func (z *zone) object(gid uint32, body []byte) {
	z.do(func(zs *zoneState) {
		rep := zs.members[gid]
		if rep == nil {
			return
		}
		rm := &zs.room
		group, obj := int(int16(binary.LittleEndian.Uint16(body[0x26:]))), int(int16(binary.LittleEndian.Uint16(body[0x28:])))
		mapID := uint32(binary.LittleEndian.Uint16(body[0x2A:]))
		oid := int(int16(binary.LittleEndian.Uint16(body[0x2C:])))
		otype, ev := binary.LittleEndian.Uint16(body[0x34:]), body[0x62]
		mi := rm.mapIndex(mapID)
		so, known := rm.set[[3]int{mi, group, obj}]
		p := so.Params
		changed := false
		set := func(e int) {
			zs.setFlag(uint32(mi*256 + e))
			changed = true
		}
		switch {
		case otype == 22 && ev == 23 && known && so.Type == 22 && len(p) > 2:
			set(201 + int(p[2]))
		case otype == 22 && ev == 24 && known && so.Type == 22:
			var evs []int
			for _, o := range []int{0x1C, 0x1E} {
				if len(p) >= o+2 {
					if e := int(int16(binary.LittleEndian.Uint16(p[o:]))); e >= 0 {
						evs = append(evs, e)
					}
				}
			}
			if len(evs) == 0 && len(p) > 2 {
				evs = []int{243 + int(p[2])}
			}
			for _, e := range evs {
				set(e)
			}
		}
		if otype == 31 && ev == 12 && known && so.Type == 31 && len(p) >= 8 { // a key picked up
			set(int(int16(binary.LittleEndian.Uint16(p[6:]))))
		}
		if ev == 12 || (otype == 49 && ev == 3) { // activate (any type); a crystal
			st := rm.objects[oid] | 1
			rm.objects[oid] = st
			zs.broadcast(func(d *member) []byte { return c1213Object(d, oid, st) })
			changed = true
		}
		if otype == 50 && ev == 9 { // healing pad tick: +10 % max HP
			if c := rep.p.char.Load(); c != nil {
				zs.lock(c)
				mx := rep.p.r.Derived(c.raw, c.st).MaxHP
				healed := c.st.HP < mx
				if healed {
					c.st.HP = min(mx, c.st.HP+mx/10)
					zs.saveLocked(rep, c)
				}
				hp, se2 := c.st.HP, c.status.SE2(monoNow())
				zs.unlock(c)
				if healed {
					rep.p.conn.Relay(c0117(rep, gid, rep.lid, se2, hp))
					rep.p.conn.Relay(c0111(rep.lid, gid, gid, rep.lid, 4, 0))
					zs.w.partyHP(rep.p, hp)
				}
			}
		}
		if changed {
			zs.missionCheck(rep)
		}
	})
}

// pickup is C>S 0x0106 event 6 (+0x34 the ground item's UUID): first picker wins.
func (z *zone) pickup(gid uint32, uuid uint32) {
	z.do(func(zs *zoneState) {
		m := zs.members[gid]
		if m == nil {
			return
		}
		c, rm := m.p.char.Load(), &zs.room
		if c == nil {
			return
		}
		idx := -1
		for i, g := range rm.ground {
			if g.UUID == uuid {
				idx = i
			}
		}
		if idx < 0 {
			return
		}
		g := rm.ground[idx]
		zs.lock(c)
		var fail uint8
		if g.Meseta {
			c.st.Meseta = uint32(min(int64(c.st.Meseta)+int64(g.Qty), 99_999_999))
		} else if _, err := m.p.r.AddItem(c.st, g.ID, g.Qty, g.UUID); err != nil {
			fail = 1
		}
		if fail == 0 {
			zs.saveLocked(m, c)
		}
		block := rules.LevelBlock(c.st, m.p.disc)
		zs.unlock(c)
		if fail != 0 {
			m.p.conn.Relay(c0106Pickup(m, rm.r, gid, m.lid, g, fail))
			return
		}
		rm.ground = append(rm.ground[:idx], rm.ground[idx+1:]...)
		if g.Meseta {
			m.p.conn.Relay(c0115(gid, m.lid, noSource, block))
		}
		zs.broadcast(func(d *member) []byte { return c0106Pickup(d, rm.r, gid, m.lid, g, 0) })
	})
}

// death counts a mission death for the rank.
func (z *zone) death() {
	z.do(func(zs *zoneState) {
		if rm := &zs.room; rm.run != nil {
			rm.run.mu.Lock()
			rm.run.run.Deaths++
			rm.run.mu.Unlock()
		}
	})
}

// --- the judge ---------------------------------------------------------------------------------------

// recordClear stores a clear for the board: the members here with a character.
func (zs *zoneState) recordClear(rep *member, elapsed, rank, score int) {
	rm := &zs.room
	c := store.Clear{At: time.Now(), Game: rep.p.disc, Uni: rm.key.uni, Quest: rm.key.quest, Secs: elapsed,
		Rank: rank, Score: score}
	rm.run.mu.Lock()
	c.Kills = rm.run.run.Kills
	rm.run.mu.Unlock()
	for _, m := range zs.members {
		if snap, ch := m.p.snap.Load(), m.p.char.Load(); snap != nil && ch != nil {
			c.Party = append(c.Party, store.Member{GID: m.p.gid, Slot: ch.slot, Name: rules.UTF16Text(snap.raw[rules.RawName:rules.RawRace])})
		}
	}
	srv := zs.w.srv
	go srv.guard("record clear", func() {
		if err := srv.Store.RecordClear(srv.ctx(), c); err != nil {
			srv.Log.Error("record clear", "err", err)
		}
	})
}

// missionCheck starts the mission when its start rule holds and clears it when its clear
// rule does: rank, rewards and results to every member here.
func (zs *zoneState) missionCheck(rep *member) {
	rm := &zs.room
	if rm.run == nil || rm.mission.Quest == nil {
		return
	}
	q, flag, at := rm.mission.Quest, rm.flagFn(), rep.area
	rm.run.mu.Lock()
	started := rm.run.started
	rm.run.mu.Unlock()
	if !started && (q.Start == nil || rules.Evaluate(q.Start, flag, nil, &at)) {
		rm.run.mu.Lock()
		rm.run.started, rm.run.t0, started = true, time.Now(), true
		rm.run.mu.Unlock()
		zs.broadcast(func(d *member) []byte { return c0c09(d.p.gid, d.lid, 0, 0) })
	}
	if !started || !rules.Evaluate(q.Clear, flag, nil, &at) { // flag takes run.mu: evaluate without it
		return
	}
	zs.finishMission(rep)
}

// forceClear starts and clears gid's mission at once (GM !clear).
func (z *zone) forceClear(gid uint32) {
	z.do(func(zs *zoneState) {
		rm := &zs.room
		rep := zs.members[gid]
		if rep == nil || rm.run == nil || rm.mission.Quest == nil {
			return
		}
		rm.run.mu.Lock()
		if !rm.run.started {
			rm.run.started, rm.run.t0 = true, time.Now()
		}
		rm.run.mu.Unlock()
		zs.finishMission(rep)
	})
}

// finishMission clears the run once: rank, rewards and results to every member here.
func (zs *zoneState) finishMission(rep *member) {
	rm := &zs.room
	q, flag := rm.mission.Quest, rm.flagFn()
	rm.run.mu.Lock()
	if rm.run.cleared {
		rm.run.mu.Unlock()
		return
	}
	rm.run.cleared = true
	elapsed := int(time.Since(rm.run.t0).Seconds())
	run := rm.run.run
	rm.run.mu.Unlock()

	rank, total, secs := rules.RankOf(q, elapsed, run, flag)
	got := rules.Rewards(q, rank, flag)
	zs.recordClear(rep, elapsed, rank, total)
	now := time.Now()
	tp := 0
	if q.Rank != nil && rank < len(q.Rank.TypePoints) {
		tp = q.Rank.TypePoints[rank]
	}
	for _, m := range zs.members {
		c := m.p.char.Load()
		if c == nil {
			continue
		}
		r := m.p.r
		zs.lock(c)
		meseta := 0
		var items []rules.Item
		var adds [][]byte
		for _, rw := range got {
			var id rules.ItemID
			if rw.Flags&4 != 0 || rw.Item == "ffffffff" || id.UnmarshalText([]byte(rw.Item)) != nil {
				meseta += int(float64(rw.Meseta) * zs.w.srv.calendar().Effects(now, m.p.disc).Meseta)
				continue
			}
			if _, ok := r.Item(id); !ok {
				continue
			}
			if added, err := r.AddItem(c.st, id, max(1, rw.Qty), 0); err == nil {
				items = append(items, added)
				adds = append(adds, c0106Add(r, m.p.disc, m.p.gid, m.lid, added))
			}
		}
		c.st.Meseta = uint32(min(int64(c.st.Meseta)+int64(meseta), 999_999_999))
		oldTL, newTL := r.AddTypeEXP(c.st, tp)
		zs.saveLocked(m, c)
		out := [][]byte{c0c09Results(m.p.gid, m.lid, uint32(elapsed), r.ResultBlock(rank, total, secs, meseta, items))}
		out = append(out, adds...)
		out = append(out, c0115(m.p.gid, m.lid, noSource, rules.LevelBlock(c.st, m.p.disc)))
		if newTL != oldTL {
			user := r.User(m.p.gid, m.lid, c.raw, c.st, m.area, m.entry, rules.Pos{}, m.p.disc, "")
			out = append(out, c010d(m.lid, m.p.gid, m.p.gid, m.lid, user, noSource, true))
		}
		zs.unlock(c)
		for _, pkt := range out {
			m.p.conn.Relay(pkt)
		}
	}
}
