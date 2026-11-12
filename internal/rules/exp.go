// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import "encoding/binary"

// AddEXP adds EXP up to the level cap's total; true when the level rose (HP refills).
func (r *Rules) AddEXP(raw []byte, st *State, amount int) bool {
	top := r.LevelCap()
	st.EXP = uint32(min(int64(st.EXP)+int64(amount), int64(r.ExpForLevel(raw, top))))
	old := st.Level
	for st.Level < top && st.EXP >= r.ExpForLevel(raw, st.Level+1) {
		st.Level++
	}
	if st.Level != old {
		st.HP = r.Derived(raw, st).MaxHP
		return true
	}
	return false
}

// typeCap is the highest type level type EXP can reach (retail 20; else the rows).
func (r *Rules) typeCap(cls int) int {
	t := r.typeTable(cls)
	rows := len(t.Types[cls])
	if t.TypeCaps != nil && cls < len(t.TypeCaps) {
		return min(t.TypeCaps[cls], rows)
	}
	return rows
}

// AddTypeEXP (0x5d6a0) adds type EXP to the current type, clamped to its cap; a type
// already past its cap keeps its level. Returns (old, new) type level.
func (r *Rules) AddTypeEXP(st *State, amount int) (int, int) {
	cls := st.Cls
	rows := r.typeRows(cls)
	limit := r.typeCap(cls)
	old := st.TypeLevel[cls]
	top := max(rows[limit-1][0], st.TypeEXP[cls])
	exp := max(0, min(st.TypeEXP[cls]+amount, top))
	st.TypeEXP[cls] = exp
	level := 1
	for k := 1; k < len(rows); k++ {
		if rows[k][0] <= exp {
			level++
		}
	}
	level = min(level, limit)
	if old <= limit {
		st.TypeLevel[cls] = level
	}
	return old, st.TypeLevel[cls]
}

// CanChangeType (0x0110 event 3): a type this disc offers, not the current one, with its
// requirements met.
func (r *Rules) CanChangeType(st *State, cls int) bool {
	if cls < 0 || cls >= r.ClassCount() || cls == st.Cls {
		return false
	}
	for i, need := range r.typeTable(cls).Requirements[cls] {
		if need > 0 && st.TypeLevel[i] < int(need) {
			return false
		}
	}
	return true
}

// TypesTable is the 0x0113 payload: 16 entries of 0x30, past this disc's types zero.
func (r *Rules) TypesTable(raw []byte, st *State) []byte {
	var out []byte
	for c := range 16 {
		if c >= len(r.ply.Types) || c >= r.ClassCount() {
			out = append(out, make([]byte, 0x30)...)
			continue
		}
		ok := uint32(0)
		if r.CanChangeType(st, c) {
			ok = 1
		}
		out = binary.LittleEndian.AppendUint32(out, ok)
		for _, q := range r.typeTable(c).Requirements[c] {
			out = append(out, byte(q))
		}
		probe := *st
		probe.Cls = c
		d := r.Derived(raw, &probe)
		out = binary.LittleEndian.AppendUint32(out, r.ExpForLevel(raw, st.Level))
		out = binary.LittleEndian.AppendUint32(out, uint32(d.MaxHP))
		for _, s := range d.Stats {
			out = binary.LittleEndian.AppendUint16(out, uint16(min(s, maxStat)))
		}
		rows := r.typeRows(c)
		tl := min(st.TypeLevel[c], len(rows))
		next := 0
		if tl < min(len(rows), r.typeCap(c)) {
			next = rows[tl][0]
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(rows[tl-1][0]))
		out = binary.LittleEndian.AppendUint16(out, uint16(next))
	}
	return out
}
