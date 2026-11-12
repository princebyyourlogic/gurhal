// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/binary"
	"errors"
	"strconv"
)

var (
	ErrInventoryFull = errors.New("inventory full")
	ErrNoItem        = errors.New("no such item")
	ErrEquipped      = errors.New("equipped")
)

// AddItem adds n of an item (the client's 0x0106 add): a stackable merges into the first
// held stack, clamped to its max. uuid 0: the next character UUID. Returns the slot to
// send (a merge: with qty n).
func (r *Rules) AddItem(st *State, id ItemID, n int, uuid uint32) (Item, error) {
	stack := r.Stackable(id)
	if stack {
		for i := range st.Inv {
			if st.Inv[i].ID == id {
				st.Inv[i].Qty = min(st.Inv[i].Qty+n, r.StackMax(id))
				out := st.Inv[i]
				out.Qty = n
				return out, nil
			}
		}
	}
	if len(st.Inv) >= MaxItems {
		return Item{}, ErrInventoryFull
	}
	if uuid == 0 {
		uuid = st.NextUUID
		for _, x := range st.Inv {
			if x.UUID < GroundBase {
				uuid = max(uuid, x.UUID+1)
			}
		}
		uuid = max(uuid, 1)
		st.NextUUID = uuid + 1
	}
	s := Item{UUID: uuid, ID: id, Qty: 1}
	if stack {
		s.Qty = n
	}
	st.Inv = append(st.Inv, s)
	return s, nil
}

// RemoveItem removes n of an item (not an equipped one). Returns the slot as it was,
// with the quantity removed.
func (r *Rules) RemoveItem(st *State, uuid uint32, n int) (Item, error) {
	for i := range st.Inv {
		s := &st.Inv[i]
		if s.UUID != uuid {
			continue
		}
		if s.Active != 0 {
			return Item{}, ErrEquipped
		}
		before := *s
		stack := r.Stackable(s.ID)
		if stack {
			n = min(n, s.Qty)
		} else {
			n = 1
		}
		s.Qty -= n
		if s.Qty <= 0 || !stack {
			st.Inv = append(st.Inv[:i], st.Inv[i+1:]...)
		}
		before.Qty = n
		return before, nil
	}
	return Item{}, ErrNoItem
}

// ItemEvent applies a C>S 0x0105 request to the inventory (+0x2C index, +0x2D event: 1
// equip, 2 unequip, 3 link, 4 unlink, 8 use; +0x30 UUID). Returns the event and the slot
// as it is now (zero when gone or unknown).
func (r *Rules) ItemEvent(st *State, req []byte) (byte, *Item) {
	idx, ev := int(req[0x2C]), req[0x2D]
	uuid := binary.LittleEndian.Uint32(req[0x30:])
	var s *Item
	for i := range st.Inv {
		if st.Inv[i].UUID == uuid {
			s = &st.Inv[i]
			break
		}
	}
	if s == nil && idx < len(st.Inv) {
		s = &st.Inv[idx]
	}
	if s == nil {
		return ev, nil
	}
	switch ev {
	case 1:
		s.Active = 1
	case 2:
		s.Active = 0
	case 3: // link PA list entry +0x2E (technique weapons: into tech slot +0x2F)
		if r.TechWeapon(s) {
			r.linkTech(st, s, int(req[0x2E]), int(req[0x2F]))
		} else {
			s.PA = linkLevel(st, s, int(req[0x2E]))
		}
	case 4:
		if r.TechWeapon(s) {
			if t := int(req[0x2F]); t < 4 && t < len(s.Techs) {
				s.Techs[t] = [2]int{}
			}
		} else {
			s.PA = [2]int{}
		}
	case 8:
		if s.Qty > 0 {
			s.Qty--
			if s.Qty == 0 {
				for i := range st.Inv {
					if &st.Inv[i] == s {
						st.Inv = append(st.Inv[:i], st.Inv[i+1:]...)
						return ev, nil
					}
				}
			}
		}
	}
	return ev, s
}

// linkLevel is the PA index and level byte for linking PA list entry idx to a weapon.
func linkLevel(st *State, s *Item, idx int) [2]int {
	if idx >= 0 && idx < len(st.PAs) {
		p := st.PAs[idx].ID
		if (p[0] == 4 || p[0] == 5) && p[1] == s.ID[1] {
			return [2]int{int(p[2]), int(p[3])}
		}
	}
	return [2]int{}
}

func (r *Rules) linkTech(st *State, s *Item, idx, tslot int) {
	if idx < 0 || idx >= len(st.PAs) || tslot < 0 || tslot >= 4 || st.PAs[idx].ID[0] != 6 {
		return
	}
	code := techCode(st.PAs[idx].ID)
	if code < 1 || code > 0x38 {
		return
	}
	if s.Techs == nil {
		s.Techs = make([][2]int, 4)
	}
	s.Techs[tslot] = [2]int{code, int(st.PAs[idx].ID[3])}
}

// EquippedWeapons are the active weapons.
func (r *Rules) EquippedWeapons(st *State) []*Item {
	var out []*Item
	for i := range st.Inv {
		if it, ok := r.items[st.Inv[i].ID]; ok && st.Inv[i].Active != 0 && it.Cat == catWeapon {
			out = append(out, &st.Inv[i])
		}
	}
	return out
}

// EquipBlock is what an equip confirmation carries at +0x3C: a weapon's sound, hitbox,
// targets and type; a line shield or unit's reach table.
func (r *Rules) EquipBlock(s *Item) []byte {
	if s == nil {
		return nil
	}
	it, ok := r.items[s.ID]
	if !ok {
		return nil
	}
	if it.Cat == 2 || it.Cat == 8 {
		return []byte{0, 0x0a, 0, 0x0a, 0, 0x0a, 0, 0, 0, 0, 0, 0}
	}
	if it.Cat != catWeapon {
		return nil
	}
	w, ok := r.weapons[strconv.Itoa(int(s.ID[1]))]
	if !ok || len(w.Hitboxes) == 0 {
		return nil
	}
	b := binary.LittleEndian.AppendUint32(nil, uint32(w.Sound))
	b = append(b, w.Hitboxes[0]...)
	return append(b, byte(max(1, w.Targets)&0x0F), 0, 0, s.ID[1])
}

// SetPP stores PP clamped to 0..max (full drops the value). True when it changed.
func (r *Rules) SetPP(s *Item, v int) bool {
	mx := r.MaxPP(s)
	old, nv := r.CurPP(s), max(0, min(v, mx))
	if nv >= mx {
		s.PP = nil
	} else {
		s.PP = &nv
	}
	return nv != old
}

// PPItem is a PP item used (0x0105 event 8; constants +0 kind 1, +2 target 4 the weapon at
// index a (0x3C both equipped hands, halved for two) or 5 every weapon, +4 amount, +7 bit
// 0x10 a %). used is the slot as it was before the use. Returns the weapons that changed.
func (r *Rules) PPItem(st *State, used Item, a int) []*Item {
	it, ok := r.items[used.ID]
	if !ok || it.Cat != 3 {
		return nil
	}
	c := it.Consts
	if c[0] != 1 || (c[2] != 4 && c[2] != 5) {
		return nil
	}
	amount := s16(c, 4)
	var targets []*Item
	switch {
	case c[2] == 5:
		for i := range st.Inv {
			if x, ok := r.items[st.Inv[i].ID]; ok && x.Cat == catWeapon {
				targets = append(targets, &st.Inv[i])
			}
		}
	case a == 0x3C:
		targets = r.EquippedWeapons(st)
		if len(targets) > 1 {
			amount = int(float64(amount) / 2)
		}
	case a < len(st.Inv):
		if x, ok := r.items[st.Inv[a].ID]; ok && x.Cat == catWeapon {
			targets = []*Item{&st.Inv[a]}
		}
	}
	var changed []*Item
	for _, w := range targets {
		gain := amount
		if c[7]&0x10 != 0 {
			gain = int(float64(r.MaxPP(w)) * float64(amount) / 100)
		}
		if r.SetPP(w, r.CurPP(w)+gain) {
			changed = append(changed, w)
		}
	}
	return changed
}

// --- PP an attack costs, regeneration (TServerActorPlayer, the offline room) ---------------------------

func (r *Rules) consts(s *Item) []byte {
	if s == nil {
		return nil
	}
	if it, ok := r.items[s.ID]; ok && it.Cat == catWeapon {
		return it.Consts
	}
	return nil
}

func ppPercent(base []byte, k, mx int) int {
	if base[3]&1 != 0 {
		return mx * int(base[4+k]) / 100
	}
	return int(base[4+k])
}

func (r *Rules) tier(key ItemID, level int) []byte {
	e, ok := r.pa[key]
	if !ok {
		return nil
	}
	return e.Base[min(level/10, len(e.Base)-1)]
}

// ppCost is what an attack costs: what = tech (arg = tech slot), bullet, striking (arg =
// PA step).
func (r *Rules) ppCost(s *Item, what string, arg int) int {
	mx := r.MaxPP(s)
	wtype, idx, level := s.ID[1], byte(s.PA[0]), s.PA[1]
	switch what {
	case "tech":
		if arg < 0 || arg >= len(s.Techs) || s.Techs[arg][0] < 1 || s.Techs[arg][0] > 0x38 {
			return 0
		}
		base := r.tier(techKey(s.Techs[arg][0]), s.Techs[arg][1])
		if base == nil || (base[0]&0x40 != 0 && base[0]&0xF != 5) {
			return 0
		}
		return max(0, s16(base, 0x26))
	case "bullet":
		var base []byte
		if idx != 0 {
			base = r.tier(ItemID{5, wtype, idx, 0}, level)
		} else if k := int(wtype) - 0x0E; k >= 0 && k < len(r.bullets) && len(r.bullets[k]) > 4 {
			base = r.bullets[k][4:]
		}
		if base == nil {
			return 0
		}
		return ppPercent(base, 0, mx)
	case "striking":
		if arg < 0 || arg > 2 {
			return 0
		}
		base := r.tier(ItemID{4, wtype, idx, 0}, level)
		if base == nil {
			return 0
		}
		return ppPercent(base, arg, mx)
	}
	return 0
}

// PPCast is a cast that may spend PP (0x0102 action 2 / 3 with the step word, 0x010F a
// left-hand shot; 0x0104 sets the left-hand mode). Returns the weapon whose PP changed.
func (r *Rules) PPCast(st *State, left *bool, cmd uint16, body []byte) *Item {
	var kind int
	var word uint32
	switch {
	case cmd == 0x0104 && len(body) >= 0x30:
		*left = binary.LittleEndian.Uint32(body[0x2C:])&2 != 0
		return nil
	case cmd == 0x0102 && len(body) >= 0x34 && (body[0x2C] == 2 || body[0x2C] == 3):
		kind, word = int(body[0x2C]), binary.LittleEndian.Uint32(body[0x30:])
	case cmd == 0x010F && len(body) >= 0x30 && body[0x2C] == 1:
		kind = 2
	default:
		return nil
	}
	var right, lefty *Item
	for _, w := range r.EquippedWeapons(st) {
		if r.consts(w)[0xD] == 2 {
			if lefty == nil {
				lefty = w
			}
		} else if right == nil {
			right = w
		}
	}
	var s *Item
	cost := 0
	switch {
	case right == nil || (r.consts(right)[0xD] != 0 && *left):
		if *left && lefty != nil && (r.consts(lefty)[0xE] == 1 || r.consts(lefty)[0xE] == 2) {
			s, cost = lefty, r.ppCost(lefty, "bullet", 0)
		}
	default:
		c := r.consts(right)
		style, hand := c[0xE], c[0xD]
		switch {
		case style == techStyle:
			slot := kind - 2
			if *left {
				slot = kind
			}
			s, cost = right, r.ppCost(right, "tech", slot)
		case style == 1 && hand == 0 && kind == 2:
			s, cost = right, r.ppCost(right, "bullet", 0)
		case style == 0 && kind == 3:
			s, cost = right, r.ppCost(right, "striking", int(word>>4)-2)
		}
	}
	if s == nil || cost <= 0 || !r.SetPP(s, r.CurPP(s)-cost) {
		return nil
	}
	return s
}

// ppTickByType is the fallback PP per regeneration tick per weapon type 1..25.
var ppTickByType = []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 3, 5, 4, 5, 5, 5, 5, 3, 8, 9, 13, 10, 4, 3}

func (r *Rules) ppTick(s *Item) int {
	c := r.consts(s)
	if c == nil {
		return 0
	}
	if div, ok := r.ppRegen[s.ID]; ok && div != 0 {
		return int(binary.LittleEndian.Uint16(c)) * 5 / div
	}
	if t := int(s.ID[1]); t >= 1 && t <= len(ppTickByType) {
		return ppTickByType[t-1]
	}
	return 1
}

// PPRegen is one regeneration tick for the equipped weapons (every 5 s in missions).
func (r *Rules) PPRegen(st *State) []*Item {
	var out []*Item
	for _, w := range r.EquippedWeapons(st) {
		if r.CurPP(w) < r.MaxPP(w) && r.SetPP(w, r.CurPP(w)+r.ppTick(w)) {
			out = append(out, w)
		}
	}
	return out
}
