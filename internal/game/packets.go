// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
	"unicode/utf16"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// Layouts from EGS (egs_proto.erl, egs_net.erl; protocol docs public domain), as
// prettystellaruniverse laid them out for the PS2 discs.

var (
	keepalive  = []byte{8, 0, 0, 0, 0, 0, 0, 0} // an empty packet every 5 s once authenticated
	ffff       = []byte{0xff, 0xff, 0xff, 0xff}
	lobbyVar   = wire.Body{}.Bytes([]byte{0, 4, 0, 0}).Zero(20).Bytes([]byte{0, 0x14, 0, 0})
	flagNames  = []string{"PSU_VALUE_FLAG", "PSU_BOOL_FLAG", "PSU_TEMP_FLAG"}
	defaultOpt = append(make([]byte, 16), 4, 0, 0, 0) // EGS egs_char:new, brightness 4
)

const (
	chrwork      = 0x80 // 0x0D01: 222 u32 character-work values from block +0x80
	saveLC       = 170  // chrwork index of SAVE_LC00
	saveQuest    = 176  // chrwork index of AotI's story chapter
	aotiChapter  = 21   // AotI's lobby NPCs pick their lines by the last offline chapter
	chrBlock     = 1016
	blacklistLen = 80 * 0x48 // 80 entries of 0x48
)

func accountFlags(gid uint32) []byte {
	b := wire.Body{}.Zero(4).Bytes(wire.ACC).U32(gid).Zero(8).Bytes(wire.ACC).U32(gid).Zero(8)
	for _, n := range flagNames {
		b = b.Bytes(fixed([]byte(n), 16))
	}
	return wire.Encode(0x0D05, b.U32(1).U32(1).U32(1))
}

func characterList(gid uint32, slots [4][]byte) []byte {
	b := wire.Body{}.Zero(4).Bytes(wire.ACC).U32(gid).Zero(8).Bytes(wire.ACC).U32(gid).Zero(8).Zero(4)
	for _, s := range slots {
		b = b.Bytes(fixed(s, rules.SlotRecLen))
	}
	return wire.Encode(0x0D03, b)
}

// c0d01 is the player's own character: char_bin, the character-work block, options.
func c0d01(gid uint32, charBin []byte, options []byte, work map[int]int32) []byte {
	block := make([]byte, chrBlock)
	for i, v := range work {
		binary.LittleEndian.PutUint32(block[chrwork+4*i:], uint32(v))
	}
	if len(options) == 0 {
		options = defaultOpt
	}
	return wire.Encode(0x0D01, wire.Body{}.Head(wire.NoLID, gid).Bytes(charBin).Bytes(block).Bytes(options))
}

func c1006PartyEvent(gid uint32, event uint8) []byte {
	return wire.Encode(0x1006, wire.Body{}.Head(wire.NoLID, gid).U8(event).Zero(3))
}

func c1005OwnParty(gid uint32, raw []byte, level, hp, maxHP int) []byte {
	be := func(s string) []byte { b, _ := hex.DecodeString(s); return b }
	member := wire.Body{}.Bytes(be("0100ffff")).Bytes(be("0000ff00")).Bytes(be("ffff0000")).Zero(80).Bytes(ffff)
	b := wire.Body{}.Head(wire.NoLID, gid).
		Bytes(be("00000100")).Zero(4).Bytes(ffff).Zero(4).Bytes(be("00011200")).U32(gid).Zero(8).
		Bytes(raw[rules.RawName:rules.RawRace]).U8(uint8(level)).Zero(2).U8(1).Bytes(be("01010000")).Zero(4).Zero(64).
		Bytes(ffff).Zero(12).Bytes(ffff).Zero(8).U32(uint32(hp)).U32(uint32(maxHP)).Zero(80)
	for range 4 {
		b = b.Bytes(member).Zero(96)
	}
	b = b.Bytes(member).Zero(56).Bytes(ffff).Zero(4).Bytes(be("ff020000"))
	for range 6 {
		b = b.Bytes(be("ffff0000"))
	}
	return wire.Encode(0x1005, b.Zero(460))
}

func c0210Time(gid uint32, now time.Time) []byte {
	return wire.Encode(0x0210, wire.Body{}.Head(0, gid).Zero(4).U32(uint32(now.Unix())))
}

func c0222Universe(gid uint32, u universe, players int) []byte {
	return wire.Encode(0x0222, wire.Body{}.U16(wire.NoLID).Zero(2).Bytes(wire.Player).U32(gid).Zero(8).
		Bytes(wire.ACC).U32(gid).Zero(8).U32(u.ID).U16(uint16(players)).U16(1000).
		Bytes(utf16Fixed(u.Name, 44)).U32(100))
}

func c1500OwnCard(gid uint32, raw []byte, slot int) []byte {
	app := raw[rules.RawAppearance:]
	return wire.Encode(0x1500, wire.Body{}.Head(wire.NoLID, gid).Bytes(raw[rules.RawName:rules.RawRace]).
		Bytes([]byte{raw[rules.RawRace], raw[rules.RawGender], raw[rules.RawClass], app[0], app[1]}).Zero(3).
		U32(gid).Zero(28).Zero(352).Bytes([]byte{1, 4, 1, byte(slot)}).Zero(8))
}

func c1501Cards(gid uint32) []byte {
	return wire.Encode(0x1501, wire.Body{}.Head(wire.NoLID, gid).U32(0))
}

func c1512Blacklist(gid uint32) []byte {
	return wire.Encode(0x1512, wire.Body{}.Head(wire.NoLID, gid).Zero(blacklistLen))
}

func c1602NPCs(gid uint32) []byte {
	return wire.Encode(0x1602, wire.Body{}.Head(wire.NoLID, gid).Zero(4).Zero(344).Zero(4).
		Bytes(utf16Fixed("My PM", 64)).Zero(4))
}

func c021bLoadEnd(gid uint32) []byte { return wire.Encode(0x021B, wire.Body{}.Head(0, gid)) }

// c021e is the Uni-cube list: per universe id, players, style 0x283 (name left,
// population right), UTF-16 name in 32 bytes, the population as "%5d".
func c021eUniverses(us []universe, counts map[uint32]int) []byte {
	b := wire.Body{}.Zero(36).U32(uint32(len(us)))
	for _, u := range us {
		n := counts[u.ID]
		b = b.U32(u.ID).U16(uint16(n)).U16(0x283).Bytes(utf16Fixed(u.Name, 32)).
			UTF16(wire.UTF16(fmt.Sprintf("%5d", n))).Zero(2)
	}
	return wire.Encode(0x021E, b)
}

func c0230LeaveArea(gid uint32) []byte {
	return wire.Encode(0x0230, wire.Body{}.U16(wire.NoLID).Zero(2).Bytes(wire.ACC).U32(gid).Zero(8).
		Bytes(wire.ACC).U32(gid).Zero(8))
}

// c0d04Ack answers a 0x0D04 work write: its body echoed with the status byte (+0x36)
// set to 1. Both discs' scripts wait for it.
func c0d04Ack(gid uint32, body []byte) []byte {
	return wire.Encode(0x0D04, wire.Body{}.Zero(20).Bytes(wire.ACC).U32(gid).Zero(8).
		Bytes(body[0x24:0x36]).U8(1).Bytes(body[0x37:0x40]))
}

// --- zone loads -------------------------------------------------------------------------------

func c0c00Quest(lid uint16, gid, quest uint32) []byte {
	return wire.Encode(0x0C00, wire.Body{}.Head(lid, gid).U32(quest).Bytes(fill(0xff, 128)))
}

// c020e is the quest NBL. PSU copies it from +0x30; AotI keeps a word there and copies
// from +0x34 (the EGS/PC layout).
func c020eQuest(disc string, nbl []byte) []byte {
	b := wire.Body{}.U16(wire.NoLID).Zero(34).U32(uint32(len(nbl)))
	if disc == rules.AotI {
		return wire.Encode(0x020E, b.Zero(4).Bytes(nbl))
	}
	return wire.Encode(0x020E, b.Bytes(nbl).Zero(4))
}

func c0a05(lid uint16, gid uint32) []byte { return wire.Encode(0x0A05, wire.Body{}.Head(lid, gid)) }

// c0111 is a player event about p for d (3 incapacitated, 4 healing aura, 6 spawn).
func c0111(dlid uint16, dgid, pgid uint32, plid uint16, event, param uint32) []byte {
	return wire.Encode(0x0111, wire.Body{}.U16(dlid).Zero(6).U32(pgid).Zero(8).Bytes(wire.ACC).U32(dgid).Zero(8).
		U32(pgid).U32(uint32(plid)).U32(event).U32(param))
}

// c010d is a character record about p for d: user record from +80. With an enemy as
// source and more EXP than before it is the client's level-up.
func c010d(dlid uint16, dgid, pgid uint32, plid uint16, user []byte, source uint32, trailer bool) []byte {
	b := wire.Body{}.Head(dlid, dgid).U32(1).Zero(4).Bytes([]byte{0, 0, 3, 0}).Bytes([]byte{0xff, 0xff, 0, 0}).
		Zero(4).U32(pgid).Zero(24).U32(pgid).U32(uint32(plid)).U32(source).Bytes(user[80:])
	if trailer {
		b = b.Zero(4)
	}
	return wire.Encode(0x010D, b)
}

// c0200 is the zone init; var is the lobby or mission variant.
func c0200ZoneInit(lid uint16, gid, zone uint32, v []byte) []byte {
	return wire.Encode(0x0200, wire.Body{}.Head(lid, gid).U16(lid).U16(uint16(zone)).U32(1).Bytes(ffff).
		Bytes(v).Bytes(ffff).Bytes(ffff))
}

func c0215Host(lid uint16, gid, host uint32) []byte {
	return wire.Encode(0x0215, wire.Body{}.Head(lid, gid).U32(host))
}

// c020f is the zone NBL: +0x2C set number (picks set_r<n>.rel), +0x2D season (0xFF none).
func c020fZone(nbl []byte, season uint8) []byte {
	return wire.Encode(0x020F, wire.Body{}.U16(wire.NoLID).Zero(34).U8(0).U8(season).Zero(2).
		U32(uint32(len(nbl))).Bytes(nbl))
}

func c0205Map(dlid uint16, dgid uint32, a rules.Area, entry uint16, nb uint32, plid uint16, seasonal uint8) []byte {
	return wire.Encode(0x0205, wire.Body{}.Head(dlid, dgid).Bytes(ffff).U32(a.Zone).U32(a.Map).U32(uint32(entry)).
		U32(nb).U16(plid).U8(0).U8(seasonal))
}

func c100eLocation(gid uint32, a rules.Area, entry uint16) []byte {
	return wire.Encode(0x100E, wire.Body{}.Bytes([]byte{0xff, 0xff, 0xff, 0xbf}).Zero(16).Bytes(wire.ACC).
		Bytes(binary.BigEndian.AppendUint32(nil, gid)).Zero(8).U8(1).U8(0).
		U16(uint16(a.Zone)).U16(uint16(a.Map)).U16(entry).U32(a.Quest).
		Bytes(utf16Fixed("dammy", 64)).Zero(4).Bytes(ffff).Zero(4))
}

func c1212(gid uint32) []byte {
	return wire.Encode(0x1212, wire.Body{}.Head(wire.NoLID, gid).Zero(2400))
}

// c0201 is a full user record about p for d.
func c0201(dlid uint16, dgid, pgid uint32, user []byte) []byte {
	return wire.Encode(0x0201, wire.Body{}.About(dlid, pgid, dgid).Bytes(user).Zero(4).Zero(76))
}

func c0a06(lid uint16, gid uint32, inv []rules.Item) []byte {
	return wire.Encode(0x0A06, wire.Body{}.U16(lid).Zero(6).U32(gid).Zero(8).Bytes(wire.ACC).U32(gid).Zero(8).
		Bytes(rules.UUIDs(inv)))
}

func c0208Loaded(lid uint16, gid, nb uint32) []byte {
	return wire.Encode(0x0208, wire.Body{}.Head(lid, gid).U32(nb))
}

func c0236Start(lid uint16, gid uint32) []byte {
	return wire.Encode(0x0236, wire.Body{}.Head(lid, gid))
}

func c0203(dlid uint16, dgid, pgid uint32, plid uint16) []byte {
	return wire.Encode(0x0203, wire.Body{}.Head(dlid, dgid).U32(pgid).U32(uint32(plid)))
}

func c0204Unspawn(dlid uint16, dgid, pgid uint32, plid uint16) []byte {
	return wire.Encode(0x0204, wire.Body{}.About(dlid, pgid, dgid).U32(pgid).U32(uint32(plid)).U32(100))
}

// c0233 lists the players already in a zone for a newcomer d. Records are 0x2D0 apart
// for PSU and 0x2CC for AotI; 80 bytes follow the last.
func c0233Players(dlid uint16, dgid uint32, disc string, users [][]byte) []byte {
	pad := 8
	if disc == rules.AotI {
		pad = 4
	}
	b := wire.Body{}.About(dlid, dgid, dgid).U32(uint32(len(users)))
	for _, u := range users {
		b = b.Bytes(u).Zero(pad)
	}
	return wire.Encode(0x0233, b.Zero(80-pad))
}

// castTo forwards a channel-1 command as the sender wrote it, with its GID and LID at
// +0x24 (the PS2 keeps data where EGS's PC rewrites more ids).
func castTo(cmd uint16, body []byte, gid uint32, lid uint16) []byte {
	out := append(append([]byte(nil), body[:0x24]...), wire.Body{}.U32(gid).U32(uint32(lid))...)
	return wire.EncodeOn(wire.ChanCast, cmd, append(out, body[0x2C:]...))
}

// c0304 relays a chat line: the client's body from +0x34 (bubble, face, length,
// channel, UTF-16 text), with the sender forced to p. PS2 has no name field.
func c0304Chat(dlid uint16, dgid, pgid uint32, body []byte) []byte {
	return wire.Encode(0x0304, wire.Body{}.U16(dlid).Zero(2).Bytes(wire.ACC).U32(pgid).Zero(8).
		Bytes(wire.ACC).U32(dgid).Zero(8).Bytes(wire.Player).U32(pgid).Zero(8).Bytes(body[0x34:]))
}

// --- helpers ----------------------------------------------------------------------------------

// fixed is b cut or zero-filled to n bytes.
func fixed(b []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, b)
	return out
}

func fill(c byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = c
	}
	return out
}

// utf16Fixed is s as UTF-16, cut or zero-filled to n bytes.
func utf16Fixed(s string, n int) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return fixed(b, n)
}
