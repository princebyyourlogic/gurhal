// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/hex"
	"math"
	"strconv"
)

// Synthesis (My Room production, the offline TItemProductManager): boards "0dTTII00" with
// a level, uses, four material slots, eight outputs (rate, element, failure group). The
// PS2 produces instantly. Materials come from the room stock.

const SynthEntries = 32

const (
	noItem   = "00000000"
	anyItem  = "ffffffff"
	stockMax = 300
)

type synthTables struct {
	Boards  map[string]*SynthBoard `json:"boards"`
	Failure map[string][]struct {
		ID     string `json:"id"`
		Weight int    `json:"weight"`
	} `json:"failure"`
	Element map[string][][2]int `json:"element"` // [percent, weight]
}

type SynthBoard struct {
	Name         string        `json:"name"`
	Cat          int           `json:"cat"`
	Level        int           `json:"level"`
	BoardB1      int           `json:"board_b1"`
	FreqMax      int           `json:"freq_max"`
	VariantBoard bool          `json:"variant_board"`
	SlotTypes    [4]int        `json:"slot_types"`
	Wildcard     bool          `json:"wildcard"`
	Outputs      []SynthOutput `json:"outputs"`
	Materials    [4][]SynthMat `json:"materials"`
}

type SynthOutput struct {
	ID        string `json:"id"`
	A         int    `json:"a"`
	Rate      int    `json:"rate"`
	Element   int    `json:"element"`
	FailGroup int    `json:"fail_group"`
}

type SynthMat struct {
	ID   string `json:"id"`
	Qty  int    `json:"qty"`
	Flag int    `json:"flag"`
}

// SynthEntry is one of a character's 32 production entries.
type SynthEntry struct {
	Board   string       `json:"board"`
	Running int          `json:"running"`
	Fail    int          `json:"fail"`
	FailID  int          `json:"fail_id"`
	Used    int          `json:"used"`
	Variant int          `json:"variant"`
	Slots   [4]string    `json:"slots"`
	Result  *SynthResult `json:"result,omitempty"`
}

type SynthResult struct {
	ID      string `json:"id"`
	Element int    `json:"element"`
	Pct     int    `json:"pct"`
	Fail    int    `json:"fail"`
}

// SynthEntryList is the character's entries, grown to 32.
func SynthEntryList(st *State) []*SynthEntry {
	for len(st.Synth) < SynthEntries {
		st.Synth = append(st.Synth, nil)
	}
	return st.Synth
}

// SynthBoard is a board's record. AotI sends Goods boards (type 9) to the WpnShoot2
// loader slot: 0d09II00 runs 0d0bII00's recipe with its own level.
func (r *Rules) SynthBoard(bid string) *SynthBoard {
	if r.synth == nil {
		return nil
	}
	b := r.synth.Boards[bid]
	if b == nil || r.disc != AotI || len(bid) != 8 || bid[2:4] != "09" {
		return b
	}
	src := r.synth.Boards["0d0b"+bid[4:6]+"00"]
	if src == nil {
		return nil
	}
	cp := *src
	cp.Name, cp.Level, cp.BoardB1 = b.Name, b.Level, b.BoardB1
	cp.VariantBoard = src.Outputs[0].A != 0 || src.Outputs[1].ID != noItem
	return &cp
}

func realItem(id string) bool { return id != "" && id != noItem && id != anyItem }

// StockCount is how many of an item the room stock holds.
func (r *Rules) StockCount(st *State, id string) int {
	n := 0
	for _, x := range st.RoomStock {
		if x.ID.String() == id {
			if r.Stackable(x.ID) {
				n += x.Qty
			} else {
				n++
			}
		}
	}
	return n
}

// SynthMaterial is a slot's (item, need): the entry's own item for slot type 1, else the
// variant's recipe item.
func (b *SynthBoard) SynthMaterial(e *SynthEntry, slot, variant int) (string, int) {
	m := b.Materials[slot][max(0, variant)]
	if b.SlotTypes[slot] == 1 && e.Slots[slot] != "" {
		return e.Slots[slot], m.Qty
	}
	if !realItem(m.ID) {
		return "", m.Qty
	}
	return m.ID, m.Qty
}

// Recipe is a variant's four material ids.
func (b *SynthBoard) Recipe(variant int) [4]string {
	var out [4]string
	for k := range out {
		out[k] = b.Materials[k][max(0, variant)].ID
	}
	return out
}

// OutputMax is how many the entry can make with the stock (variant boards: 1).
func (r *Rules) OutputMax(st *State, e *SynthEntry, b *SynthBoard) int {
	if b.VariantBoard {
		return 1
	}
	n := b.FreqMax - e.Used
	for k := range 4 {
		if id, need := b.SynthMaterial(e, k, 0); id != "" && need > 0 {
			n = min(n, r.StockCount(st, id)/need)
		}
	}
	if n <= 0 {
		return max(n, -10)
	}
	return n
}

// SynthRate is the success rate in %.
func (r *Rules) SynthRate(st *State, b *SynthBoard, variant int) int {
	out := b.Outputs[max(0, variant)]
	var stats [5]int
	if st.Probo != nil {
		stats = st.Probo.Stats
	}
	if r.disc == AotI {
		rate := out.Rate
		if rate >= 2 && rate != 100 {
			skill := r.synthSkill(stats, out.ID)
			m := rz(f32(95.0) + rz(f32(-0.33)*f32(float64(10*b.Level-skill))))
			m = math.Min(m, 100.0)
			f := rz(rz(f32(0.01)*m) * rz(f32(0.01)*f32(float64(rate))))
			rate = int(rz(f32(100.0) * f))
		}
		return max(0, min(rate, 100))
	}
	if out.Rate >= 100 {
		return 100
	}
	skill := 1
	if b.Cat < 4 {
		skill = max(0, stats[b.Cat]/100)
	}
	skill = max(1, min(skill, 200))
	return int(float64(out.Rate) * math.Min(100.0, 95+0.33*float64(skill-10*b.Level)) / 100)
}

// synthSkill is AotI's skill for an output: armour Probo stat 3, weapons by type.
func (r *Rules) synthSkill(stats [5]int, id string) int {
	iid, _ := hex.DecodeString(id)
	l := 1
	if len(iid) == 4 {
		switch iid[0] {
		case 2:
			l = lv(stats[3])
		case 1:
			switch t := iid[1]; {
			case t < 14:
				l = lv(stats[0])
			case t < 24 || t == 27:
				l = lv(stats[1])
			case t <= 26:
				l = lv(stats[2])
			default:
				l = lv(1)
			}
		}
	}
	return max(1, min(l, 200))
}

func weighted(rng Rand, ws []int) int {
	total := 0
	for _, w := range ws {
		total += w
	}
	if total <= 0 {
		return -1
	}
	x := rng.Intn(total)
	for i, w := range ws {
		if x < w {
			return i
		}
		x -= w
	}
	return len(ws) - 1
}

func cum1000(rng Rand, ws []int) int {
	x, acc := rng.Intn(1000)+1, 0
	for i, w := range ws {
		if acc += w; x <= acc {
			return i
		}
	}
	return -1
}

// SynthRoll is the entry's result, rolled once and kept.
func (r *Rules) SynthRoll(rng Rand, st *State, e *SynthEntry, b *SynthBoard) *SynthResult {
	if e.Result != nil {
		return e.Result
	}
	v := max(0, e.Variant)
	out := b.Outputs[v]
	fails := r.synth.Failure[strconv.Itoa(out.FailGroup)]
	failW := make([]int, len(fails))
	for i, f := range fails {
		failW[i] = f.Weight
	}
	elems := r.synth.Element[strconv.Itoa(out.Element)]
	var res *SynthResult
	if r.disc == AotI {
		rate := r.SynthRate(st, b, v)
		ok := b.Outputs[0].Rate == 100 || rate == 100 || rng.Intn(100) < rate
		pick := -1
		if !ok {
			pick = cum1000(rng, failW)
		}
		if pick >= 0 {
			res = &SynthResult{ID: fails[pick].ID, Fail: 1}
		} else {
			pct := 0
			if out.Element == 0 {
				elems = nil
			}
			ws := make([]int, len(elems))
			for i, el := range elems {
				ws[i] = el[1]
			}
			if k := cum1000(rng, ws); k >= 0 { // draws even without an element, as the client does
				pct = elems[k][0]
			}
			res = &SynthResult{ID: out.ID, Element: out.Element, Pct: pct}
		}
	} else {
		ok := rng.Intn(100) < r.SynthRate(st, b, v)+4 || b.Outputs[0].Rate >= 100
		res = &SynthResult{ID: out.ID, Element: out.Element}
		if ok {
			if elems == nil {
				elems = [][2]int{{0, 100}}
			}
			ws := make([]int, len(elems))
			for i, el := range elems {
				ws[i] = el[1]
			}
			if k := weighted(rng, ws); k >= 0 {
				res.Pct = elems[k][0]
			}
		} else if len(fails) > 0 {
			if k := weighted(rng, failW); k >= 0 {
				res = &SynthResult{ID: fails[k].ID, Fail: 1}
			}
		}
	}
	e.Result = res
	return res
}

// SynthConsume takes n products' materials from the room stock.
func (r *Rules) SynthConsume(st *State, e *SynthEntry, b *SynthBoard, variant, n int) {
	for k := range 4 {
		id, need := b.SynthMaterial(e, k, variant)
		left := need * n
		if id == "" || left <= 0 {
			continue
		}
		var keep []Item
		for _, x := range st.RoomStock {
			if left > 0 && x.ID.String() == id {
				stack := r.Stackable(x.ID)
				take := 1
				if stack {
					take = min(left, x.Qty)
				}
				x.Qty -= take
				left -= take
				if x.Qty <= 0 || !stack {
					continue
				}
			}
			keep = append(keep, x)
		}
		st.RoomStock = keep
	}
}

// Rooms is how many more of an item the stock and the inventory can take (1, 1 for
// non-stackables).
func (r *Rules) Rooms(st *State, id string) (int, int) {
	var iid ItemID
	if iid.UnmarshalText([]byte(id)) != nil || !r.Stackable(iid) {
		return 1, 1
	}
	mx := r.StackMax(iid)
	held := 0
	for _, x := range st.Inv {
		if x.ID == iid {
			held += x.Qty
		}
	}
	return max(0, mx-r.StockCount(st, id)), max(0, mx-held)
}

// Rec20 is an item's 0x20-byte record for the production windows (zero for none).
func (r *Rules) Rec20(id string, qty int, uuid uint32, element, pct int, withElement bool) []byte {
	var iid ItemID
	if !realItem(id) || iid.UnmarshalText([]byte(id)) != nil {
		return make([]byte, 0x20)
	}
	it, ok := r.items[iid]
	if !ok {
		return make([]byte, 0x20)
	}
	rec := r.VarRec(&Item{UUID: uuid, ID: iid, Qty: qty}, r.disc)
	if it.Cat == catWeapon && withElement {
		rec[0x18], rec[0x19] = byte(element), byte(pct)
	}
	return rec[4:]
}

// ItemName is an item's 0x2E-byte UTF-16 name field.
func (r *Rules) ItemName(id string) []byte {
	var iid ItemID
	if iid.UnmarshalText([]byte(id)) != nil {
		return make([]byte, 0x2E)
	}
	it, ok := r.items[iid]
	if !ok {
		return make([]byte, 0x2E)
	}
	return fixedUTF16(it.Name, 0x2C, 0x2E)
}

// ConstRecID is the constants record of an item id (zero for an unknown one).
func (r *Rules) ConstRecID(id string) []byte {
	var iid ItemID
	if iid.UnmarshalText([]byte(id)) == nil {
		if _, ok := r.items[iid]; ok {
			return r.ConstRec(&Item{ID: iid, Qty: 1})
		}
	}
	return make([]byte, 0x48)
}
