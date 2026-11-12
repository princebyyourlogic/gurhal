// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import "encoding/binary"

// What one disc's player data may show a player on the other. AotI ships everything PSU
// draws and more, so only an AotI player shown to a PSU recipient is translated.
//
// Appearance block: +0 voice type, +1 pitch, +5 six LE part words (slot << 24 | flags
// << 16 | number << 8 | variant), +0x1D..+0x1F clothing colours. PSU looks a part word
// up with no NULL check, so a word outside PSU's parts.json is replaced by its
// race/gender's first creation choice, and ears/face/hair are replaced together.

const (
	appPartWords = 5
	appColours   = 0x1D
	noPart       = 0xFFFFFFFF
	cast         = 2
	psuPATop     = 29 // PSU's PA tables stop at Lv30
)

type voiceRange struct{ first, n int }

// player voice groups per (cast, gender): PSU's ranges and AotI's additions
var (
	psuVoices  = map[[2]int]voiceRange{{0, 0}: {1, 14}, {0, 1}: {15, 12}, {1, 0}: {27, 12}, {1, 1}: {39, 12}}
	aotiVoices = []voiceRange{{76, 8}, {84, 5}, {89, 8}, {97, 5}}
)

func visibleVoice(voice, race, gender uint8, recipient, owner string) uint8 {
	if recipient != PSU || owner == PSU {
		return voice
	}
	c := 0
	if race == cast {
		c = 1
	}
	g := psuVoices[[2]int{c, int(gender & 1)}]
	for _, a := range aotiVoices {
		if v := int(voice); a.first <= v && v < a.first+a.n {
			return uint8(g.first + (v-a.first)%g.n)
		}
	}
	return voice
}

// AppearanceFor is the 97-byte appearance block as a recipient may be sent it.
func (s *Set) AppearanceFor(block []byte, race, gender uint8, recipient, owner string) []byte {
	if recipient != PSU || owner == "" || owner == PSU {
		return block
	}
	b := append([]byte(nil), block...)
	b[0] = visibleVoice(b[0], race, gender, recipient, owner)
	p := s.psu
	if p.partSet == nil {
		return b
	}
	var words [6]uint32
	var bad [6]bool
	for i := range words {
		words[i] = binary.LittleEndian.Uint32(b[appPartWords+4*i:])
		bad[i] = words[i] != noPart && (int(words[i]>>24) != i || !p.partSet[words[i]])
	}
	if bad[3] || bad[4] || bad[5] {
		bad[3], bad[4], bad[5] = true, true, true
	}
	rg := int(race)*2 + int(gender)
	if rg >= len(p.parts.Defaults) || len(p.parts.Defaults[rg]) < 6 {
		return b
	}
	dflt := p.parts.Defaults[rg]
	for i := range words {
		if bad[i] && words[i] != dflt[i] {
			binary.LittleEndian.PutUint32(b[appPartWords+4*i:], dflt[i])
			if i < 3 {
				b[appColours+i] = 0
			}
		}
	}
	return b
}

// CharFor is an owner's char_bin as a recipient may see it ("" owner: a player's own
// record, never changed).
func (s *Set) CharFor(cb []byte, recipient, owner string) []byte {
	if recipient != PSU || owner == "" || owner == PSU {
		return cb
	}
	out := append([]byte(nil), cb[:appearInBin]...)
	out = append(out, s.AppearanceFor(cb[appearInBin:appearInBin+AppearanceLen], cb[0x40], cb[0x41], recipient, owner)...)
	return append(out, cb[appearInBin+AppearanceLen:]...)
}

// PALevel is a PA level byte as a recipient may see it (PSU stops at Lv30).
func PALevel(level int, recipient string) byte {
	if recipient == PSU {
		return byte(min(level, psuPATop))
	}
	return byte(level)
}

// PAID is a PA list id as a recipient may see it.
func PAID(id ItemID, recipient string) ItemID {
	if recipient == PSU && id[3] > psuPATop {
		id[3] = psuPATop
	}
	return id
}
