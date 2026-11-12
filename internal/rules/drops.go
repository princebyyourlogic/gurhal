// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/binary"
	"strconv"
)

// Drops (drops.json from GA_ITEMPARAM itemEnemyDrop / itemObjectDrop and the area tables):
// every roll is rand(50000) against cumulative thresholds (0x2081a0).

const (
	dropRoll   = 50000
	GroundBase = 0x40000000 // ground UUIDs: their own range, never an inventory UUID
)

var mesetaID = ItemID{0x12, 0, 0, 0}

type dropTables struct {
	Bands []int `json:"bands"`
	Enemy map[string]struct {
		Common    int        `json:"common"`
		T         []int      `json:"t"`
		RareT     int        `json:"rare_t"`
		Rare      ItemID     `json:"rare"`
		Meseta    [2]int     `json:"meseta"`
		RareBands []struct { // AotI
			T  int
			ID ItemID
		} `json:"-"`
	} `json:"enemy"`
	Common []struct {
		IDs []ItemID `json:"ids"`
		T   []int    `json:"t"`
	} `json:"common"`
	Object [][]struct {
		Type   int `json:"type"`
		Chance int `json:"chance"`
		Bands  []struct {
			IDs []ItemID `json:"ids"`
			T   []int    `json:"t"`
		} `json:"bands"`
	} `json:"object"`
	Area map[string]struct {
		Enemy []struct {
			ID ItemID `json:"id"`
			T  []int  `json:"t"`
		} `json:"enemy"`
		Object []struct {
			IDs []ItemID `json:"ids"`
			T   []int    `json:"t"`
		} `json:"object"`
	} `json:"area"`
	Zones map[string]map[string][2]int `json:"zones"` // quest -> zone -> (area, box level)
}

func (t *dropTables) band(level int) int {
	for i := range 9 {
		if level <= t.Bands[i] {
			return i
		}
	}
	return 9
}

func pick(r int, thresholds []int) int {
	for k, v := range thresholds {
		if r <= v {
			return k
		}
	}
	return len(thresholds)
}

// Drop is a roll's result: meseta or an item.
type Drop struct {
	Meseta int
	Item   ItemID
}

// RollEnemy (0xb6b90): the variant's slot table picks nothing, meseta, the area's enemy
// table (row by level band) or rare-else-common.
func (r *Rules) RollEnemy(rng Rand, variant, level int, area int) (Drop, bool) {
	t := r.drops
	if t == nil {
		return Drop{}, false
	}
	e, ok := t.Enemy[strconv.Itoa(variant)]
	if !ok {
		return Drop{}, false
	}
	var out ItemID
	switch pick(rng.Intn(dropRoll), e.T) {
	case 0:
		return Drop{}, false
	case 1:
		lo, hi := e.Meseta[0], e.Meseta[1]
		m := lo
		if hi > lo {
			m += rng.Intn(hi - lo)
		}
		return Drop{Meseta: m}, true
	case 2:
		a, ok := t.Area[strconv.Itoa(area)]
		if !ok {
			return Drop{}, false
		}
		rows, b := a.Enemy, t.band(level)
		x := rng.Intn(dropRoll)
		j := len(rows) - 1
		for k := range len(rows) - 1 {
			if x <= rows[k].T[b] {
				j = k
				break
			}
		}
		out = rows[j].ID
	default:
		if rng.Intn(dropRoll) > e.RareT {
			out = e.Rare
		} else {
			c := t.Common[e.Common]
			out = c.IDs[pick(rng.Intn(dropRoll), c.T)]
		}
	}
	if out == (ItemID{}) {
		return Drop{}, false
	}
	return Drop{Item: out}, true
}

// RollObject (0xb6870): a box's record by drop type and index, its chance, then the
// band's three ids or the area's object table.
func (r *Rules) RollObject(rng Rand, dtype, index, level, area int) (Drop, bool) {
	t := r.drops
	if t == nil || dtype < 0 || dtype >= 5 || index < 0 || index >= len(t.Object[dtype]) {
		return Drop{}, false
	}
	rec := t.Object[dtype][index]
	if rec.Type != dtype || rng.Intn(dropRoll) > rec.Chance {
		return Drop{}, false
	}
	bd := rec.Bands[t.band(level)]
	var out ItemID
	if k := pick(rng.Intn(dropRoll), bd.T); k < 3 {
		out = bd.IDs[k]
	} else {
		a, ok := t.Area[strconv.Itoa(area)]
		if !ok {
			return Drop{}, false
		}
		y := a.Object[dtype]
		out = y.IDs[pick(rng.Intn(dropRoll), y.T)]
	}
	if out == (ItemID{}) {
		return Drop{}, false
	}
	return Drop{Item: out}, true
}

// ZoneArea is a mission zone's (area table, box level), or false.
func (r *Rules) ZoneArea(quest, zone uint32) (int, int, bool) {
	if r.drops == nil {
		return 0, 1, false
	}
	za, ok := r.drops.Zones[strconv.FormatUint(uint64(quest), 10)][strconv.FormatUint(uint64(zone), 10)]
	if !ok {
		return 0, 1, false
	}
	return za[0], za[1], true
}

// Ground is an item lying in a zone.
type Ground struct {
	UUID   uint32
	ID     ItemID
	Qty    int
	Map    uint32
	Pos    []byte // 12 bytes xyz
	Meseta bool
}

// GroundItem makes a ground item from a drop, or false for an id this disc lacks.
func (r *Rules) GroundItem(uuid uint32, d Drop, mapID uint32, pos []byte) (Ground, bool) {
	if d.Meseta > 0 {
		return Ground{UUID: uuid, ID: mesetaID, Qty: d.Meseta, Map: mapID, Pos: pos, Meseta: true}, true
	}
	id := d.Item
	if _, ok := r.items[id]; !ok {
		id[3] = 0
		if _, ok := r.items[id]; !ok {
			return Ground{}, false
		}
	}
	return Ground{UUID: uuid, ID: id, Qty: 1, Map: mapID, Pos: pos}, true
}

// Item20 is the 0x20-byte item the client carries (variable record from +4).
func (r *Rules) Item20(g Ground) []byte {
	if g.Meseta {
		b := binary.LittleEndian.AppendUint32(nil, g.UUID)
		b = append(b, mesetaID[:]...)
		b = binary.LittleEndian.AppendUint32(b, uint32(g.Qty))
		return append(b, make([]byte, 20)...)
	}
	return r.VarRec(&Item{UUID: g.UUID, ID: g.ID, Qty: g.Qty}, r.disc)[4:]
}

// GroundRecord is the 0x74-byte 0x0A02 / 0x0A03 record: map, name, price, constants,
// the item, xyz.
func (r *Rules) GroundRecord(g Ground) []byte {
	b := binary.LittleEndian.AppendUint16(nil, uint16(int16(g.Map)))
	if g.Meseta {
		b = append(b, make([]byte, 0x2E+4+20)...)
	} else {
		c := r.ConstRec(&Item{UUID: g.UUID, ID: g.ID, Qty: 1})
		b = append(append(b, c[:0x2E]...), c[0x30:0x48]...)
	}
	return append(append(b, r.Item20(g)...), g.Pos...)
}
