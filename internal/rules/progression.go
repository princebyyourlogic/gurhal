// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/binary"
	"math"
)

// The 0x0D02 creation body (kept as the character's raw record).
const (
	RawName       = 0x28 // UTF-16, 64 bytes
	RawRace       = 0x68
	RawGender     = 0x69
	RawClass      = 0x6A
	RawAppearance = 0x6B // 97 bytes
	AppearanceLen = 97
	RawMin        = RawAppearance + AppearanceLen

	retailCurve      = "retail-1"
	psuClientTypeTop = 10 // a PSU client has 10 type-level rows per type
	noLevel          = 100_000_000
	maxStat          = 0xFFFF
)

// NewState is a new character: level 1 in the type chosen at creation (a type this
// disc does not offer starts as Hunter), 1000 meseta, the offline starter pack.
func (r *Rules) NewState(raw []byte) *State {
	cls := int(raw[RawClass])
	if cls >= r.ClassCount() {
		cls = 0
	}
	st := &State{Cls: cls, Level: 1, Meseta: 1000, Luck: 3}
	for i := range st.TypeLevel {
		st.TypeLevel[i] = 1
	}
	if r.set.retailCurve() {
		st.TypeCurve = retailCurve
	}
	st.HP = r.Derived(raw, st).MaxHP
	st.Inv, st.PAs, st.NextUUID = r.Starter(raw, 1)
	return st
}

type Stats struct {
	MaxHP int
	Stats [8]int // ATP, DFP, ATA, EVP, STA, (hidden), TP, MST
}

// Derived is max HP and the stats for the character's level, type and type level
// (client 0x1f4ac0): base * type % * race % // 10000.
func (r *Rules) Derived(raw []byte, st *State) Stats {
	lv := r.ply.Levels[int(raw[RawRace])*2+int(raw[RawGender])]
	row := lv[min(st.Level, len(lv))-1]
	rows := r.typeRows(st.Cls)
	trow := rows[min(st.TypeLevel[st.Cls], len(rows))-1]
	k := trow[10+int(raw[RawRace])]
	out := Stats{MaxHP: (row[1] & 0xFFFF) * trow[1] * k / 10000}
	for i := range out.Stats {
		out.Stats[i] = (row[2+i] & 0xFFFF) * trow[2+i] * k / 10000
	}
	return out
}

// LevelCap is the highest level with a row in this disc's table (PSU 100, AotI 200).
func (r *Rules) LevelCap() int {
	if r.disc == PSU {
		return psuLevels
	}
	top := psuLevels
	for i, row := range r.ply.Levels[0] {
		if row[1] != 0 {
			top = i + 1
		}
	}
	return top
}

// ExpForLevel is the running EXP that reaches level (client 0x1f4bd0).
func (r *Rules) ExpForLevel(raw []byte, level int) uint32 {
	if level > r.LevelCap() {
		return noLevel
	}
	row := r.ply.Levels[int(raw[RawRace])*2+int(raw[RawGender])][level-1]
	if level >= 2 && row[0] == 0 {
		return noLevel
	}
	return uint32(row[0])
}

// VisibleClass is the type byte a recipient's client is shown: a PSU client has no
// AotI types, so it sees the PSU type nearest in role.
func VisibleClass(cls int, recipient string) uint8 {
	if recipient == AotI || cls < psuClasses {
		return uint8(cls)
	}
	return map[int]uint8{10: 0, 11: 5, 12: 6, 13: 7, 14: 8, 15: 3}[cls]
}

func visibleTypeLevel(tl int, recipient string) int {
	if recipient == PSU {
		return min(tl, psuClientTypeTop)
	}
	return tl
}

// LevelBlock is char_bin's 112-byte level block: u16 level, 0, u16 blast, s8 luck,
// 5 zero, u32 EXP, 4 zero, u32 meseta, u32 play time, 20 zero, 16 x (type level | type
// EXP << 16).
func LevelBlock(st *State, recipient string) []byte {
	b := make([]byte, 0, 112)
	b = binary.LittleEndian.AppendUint16(b, uint16(st.Level))
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, uint16(st.Blast))
	b = append(b, byte(int8(st.Luck)))
	b = append(b, make([]byte, 5)...)
	b = binary.LittleEndian.AppendUint32(b, st.EXP)
	b = append(b, make([]byte, 4)...)
	b = binary.LittleEndian.AppendUint32(b, st.Meseta)
	b = binary.LittleEndian.AppendUint32(b, st.Playtime)
	b = append(b, make([]byte, 20)...)
	for i := range 16 {
		w := uint32(visibleTypeLevel(st.TypeLevel[i], recipient))&0xFFFF | uint32(st.TypeEXP[i]&0xFFFF)<<16
		b = binary.LittleEndian.AppendUint32(b, w)
	}
	return b
}

// StatusBlock is the 0x164 bytes after char_bin in a user record, laid out for the
// recipient's disc (AotI reads EGS's PC layout).
func (r *Rules) StatusBlock(raw []byte, st *State, recipient string) []byte {
	d := r.Derived(raw, st)
	stats := make([]byte, 0, 16)
	for _, s := range d.Stats {
		stats = binary.LittleEndian.AppendUint16(stats, uint16(min(s, maxStat)))
	}
	hp := min(st.HP, d.MaxHP)
	one := binary.LittleEndian.AppendUint32(nil, math.Float32bits(1))
	b := binary.LittleEndian.AppendUint32(nil, r.ExpForLevel(raw, st.Level+1))
	b = binary.LittleEndian.AppendUint32(b, r.ExpForLevel(raw, st.Level))
	b = binary.LittleEndian.AppendUint32(b, uint32(d.MaxHP))
	b = append(b, stats...)
	hpBlock := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, uint32(hp)), uint32(d.MaxHP))
	if recipient == AotI {
		b = append(b, make([]byte, 12)...)
		b = binary.LittleEndian.AppendUint32(b, uint32(st.Level&0xFF))
		b = append(append(append(b, stats...), hpBlock...), make([]byte, 168)...)
		for range 4 {
			b = append(append(b, one...), make([]byte, 8)...)
		}
		b = append(append(append(append(b, one...), make([]byte, 20)...), one...), make([]byte, 44)...)
	} else {
		b = append(b, make([]byte, 16)...)
		b = append(b, byte(st.Level), 0, 0, 0)
		b = append(append(append(b, stats...), hpBlock...), make([]byte, 168)...)
		for range 5 {
			b = append(append(b, one...), make([]byte, 8)...)
		}
		b = append(append(append(b, make([]byte, 20)...), one...), make([]byte, 32)...)
	}
	return b
}
