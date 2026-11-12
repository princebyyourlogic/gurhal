// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import "encoding/binary"

// Hits (C>S 0x0E00): +0x24 count, +0x28 sender, +0x2C sequence, then 0x50-byte hits: +0
// xyz, +0xC attacker, +0x10 target, +0x14 attack id, +0x18 opaque, +0x1C block B, +0x2C
// block C, +0x3C attack block D. Results (S>C 0x0E07) are 0x68-byte entries: 1, flag2,
// damage, xyz, attacker, target, the two opaque words, attacker SE1 SE2 HP + barrier,
// target SE1 SE2 HP + barrier, B, D. Ids below 6 are players' LIDs; anything else is an
// enemy (or a breakable box).

const maxHits = 78

// Box is a breakable box of a mission zone (type 12): map index, set params, hits taken,
// its object id and where its drop lands.
type Box struct {
	MapIndex int
	Params   []byte
	Hits     int
	OID      int
	Pos      []float64
}

// Battle is one 0x0E00 batch: everything it may touch. The caller holds the players'
// character locks.
type Battle struct {
	R       *Rules // the reporting player's disc
	Rng     Rand
	Now     float64
	Players map[uint16]*Fighter // by LID
	Enemies map[uint32]*Enemy
	Roster  map[uint32]*EnemySpec
	Boxes   map[uint32]*Box
	Level   int    // the reporter's level: enemies outside the set data get that row
	Self    uint32 // the reporter's GID (kills without a player attacker)
}

type Growth struct {
	F *Fighter
	PAGrowth
}

type Broken struct {
	ID  uint32
	Box *Box
	Pos []byte
}

// Outcome is what a batch did.
type Outcome struct {
	Entries  [][]byte
	Fresh    []*Enemy // enemies first seen in this batch (their 0x0E08 goes out first)
	Kills    []*Enemy
	Hurt     []*Fighter // players whose HP changed
	Grown    []Growth
	Broken   []Broken
	Buffed   []*Fighter // players with a new status (expiry watch)
	Statused []*Enemy   // enemies with a new status, still alive
}

func le32(b []byte, o int) uint32 { return binary.LittleEndian.Uint32(b[o:]) }

func actorBlock(se1, se2 uint32, hp int, barrier [2]int) []byte {
	b := binary.LittleEndian.AppendUint32(nil, se1)
	b = binary.LittleEndian.AppendUint32(b, se2)
	b = binary.LittleEndian.AppendUint32(b, uint32(hp))
	return append(b, byte(barrier[0]), byte(barrier[1]), 0, 0)
}

func entry(flag2 byte, dmg int, h []byte, a, t []byte, d []byte) []byte {
	e := []byte{1, flag2, 0, 0}
	e = binary.LittleEndian.AppendUint32(e, uint32(dmg))
	e = append(e, h[0:0xC]...)    // position
	e = append(e, h[0xC:0x1C]...) // attacker, target, the two opaque words
	e = append(append(e, a...), t...)
	return append(append(e, h[0x1C:0x2C]...), d...)
}

func (b *Battle) enemy(id uint32, out *Outcome) *Enemy {
	e := b.R.NewEnemy(id, b.Roster[id], b.Level)
	b.Enemies[id] = e
	out.Fresh = append(out.Fresh, e)
	return e
}

// Resolve resolves one 0x0E00 body.
func (b *Battle) Resolve(body []byte) Outcome {
	var out Outcome
	statused := map[*Enemy]bool{}
	var statusedOrder []*Enemy
	addStatused := func(e *Enemy) {
		if !statused[e] {
			statused[e] = true
			statusedOrder = append(statusedOrder, e)
		}
	}
	now, disc := b.Now, b.R.disc
	count := int(le32(body, 0x24))
	for i := range min(count, maxHits) {
		o := 0x30 + 0x50*i
		if o+0x50 > len(body) {
			break
		}
		h := body[o : o+0x50]
		src, dst, w := le32(h, 0xC), le32(h, 0x10), le32(h, 0x14)
		d := h[0x3C:0x50]
		ap := b.player(src)
		tp := b.player(dst)
		if ap != nil {
			if sup, ok := ap.supportAttack(w, d, now); ok { // support technique: players only, no damage
				if tp == nil {
					continue
				}
				amount, se, ok := tp.applySupport(sup, now)
				if !ok {
					continue
				}
				out.Hurt = append(out.Hurt, tp)
				out.Buffed = append(out.Buffed, tp)
				if se&0x10C0000 != 0 {
					if g, ok := ap.PAHit(w); ok {
						out.Grown = append(out.Grown, Growth{ap, g})
					}
				}
				aSE := uint32(0)
				if ap == tp {
					aSE = se
				}
				out.Entries = append(out.Entries, entry(0, amount, h,
					actorBlock(aSE, ap.Status.SE2(now), ap.HP(), [2]int{}),
					actorBlock(se, tp.Status.SE2(now), tp.HP(), [2]int{}), sup.d))
				continue
			}
		}
		if tp != nil && ap != nil && tp.HP() == 0 {
			heal, ok := ap.reviveAmount(w, tp)
			if !ok {
				continue
			}
			tp.St.HP = heal
			out.Hurt = append(out.Hurt, tp)
			out.Entries = append(out.Entries, entry(0, heal, h,
				actorBlock(0, ap.Status.SE2(now), ap.HP(), [2]int{}),
				actorBlock(0x40000, tp.Status.SE2(now), heal, [2]int{}), d))
			continue
		}
		if tp != nil && ap != nil {
			if heal, ok := ap.healAmount(w, tp); ok { // a healing item on a player
				hp := min(tp.MaxHP(), tp.HP()+heal)
				tp.St.HP = hp
				out.Hurt = append(out.Hurt, tp)
				out.Entries = append(out.Entries, entry(0, heal, h,
					actorBlock(0, ap.Status.SE2(now), ap.HP(), [2]int{}),
					actorBlock(0x80000, tp.Status.SE2(now), hp, [2]int{}), d))
				continue
			}
			if se, d2, ok := ap.itemStatus(b.Rng, w, tp, now); ok { // cure / status item
				out.Hurt = append(out.Hurt, tp)
				out.Buffed = append(out.Buffed, tp)
				aSE := uint32(0)
				if ap == tp {
					aSE = se
				}
				out.Entries = append(out.Entries, entry(0, 0, h,
					actorBlock(aSE, ap.Status.SE2(now), ap.HP(), ap.Status.Barrier),
					actorBlock(se, tp.Status.SE2(now), tp.HP(), tp.Status.Barrier), d2))
				continue
			}
		}
		if ap != nil {
			if key, level, _, ok := ap.techLink(w); ok && (debuffTechs[key] || barrierTechs[key]) {
				if barrierTechs[key] != (tp != nil) { // kind 3 on enemies, kind 2 on players
					continue
				}
				var t Target
				var st *Status
				var tHP int
				var en *Enemy
				if tp != nil {
					t, st = tp, tp.Status
				} else {
					en = b.Enemies[dst]
					if en == nil {
						if _, box := b.Boxes[dst]; box {
							continue
						}
						en = b.enemy(dst, &out)
					}
					if en.Dead {
						continue
					}
					t, st = en, &en.Status
				}
				d2 := ap.statusAttack(key, level, d)
				var sta int
				var immune uint32
				if tp != nil {
					sta, immune = tp.TotalStats()[4], tp.Equipment().Immune
				} else {
					sta = en.Stats[4]
				}
				got := st.Inflict(b.Rng, le32(d2, 4), int(d2[0x12]), s16(d2, 0xE), sta, le32(d2, 0)&0x8000 != 0, immune, now)
				if got == 0 {
					continue
				}
				if tp != nil {
					out.Buffed = append(out.Buffed, tp)
					tHP = tp.HP()
				} else {
					addStatused(en)
					en.Hitters[ap.GID] = true
					tHP = en.HP
				}
				if g, ok := ap.PAHit(w); ok {
					out.Grown = append(out.Grown, Growth{ap, g})
				}
				aSE := uint32(0)
				if tp != nil && ap == tp {
					aSE = 0x800000
				}
				out.Entries = append(out.Entries, entry(0, 0, h,
					actorBlock(aSE, ap.Status.SE2(now), ap.HP(), ap.Status.Barrier),
					actorBlock(0x800000, t.status().SE2(now), tHP, t.status().Barrier), d2))
				continue
			}
		}
		var dmg, aHP, tHP int
		var se, aSE uint32
		var e *Enemy
		var attacker, target Target
		if tp != nil { // an enemy (or PvP) hitting a player
			e = b.Enemies[src]
			if e == nil && ap == nil {
				e = b.enemy(src, &out)
			}
			if e != nil && e.Status.SE2(now)&0x7C000 != 0 {
				d = debuffedAttack(&e.Status, d, now)
			}
			stats := tp.TotalStats()
			var est *[8]int
			if e != nil {
				est = &e.Stats
			}
			dmg, se = Damage(b.Rng, d, buffedStats(tp.Status, stats, now), tp.HP(), est, tp, disc, now)
			hp := max(0, tp.HP()-dmg)
			var gotStatus bool
			se, hp, dmg, gotStatus = afterHit(b.Rng, tp.Status, d, se, hp, dmg, stats[4], tp.Equipment().Immune, now)
			if gotStatus {
				out.Buffed = append(out.Buffed, tp)
			}
			tp.St.HP = hp
			if hp == 0 {
				se |= 0x20000
				tp.Status.Clear()
			}
			out.Hurt = append(out.Hurt, tp)
			if e != nil {
				aHP = e.HP
			}
			if ap != nil {
				attacker = ap
			} else if e != nil {
				attacker = e
			}
			target = tp
			tHP = hp
		} else { // a player hitting an enemy
			e = b.Enemies[dst]
			if e == nil {
				if box := b.Boxes[dst]; box != nil { // a breakable box (TServerActorObjBreak)
					box.Hits++
					need := 1
					if len(box.Params) > 4 {
						need = max(1, int(box.Params[4]))
					}
					if box.Hits == need {
						out.Broken = append(out.Broken, Broken{dst, box, append([]byte(nil), h[0:0xC]...)})
					}
					continue
				}
				e = b.enemy(dst, &out)
			}
			if e.Dead {
				continue
			}
			if ap != nil {
				d = ap.playerAttack(w, d, now)
			}
			dmg, se = Damage(b.Rng, d, e.stats(now), e.HP, nil, e, disc, now)
			hp := max(0, e.HP-dmg)
			var gotStatus bool
			se, hp, dmg, gotStatus = afterHit(b.Rng, &e.Status, d, se, hp, dmg, e.Stats[4], 0, now)
			if gotStatus {
				addStatused(e)
			}
			e.HP = hp
			if ap != nil {
				e.Hitters[ap.GID] = true
				if dmg > 0 {
					if g, ok := ap.PAHit(w); ok {
						out.Grown = append(out.Grown, Growth{ap, g})
					}
				}
			}
			if e.HP == 0 {
				se |= 0x20000
				e.Dead = true
				e.Status.Clear()
				e.Killer = b.Self
				if ap != nil {
					e.Killer = ap.GID
				}
				e.KillPos = append([]byte(nil), h[0:0xC]...)
				out.Kills = append(out.Kills, e)
			}
			if ap != nil {
				aHP = ap.HP()
				attacker = ap
			}
			target = e
			tHP = e.HP
		}
		var flag2 byte
		if le32(d, 0)&0x80000 != 0 {
			flag2 = d[0x13]
		}
		var aSE2 uint32
		var aBar, tBar [2]int
		if attacker != nil {
			aSE2, aBar = attacker.status().SE2(now), attacker.status().Barrier
		}
		tSE2, tBar := target.status().SE2(now), target.status().Barrier
		out.Entries = append(out.Entries, entry(flag2, dmg, h,
			actorBlock(aSE, aSE2, aHP, aBar), actorBlock(se, tSE2, tHP, tBar), d))
	}
	for _, e := range statusedOrder {
		if !e.Dead {
			out.Statused = append(out.Statused, e)
		}
	}
	return out
}

func (b *Battle) player(id uint32) *Fighter {
	if id > 0xFFFF {
		return nil
	}
	return b.Players[uint16(id)]
}

// debuffedAttack applies an enemy's own debuffs to the block its client built: power x
// slot 0 (2 for techniques), accuracy x slot 3.
func debuffedAttack(s *Status, d []byte, now float64) []byte {
	flags := le32(d, 0)
	out := append([]byte(nil), d...)
	binary.LittleEndian.PutUint16(out[8:], uint16(int16(scaled(s16(d, 8), s.buffMult(3, now)))))
	slot := 0
	if flags&0xC == 0xC {
		slot = 2
	}
	binary.LittleEndian.PutUint16(out[0xC:], uint16(int16(scaled(s16(d, 0xC), s.buffMult(slot, now)))))
	return out
}

// afterHit (0x1c0350): a landed damaging hit counts down hit-ended statuses; then a block
// with 0x2000 tries its status (instant death takes the rest of the HP).
func afterHit(rng Rand, s *Status, d []byte, se uint32, hp, dmg, sta int, immune uint32, now float64) (uint32, int, int, bool) {
	flags, bits := le32(d, 0), le32(d, 4)
	landed := dmg > 0 && se&0x1 != 0
	if landed && flags&0x100000 == 0 {
		s.breakOnHit()
	}
	got := false
	if flags&0x2000 != 0 && bits != 0 && hp > 0 && (landed || (flags&0x200 != 0 && flags&0x1000 == 0)) {
		if g := s.Inflict(rng, bits, int(d[0x12]), s16(d, 0xE), sta, flags&0x8000 != 0, immune, now); g != 0 {
			se |= 0x800000
			got = true
			if g&0x400 != 0 {
				dmg, hp = dmg+hp, 0
			}
		}
	}
	return se, hp, dmg, got
}
