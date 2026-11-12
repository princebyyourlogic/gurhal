// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/binary"
	"fmt"
)

// Fighter is a player in combat: their character and live status. The caller holds the
// character's lock for as long as it uses one.
type Fighter struct {
	R         *Rules // the player's own disc
	GID       uint32
	LID       uint16
	Raw       []byte
	St        *State
	Status    *Status
	RecentUse map[uint32]Item // items just used (0x0105 use): a hit may still name them
}

func (f *Fighter) status() *Status { return f.Status }

func (f *Fighter) defenceElement() (uint8, uint8) {
	eq := f.Equipment()
	return eq.Element, eq.Pct
}

// HP is the player's current HP.
func (f *Fighter) HP() int { return f.St.HP }

func (f *Fighter) MaxHP() int { return f.R.Derived(f.Raw, f.St).MaxHP }

// item finds an inventory slot by attack id (low 29 bits = UUID), recently used ones too.
func (f *Fighter) item(attackID uint32) *Item {
	uuid := attackID & 0x1FFFFFFF
	for i := range f.St.Inv {
		if f.St.Inv[i].UUID == uuid {
			return &f.St.Inv[i]
		}
	}
	if it, ok := f.RecentUse[uuid]; ok {
		return &it
	}
	return nil
}

func (f *Fighter) invItem(attackID uint32) *Item {
	uuid := attackID & 0x1FFFFFFF
	for i := range f.St.Inv {
		if f.St.Inv[i].UUID == uuid {
			return &f.St.Inv[i]
		}
	}
	return nil
}

// --- equipment (line shield + units, 0x263db0) -------------------------------------------------------

type Equipment struct {
	Stats   [8]int
	Element uint8
	Pct     uint8
	Immune  uint32
}

var (
	unitStats  = map[uint8][2]int{1: {6, 7}, 2: {0, 2}, 3: {1, 3}, 4: {4, 5}}
	armorStats = [7][2]int{{4, 0}, {6, 6}, {8, 1}, {0xA, 7}, {0xC, 2}, {0xE, 3}, {0x10, 4}}
	immuneBits = []struct {
		off   int
		masks []uint32
	}{{0x9, []uint32{0x1, 0x2, 0x4, 0x8, 0x10}}, {0xA, []uint32{0x20, 0x40, 0x80, 0x100}}, {0xB, []uint32{0x400, 0xF80000, 0x7C000}}}
)

func s16(b []byte, o int) int { return int(int16(binary.LittleEndian.Uint16(b[o:]))) }

// Equipment is what the equipped line shield and units add, the shield's element and
// the type-3 unit's immunities.
func (f *Fighter) Equipment() Equipment {
	var out Equipment
	var unit3 []byte
	for _, s := range f.St.Inv {
		if s.Active == 0 {
			continue
		}
		it, ok := f.R.items[s.ID]
		if !ok || (it.Cat != 2 && it.Cat != 8) || len(it.Rec) == 0 {
			continue
		}
		r := it.Rec
		if it.Cat == 2 && len(r) >= 0x12 {
			for _, a := range armorStats {
				out.Stats[a[1]] += s16(r, a[0])
			}
			e := r[1] & 0xF
			if f.R.disc == AotI {
				e = r[1]
			}
			if e >= 1 && e <= 6 {
				out.Element, out.Pct = elementCode[e], r[2]
			}
		} else if it.Cat == 8 && len(r) >= 0xC {
			k, ok := unitStats[s.ID[1]]
			if !ok {
				continue
			}
			out.Stats[k[0]] += s16(r, 4)
			out.Stats[k[1]] += s16(r, 6)
			if s.ID[1] == 3 {
				unit3 = r
			}
		}
	}
	if unit3 != nil {
		if out.Element != 0 {
			out.Pct = uint8(max(0, min(127, int(out.Pct)+int(int8(unit3[8])))))
		}
		for _, ib := range immuneBits {
			for i, m := range ib.masks {
				if unit3[ib.off]&(1<<i) != 0 {
					out.Immune |= m
				}
			}
		}
	}
	return out
}

// TotalStats is the derived stats plus equipment.
func (f *Fighter) TotalStats() [8]int {
	base := f.R.Derived(f.Raw, f.St).Stats
	eq := f.Equipment().Stats
	if eq == [8]int{} {
		return base
	}
	for i := range base {
		base[i] = max(0, min(0x7FFF, base[i]+eq[i]))
	}
	return base
}

// buffedStats are defender stats with buffs (0x5ecb0: DFP x slot 1, MST x 2, EVP x 3).
func buffedStats(s *Status, stats [8]int, now float64) [8]int {
	if s.SE2(now) == 0 {
		return stats
	}
	for _, p := range [3][2]int{{1, 1}, {7, 2}, {3, 3}} {
		stats[p[0]] = scaled(stats[p[0]], s.buffMult(p[1], now))
	}
	return stats
}

// --- PA keys and growth ------------------------------------------------------------------------------

func techKey(code int) ItemID { return ItemID{6, byte((code-1)/10 + 1), byte((code - 1) % 10), 0} }

func techCode(id ItemID) int { return (int(id[1])-1)*10 + int(id[2]) + 1 }

func (r *Rules) style(s *Item) (byte, bool) {
	it, ok := r.items[s.ID]
	if !ok || it.Cat != catWeapon {
		return 0, false
	}
	return it.Consts[0xE], true
}

// TechWeapon reports a technique weapon (constants style 3: rods, wands).
func (r *Rules) TechWeapon(s *Item) bool {
	st, ok := r.style(s)
	return ok && st == techStyle
}

// weaponKey is the PA (3 bytes, level 0) a hit with word w grows, or false.
func (r *Rules) weaponKey(s *Item, w uint32) (ItemID, bool) {
	style, ok := r.style(s)
	if !ok {
		return ItemID{}, false
	}
	wtype, idx := s.ID[1], byte(s.PA[0])
	switch {
	case style == 0 && w>>29 != 0:
		return ItemID{4, wtype, idx, 0}, true
	case (style == 1 || style == 2) && idx != 0:
		return ItemID{5, wtype, idx, 0}, true
	case style == techStyle:
		t := int(w >> 29)
		if t < len(s.Techs) && s.Techs[t][0] >= 1 && s.Techs[t][0] <= 0x38 {
			return techKey(s.Techs[t][0]), true
		}
	}
	return ItemID{}, false
}

// paCap is the top level byte: one per threshold, else the disc's longest (PSU 29).
func (r *Rules) paCap(thr []int) int {
	if len(thr) > 0 {
		return len(thr)
	}
	return r.paTop
}

// PAGrowth is a PA that grew: its list entry after the hit and whether it levelled.
type PAGrowth struct {
	PA PA
	Up bool
}

// PAHit is one landed hit with word w (0x296180): +1 PA EXP to the PA the weapon uses,
// levelling at its threshold; every weapon linked to it then shows the new level.
func (f *Fighter) PAHit(w uint32) (PAGrowth, bool) {
	s := f.invItem(w)
	if s == nil {
		return PAGrowth{}, false
	}
	key, ok := f.R.weaponKey(s, w)
	if !ok {
		return PAGrowth{}, false
	}
	var e *PA
	for i := range f.St.PAs {
		if p := f.St.PAs[i].ID; p[0] == key[0] && p[1] == key[1] && p[2] == key[2] {
			e = &f.St.PAs[i]
			break
		}
	}
	if e == nil {
		return PAGrowth{}, false
	}
	level := int(e.ID[3])
	thr := f.R.paThresholds[key]
	if level >= f.R.paCap(thr) {
		return PAGrowth{}, false
	}
	e.EXP++
	up := level < len(thr) && int(e.EXP) >= thr[level]
	if up {
		level++
		e.ID[3] = byte(level)
		for i := range f.St.Inv {
			x := &f.St.Inv[i]
			if key[0] == 6 {
				for t := range x.Techs {
					if x.Techs[t][0] != 0 && techKey(x.Techs[t][0]) == key {
						x.Techs[t][1] = level
					}
				}
			} else if x.PA != [2]int{} {
				if k, ok := f.R.weaponKey(x, 1<<29); ok && k == key {
					x.PA = [2]int{x.PA[0], level}
				}
			}
		}
	}
	return PAGrowth{*e, up}, true
}

// techLink is the technique in slot w >> 29 of the technique weapon w names.
func (f *Fighter) techLink(w uint32) (key ItemID, level int, techs [][2]int, ok bool) {
	s := f.invItem(w)
	if s == nil || !f.R.TechWeapon(s) {
		return
	}
	t := int(w >> 29)
	if t >= len(s.Techs) || s.Techs[t][0] < 1 || s.Techs[t][0] > 0x38 {
		return
	}
	return techKey(s.Techs[t][0]), s.Techs[t][1], s.Techs, true
}

// --- attack blocks -----------------------------------------------------------------------------------

type supportTier struct{ pow, inc, rflags, effect, strength int }

// supportTechs are the techniques with a non-damaging block: (kind, three tiers).
var supportTechs = map[ItemID]struct {
	kind  int
	tiers [3]supportTier
}{
	{6, 1, 5, 0}: {2, [3]supportTier{{500, 10, 0, 0x11, 1}, {700, 10, 0, 0x11, 2}, {900, 10, 0, 0x11, 3}}},       // Shifta
	{6, 2, 5, 0}: {2, [3]supportTier{{500, 20, 0, 0x13, 1}, {700, 20, 0, 0x13, 2}, {900, 10, 0, 0x13, 3}}},       // Deband
	{6, 3, 5, 0}: {2, [3]supportTier{{500, 20, 0, 0x17, 1}, {700, 20, 0, 0x17, 2}, {900, 10, 0, 0x17, 3}}},       // Zodial
	{6, 5, 0, 0}: {1, [3]supportTier{{101, 1, 0, 0, 0}, {126, 1, 0, 0, 0}, {151, 1, 0, 0, 0}}},                   // Resta
	{6, 5, 1, 0}: {1, [3]supportTier{{151, 1, 0x40, 0x10, 1}, {176, 1, 0x40, 0x10, 2}, {201, 1, 0x40, 0x10, 3}}}, // Giresta
	{6, 5, 2, 0}: {1, [3]supportTier{{0, 0, 0x20, 0, 0}, {0, 0, 0x20, 0, 0}, {0, 0, 0x20, 0, 0}}},                // Reverser
	{6, 5, 6, 0}: {2, [3]supportTier{{500, 10, 0, 0x15, 1}, {700, 10, 0, 0x15, 2}, {900, 10, 0, 0x15, 3}}},       // Retier
	{6, 6, 5, 0}: {2, [3]supportTier{{500, 10, 0, 0x1A, 1}, {700, 10, 0, 0x1A, 2}, {900, 10, 0, 0x1A, 3}}},       // Megistar
}

var effectBits = map[int]uint32{0x10: 0x1000, 0x11: 0x80000, 0x13: 0x100000, 0x15: 0x200000, 0x17: 0x400000, 0x1A: 0x802000}

func cureBits(disc string) uint32 {
	if disc == AotI {
		return 0x2003C3FF
	}
	return 0x3C3FF
}

func damagingBits(disc string) uint32 {
	if disc == AotI {
		return 0x3807C3FF
	}
	return 0x1807C3FF
}

// techFX is the (effect, strength) per tier of attack techniques, enemy debuffs (kind 3)
// and Dizas / Rentis barriers (kind 2).
var techFX = map[ItemID][3][2]int{
	{6, 1, 0, 0}: {{1, 2}, {1, 3}, {1, 4}}, {6, 1, 1, 0}: {{1, 1}, {1, 2}, {1, 3}}, {6, 1, 2, 0}: {{1, 3}, {1, 3}, {1, 3}},
	{6, 1, 3, 0}: {{1, 2}, {1, 3}, {1, 4}}, {6, 2, 0, 0}: {{6, 2}, {6, 2}, {6, 2}}, {6, 2, 1, 0}: {{6, 2}, {6, 3}, {6, 4}},
	{6, 2, 2, 0}: {{6, 3}, {6, 3}, {6, 3}}, {6, 2, 3, 0}: {{6, 2}, {6, 3}, {6, 4}}, {6, 3, 0, 0}: {{4, 2}, {4, 2}, {4, 2}},
	{6, 3, 1, 0}: {{4, 2}, {4, 3}, {4, 4}}, {6, 3, 2, 0}: {{4, 2}, {4, 3}, {4, 4}}, {6, 3, 3, 0}: {{4, 2}, {4, 3}, {4, 4}},
	{6, 4, 0, 0}: {{5, 2}, {5, 3}, {5, 4}}, {6, 4, 2, 0}: {{2, 3}, {2, 3}, {2, 3}}, {6, 4, 4, 0}: {{8, 2}, {8, 2}, {8, 2}},
	{6, 6, 0, 0}: {{11, 2}, {11, 3}, {11, 4}}, {6, 6, 1, 0}: {{9, 2}, {9, 3}, {9, 4}}, {6, 6, 2, 0}: {{3, 2}, {3, 2}, {3, 2}},
	{6, 1, 6, 0}: {{18, 1}, {18, 2}, {18, 3}}, {6, 2, 6, 0}: {{20, 1}, {20, 2}, {20, 3}}, {6, 3, 6, 0}: {{24, 1}, {24, 2}, {24, 3}},
	{6, 4, 5, 0}: {{27, 2}, {27, 3}, {27, 4}}, {6, 5, 5, 0}: {{28, 2}, {28, 3}, {28, 4}},
}

var (
	debuffTechs  = map[ItemID]bool{{6, 1, 6, 0}: true, {6, 2, 6, 0}: true, {6, 3, 6, 0}: true} // Jellen, Zalure, Zoldeel
	barrierTechs = map[ItemID]bool{{6, 4, 5, 0}: true, {6, 5, 5, 0}: true}                     // Dizas, Rentis
)

// support is a support technique's block for a hit (0x262600 + 0xb4fd0).
type support struct {
	d     []byte
	flags uint32
	bits  uint32
	power int
	level int
}

func (f *Fighter) supportAttack(w uint32, d []byte, now float64) (support, bool) {
	key, level, _, ok := f.techLink(w)
	if !ok {
		return support{}, false
	}
	st, ok := supportTechs[key]
	if !ok {
		return support{}, false
	}
	tier := min(level/10, len(st.tiers)-1)
	t := st.tiers[tier]
	pct := max(0, min(0x7FFF, t.pow+t.inc*(level-10*tier)))
	flags, bits, lvl := uint32(0xC), uint32(0), 0
	if st.kind == 1 && key[1] == 5 {
		if t.rflags&0x40 != 0 {
			flags |= 0x800
		}
		if t.rflags&0x20 != 0 {
			flags |= 0x2400
			bits = cureBits(f.R.disc)
		}
		if pct > 0 {
			flags |= 0x1400
			if t.rflags&4 != 0 {
				flags |= 0x20000
			}
		}
	} else if st.kind != 1 {
		pct = 0
	}
	if t.strength >= 1 && t.strength <= 5 {
		flags |= 0x2000
		bits = effectBits[t.effect]
		if bits&0xF80000 != 0 {
			flags |= 0x2400
		} else if bits&damagingBits(f.R.disc) != 0 {
			flags |= 0x200
		}
		lvl = t.strength - 1
	}
	flags |= 0x4000
	if t.rflags&2 != 0 {
		flags |= 0x10000
	}
	if t.rflags&1 != 0 {
		flags |= 0x8000
	}
	if flags&0x400 == 0 || flags&0x200 != 0 {
		return support{}, false
	}
	power := 0
	if pct != 0 {
		s := f.invItem(w)
		it := f.R.items[s.ID]
		base := f.TotalStats()[6] + it.ATP + s.ATPBonus
		power = scaled(int(f32(f32(float64(base*pct))/100.0)), f.Status.buffMult(2, now))
		power = max(0, min(0x7FFF, power))
	}
	out := append([]byte(nil), d...)
	binary.LittleEndian.PutUint32(out, flags)
	binary.LittleEndian.PutUint32(out[4:], bits)
	binary.LittleEndian.PutUint16(out[0xA:], 0)
	binary.LittleEndian.PutUint16(out[0xC:], uint16(int16(power)))
	out[0x12] = byte(lvl)
	return support{out, flags, bits, power, lvl}, true
}

// applySupport is a support block on a player (0x1c0350): (amount, SE1) or false.
func (t *Fighter) applySupport(s support, now float64) (int, uint32, bool) {
	mx := t.MaxHP()
	hp := t.HP()
	if hp == 0 {
		if s.flags&0x800 == 0 {
			return 0, 0, false
		}
		if s.flags&0x20000 != 0 {
			hp = mx
		} else {
			hp = min(mx, max(1, s.power))
		}
		t.St.HP = hp
		return hp, 0x40000, true
	}
	amount, se := 0, uint32(0)
	if s.flags&0x1000 != 0 {
		healed := min(s.power, mx-hp)
		if s.flags&0x20000 != 0 {
			healed = mx - hp
		}
		if healed > 0 {
			t.St.HP = hp + healed
			amount, se = healed, se|0x80000
		}
	}
	if s.flags&0x2000 != 0 {
		if t.Status.Cure(s.bits, now) && s.flags&0x100000 == 0 {
			se |= 0x80000
		}
		if t.Status.ApplyBuffs(s.bits, s.level, now) {
			se |= 0x1000000
		}
	}
	return amount, se, se != 0
}

// reviveAmount is the HP an item revives a dead target with (constants +7 flag 0x80;
// 0x10: a % of max HP).
func (f *Fighter) reviveAmount(attackID uint32, t *Fighter) (int, bool) {
	s := f.item(attackID)
	if s == nil {
		return 0, false
	}
	it, ok := f.R.items[s.ID]
	if !ok || it.Consts[7]&0x80 == 0 {
		return 0, false
	}
	amount := int(binary.LittleEndian.Uint16(it.Consts[4:]))
	mx := t.MaxHP()
	if it.Consts[7]&0x10 != 0 {
		return max(1, mx*amount/100), true
	}
	return max(1, min(amount, mx)), true
}

// healAmount is the HP a healing item gives (constants +7 flag 0x20, amount +4; 0x10 %).
func (f *Fighter) healAmount(attackID uint32, t *Fighter) (int, bool) {
	s := f.item(attackID)
	if s == nil {
		return 0, false
	}
	it, ok := f.R.items[s.ID]
	if !ok || it.Cat != 3 {
		return 0, false
	}
	c := it.Consts
	if c[7]&0x20 == 0 || c[7]&0x80 != 0 {
		return 0, false
	}
	amount := int(binary.LittleEndian.Uint16(c[4:]))
	if amount == 0 && c[7]&0x40 != 0 {
		return 0, false
	}
	mx := t.MaxHP()
	if c[7]&0x10 != 0 {
		return max(1, mx*amount/100), true
	}
	return max(1, amount), true
}

// itemStatus is a cure or status item used on t (0x2623c0): (SE1, D) or false.
func (f *Fighter) itemStatus(rng Rand, attackID uint32, t *Fighter, now float64) (uint32, []byte, bool) {
	s := f.item(attackID)
	if s == nil {
		return 0, nil, false
	}
	it, ok := f.R.items[s.ID]
	if !ok || it.Cat != 3 {
		return 0, nil, false
	}
	c := it.Consts
	if c[7]&0x80 != 0 || (c[7]&0x20 != 0 && s16(c, 4) != 0) {
		return 0, nil, false
	}
	d := make([]byte, 20)
	d[0xE] = 100
	var bits, flags uint32
	lvl := 0
	switch {
	case c[7]&0x40 != 0:
		bits, flags = cureBits(f.R.disc), 0x2400
	case c[7]&0xF >= 1 && c[7]&0xF <= 5 && int(c[6]) < len(effectSE2) && effectSE2[c[6]] != 0:
		bits, lvl = effectSE2[c[6]], int(c[7]&0xF)-1
		flags = 0x2200
		if bits&0xF83000 != 0 {
			flags = 0x2400 | 0x2000
		}
	default:
		return 0, nil, false
	}
	binary.LittleEndian.PutUint32(d, flags)
	binary.LittleEndian.PutUint32(d[4:], bits)
	d[0x12] = byte(lvl)
	if t.HP() == 0 {
		return 0, nil, false
	}
	var se uint32
	if flags&0x400 != 0 {
		if c[7]&0x40 != 0 && t.Status.Cure(bits, now) {
			se |= 0x80000
		}
		if c[7]&0x40 == 0 && t.Status.ApplyBuffs(bits, lvl, now) {
			se |= 0x1000000
		}
	} else if t.Status.Inflict(rng, bits, lvl, 100, t.TotalStats()[4], false, t.Equipment().Immune, now) != 0 {
		se |= 0x800000
	}
	return se, d, se != 0
}

func schoolBonus(techs [][2]int) int {
	var schools []int
	for _, t := range techs[:min(len(techs), 4)] {
		if t[0] != 0 {
			schools = append(schools, (t[0]-1)/10+1)
		}
	}
	for _, s := range schools {
		if s != schools[0] {
			return 0
		}
	}
	if len(schools) == 0 {
		return 0
	}
	return 4 * (len(schools) - 1)
}

// attackStatus is the status a hit carries: a technique's tier record, or a bullet PA's
// base param +0xA / +0xB. Returns (SE2 bits, level).
func (f *Fighter) attackStatus(w uint32, s *Item, it *ItemDef) (uint32, int) {
	var eff, strength int
	if it.Consts[0xE] == techStyle {
		key, level, _, ok := f.techLink(w)
		tiers, known := techFX[key]
		if !ok || !known {
			return 0, 0
		}
		e := tiers[min(level/10, len(tiers)-1)]
		eff, strength = e[0], e[1]
	} else {
		key, ok := f.R.weaponKey(s, w)
		if !ok || key[0] != 5 {
			return 0, 0
		}
		e, ok := f.R.pa[key]
		if !ok {
			return 0, 0
		}
		base := e.Base[min(s.PA[1]/10, len(e.Base)-1)]
		eff, strength = int(base[0xA]), int(base[0xB])
	}
	var bits uint32
	if eff < len(effectSE2) {
		bits = effectSE2[eff]
	}
	if strength < 1 || strength > 5 || bits&damagingBits(f.R.disc) == 0 {
		return 0, 0
	}
	return bits, strength - 1
}

// PlayerAttack builds a player's attack block from the attack id (0x5f120): the
// weapon's power and accuracy with the player's stats, element, and status.
func (f *Fighter) playerAttack(attackID uint32, d []byte, now float64) []byte {
	s := f.invItem(attackID)
	if s == nil {
		return d
	}
	it, ok := f.R.items[s.ID]
	if !ok || it.Cat != catWeapon {
		return d
	}
	stats := f.TotalStats()
	c := it.Consts
	tech := c[0xE] == techStyle
	power := stats[0]
	if tech {
		power = stats[6]
	}
	power += it.ATP + s.ATPBonus
	acc := stats[2] + it.ATA
	if f.Status.SE2(now) != 0 {
		slot := 0
		if tech {
			slot = 2
		}
		power = scaled(power, f.Status.buffMult(slot, now))
		if !tech {
			acc = scaled(acc, f.Status.buffMult(3, now))
		}
	}
	flags := binary.LittleEndian.Uint32(d) | 0x200 | 0x1000
	if tech {
		flags |= 0x4008
	} else {
		flags |= 0x4
	}
	out := append([]byte(nil), d...)
	binary.LittleEndian.PutUint16(out[8:], uint16(int16(min(acc, 0x7FFF))))
	binary.LittleEndian.PutUint16(out[0xA:], 0)
	binary.LittleEndian.PutUint16(out[0xC:], uint16(int16(min(power, 0x7FFF))))
	if tech {
		if key, level, techs, ok := f.techLink(attackID); ok {
			if e := key[1]; e >= 1 && e <= 6 {
				out[0x10] = elementCode[e]
			} else {
				out[0x10] = 0
			}
			out[0x11] = byte(min(100, level+10+schoolBonus(techs)))
		}
	} else if c[0xB] >= 1 && c[0xB] <= 6 {
		out[0x10], out[0x11] = elementCode[c[0xB]], min(c[0xC], 127)
	}
	if bits, level := f.attackStatus(attackID, s, it); bits != 0 {
		flags |= 0x2000
		binary.LittleEndian.PutUint32(out[4:], bits)
		binary.LittleEndian.PutUint16(out[0xE:], uint16(int16(min(stats[5], 0x7FFF))))
		out[0x12] = byte(level)
	}
	binary.LittleEndian.PutUint32(out, flags)
	return out
}

// statusAttack is the block of a kind 3 (Jellen, Zalure, Zoldeel) or barrier hit.
func (f *Fighter) statusAttack(key ItemID, level int, d []byte) []byte {
	tiers := techFX[key]
	e := tiers[min(level/10, len(tiers)-1)]
	out := append([]byte(nil), d...)
	binary.LittleEndian.PutUint32(out, binary.LittleEndian.Uint32(d)|0x200|0x2000|0x4000|0xC)
	binary.LittleEndian.PutUint32(out[4:], effectSE2[e[0]])
	for o := 8; o < 0xE; o += 2 {
		binary.LittleEndian.PutUint16(out[o:], 0)
	}
	binary.LittleEndian.PutUint16(out[0xE:], uint16(int16(min(f.TotalStats()[5], 0x7FFF))))
	out[0x12] = byte(max(0, e[1]-1))
	return out
}

func (f *Fighter) String() string { return fmt.Sprintf("player %#08x/%d", f.GID, f.LID) }
