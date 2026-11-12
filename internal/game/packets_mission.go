// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

const noSource = 0xFFFFFFFF // 0x010D: no EXP source, the client shows nothing

var missionVar = wire.Body{}.Bytes([]byte{6, 0, 5, 0}).Bytes([]byte{1, 0, 0, 0}).Zero(8).Bytes([]byte{0, 4, 0, 0}).
	Bytes([]byte{0, 1, 0, 0}).Bytes([]byte{0, 0x14, 0, 0})

// roomVar is the My Room 0x0200 variant.
var roomVar = wire.Body{}.Bytes([]byte{6, 0, 0, 0, 2, 0, 0, 0}).Zero(8).Bytes([]byte{0x40, 0, 0, 0}).
	Bytes([]byte{0, 1, 0, 0}).Bytes([]byte{0, 1, 0, 0})

// head is the common opening addressed to member d.
func head(d *member) wire.Body { return wire.Body{}.Head(d.lid, d.p.gid) }

// --- counters and missions ---------------------------------------------------------------------------

func c1711CounterBG(gid uint32) []byte {
	return wire.Encode(0x1711, wire.Body{}.Head(0, gid).Bytes([]byte{255, 0, 0, 0}))
}

// c0c06 is a counter's table: PSU +0x24 1, 0, s16 option count, u32 NBL size, the option
// bytes (padded to 4), the counter table NBL.
func c0c06Table(gid uint32, opts, nbl []byte) []byte {
	pad := (4 - len(opts)%4) % 4
	return wire.Encode(0x0C06, wire.Body{}.Head(wire.NoLID, gid).U8(1).U8(0).U16(uint16(len(opts))).
		U32(uint32(len(nbl))).Bytes(opts).Zero(pad).Bytes(nbl))
}

func c0c10Options(gid uint32, opts []byte) []byte {
	return wire.Encode(0x0C10, wire.Body{}.Head(wire.NoLID, gid).U8(1).U8(0).U16(uint16(len(opts))).Bytes(opts))
}

func c170cLocations(gid uint32, table []byte) []byte {
	return wire.Encode(0x170C, wire.Body{}.Head(wire.NoLID, gid).Bytes(table))
}

func c1020(gid uint32) []byte { return wire.Encode(0x1020, wire.Body{}.Head(wire.NoLID, gid)) }

// c1015 is the "Mission start" banner with the quest NBL.
func c1015(gid, qid uint32, nbl []byte) []byte {
	return wire.Encode(0x1015, wire.Body{}.Head(wire.NoLID, gid).U32(qid).Bytes([]byte{1, 1, 0, 0}).U32(qid).
		U32(uint32(len(nbl))).Bytes(nbl))
}

// c0c02 answers an accept: 0 accepted, 4 prerequisites, other rejected.
func c0c02(gid uint32, result uint8) []byte {
	return wire.Encode(0x0C02, wire.Body{}.Head(wire.NoLID, gid).U8(result).Zero(3))
}

// c0c09 is a mission event: 0 start, 1 clear, 2 fail; +0x28 elapsed seconds.
func c0c09(gid uint32, lid uint16, event uint8, secs uint32) []byte {
	return wire.Encode(0x0C09, wire.Body{}.Head(lid, gid).U8(event).Zero(3).U32(secs))
}

func c0c09Results(gid uint32, lid uint16, secs uint32, block []byte) []byte {
	return wire.Encode(0x0C09, wire.Body{}.Head(lid, gid).Bytes([]byte{1, 1, 0, 0}).U32(secs).Bytes(block))
}

// c0c12 completes an AotI story script's item query (0x0C11 / 0x0C13) with success.
func c0c12Seq(cmd uint16, gid, wait uint32) []byte {
	return wire.Encode(cmd, wire.Body{}.Head(0, gid).U32(wait).U32(1))
}

func c1006(d *member, event, slot uint8) []byte {
	return wire.Encode(0x1006, head(d).U8(event).U8(slot).Zero(2))
}

// --- instance flags and objects ----------------------------------------------------------------------

func c1201(d *member, body []byte) []byte {
	return wire.Encode(0x1201, wire.Body{}.Head(wire.NoLID, d.p.gid).Bytes(body[0x24:0x3C]))
}

func c1202Ready(d *member) []byte { return wire.Encode(0x1202, head(d).U32(0).U32(0)) }

func c1204FlagsB(d *member, b []byte) []byte {
	return wire.Encode(0x1204, head(d).U32(0).U32(uint32(len(b))).Bytes(b))
}

func c1206FlagsA(d *member, a []byte) []byte {
	return wire.Encode(0x1206, head(d).U32(0).U32(uint32(len(a))).Bytes(a))
}

func c1205Flag(d *member, fid, mode uint32, b bool) []byte {
	cmd := uint16(0x1205)
	if b {
		cmd = 0x1203
	}
	return wire.Encode(cmd, head(d).U32(fid).U32(mode))
}

func c1213Object(d *member, oid int, state uint32) []byte {
	return wire.Encode(0x1213, head(d).U16(uint16(int16(oid))).U16(0).U32(state))
}

// --- combat ------------------------------------------------------------------------------------------

func c0e07Hits(d *member, entries [][]byte) []byte {
	b := head(d).U32(uint32(len(entries)))
	for _, e := range entries {
		b = b.Bytes(e)
	}
	return wire.Encode(0x0E07, b)
}

// c0e08 is an enemy's status image: PSU 0x114 bytes from +0x28 (first word skipped),
// AotI 0x110, every field 4 bytes earlier.
func c0e08Enemy(d *member, e *rules.Enemy) []byte {
	img := wire.Body{}.Zero(4).U32(0).U32(0).U32(0).U8(uint8(e.Level)).Zero(3)
	for _, s := range e.Stats {
		img = img.U16(uint16(int16(s)))
	}
	img = img.U32(uint32(e.HP)).U32(uint32(e.MaxHP))
	img = append(img, make([]byte, 0x114-len(img))...)
	if d.p.disc == rules.AotI {
		img = img[4:]
	}
	return wire.Encode(0x0E08, head(d).U32(e.ID).Bytes(img))
}

// c0117 is a player's HP and status refresh about p for d.
func c0117(d *member, pgid uint32, plid uint16, se2 uint32, hp int) []byte {
	return wire.Encode(0x0117, head(d).U32(pgid).U32(uint32(plid)).U32(0).U32(se2).U32(uint32(hp)).Zero(4))
}

// c1022 is a party member's HP for the HUD.
func c1022HP(gid uint32, lid uint16, hp int, slot int) []byte {
	return wire.Encode(0x1022, wire.Body{}.Head(lid, gid).U32(uint32(hp)).U8(uint8(slot)).Zero(3))
}

// c0115 is EXP / meseta: the recipient's level block, naming the enemy (0xFFFFFFFF none).
func c0115(gid uint32, lid uint16, enemy uint32, levelBlock []byte) []byte {
	return wire.Encode(0x0115, wire.Body{}.Head(lid, gid).U32(gid).U32(uint32(lid)).U32(enemy).Bytes(levelBlock))
}

// c0108 is a PA's EXP / level-up about p for d.
func c0108(d *member, pgid uint32, plid uint16, pa rules.PA, up bool) []byte {
	id := rules.PAID(pa.ID, d.p.disc)
	flag := uint8(0)
	if up && id == pa.ID {
		flag = 1
	}
	return wire.Encode(0x0108, head(d).U32(pgid).U32(uint32(plid)).U8(flag).Zero(3).U32(pa.EXP).Bytes(id[:]))
}

// --- items -------------------------------------------------------------------------------------------

func itemHead(gid uint32, lid uint16) wire.Body {
	return wire.Body{}.U16(lid).Zero(6).U32(gid).Zero(8).Bytes(wire.ACC).U32(gid).Zero(8)
}

// c0105Reply confirms a C>S 0x0105 (target, index, event, category, A, UUID); extra is
// the weapon block on an equip.
func c0105Reply(gid uint32, lid uint16, req []byte, extra []byte) []byte {
	tgt := binary.LittleEndian.Uint32(req[0x24:])
	return wire.Encode(0x0105, wire.Body{}.U16(lid).Zero(6).U32(tgt).Zero(8).Bytes(wire.ACC).U32(gid).Zero(8).
		Bytes(req[0x24:0x34]).Bytes(extra))
}

// c0105PP sets a weapon's PP to what the server holds (event 0x0A, flag 8).
func c0105PP(gid uint32, lid uint16, uuid uint32, pp int) []byte {
	return wire.Encode(0x0105, itemHead(gid, lid).U32(gid).U32(uint32(lid)).U8(0).U8(0x0A).U8(0).U8(8).
		U32(uuid).U32(uint32(int32(pp))))
}

func c0106Add(r *rules.Rules, disc string, gid uint32, lid uint16, s rules.Item) []byte {
	return wire.Encode(0x0106, wire.Body{}.Head(lid, gid).U32(gid).U32(uint32(lid)).Bytes([]byte{0, 0x0B, 0, 0}).
		Bytes(ffff).Bytes(r.VarRec(&s, disc)[4:]).Bytes(r.ConstRec(&s)))
}

func c0106Remove(r *rules.Rules, disc string, gid uint32, lid uint16, s rules.Item) []byte {
	return wire.Encode(0x0106, wire.Body{}.Head(lid, gid).U32(gid).U32(uint32(lid)).Bytes([]byte{0, 0x0C, 0, 0}).
		Bytes(ffff).Bytes(r.VarRec(&s, disc)[4:]))
}

// c0106Pickup tells d that picker took ground item g (fail: 1 inventory full, to the
// picker only).
func c0106Pickup(d *member, r *rules.Rules, pgid uint32, plid uint16, g rules.Ground, fail uint8) []byte {
	tail := make([]byte, 0x48)
	if !g.Meseta && fail == 0 {
		tail = r.ConstRec(&rules.Item{UUID: g.UUID, ID: g.ID, Qty: g.Qty})
	}
	code := uint8(0)
	if fail != 0 {
		code = 3
	}
	return wire.Encode(0x0106, head(d).U32(pgid).U32(uint32(plid)).Bytes([]byte{0, 6, code, fail}).Bytes(ffff).
		Bytes(r.Item20(g)).Bytes(tail))
}

func c0a02Ground(d *member, r *rules.Rules, g rules.Ground) []byte {
	return wire.Encode(0x0A02, head(d).Bytes(r.GroundRecord(g)))
}

func c0a03Ground(d *member, r *rules.Rules, gs []rules.Ground) []byte {
	b := head(d).U32(uint32(len(gs)))
	for _, g := range gs {
		b = b.Bytes(r.GroundRecord(g))
	}
	return wire.Encode(0x0A03, b)
}

func c0113Types(gid uint32, table []byte) []byte {
	return wire.Encode(0x0113, wire.Body{}.Zero(8).U32(gid).Zero(8).Bytes(wire.ACC).U32(gid).Zero(8).U32(gid).Zero(4).
		Bytes(table))
}

// --- parties -----------------------------------------------------------------------------------------

var areaNames = map[uint32]string{colonyQuest: "Colony", 1101000: "Parum", 1102000: "Neudaiz", 1103000: "Moatoob",
	spaceportQuest: "PPT Spaceport", myRoomQuest: "My Room"}

func locationText(q uint32) string {
	if n, ok := areaNames[q]; ok {
		return n
	}
	if q != 0 {
		return "On a mission"
	}
	return ""
}

func locText(q uint32) []byte {
	var clean []rune
	for _, r := range locationText(q) {
		if r != '%' {
			clean = append(clean, r)
		}
	}
	return fixed(utf16Fixed(string(clean), 0x3E), 0x40)
}

// memberRecord is a party member's 0xC0-byte record as a recipient's disc sees it.
type memberInfo struct {
	gid              uint32
	disc             string
	raw              []byte
	level, hp, maxHP int
	where            place
}

func memberRecord(m memberInfo, slot int, recipient string) []byte {
	a := mapFor(m.where.area, recipient, m.disc)
	if m.where.area == (rules.Area{}) {
		a = rules.Area{Quest: 0xFFFFFFFF}
	}
	return wire.Body{}.Bytes([]byte{0, 1, 0x12, 0}).U32(m.gid).Zero(8).Bytes(m.raw[rules.RawName:rules.RawRace]).
		U16(uint16(m.level)).U16(0xFFFF).Bytes([]byte{1, 1, byte(slot), 1}).U16(0xFFFF).U16(0).
		Bytes(locText(m.where.area.Quest)).U32(a.Quest).U32(a.Zone).U32(a.Map).U32(uint32(m.where.entry)).
		U32(uint32(m.where.lid)).Bytes(ffff).Bytes([]byte{0xFF, 0, 0, 0}).U32(uint32(m.hp)).U32(uint32(m.maxHP))
}

func partyHead(gid uint32, lid uint16) wire.Body { return wire.Body{}.Head(lid, gid) }

func c1004Member(gid uint32, lid uint16, rec []byte) []byte {
	return wire.Encode(0x1004, partyHead(gid, lid).Bytes(rec).Zero(0x130-0x24-0xC0))
}

// c1005Party is the whole party for one member: its slot, the leader, six records.
func c1005Party(gid uint32, lid uint16, slot, leader int, recs [6][]byte) []byte {
	empty, tail := soloParts()
	b := partyHead(gid, lid).U8(uint8(slot)).U8(uint8(leader)).U8(1).U8(0).Zero(4).Bytes(ffff).Zero(4)
	for _, r := range recs {
		if r == nil {
			r = empty
		}
		b = b.Bytes(r)
	}
	return wire.Encode(0x1005, b.Bytes(tail))
}

// c1005Solo is a player alone (EGS's own-party record at its defaults).
func c1005Solo(gid uint32, lid uint16, raw []byte) []byte {
	solo := c1005OwnParty(gid, raw, 1, 100, 100)
	return wire.Encode(0x1005, partyHead(gid, lid).Bytes(solo[8+0x24:]))
}

// soloParts are EGS's empty member record and the tail, cut from the solo 1005.
func soloParts() ([]byte, []byte) {
	b := c1005OwnParty(0, make([]byte, 0x200), 1, 100, 100)[8:]
	return b[0xF4 : 0xF4+0xC0], b[0x4B4:]
}

func c1006Party(gid uint32, lid uint16, event, slot uint8) []byte {
	return wire.Encode(0x1006, partyHead(gid, lid).U8(event).U8(slot).Zero(2))
}

func c100aInvite(gid uint32, lid uint16, inviter uint32, raw []byte) []byte {
	return wire.Encode(0x100A, partyHead(gid, lid).Bytes([]byte{0, 1, 0x12, 0}).U32(inviter).Zero(8).
		Bytes(raw[rules.RawName:rules.RawRace]))
}

func c100cResult(gid uint32, lid uint16, body []byte) []byte {
	return wire.Encode(0x100C, partyHead(gid, lid).Bytes(body[0x24:0x78]))
}

func c100cError(gid uint32, lid uint16, key uint32, result uint8, name []byte) []byte {
	return wire.Encode(0x100C, partyHead(gid, lid).Bytes([]byte{0, 1, 0x12, 0}).U32(key).Zero(8).U8(result).Zero(3).
		Bytes(name))
}

func c100eMember(gid uint32, lid uint16, slot int, m memberInfo, recipient string) []byte {
	a := mapFor(m.where.area, recipient, m.disc)
	if m.where.area == (rules.Area{}) {
		a = rules.Area{Quest: 0xFFFFFFFF}
	}
	return wire.Encode(0x100E, partyHead(gid, lid).U8(0).U8(uint8(slot)).U16(uint16(a.Zone)).U16(uint16(a.Map)).
		U16(m.where.entry).U32(a.Quest).Bytes(locText(m.where.area.Quest)).U32(uint32(m.where.lid)).U32(0).U8(0xFF).Zero(3))
}

func c101dAck(gid uint32, lid uint16, v byte) []byte {
	return wire.Encode(0x101D, partyHead(gid, lid).U8(v).Zero(3))
}
