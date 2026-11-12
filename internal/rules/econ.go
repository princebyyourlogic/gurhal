// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"unicode/utf16"
)

// Shops, grinding, PA disks, Lumilass and Probo growth (econ tables from the disc,
// extract/econdata.py; PSU_US.erx addresses in prettystellaruniverse's docs).

type shopTables struct {
	NPCs  [][2]int                          `json:"npcs"`  // npc -> (list 0..3, category)
	Tiers map[string]map[string][4][]string `json:"tiers"` // tier -> category -> 4 lists of item ids
}

type grindTables struct {
	Success  [][]int `json:"success"` // [weapon rank][grind] %
	Grinders map[string]struct {
		Rank, Bonus int
	} `json:"grinders"`
	Weapons map[string]struct {
		Rank     int        `json:"rank"`
		MaxGrind int        `json:"max_grind"`
		PPInc    []int      `json:"pp_inc"`
		ATPInc   []int      `json:"atp_inc"`
		Refine   *refineRow `json:"refine"`
	} `json:"weapons"`
	Refine map[string]refineRow `json:"refine"` // AotI: by weapon type
}

type refineRow struct {
	PP, ATP, Rand int
	Mult          []float64
}

type proboTables struct {
	Coef       [][]float64 `json:"coef"` // [stat][gift kind]
	Next       [][]int     `json:"next"` // [type] candidate types
	Req        [][]int     `json:"req"`  // [type] total, then 5 stat levels
	Device     [][]int     `json:"device"`
	DeviceNeed []int       `json:"device_need"`
}

// --- NPC shops ---------------------------------------------------------------------------------------

// ShopEntry is one line of a shop list: the item and the id the disc lists (clothing
// carries a colour in byte 3).
type ShopEntry struct {
	Key    ItemID
	DiscID ItemID
}

// ShopTier is the shop tier (file 01..12) for a level: it grows with the story.
func ShopTier(level int) int { return max(1, min(12, 1+(level-1)/8)) }

func (r *Rules) itemKey(id ItemID) (ItemID, bool) {
	if _, ok := r.items[id]; ok {
		return id, true
	}
	id[3] = 0
	_, ok := r.items[id]
	return id, ok
}

func (r *Rules) shopEntries(tier, cat, lst int) []ShopEntry {
	lists, ok := r.shops.Tiers[strconv.Itoa(tier)][strconv.Itoa(cat)]
	if !ok {
		return nil
	}
	var out []ShopEntry
	for _, s := range lists[lst] {
		var id ItemID
		if id.UnmarshalText([]byte(s)) != nil {
			continue
		}
		if k, ok := r.itemKey(id); ok && r.items[k].ValueBase > 0 {
			out = append(out, ShopEntry{k, id})
		}
	}
	return out
}

// ShopList is NPC npc's list and category for a level. An empty list in that tier (the
// client cannot hold 0 entries) comes from the nearest tier with one, later tiers first.
func (r *Rules) ShopList(npc, level int) ([]ShopEntry, int, bool) {
	if r.shops == nil || npc < 0 || npc >= len(r.shops.NPCs) {
		return nil, 0, false
	}
	lst, cat := r.shops.NPCs[npc][0], r.shops.NPCs[npc][1]
	tier := ShopTier(level)
	order := []int{tier}
	for t := tier + 1; t <= 12; t++ {
		order = append(order, t)
	}
	for t := tier - 1; t >= 1; t-- {
		order = append(order, t)
	}
	for _, t := range order {
		if out := r.shopEntries(t, cat, lst); len(out) > 0 {
			return out, cat, true
		}
	}
	return []ShopEntry{}, cat, true
}

// ShopRecord is a 0x4C-byte list entry: name, rarity, the disc id, buy price, constants.
func (r *Rules) ShopRecord(e ShopEntry) []byte {
	it := r.items[e.Key]
	b := fixedUTF16(it.Name, 0x2C, 0x2E)
	b = append(b, it.Rarity0, 0)
	b = append(b, e.DiscID[:]...)
	b = binary.LittleEndian.AppendUint32(b, uint32(it.ValueBase))
	return append(b, it.Consts...)
}

var ErrMeseta = errors.New("not enough meseta")

// Buy adds qty of an item, paying its value each. Returns the slot for the reply (a merge
// into a held stack still names the next UUID, as the client expects).
func (r *Rules) Buy(st *State, key ItemID, qty int) (Item, error) {
	it := r.items[key]
	qty = max(1, qty)
	stack := r.Stackable(key)
	if !stack {
		qty = 1
	}
	cost := it.ValueBase * qty
	if int(st.Meseta) < cost {
		return Item{}, ErrMeseta
	}
	next := max(st.NextUUID, 1)
	for _, x := range st.Inv {
		if x.UUID < GroundBase {
			next = max(next, x.UUID+1)
		}
	}
	held := -1
	if stack {
		for i, x := range st.Inv {
			if x.ID == key {
				held = i
				break
			}
		}
	}
	if held < 0 && len(st.Inv) >= MaxItems {
		return Item{}, ErrInventoryFull
	}
	st.Meseta -= uint32(cost)
	st.NextUUID = next + 1
	s := Item{UUID: next, ID: key, Qty: qty}
	if held >= 0 {
		st.Inv[held].Qty = min(st.Inv[held].Qty+qty, r.StackMax(key))
	} else {
		st.Inv = append(st.Inv, s)
	}
	return s, nil
}

// Sell sells qty of an item (not an equipped one) at its constants price. Returns the
// quantity sold and the meseta gained.
func (r *Rules) Sell(st *State, uuid uint32, qty int) (int, int) {
	for i := range st.Inv {
		s := &st.Inv[i]
		if s.UUID != uuid || s.Active != 0 {
			continue
		}
		qty = max(1, min(qty, s.Qty))
		gain := int(r.items[s.ID].Price) * qty
		if s.Qty -= qty; s.Qty <= 0 {
			st.Inv = append(st.Inv[:i], st.Inv[i+1:]...)
		}
		st.Meseta += uint32(gain)
		return qty, gain
	}
	return 0, 0
}

// --- grinding ----------------------------------------------------------------------------------------

// GrindChance is the chance in 1/100 % of grinder on weapon, or false when refused.
func (r *Rules) GrindChance(grinder, weapon *Item) (int, bool) {
	if r.grind == nil {
		return 0, false
	}
	gr, ok1 := r.grind.Grinders[grinder.ID.String()]
	wp, ok2 := r.grind.Weapons[weapon.ID.String()]
	if !ok1 || !ok2 {
		return 0, false
	}
	top := wp.MaxGrind
	if r.disc == AotI {
		top -= weapon.Fails
	}
	if gr.Rank < wp.Rank || weapon.Grind >= 10 || weapon.Grind >= top || wp.Rank >= len(r.grind.Success) {
		return 0, false
	}
	return 10000 - (100-r.grind.Success[wp.Rank][weapon.Grind])*(100-5*gr.Bonus), true
}

// Grind commits a grind of inventory slot wi with the grinder of UUID guuid. Returns the
// grinder's index, success, and false when refused.
func (r *Rules) Grind(rng Rand, st *State, guuid uint32, wi int) (int, bool, bool) {
	gi := -1
	for i, x := range st.Inv {
		if x.UUID == guuid {
			gi = i
		}
	}
	if gi < 0 || wi < 0 || wi >= len(st.Inv) {
		return 0, false, false
	}
	w := &st.Inv[wi]
	chance, ok := r.GrindChance(&st.Inv[gi], w)
	if !ok {
		return 0, false, false
	}
	if r.disc == AotI {
		return gi, r.refine(rng, w, chance), true
	}
	success := rng.Intn(10000) < chance
	if success {
		wp := r.grind.Weapons[w.ID.String()]
		w.PPBonus = min(9999, w.PPBonus+wp.PPInc[w.Grind])
		w.ATPBonus = min(9999, w.ATPBonus+wp.ATPInc[w.Grind])
		w.Grind++
	}
	return gi, success, true
}

// refine is AotI's commit (0x299950): success grows the weapon by its type's refine row,
// failure resets it and counts the failure.
func (r *Rules) refine(rng Rand, w *Item, chance int) bool {
	wp := r.grind.Weapons[w.ID.String()]
	if rng.Intn(10000) > chance {
		w.PPBonus, w.ATPBonus, w.Grind = 0, 0, 0
		w.Fails = min(15, w.Fails+1)
		return false
	}
	s := wp.Refine
	if s == nil {
		if row, ok := r.grind.Refine[strconv.Itoa(int(w.ID[1]))]; ok {
			s = &row
		}
	}
	if s != nil {
		it := r.items[w.ID]
		stars := int(it.Rarity0 & 0x3F)
		f := f32(s.Mult[min(stars, len(s.Mult)-1)])
		var d [2]int
		for k := range d {
			x := 1
			if s.Rand > 0 {
				x = rng.Intn(s.Rand) + 1
			}
			d[k] = max(1, int(rz(f*f32(float64(x)))))
		}
		base := int(binary.LittleEndian.Uint16(it.Consts))
		pp := max(0, min(9999, base+w.PPBonus+s.PP+d[0]))
		w.PPBonus = pp - base
		w.ATPBonus = max(0, min(9999, w.ATPBonus+s.ATP+d[1]))
	}
	w.Grind++
	return true
}

// --- PA disks ----------------------------------------------------------------------------------------

// diskPA is the PA (level 0) a disk teaches: a bullet disk 05 b1 b2 is PA 05 (b1 + 0x0D)
// (b2 + 1); striking and technique disks are their PA.
func diskPA(id ItemID) ItemID {
	if id[0] == 5 {
		return ItemID{5, id[1] + 0x0D, id[2] + 1, 0}
	}
	return ItemID{id[0], id[1], id[2], 0}
}

func (r *Rules) diskProblem(s *Item, pas []PA) string {
	it, ok := r.items[s.ID]
	if !ok || it.Cat < 4 || it.Cat > 6 {
		return "not a disk"
	}
	key := diskPA(s.ID)
	_, thr := r.paThresholds[key]
	_, param := r.pa[key]
	if !thr && !param {
		return "no such PA on this disc"
	}
	for _, p := range pas {
		if p.ID[0] == key[0] && p.ID[1] == key[1] && p.ID[2] == key[2] {
			return "already learned"
		}
	}
	return ""
}

// LearnDisk is C>S 0x0105 event 7 (+0x2C index, +0x2E list position to replace when full,
// +0x30 UUID): the disk the request means (by UUID, else index, else the learnable disk
// nearest the index: the client's pack can disagree with the saved one) becomes a Lv1 PA.
// Returns the list position, the entry and the disk's category.
func (r *Rules) LearnDisk(st *State, req []byte) (int, PA, int, error) {
	idx, pos := int(req[0x2C]), int(req[0x2E])
	uuid := binary.LittleEndian.Uint32(req[0x30:])
	var asked []int
	for i, s := range st.Inv {
		if s.UUID == uuid {
			asked = append(asked, i)
		}
	}
	if idx < len(st.Inv) {
		asked = append(asked, idx)
	}
	pick := -1
	for _, i := range asked {
		if r.diskProblem(&st.Inv[i], st.PAs) == "" {
			pick = i
			break
		}
	}
	if pick < 0 {
		best := 1 << 30
		for i := range st.Inv {
			if d := abs(i - idx); r.diskProblem(&st.Inv[i], st.PAs) == "" && d < best {
				best, pick = d, i
			}
		}
	}
	if pick < 0 {
		if len(asked) > 0 {
			s := st.Inv[asked[0]]
			return 0, PA{}, r.items[s.ID].Cat, errors.New(r.diskProblem(&s, st.PAs))
		}
		return 0, PA{}, 0, errors.New("not a disk")
	}
	s := st.Inv[pick]
	cat := r.items[s.ID].Cat
	entry := PA{ID: diskPA(s.ID)}
	switch {
	case len(st.PAs) < MaxPAs:
		pos = len(st.PAs)
		st.PAs = append(st.PAs, entry)
	case pos < MaxPAs:
		st.PAs[pos] = entry
	default:
		return 0, PA{}, cat, errors.New("PA list full")
	}
	st.Inv = append(st.Inv[:pick], st.Inv[pick+1:]...)
	return pos, entry, cat, nil
}

// PAParam is a learned PA's 0x30-byte parameter entry.
func (r *Rules) PAParam(p PA) []byte { return r.paParam(p) }

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// --- Lumilass ----------------------------------------------------------------------------------------

// HairList is the hair words the beauty parlour offers a character's race and gender.
func (r *Rules) HairList(raw []byte) []uint32 {
	g := int(raw[RawRace])*2 + int(raw[RawGender])
	if g >= len(r.lumilass) {
		return nil
	}
	var out []uint32
	for _, lst := range r.lumilass[g] {
		for _, w := range lst {
			if w>>24 == 5 && len(out) < 120 {
				out = append(out, w)
			}
		}
	}
	return out
}

// ApplyLumilass is C>S 0x1A05: +0x28 hair list index, +0x30 9 words of appearance
// +0x24..+0x44, +0x54 five bytes of appearance +0x18 +0x19 +0x1A +0x1C +0x20. Returns
// the new raw record.
func (r *Rules) ApplyLumilass(raw, body []byte) []byte {
	words := r.HairList(raw)
	out := append([]byte(nil), raw...)
	if idx := int(binary.LittleEndian.Uint32(body[0x28:])); idx < len(words) {
		binary.LittleEndian.PutUint32(out[0x70+0x14:], words[idx])
	}
	copy(out[0x70+0x24:0x70+0x48], body[0x30:0x54])
	for i, off := range []int{0x18, 0x19, 0x1A, 0x1C, 0x20} {
		out[0x70+off] = body[0x54+i]
	}
	return out
}

// --- Probo growth (0x37e470, verified by emulation in prettystellaruniverse) -------------------------

// rz is a float32 rounded toward zero (the R5900 FPU truncates).
func rz(x float64) float64 {
	y := float32(x)
	if math.Abs(float64(y)) > math.Abs(x) {
		y = math.Float32frombits(math.Float32bits(y) - 1)
	}
	return float64(y)
}

func lv(x int) int {
	if x < 0 {
		return 0
	}
	return x / 100
}

func s8(x int) int { return (x+128)&0xFF - 128 }

func giftKind(id ItemID, style byte) (int, bool) {
	switch c := int(id[0]); {
	case c == 1:
		if k, ok := map[byte]int{0: 16, 1: 17, 2: 17, 3: 18}[style]; ok {
			return k, true
		}
		return 16, true
	case c >= 2 && c <= 13:
		return c - 1, true
	case c == 15:
		return 14, true
	case c == 16:
		return 15, true
	}
	return 0, false
}

// ProboGift grows the Probo from a gifted item. Returns evolved, the stat deltas.
func (r *Rules) ProboGift(st *State, s Item) (bool, [5]int) {
	if st.Probo == nil {
		st.Probo = &Probo{}
	}
	pr := st.Probo
	var deltas [5]int
	p := r.probo
	if p == nil || len(p.Coef) < 4 {
		return false, deltas
	}
	if r.disc == AotI && s.ID[0] == 0x0E {
		return r.proboDevice(pr, int(s.ID[2])), deltas
	}
	it := r.items[s.ID]
	rec := r.VarRec(&s, r.disc)
	kind, ok := giftKind(s.ID, rec[0x1A])
	stats := pr.Stats
	if ok {
		factor := int(it.Rarity0) + 2
		if s.ID[0] == 1 {
			factor = int(it.Rarity0) + 1
		}
		for i := range 4 {
			if kind >= len(p.Coef[i]) {
				continue
			}
			c := p.Coef[i][kind]
			if c == 0 {
				continue
			}
			f := rz(f32(float64(factor)) * f32(c))
			switch {
			case f > 0:
				d := int(rz(f + f32(0.9)))
				nv := stats[i] + d
				others := 0
				for j := range 4 {
					if j != i {
						others += lv(stats[j])
					}
				}
				if lv(nv)+others <= 100 {
					stats[i], deltas[i] = nv, s8(d)
				} else {
					l := lv(stats[i])
					deltas[i], stats[i] = s8(l*100+99-stats[i]), l*100+99
				}
			case f < 0:
				d := int(rz(f - f32(0.9)))
				switch {
				case lv(stats[i]+d) < lv(stats[i]):
					l := lv(stats[i])
					deltas[i], stats[i] = s8(l*100-stats[i]), l*100
				case stats[i] <= 0:
					stats[i] = 0
				case stats[i]+d < 0:
					deltas[i], stats[i] = s8(-stats[i]), 0
				default:
					stats[i], deltas[i] = stats[i]+d, s8(d)
				}
			}
		}
	}
	evolved, t := false, pr.Type
	if !(t >= 8 && t <= 12 || t >= 16 && t <= 40) && t < len(p.Next) {
		var l [5]int
		for k := range l {
			l[k] = lv(stats[k])
		}
		for _, n := range p.Next[t] {
			if n < 0 || n >= len(p.Req) {
				continue
			}
			q := p.Req[n]
			okAll := l[0]+l[1]+l[2]+l[3] >= q[0]
			for k := 0; k < 5 && okAll; k++ {
				okAll = l[k] >= q[k+1]
			}
			if okAll {
				t, evolved = n, true
				break
			}
		}
	}
	pr.Type, pr.Stats = t, stats
	return evolved, deltas
}

// proboDevice is AotI's PM device: device 0x25 resets, others change the type when the
// stat 4 need is met.
func (r *Rules) proboDevice(pr *Probo, idx int) bool {
	p, t := r.probo, pr.Type
	if idx == 0x25 {
		pr.Stats = [5]int{}
		if t != 0 && t != 1 {
			pr.Type = 0
			return true
		}
		return false
	}
	nv := -1
	if idx < min(len(p.Device), 0x44) && t >= 0 && t < min(len(p.Device[idx]), 41) {
		nv = p.Device[idx][t]
	}
	if idx < len(p.DeviceNeed) && pr.Stats[4]&0xFFFF < p.DeviceNeed[idx] {
		nv = -1
	}
	if nv >= 0 && nv < 41 {
		pr.Type = nv
		return true
	}
	return false
}

// fixedUTF16 is s as UTF-16 cut to cut bytes and padded to n.
func fixedUTF16(s string, cut, n int) []byte {
	b := make([]byte, n)
	o := 0
	for _, u := range utf16.Encode([]rune(s)) {
		if o+2 > cut {
			break
		}
		binary.LittleEndian.PutUint16(b[o:], u)
		o += 2
	}
	return b
}
