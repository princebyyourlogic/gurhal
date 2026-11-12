// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"bytes"
	"encoding/binary"
	"unicode/utf16"
)

const (
	MaxItems = 60
	MaxPAs   = 36

	catWeapon = 1
	techStyle = 3 // constants byte 0xE: technique weapons (rods, wands)
	monomate  = "03010000"
)

var stackCats = map[int]bool{3: true, 7: true, 12: true, 15: true} // consumables, materials, traps, grinders

func mustID(s string) ItemID {
	var id ItemID
	if err := id.UnmarshalText([]byte(s)); err != nil {
		panic(err)
	}
	return id
}

// Starter is the offline new-game pack (DE_ITEMPACK playerItemPack.snr): the race's
// manufacturer's weapon pair and shield, Monomate x5, Goggles, Photon Eraser and
// Reflector, and the PA Rising Strike.
func (r *Rules) Starter(raw []byte, next uint32) ([]Item, []PA, uint32) {
	m := [4]byte{0, 1, 0, 2}[raw[RawRace]&3] // GRM human / cast, Yohmei newman, Tenora beast
	ids := []ItemID{{0x01, 0x09, 0, m}, {0x01, 0x14, 0, m}, {0x02, 0x01, 0, m},
		mustID(monomate), mustID("11010000"), mustID("11020000"), mustID("11020100")}
	var inv []Item
	for _, id := range ids {
		if _, ok := r.items[id]; !ok {
			continue
		}
		qty := 1
		if id == mustID(monomate) {
			qty = 5
		}
		inv = append(inv, Item{UUID: next, ID: id, Qty: qty})
		next++
	}
	return inv, []PA{{ID: mustID("04090600")}}, next
}

// Stackable is the client's rule (0xafb70): categories 3, 7, 12 and 15 stack unless id
// byte 3 has 0x40.
func (r *Rules) Stackable(id ItemID) bool {
	it, ok := r.items[id]
	return ok && stackCats[it.Cat] && id[3]&0x40 == 0
}

// StackMax is the most one stack holds (variable record +0x10).
func (r *Rules) StackMax(id ItemID) int {
	if !r.Stackable(id) {
		return 1
	}
	it := r.items[id]
	m := 99
	switch it.Cat {
	case 3:
		m = int(it.Consts[1])
	case 12:
		if len(it.Rec) > 2 {
			m = int(it.Rec[2])
		} else {
			m = int(it.Consts[7])
		}
	case 15:
		m = int(it.Consts[0])
	}
	return max(1, min(m, 99))
}

// MaxPP is a weapon's PP: its constants plus grinding.
func (r *Rules) MaxPP(s *Item) int {
	it, ok := r.items[s.ID]
	if !ok || it.Cat != catWeapon {
		return 0
	}
	return max(0, int(binary.LittleEndian.Uint16(it.Consts))+s.PPBonus)
}

func (r *Rules) CurPP(s *Item) int {
	mx := r.MaxPP(s)
	if s.PP == nil {
		return mx
	}
	return max(0, min(*s.PP, mx))
}

// VarRec is an item's 0x24-byte variable record as a recipient's client may read it.
func (r *Rules) VarRec(s *Item, recipient string) []byte {
	it := r.items[s.ID]
	c := it.Consts
	b := make([]byte, 0x24)
	b[0], b[1] = s.Active, s.Slots
	binary.LittleEndian.PutUint32(b[4:], s.UUID)
	copy(b[8:], s.ID[:])
	switch {
	case it.Cat == catWeapon:
		binary.LittleEndian.PutUint16(b[0x10:], uint16(r.CurPP(s)))
		binary.LittleEndian.PutUint16(b[0x12:], uint16(r.MaxPP(s)))
		binary.LittleEndian.PutUint16(b[0x14:], uint16(int16(s.ATPBonus)))
		b[0x16] = byte(s.Grind<<4) | c[0xF]&0xF
		b[0x17] = it.Rarity0
		b[0x18], b[0x19], b[0x1A], b[0x1B] = c[0xB], c[0xC], c[0xE], c[0xD]
		if s.Fails > 0 && recipient == AotI { // AotI keeps grind failures in +0x1B's high nibble
			b[0x1B] = c[0xD] + byte(min(s.Fails, 15)<<4)
		}
		if c[0xE] == techStyle {
			for i, t := range s.Techs[:min(len(s.Techs), 4)] {
				b[0x1C+2*i], b[0x1D+2*i] = byte(t[0]), PALevel(t[1], recipient)
			}
		} else {
			b[0x1C], b[0x1D], b[0x1E] = s.ID[1], byte(s.PA[0]), PALevel(s.PA[1], recipient)
		}
	case r.Stackable(s.ID):
		binary.LittleEndian.PutUint32(b[0xC:], uint32(s.Qty))
		binary.LittleEndian.PutUint32(b[0x10:], uint32(r.StackMax(s.ID)))
	default:
		binary.LittleEndian.PutUint32(b[0xC:], uint32(s.Qty))
	}
	return b
}

// ConstRec is an item's 0x48-byte constants record: UTF-16 name, rarity, category,
// price, the 20-byte constants block.
func (r *Rules) ConstRec(s *Item) []byte {
	it := r.items[s.ID]
	name := make([]byte, 0x2E)
	n := 0
	for _, u := range utf16.Encode([]rune(it.Name)) {
		if n+2 > 0x2C {
			break
		}
		binary.LittleEndian.PutUint16(name[n:], u)
		n += 2
	}
	b := append(name, it.Rarity0, byte(it.Cat))
	b = binary.LittleEndian.AppendUint32(b, it.Price)
	return append(b, it.Consts...)
}

// paParam is one learned PA's 0x30-byte entry: s16 ATP %, s16 ATA %, u32 PA EXP, base
// parameters for its level.
func (r *Rules) paParam(p PA) []byte {
	key := p.ID
	key[3] = 0
	e, ok := r.pa[key]
	out := make([]byte, 0x30)
	if !ok {
		return out
	}
	l := min(int(p.ID[3]), len(e.Pct)-1)
	binary.LittleEndian.PutUint16(out, uint16(e.Pct[l][0]))
	binary.LittleEndian.PutUint16(out[2:], uint16(e.Pct[l][1]))
	binary.LittleEndian.PutUint32(out[4:], p.EXP)
	copy(out[8:], e.Base[min(l/10, len(e.Base)-1)])
	return out
}

// Inventory is the 0x0A0A body after its header: counts, item palette, PA list,
// variable and constants records, PA parameters, default bullets, and fixed tails.
func (r *Rules) Inventory(st *State, recipient string) []byte {
	pas := make([]PA, len(st.PAs))
	for i, p := range st.PAs {
		pas[i] = PA{EXP: p.EXP, ID: PAID(p.ID, recipient)}
	}
	var b bytes.Buffer
	b.Write([]byte{byte(len(st.Inv)), byte(len(pas)), 6, 0})
	b.Write(make([]byte, 8)) // two f32 0.0
	b.Write(padded(st.Palette, 24, 0))
	var list, vars, consts, params []byte
	for _, p := range pas {
		list = append(binary.LittleEndian.AppendUint32(list, p.EXP), p.ID[:]...)
	}
	for i := range st.Inv {
		vars = append(vars, r.VarRec(&st.Inv[i], recipient)...)
		consts = append(consts, r.ConstRec(&st.Inv[i])...)
	}
	for _, p := range pas[:min(len(pas), MaxPAs)] {
		params = append(params, r.paParam(p)...)
	}
	b.Write(padded(list, 0x120, 0))
	b.Write(padded(vars, 0x870, 0))
	b.Write(padded(consts, 0x10E0, 0))
	b.Write(padded(params, 0x6C0, 0))
	for _, bl := range r.bullets {
		b.Write(padded(bl, 0x1C, 0))
	}
	b.Write(make([]byte, 0x60))
	b.Write(bytes.Repeat([]byte{0x44}, 200))
	for _, v := range []uint16{10000, 10000, 10000, 1000} {
		b.Write(binary.LittleEndian.AppendUint16(nil, v))
	}
	b.Write(bytes.Repeat([]byte{0x44}, 16))
	return b.Bytes()
}

// UUIDs is the 0x0A06 list: every item's UUID in slot order, 0xff padded to 60.
func UUIDs(inv []Item) []byte {
	var b []byte
	for _, s := range inv {
		b = binary.LittleEndian.AppendUint32(b, s.UUID)
	}
	return padded(b, 240, 0xFF)
}

// padded is b cut or filled with fill to n bytes.
func padded(b []byte, n int, fill byte) []byte {
	out := bytes.Repeat([]byte{fill}, n)
	copy(out, b)
	return out
}
