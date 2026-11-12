// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/binary"
	"math"
)

// The character records every player view is built from (EGS character_to_binary and
// character_user_to_binary, PS2 layouts).

const (
	charBinLen  = 276
	UserRecLen  = 712
	SlotRecLen  = 348
	spaceport   = 1104000
	dirScale    = 182.0416 // degrees -> the client's 16-bit direction
	appearInBin = 0x43     // the appearance block inside char_bin
)

// Area is a quest, zone and map.
type Area struct {
	Quest uint32
	Zone  uint32
	Map   uint32
}

// Pos is where a player stands: x, y, z and the client's 16-bit direction.
type Pos struct {
	X, Y, Z float32
	Dir     uint16
}

// CharBin is the 276-byte character: name, race, gender, type, appearance, level block.
// recipient: the disc that reads it.
func (r *Rules) CharBin(raw []byte, st *State, recipient string) []byte {
	race, gender := raw[RawRace], raw[RawGender]
	app := raw[RawAppearance : RawAppearance+AppearanceLen]
	if recipient == PSU {
		// accounts are shared by both discs: an AotI-made character loaded on PSU must not carry
		// AotI-only parts or voices (PSU's part lookup has no NULL check)
		app = r.set.AppearanceFor(app, race, gender, PSU, AotI)
	}
	b := make([]byte, 0, charBinLen)
	b = append(b, raw[RawName:RawRace]...)
	b = append(b, race, gender, VisibleClass(st.Cls, recipient))
	b = append(b, app...)
	return append(b, LevelBlock(st, recipient)...)
}

// SlotRecord is one 348-byte 0x0D03 character-list slot; nil raw is an empty slot.
func (r *Rules) SlotRecord(raw []byte, st *State) []byte {
	out := make([]byte, SlotRecLen)
	if raw != nil {
		out[1] = 1
		copy(out[8:], r.CharBin(raw, st, r.disc))
	}
	return out
}

// User is the 712-byte user record of a player: an 80-byte location part, char_bin,
// the status block. recipient reads it; owner is the record's player's disc ("" for a
// player's own record).
func (r *Rules) User(gid uint32, lid uint16, raw []byte, st *State, a Area, entry uint16, p Pos, recipient, owner string) []byte {
	if owner != "" && owner != recipient && a.Quest == spaceport && (a.Map == 900 || a.Map == 901) {
		a.Map = 900 // the shared Spaceport is map 900 on PSU and 901 on AotI
		if recipient == AotI {
			a.Map = 901
		}
	}
	b := make([]byte, 0, UserRecLen)
	b = append(b, 0x00, 0x00, 0x12, 0x00)
	b = binary.LittleEndian.AppendUint32(b, gid)
	b = append(b, make([]byte, 8)...)
	b = binary.LittleEndian.AppendUint16(b, lid)
	b = append(b, make([]byte, 6)...)
	b = binary.LittleEndian.AppendUint32(b, a.Quest)
	b = binary.LittleEndian.AppendUint32(b, a.Zone)
	b = binary.LittleEndian.AppendUint32(b, a.Map)
	b = binary.LittleEndian.AppendUint16(b, entry)
	b = append(b, 0, 0, 0x01, 0x00)
	b = binary.LittleEndian.AppendUint16(b, p.Dir)
	for _, f := range []float32{p.X, p.Y, p.Z} {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(f))
	}
	b = append(b, make([]byte, 24)...)
	b = append(b, r.set.CharFor(r.CharBin(raw, st, recipient), recipient, owner)...)
	return append(b, r.StatusBlock(raw, st, recipient)...)
}

// DirFromDegrees turns a direction in degrees into the client's 16 bits (truncated).
func DirFromDegrees(d float64) uint16 { return uint16(int(d * dirScale)) }

// DirFromWire is a direction a client sent (0x0102 +0x2E) as it goes back out in a user
// record: kept in degrees and scaled back, which can lose one step.
func DirFromWire(d16 uint16) uint16 { return DirFromDegrees(float64(d16) / dirScale) }
