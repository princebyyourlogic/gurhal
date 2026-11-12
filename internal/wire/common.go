// SPDX-License-Identifier: GPL-3.0-or-later

package wire

// Markers EGS writes big-endian (16#...:32) in front of a GID.
var (
	ACC    = []byte{0x00, 0x01, 0x13, 0x00} // the addressee of a reply
	Player = []byte{0x00, 0x00, 0x12, 0x00} // a player a packet is about
	Tag    = []byte{0x00, 0x00, 0x0f, 0x00} // login and game-server replies
)

const (
	NoLID    = 0xFFFF // destination before the player has a zone LID
	CmdHello = 0x0202
)

// Head is the 0x24-byte opening most game packets share: destination LID, 18 zero,
// ACC + the addressee's GID, 8 zero.
func (b Body) Head(lid uint16, gid uint32) Body {
	return b.U16(lid).Zero(18).Bytes(ACC).U32(gid).Zero(8)
}

// About opens a packet to d about player p: d's LID, 2 zero, Player + p's GID, 8 zero,
// ACC + d's GID, 8 zero.
func (b Body) About(lid uint16, pgid, dgid uint32) Body {
	return b.U16(lid).Zero(2).Bytes(Player).U32(pgid).Zero(8).Bytes(ACC).U32(dgid).Zero(8)
}

// Hello is the first packet on the login and game servers: no target and a GID.
func Hello(gid uint32) []byte {
	return Encode(CmdHello, Body{}.U16(NoLID).Zero(34).U32(gid).Zero(128))
}
