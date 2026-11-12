// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// State is a character's progression and inventory: what the server owns and the
// client only shows. Stored as JSON next to the raw 0x0D02 body.
type State struct {
	Cls       int              `json:"cls"` // type: 0 Hunter .. 9 Protranser; AotI adds 10..15
	Level     int              `json:"level"`
	EXP       uint32           `json:"exp"` // running total
	Meseta    uint32           `json:"meseta"`
	Blast     int              `json:"blast"`
	Luck      int              `json:"luck"`
	Playtime  uint32           `json:"playtime"` // seconds
	TypeLevel [16]int          `json:"type_level"`
	TypeEXP   [16]int          `json:"type_exp"`
	TypeCurve string           `json:"type_curve,omitempty"`
	HP        int              `json:"hp"`
	Inv       []Item           `json:"inv"`
	PAs       []PA             `json:"pas"`
	NextUUID  uint32           `json:"next_uuid"`
	Palette   HexBytes         `json:"apal,omitempty"` // item palette as the client last sent it (0x0109)
	Game      string           `json:"game,omitempty"` // the disc it last played on
	ChrFlags  map[string]int32 `json:"chrflags,omitempty"`

	Room      []byte              `json:"room,omitempty"`       // 32 furniture slot bytes (0xff empty)
	RoomStock []Item              `json:"room_stock,omitempty"` // My Room item stock
	RoomGifts int                 `json:"room_gifts,omitempty"`
	Probo     *Probo              `json:"probo,omitempty"`
	Synth     []*SynthEntry       `json:"synth,omitempty"`     // 32 production entries
	Shortcuts map[string]HexBytes `json:"shortcuts,omitempty"` // chat shortcuts by index
	Shop      *RoomShop           `json:"shop,omitempty"`
	Visitors  []Visitor           `json:"room_visitors,omitempty"`
}

// RoomShop is a player's My Shop.
type RoomShop struct {
	Open  bool       `json:"open"`
	Ad    HexBytes   `json:"ad,omitempty"` // UTF-16
	Items []ShopItem `json:"items,omitempty"`
}

type ShopItem struct {
	Slot  int  `json:"slot"`
	Price int  `json:"price"`
	Item  Item `json:"item"`
}

// Visitor is one entry of the room's visitor log.
type Visitor struct {
	GID  uint32   `json:"gid"`
	Slot int      `json:"slot"`
	Name HexBytes `json:"name"` // UTF-16 character name
	T    int64    `json:"t"`    // unix seconds
}

// Probo is the My Room partner machine: its type and five stats.
type Probo struct {
	Type  int      `json:"type"`
	Stats [5]int   `json:"stats"`
	Name  HexBytes `json:"name,omitempty"`
}

// Item is one inventory slot.
type Item struct {
	UUID     uint32   `json:"uuid"`
	ID       ItemID   `json:"id"`
	Qty      int      `json:"qty"`
	Active   uint8    `json:"active"`
	Slots    uint8    `json:"slots"` // weapon palette bits
	Grind    int      `json:"grind"`
	PA       [2]int   `json:"pa"`           // linked PA index, level byte
	PP       *int     `json:"pp,omitempty"` // absent: full
	PPBonus  int      `json:"pp_bonus,omitempty"`
	ATPBonus int      `json:"atp_bonus,omitempty"`
	Techs    [][2]int `json:"techs,omitempty"` // technique weapons: (code, level byte)
	Fails    int      `json:"fails,omitempty"` // AotI grind failures
}

// PA is a learned photon art: its EXP and id, the 0-based level in byte 3. Stored as
// [exp, "id"] like the Python server.
type PA struct {
	EXP uint32
	ID  ItemID
}

func (p PA) MarshalJSON() ([]byte, error) { return json.Marshal([]any{p.EXP, p.ID}) }

func (p *PA) UnmarshalJSON(b []byte) error {
	var v [2]json.RawMessage
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if err := json.Unmarshal(v[0], &p.EXP); err != nil {
		return err
	}
	return json.Unmarshal(v[1], &p.ID)
}

// ItemID is a 4-byte item (or PA) id: category, type, index, manufacturer. JSON and
// table keys write it as 8 hex digits.
type ItemID [4]byte

func (id ItemID) String() string { return hex.EncodeToString(id[:]) }

func (id ItemID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

func (id *ItemID) UnmarshalText(b []byte) error {
	if len(b) != 8 {
		return fmt.Errorf("rules: item id %q", b)
	}
	_, err := hex.Decode(id[:], b)
	return err
}

// HexBytes is a byte string written as hex in JSON.
type HexBytes []byte

func (h HexBytes) MarshalText() ([]byte, error) { return []byte(hex.EncodeToString(h)), nil }

func (h *HexBytes) UnmarshalText(b []byte) error {
	out, err := hex.DecodeString(string(b))
	*h = out
	return err
}
