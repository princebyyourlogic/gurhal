// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/binary"
	"errors"
	"time"
)

// My Room: three furniture spots, the item stock, the Probo, the room record (with the
// player's shop and visitor log) the console menus read.

const (
	RoomSlots     = 32
	ShopSlots     = 32
	VisitorMax    = 20
	StockCapacity = 300
	roomRecSize   = 0xC40
	GiftLimit     = 100
	goodsCat      = 0x0B // "0b 01 XX 00" goods: the furniture
)

var roomSpots = map[int]bool{1: true, 2: true, 3: true}

var ErrRoom = errors.New("room")

// RoomSlotsOf are the 32 furniture slot bytes (0xff empty).
func RoomSlotsOf(st *State) []byte {
	if len(st.Room) == RoomSlots {
		return st.Room
	}
	out := make([]byte, RoomSlots)
	for i := range out {
		out[i] = 0xFF
	}
	return out
}

// SetObject is C>S 0x130b: put a goods item (by UUID) on a spot, or take it back
// (0xFFFFFFFF). Returns the goods id for 0x130c and the inventory change (added or
// removed slot).
func (r *Rules) SetObject(st *State, slot int, uuid uint32) (int, Item, bool, error) {
	if !roomSpots[slot] {
		return 0, Item{}, false, errors.New("not a furniture spot")
	}
	room := append([]byte(nil), RoomSlotsOf(st)...)
	if uuid == 0xFFFFFFFF {
		g := room[slot]
		if g == 0xFF {
			return 0, Item{}, false, errors.New("empty")
		}
		added, err := r.AddItem(st, ItemID{goodsCat, 1, g, 0}, 1, 0)
		if err != nil {
			return 0, Item{}, false, err
		}
		room[slot] = 0xFF
		st.Room = room
		return 0xFF, added, true, nil
	}
	var g byte
	found := false
	for _, x := range st.Inv {
		if x.UUID == uuid && x.ID[0] == goodsCat && x.ID[1] == 1 {
			g, found = x.ID[2], true
		}
	}
	if !found {
		return 0, Item{}, false, errors.New("not a room goods item")
	}
	if room[slot] != 0xFF {
		return 0, Item{}, false, errors.New("spot taken")
	}
	removed, err := r.RemoveItem(st, uuid, 1)
	if err != nil {
		return 0, Item{}, false, err
	}
	room[slot] = g
	st.Room = room
	return int(g), removed, false, nil
}

// Deposit moves n of an inventory item into the room stock (C>S 0x130f sub 0).
func (r *Rules) Deposit(st *State, uuid uint32, n int) (Item, error) {
	held := -1
	for _, x := range st.Inv {
		if x.UUID == uuid && r.Stackable(x.ID) {
			for i, y := range st.RoomStock {
				if y.ID == x.ID {
					held = i
					break
				}
			}
		}
	}
	found := false
	for _, x := range st.Inv {
		found = found || x.UUID == uuid
	}
	if !found {
		return Item{}, ErrNoItem
	}
	if held < 0 && len(st.RoomStock) >= StockCapacity {
		return Item{}, errors.New("stock full")
	}
	removed, err := r.RemoveItem(st, uuid, max(1, n))
	if err != nil {
		return Item{}, err
	}
	if held >= 0 {
		st.RoomStock[held].Qty += removed.Qty
	} else {
		x := removed
		x.Active, x.Slots = 0, 0
		st.RoomStock = append(st.RoomStock, x)
	}
	return removed, nil
}

// Withdraw moves want of stock entry index into the inventory (C>S 0x130d sub 0).
func (r *Rules) Withdraw(st *State, index, want int) (Item, error) {
	if index < 0 || index >= len(st.RoomStock) {
		return Item{}, errors.New("no such stock entry")
	}
	e := st.RoomStock[index]
	stack := r.Stackable(e.ID)
	n := 1
	if stack {
		n = min(max(1, want), e.Qty)
	}
	added, err := r.addFrom(st, e, n)
	if err != nil {
		return Item{}, err
	}
	if st.RoomStock[index].Qty -= n; st.RoomStock[index].Qty <= 0 || !stack {
		st.RoomStock = append(st.RoomStock[:index], st.RoomStock[index+1:]...)
	}
	return added, nil
}

// addFrom adds n of template's item keeping its grind, PA and bonuses (a new slot).
func (r *Rules) addFrom(st *State, tmpl Item, n int) (Item, error) {
	added, err := r.AddItem(st, tmpl.ID, n, 0)
	if err != nil || r.Stackable(tmpl.ID) {
		return added, err
	}
	s := &st.Inv[len(st.Inv)-1]
	uuid := s.UUID
	*s = tmpl
	s.UUID, s.Qty, s.Active, s.Slots = uuid, 1, 0, 0
	return *s, nil
}

// GiveItem is C>S 0x130f sub 4: one of the item goes to the Probo.
func (r *Rules) GiveItem(st *State, uuid uint32) (Item, bool) {
	removed, err := r.RemoveItem(st, uuid, 1)
	if err != nil {
		return Item{}, false
	}
	st.RoomGifts++
	return removed, true
}

// StockRecords are the stock's 0x68-byte records: the 0x20-byte item and its constants.
func (r *Rules) StockRecords(st *State, recipient string) [][]byte {
	var out [][]byte
	for i := range st.RoomStock {
		x := &st.RoomStock[i]
		out = append(out, append(r.VarRec(x, recipient)[4:], r.ConstRec(x)...))
	}
	return out
}

func (r *Rules) shopVisible(e ShopItem) bool { _, ok := r.items[e.Item.ID]; return ok }

// RoomRecord is the 0xC40-byte room record R: shop name, ad, shop items and prices,
// open flag, visitor log, stock capacity.
func (r *Rules) RoomRecord(st *State, name []byte, recipient string) []byte {
	b := make([]byte, roomRecSize)
	copy(b, fixedBytes(name, 0x40))
	sh := st.Shop
	if sh == nil {
		sh = &RoomShop{}
	}
	copy(b[0x48:0x140], fixedBytes(sh.Ad, 0xF8))
	for i := range ShopSlots {
		binary.LittleEndian.PutUint32(b[0x144+i*0x20:], 0xFFFFFFFF)
	}
	for _, e := range sh.Items {
		if e.Slot >= 0 && e.Slot < ShopSlots && r.shopVisible(e) {
			it := e.Item
			it.Active, it.Slots = 0, 0
			rec := r.VarRec(&it, recipient)[4:]
			binary.LittleEndian.PutUint32(rec, uint32(e.Slot))
			copy(b[0x144+e.Slot*0x20:], rec)
			binary.LittleEndian.PutUint32(b[0x544+e.Slot*4:], uint32(e.Price))
		}
	}
	if sh.Open {
		b[0x5C4] = 1
	}
	log := st.Visitors[:min(len(st.Visitors), VisitorMax)]
	binary.LittleEndian.PutUint32(b[0x5C8:], uint32(len(log)))
	for i, v := range log {
		copy(b[0x5CC+i*0x50:], VisitorRecord(v))
	}
	binary.LittleEndian.PutUint16(b[0xC0E:], StockCapacity)
	return b
}

// VisitorRecord is a 0x50-byte visitor log entry: date and time, GID, slot, name.
func VisitorRecord(v Visitor) []byte {
	t := time.Unix(v.T, 0).UTC()
	b := binary.LittleEndian.AppendUint16(nil, uint16(t.Year()))
	b = append(b, byte(t.Month()), byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0)
	b = binary.LittleEndian.AppendUint32(b, v.GID)
	b = append(b, byte(v.Slot), 0, 0, 0)
	return append(b, fixedBytes(v.Name, 0x40)...)
}

// ShopConsts is a 32 x 0x48 list of the shop items' constants.
func (r *Rules) ShopConsts(st *State) []byte {
	b := make([]byte, ShopSlots*0x48)
	if st.Shop == nil {
		return b
	}
	for _, e := range st.Shop.Items {
		if e.Slot >= 0 && e.Slot < ShopSlots && r.shopVisible(e) {
			copy(b[e.Slot*0x48:], r.ConstRec(&e.Item))
		}
	}
	return b
}

// --- partner cards and trades ------------------------------------------------------------------------

// CardRecord is a player's 0x1D4-byte partner card: name, race, gender, type, voice,
// GID, comment.
func CardRecord(raw []byte, gid uint32, charSlot int) []byte {
	b := append([]byte(nil), raw[RawName:RawRace]...)
	b = append(b, raw[RawRace], raw[RawGender], raw[RawClass], raw[RawAppearance], raw[RawAppearance+1], 0, 0, 0)
	b = binary.LittleEndian.AppendUint32(b, gid)
	b = append(b, make([]byte, 0x1C+0xB0+0xB0)...)
	return append(append(b, 1, 0, 4, byte(charSlot)), make([]byte, 8)...)
}

// TradeRecord is the 0x50-byte record a trade partner sees: item data from the id on,
// the name, the count.
func (r *Rules) TradeRecord(s *Item, count int, recipient string) []byte {
	b := fixedBytes(r.VarRec(s, recipient)[8:0x24], 0x20)
	b = append(b, fixedUTF16(r.items[s.ID].Name, 0x2C, 0x2E)...)
	return binary.LittleEndian.AppendUint16(b, uint16(count))
}

// Offer is one side of a trade: UUID -> count, and meseta.
type Offer struct {
	Items  map[uint32]int
	Meseta int
}

// Change is an inventory update a trade sends (0x0106 add or remove).
type Change struct {
	ToA  bool // the change is in side A's inventory
	Add  bool
	Item Item
	N    int
}

// TradeSwap moves both offers between a and b, or explains why it cannot.
func (r *Rules) TradeSwap(a, b *State, oa, ob Offer) ([]Change, error) {
	sides := []struct {
		st *State
		o  Offer
	}{{a, oa}, {b, ob}}
	for _, s := range sides {
		for uuid, n := range s.o.Items {
			ok := false
			for _, x := range s.st.Inv {
				if x.UUID == uuid && x.Qty >= n && x.Active == 0 {
					ok = true
				}
			}
			if !ok {
				return nil, errors.New("an offered item is gone")
			}
		}
		if int(s.st.Meseta) < s.o.Meseta {
			return nil, errors.New("short of meseta")
		}
	}
	if len(b.Inv)+len(oa.Items) > MaxItems || len(a.Inv)+len(ob.Items) > MaxItems {
		return nil, ErrInventoryFull
	}
	var out []Change
	for i, s := range sides {
		other := sides[1-i].st
		for uuid, n := range s.o.Items {
			removed, _ := r.RemoveItem(s.st, uuid, n)
			out = append(out, Change{ToA: i == 0, Item: removed, N: removed.Qty})
			added, _ := r.addFrom(other, removed, removed.Qty)
			out = append(out, Change{ToA: i == 1, Add: true, Item: added, N: removed.Qty})
		}
		s.st.Meseta -= uint32(s.o.Meseta)
		other.Meseta += uint32(s.o.Meseta)
	}
	return out, nil
}

// TypeMenu is 0x1A07's 16 enabled bytes: the current type and every type it may change to.
func (r *Rules) TypeMenu(st *State) []byte {
	out := make([]byte, 16)
	for c := range 16 {
		if c < r.ClassCount() && (c == st.Cls || r.CanChangeType(st, c)) {
			out[c] = 1
		}
	}
	return out
}

func fixedBytes(b []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, b)
	return out
}
