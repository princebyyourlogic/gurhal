// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"sort"
	"strings"
	"unicode/utf16"
)

// My Shop: a player sells from the room stock in their My Room, open or closed, the
// owner online or not (the TMyRoomManager routines of the client's offline room server).

const (
	MesetaMax     = 99_999_999
	ShopResults   = 30
	shopEntrySize = 0x148
)

func (st *State) shop() *RoomShop {
	if st.Shop == nil {
		st.Shop = &RoomShop{}
	}
	return st.Shop
}

func (st *State) shopSlot(slot int) int {
	if st.Shop == nil {
		return -1
	}
	return slices.IndexFunc(st.Shop.Items, func(e ShopItem) bool { return e.Slot == slot })
}

// toStock puts n of item into the room stock (onto a stack of the same item if any).
func (r *Rules) toStock(st *State, item Item, n int) error {
	held := -1
	if r.Stackable(item.ID) {
		held = slices.IndexFunc(st.RoomStock, func(x Item) bool { return x.ID == item.ID })
	}
	if held >= 0 {
		st.RoomStock[held].Qty += n
		return nil
	}
	if len(st.RoomStock) >= StockCapacity {
		return errors.New("storage full")
	}
	uuid := st.NextUUID
	for _, x := range slices.Concat(st.Inv, st.RoomStock) {
		if x.UUID < 0x40000000 {
			uuid = max(uuid, x.UUID+1)
		}
	}
	st.NextUUID = uuid + 1
	item.UUID, item.Qty, item.Active, item.Slots = uuid, n, 0, 0
	st.RoomStock = append(st.RoomStock, item)
	return nil
}

// ShopPut is 0x130f sub 0x11: stock entry key (its UUID's low 16 bits, else its index)
// into the first free shop slot.
func (r *Rules) ShopPut(st *State, key, price, count int) (int, error) {
	i := slices.IndexFunc(st.RoomStock, func(x Item) bool { return int(x.UUID&0xFFFF) == key&0xFFFF })
	if i < 0 && key >= 0 && key < len(st.RoomStock) {
		i = key
	}
	if i < 0 {
		return -1, errors.New("no such storage entry")
	}
	if price < 0 || price > MesetaMax {
		return -1, errors.New("bad price")
	}
	sh := st.shop()
	slot := -1
	for s := range ShopSlots {
		if st.shopSlot(s) < 0 {
			slot = s
			break
		}
	}
	if slot < 0 {
		return -1, errors.New("shop full")
	}
	e := &st.RoomStock[i]
	stack := r.Stackable(e.ID)
	n := 1
	if stack {
		n = min(max(1, count), e.Qty)
	}
	item := *e
	item.Qty, item.Active, item.Slots = n, 0, 0
	if e.Qty -= n; e.Qty <= 0 || !stack {
		st.RoomStock = slices.Delete(st.RoomStock, i, i+1)
	}
	sh.Items = append(sh.Items, ShopItem{Slot: slot, Price: price, Item: item})
	return slot, nil
}

// ShopTakeBack moves count (< 0: all) of a shop slot back to the stock.
func (r *Rules) ShopTakeBack(st *State, slot, count int) error {
	i := st.shopSlot(slot)
	if i < 0 {
		return errors.New("empty slot")
	}
	e := &st.Shop.Items[i]
	qty := max(e.Item.Qty, 1)
	n := count
	if count < 0 || count >= qty || !r.Stackable(e.Item.ID) {
		n = qty
	}
	if err := r.toStock(st, e.Item, n); err != nil {
		return err
	}
	if n >= qty {
		st.Shop.Items = slices.Delete(st.Shop.Items, i, i+1)
	} else {
		e.Item.Qty = qty - n
	}
	return nil
}

func (st *State) ShopPrice(slot, price int) error {
	i := st.shopSlot(slot)
	if i < 0 || price < 0 || price > MesetaMax {
		return errors.New("bad slot or price")
	}
	st.Shop.Items[i].Price = price
	return nil
}

// ShopSetOpen reports whether the flag changed.
func (st *State) ShopSetOpen(open bool) bool {
	sh := st.shop()
	changed := sh.Open != open
	sh.Open = open
	return changed
}

// ShopBuy is 0x130f sub 0x14: count of owner's shop slot at price (what the buyer saw;
// seen is the item record it showed, nil to skip the check) into buyer's inventory, the
// meseta to the owner. It returns the inventory slot added, the units and the total.
func (r *Rules) ShopBuy(owner, buyer *State, slot, price, count int, seen []byte) (Item, int, int, error) {
	if owner.Shop == nil || !owner.Shop.Open {
		return Item{}, 0, 0, errors.New("shop closed")
	}
	i := owner.shopSlot(slot)
	if i < 0 || !r.shopVisible(owner.Shop.Items[i]) {
		return Item{}, 0, 0, errors.New("no such item")
	}
	e := &owner.Shop.Items[i]
	if e.Price != price {
		return Item{}, 0, 0, errors.New("price changed")
	}
	if seen != nil && !bytes.Equal(seen[4:8], e.Item.ID[:]) {
		return Item{}, 0, 0, errors.New("item changed")
	}
	stack := r.Stackable(e.Item.ID)
	qty := max(e.Item.Qty, 1)
	n := 1
	if stack {
		n = min(max(1, count), qty)
	}
	total := price * n
	if int(buyer.Meseta) < total {
		return Item{}, 0, 0, errors.New("meseta")
	}
	added, err := r.addFrom(buyer, e.Item, n)
	if err != nil {
		return Item{}, 0, 0, err
	}
	buyer.Meseta -= uint32(total)
	owner.Meseta = uint32(min(int(owner.Meseta)+total, MesetaMax))
	if n >= qty {
		owner.Shop.Items = slices.Delete(owner.Shop.Items, i, i+1)
	} else {
		e.Item.Qty = qty - n
	}
	return added, n, total, nil
}

// NoteVisit puts a visitor first in the owner's log (one entry per visitor, at most 20).
func NoteVisit(owner *State, gid uint32, name []byte, slot int, now int64) {
	log := slices.DeleteFunc(slices.Clone(owner.Visitors), func(v Visitor) bool { return v.GID == gid })
	owner.Visitors = append([]Visitor{{GID: gid, Slot: slot, Name: slices.Clone(name), T: now}}, log[:min(len(log), VisitorMax-1)]...)
}

// ShopRow is a character with an open shop, for the search.
type ShopRow struct {
	GID  uint32
	Slot int
	Name []byte // the 0x40-byte UTF-16 name
	St   *State
}

// UTF16Text decodes a NUL-terminated UTF-16LE field.
func UTF16Text(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := uint16(b[i]) | uint16(b[i+1])<<8
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// SearchShops is 0x1313: the 0x148-byte 0x1314 entries of the open shops with something
// to show, matching mode 0 name / 1 ad / 4 item name / 5 any, the fullest first.
func (r *Rules) SearchShops(rows []ShopRow, mode int, text string) [][]byte {
	want := strings.ToLower(strings.TrimSpace(text))
	type hit struct {
		n   int
		row ShopRow
	}
	var hits []hit
	for _, row := range rows {
		sh := row.St.Shop
		if sh == nil || !sh.Open {
			continue
		}
		var shown []ShopItem
		for _, e := range sh.Items {
			if r.shopVisible(e) {
				shown = append(shown, e)
			}
		}
		if len(shown) == 0 {
			continue
		}
		if want != "" {
			switch mode {
			case 0:
				if !strings.Contains(strings.ToLower(UTF16Text(row.Name)), want) {
					continue
				}
			case 1:
				if !strings.Contains(strings.ToLower(UTF16Text(sh.Ad)), want) {
					continue
				}
			case 4:
				if !slices.ContainsFunc(shown, func(e ShopItem) bool {
					return strings.Contains(strings.ToLower(r.items[e.Item.ID].Name), want)
				}) {
					continue
				}
			}
		}
		hits = append(hits, hit{len(shown), row})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].n > hits[j].n })
	var out [][]byte
	for _, h := range hits[:min(len(hits), ShopResults)] {
		b := make([]byte, 0, shopEntrySize)
		b = append(b, fixedBytes(h.row.Name, 0x40)...)
		b = append(b, byte(min(h.n, 255)), 1, 0, 0, 0, 0, 0, 0)
		b = binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(b, h.row.GID), uint32(h.row.Slot))
		out = append(out, append(b, fixedBytes(h.row.St.Shop.Ad, 0xF8)...))
	}
	return out
}
