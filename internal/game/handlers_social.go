// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// Player to player (PS2 layouts):
//
//	cards     C>S 0x1514 offer (+0x24 GID) -> 0x1503; 0x1504 answer (+0x24 s16 slot, +0x26 accept)
//	          -> 0x1509; 0x1508 save a slot; 0x1506 -> 0x1507 online flags; 0x1515 -> 0x1516
//	mail      C>S 0x150B send (+0x24 record 0x1AC, +0x1D0 count, +0x1D4 GIDs) -> 0x150F to the
//	          online ones; 0x150D read (+0x24 sender, +0x28 serial) -> 0x150E
//	blacklist C>S 0x1513 (+0x24 op 1 add, +0x28 GID + entry)
//	shortcuts C>S 0x0302 (+0x24 index, +0x28 0xA4 text + modifiers), sent back at load (0x0303)
//	trade     C>S 0x1901 (+0x24 partner, +0x28 action, +0x29 param), 0x1903 offers
//	view      C>S 0x0A07 -> 0x0A07 (empty)

const (
	maxCards    = 200
	maxMail     = 100
	maxBlack    = 80
	blackEntry  = 0x48
	cardLen     = 0x1D4
	mailLen     = 0x1AC
	viewSecs    = 300 // a card holder window refreshes this long after it was opened
	shortcutLen = 0xA4
)

func init() {
	handle(0x1514, 0x28, (*session).cardOffer)
	handle(0x1504, 0x27, (*session).cardAnswer)
	handle(0x1508, 0x28+cardLen, (*session).cardSave)
	handle(0x1506, 0, (*session).cardStatus)
	handle(0x1515, 0x28, (*session).cardHolder)
	handle(0x150B, 0x1D4, (*session).mailSend)
	handle(0x150D, 0x2C, (*session).mailRead)
	handle(0x1513, 0x70, (*session).blacklistSet)
	handle(0x0302, 0xCC, (*session).shortcut)
	handle(0x0A07, 0x28, (*session).viewStats)
	handle(0x1901, 0x2A, (*session).trade)
	handle(0x1903, 0x38, (*session).tradeItem)
}

// social is the cross-session state: pending card offers, trades, card viewers.
type social struct {
	mu      sync.Mutex
	cards   map[uint32]*player // receiver -> card sender
	asked   map[uint32]uint32  // asked player -> trade requester
	trades  map[uint32]*tradeState
	viewers map[uint32]map[uint32]time.Time // card owner -> viewer -> when it last asked
	holders map[uint32]map[uint32]bool      // card owner -> online players holding its card
}

func newSocial() *social {
	return &social{cards: map[uint32]*player{}, asked: map[uint32]uint32{}, trades: map[uint32]*tradeState{},
		viewers: map[uint32]map[uint32]time.Time{}, holders: map[uint32]map[uint32]bool{}}
}

func cardOwner(rec []byte) uint32 {
	if len(rec) < 0x4C {
		return 0
	}
	return binary.LittleEndian.Uint32(rec[0x48:])
}

// hold notes that holder (online) holds owner's card, or no longer does.
func (sc *social) hold(holder, owner uint32, yes bool) {
	if owner == 0 {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if yes {
		if sc.holders[owner] == nil {
			sc.holders[owner] = map[uint32]bool{}
		}
		sc.holders[owner][holder] = true
	} else if delete(sc.holders[owner], holder); len(sc.holders[owner]) == 0 {
		delete(sc.holders, owner)
	}
}

// offline forgets what holder holds.
func (sc *social) offline(holder uint32) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for owner, hs := range sc.holders {
		if delete(hs, holder); len(hs) == 0 {
			delete(sc.holders, owner)
		}
	}
}

func me(d *player) wire.Body { return wire.Body{}.Head(d.lid(), d.gid) }

// --- load ----------------------------------------------------------------------------------------------

// socialLoad replaces the load's empty cards and blacklist and adds the shortcuts and mail.
func (s *session) socialLoad(seq [][]byte, st *rules.State) [][]byte {
	cards, _ := s.srv.Store.Cards(s.ctx, s.gid)
	b := wire.Body{}.Head(wire.NoLID, s.gid).U32(uint32(len(cards)))
	for slot := range maxCards {
		if rec, ok := cards[slot]; ok {
			b = b.Bytes(rec).U32(uint32(slot))
			s.srv.social.hold(s.gid, cardOwner(rec), true)
		}
	}
	black, _ := s.srv.Store.Blacklist(s.ctx, s.gid)
	bl := wire.Body{}.Head(wire.NoLID, s.gid)
	for _, e := range black[:min(len(black), maxBlack)] {
		bl = bl.Bytes(e)
	}
	bl = append(bl, make([]byte, 0x24+maxBlack*blackEntry-len(bl))...)
	for i, p := range seq {
		switch binary.BigEndian.Uint16(p[4:]) {
		case 0x1501:
			seq[i] = wire.Encode(0x1501, b)
		case 0x1512:
			seq[i] = wire.Encode(0x1512, bl)
		}
	}
	if len(st.Shortcuts) > 0 {
		sc := wire.Body{}.Head(wire.NoLID, s.gid)
		for i := range 12 {
			sc = sc.Bytes(fixed(st.Shortcuts[itoa(i)], shortcutLen))
		}
		seq = append(seq, wire.Encode(0x0303, sc))
	}
	if mail, _ := s.srv.Store.Mail(s.ctx, s.gid, maxMail); len(mail) > 0 {
		m := wire.Body{}.Head(wire.NoLID, s.gid).U32(uint32(len(mail)))
		for _, x := range mail {
			m = m.Bytes(x.Rec)
		}
		seq = append(seq, wire.Encode(0x150F, m))
	}
	return seq
}

func itoa(i int) string { return string(rune('0'+i/10)) + string(rune('0'+i%10)) }

// --- cards ---------------------------------------------------------------------------------------------

func (s *session) cardOffer(b []byte) error {
	if s.me == nil || s.char == nil || !s.socialRate.allow(time.Now()) {
		return nil
	}
	other := s.srv.world.find(binary.LittleEndian.Uint32(b[0x24:]))
	if other == nil {
		return nil
	}
	s.srv.social.mu.Lock()
	s.srv.social.cards[other.gid] = s.me
	s.srv.social.mu.Unlock()
	s.char.mu.Lock()
	rec := rules.CardRecord(s.char.raw, s.gid, 0)
	s.char.mu.Unlock()
	other.conn.Relay(wire.Encode(0x1503, me(other).Bytes(rec)))
	return nil
}

func (s *session) cardAnswer(b []byte) error {
	if s.me == nil {
		return nil
	}
	want, accept := int(int16(binary.LittleEndian.Uint16(b[0x24:]))), b[0x26]
	s.srv.social.mu.Lock()
	sender := s.srv.social.cards[s.gid]
	delete(s.srv.social.cards, s.gid)
	s.srv.social.mu.Unlock()
	if accept != 1 || sender == nil {
		return nil
	}
	cards, err := s.srv.Store.Cards(s.ctx, s.gid)
	if err != nil {
		return err
	}
	slot := want
	if slot < 0 {
		slot = -1
		for i := range maxCards {
			if _, used := cards[i]; !used {
				slot = i
				break
			}
		}
	}
	c := sender.char.Load()
	if slot < 0 || c == nil {
		return nil
	}
	c.mu.Lock()
	rec := rules.CardRecord(c.raw, sender.gid, 0)
	c.mu.Unlock()
	if err := s.srv.Store.SetCard(s.ctx, s.gid, slot, rec); err != nil {
		return err
	}
	s.srv.social.hold(s.gid, sender.gid, true)
	return s.c.Send(wire.Encode(0x1509, me(s.me).U16(uint16(int16(slot))).U16(0).Bytes(rec)))
}

// cardSave stores a slot as the client edited it (+0x1C8 of the record 0: deleted).
func (s *session) cardSave(b []byte) error {
	slot := int(int16(binary.LittleEndian.Uint16(b[0x24:])))
	if slot < 0 || slot >= maxCards {
		return nil
	}
	rec := append([]byte(nil), b[0x28:0x28+cardLen]...)
	if rec[0x1C8] == 0 {
		rec = nil
	}
	if old, _ := s.srv.Store.Cards(s.ctx, s.gid); old[slot] != nil {
		s.srv.social.hold(s.gid, cardOwner(old[slot]), false)
	}
	s.srv.social.hold(s.gid, cardOwner(rec), rec != nil)
	return s.srv.Store.SetCard(s.ctx, s.gid, slot, rec)
}

func (s *session) cardStatus(_ []byte) error {
	if s.me == nil {
		return nil
	}
	s.me.conn.Relay(s.srv.world.cardFlags(s.me))
	return nil
}

// cardFlags is 0x1507 for d: one byte per card slot, 1 when its owner is online.
func (w *world) cardFlags(d *player) []byte {
	cards, _ := w.srv.Store.Cards(w.srv.ctx(), d.gid)
	status := make([]byte, maxCards)
	for slot, rec := range cards {
		if o := cardOwner(rec); o != 0 && w.find(o) != nil {
			status[slot] = 1
		}
	}
	return wire.Encode(0x1507, me(d).Bytes(status))
}

// presence tells the online holders of p's card that p came or went (0x1507).
func (w *world) presence(p *player) {
	sc := w.srv.social
	sc.mu.Lock()
	var to []uint32
	for h := range sc.holders[p.gid] {
		to = append(to, h)
	}
	sc.mu.Unlock()
	for _, h := range to {
		if o := w.find(h); o != nil && o != p {
			o.conn.Relay(w.cardFlags(o))
		}
	}
}

func (s *session) cardHolder(b []byte) error {
	if s.me == nil {
		return nil
	}
	owner := binary.LittleEndian.Uint32(b[0x24:])
	sc := s.srv.social
	sc.mu.Lock()
	if sc.viewers[owner] == nil {
		sc.viewers[owner] = map[uint32]time.Time{}
	}
	sc.viewers[owner][s.gid] = time.Now()
	sc.mu.Unlock()
	if o := s.srv.world.find(owner); o != nil {
		s.me.conn.Relay(s.srv.c1516(s.me, o))
	}
	return nil
}

// c1516 is the card holder window about p for d: name, race, type, level, type level,
// universe, location.
func (srv *Server) c1516(d, p *player) []byte {
	c := p.char.Load()
	if c == nil {
		return nil
	}
	c.mu.Lock()
	raw, cls, level := c.raw, c.st.Cls, c.st.Level
	tl := c.st.TypeLevel[min(cls, 15)]
	c.mu.Unlock()
	u, _ := srv.universe(p.uni.Load())
	var q uint32
	if w := p.where.Load(); w != nil {
		q = w.area.Quest
	}
	return wire.Encode(0x1516, me(d).U32(p.gid).Bytes(raw[rules.RawName:rules.RawRace]).
		U8(raw[rules.RawRace]).U8(raw[rules.RawGender]).U8(rules.VisibleClass(cls, d.disc)).Zero(5).
		U16(uint16(level)).U16(uint16(tl)).Bytes(fixed(utf16Fixed(u.Name, 0x1E), 0x20)).
		Bytes(fixed(utf16Fixed(locationText(q), 0xFE), 0x100)))
}

// cardViewers refreshes p's holder window for whoever asked lately (after a zone change).
func (w *world) cardViewers(p *player) {
	sc := w.srv.social
	sc.mu.Lock()
	var to []uint32
	for v, t := range sc.viewers[p.gid] {
		if time.Since(t) < viewSecs*time.Second {
			to = append(to, v)
		} else {
			delete(sc.viewers[p.gid], v)
		}
	}
	sc.mu.Unlock()
	for _, v := range to {
		if d := w.find(v); d != nil {
			if pkt := w.srv.c1516(d, p); pkt != nil {
				d.conn.Relay(pkt)
			}
		}
	}
}

// --- mail, blacklist, shortcuts, view -------------------------------------------------------------------

func (s *session) mailSend(b []byte) error {
	if !s.socialRate.allow(time.Now()) {
		return nil
	}
	rec := append([]byte(nil), b[0x24:0x24+mailLen]...)
	n := min(int(binary.LittleEndian.Uint32(b[0x1D0:])), 32)
	for i := range n {
		if 0x1D4+4*i+4 > len(b) {
			break
		}
		to := binary.LittleEndian.Uint32(b[0x1D4+4*i:])
		if s.srv.Store.AddMail(s.ctx, to, rec) != nil {
			continue
		}
		if o := s.srv.world.find(to); o != nil {
			o.conn.Relay(wire.Encode(0x150F, me(o).U32(1).Bytes(rec)))
		}
	}
	return nil
}

func (s *session) mailRead(b []byte) error {
	sender, serial := binary.LittleEndian.Uint32(b[0x24:]), binary.LittleEndian.Uint32(b[0x28:])
	mail, err := s.srv.Store.Mail(s.ctx, s.gid, 1<<20)
	if err != nil {
		return err
	}
	for _, m := range mail {
		if len(m.Rec) > 0x19C && binary.LittleEndian.Uint32(m.Rec) == sender && binary.LittleEndian.Uint32(m.Rec[4:]) == serial {
			rec := append([]byte(nil), m.Rec...)
			rec[0x19C] |= 1
			s.srv.Store.SetMail(s.ctx, m.ID, rec)
		}
	}
	if s.me == nil {
		return nil
	}
	return s.c.Send(wire.Encode(0x150E, me(s.me).U32(sender).U32(serial)))
}

func (s *session) blacklistSet(b []byte) error {
	who := binary.LittleEndian.Uint32(b[0x28:])
	var entry []byte
	if b[0x24] == 1 {
		entry = append([]byte(nil), b[0x28:0x70]...)
	}
	return s.srv.Store.SetBlacklist(s.ctx, s.gid, who, entry)
}

func (s *session) shortcut(b []byte) error {
	idx := int(binary.LittleEndian.Uint32(b[0x24:]))
	if idx >= 12 || s.char == nil {
		return nil
	}
	s.char.mu.Lock()
	defer s.char.mu.Unlock()
	if s.char.st.Shortcuts == nil {
		s.char.st.Shortcuts = map[string]rules.HexBytes{}
	}
	s.char.st.Shortcuts[itoa(idx)] = append(rules.HexBytes(nil), b[0x28:0xCC]...)
	s.saveLocked()
	return nil
}

func (s *session) viewStats(b []byte) error {
	if s.me == nil {
		return nil
	}
	return s.c.Send(wire.Encode(0x0A07, me(s.me).U32(binary.LittleEndian.Uint32(b[0x24:])).Zero(0x6BC-8-0x24-4)))
}

// --- trades --------------------------------------------------------------------------------------------

// tradeState is an open trade (TItemExchangeManager): both offers, who confirmed, who
// gave the final OK.
type tradeState struct {
	a, b      uint32 // requester, asked
	offers    map[uint32]*rules.Offer
	confirmed map[uint32]bool
	final     map[uint32]bool
}

func (t *tradeState) other(g uint32) uint32 {
	if g == t.a {
		return t.b
	}
	return t.a
}

func c1902(d *player, partner uint32, action, param uint8) []byte {
	return wire.Encode(0x1902, me(d).U32(1).U32(partner).U8(action).U8(param).Zero(2).Zero(8))
}

func (s *session) trade(b []byte) error {
	if s.me == nil {
		return nil
	}
	partner, action, param := binary.LittleEndian.Uint32(b[0x24:]), b[0x28], b[0x29]
	other := s.srv.world.find(partner)
	sc := s.srv.social
	sc.mu.Lock()
	t := sc.trades[s.gid]
	end := func() {
		for _, g := range []uint32{t.a, t.b} {
			if sc.trades[g] == t {
				delete(sc.trades, g)
			}
		}
	}
	if other == nil {
		if t != nil {
			end()
		}
		sc.mu.Unlock()
		return nil
	}
	var tellMe, tellOther []byte
	var swap *tradeState
	switch {
	case action == 1 && !s.socialRate.allow(time.Now()): // flooding requests: dropped
	case action == 1: // request
		sc.asked[other.gid] = s.gid
		other.conn.Relay(wire.Encode(0x1901, me(other).U32(s.gid).U32(1).U8(2).Zero(3).Zero(8)))
	case action == 2 && sc.asked[s.gid] == other.gid: // the asked side accepts
		delete(sc.asked, s.gid)
		t = &tradeState{a: other.gid, b: s.gid, offers: map[uint32]*rules.Offer{
			other.gid: {Items: map[uint32]int{}}, s.gid: {Items: map[uint32]int{}}},
			confirmed: map[uint32]bool{}, final: map[uint32]bool{}}
		sc.trades[s.gid], sc.trades[other.gid] = t, t
		tellOther = c1902(other, s.gid, 1, 1)
	case action == 2 && t != nil && t.a == s.gid: // the requester's window
		tellMe = c1902(s.me, other.gid, 2, 1)
	case action == 8: // decline / busy
		delete(sc.asked, s.gid)
		tellOther = c1902(other, s.gid, 1, 8)
		if t != nil {
			end()
		}
	case action == 9 && param == 6 && t != nil: // "no" at the final dialog: back to the window
		t.confirmed, t.final = map[uint32]bool{}, map[uint32]bool{}
		tellOther = c1902(other, s.gid, 5, 3)
	case action == 9: // cancel
		delete(sc.asked, s.gid)
		delete(sc.asked, other.gid)
		tellOther = c1902(other, s.gid, 0, 10)
		if t != nil {
			end()
		}
	case action == 4 && t != nil: // items confirmed
		t.confirmed[s.gid] = true
		if len(t.confirmed) == 2 {
			tellMe, tellOther = c1902(s.me, other.gid, 4, 1), c1902(other, s.gid, 4, 1)
		} else {
			tellOther = c1902(other, s.gid, 4, 5)
		}
	case action == 6 && t != nil: // final OK
		t.final[s.gid] = true
		if len(t.final) < 2 {
			tellOther = c1902(other, s.gid, 5, 5)
		} else {
			swap = t
			end()
		}
	}
	// Told under sc.mu: the other side's next step must not overtake these.
	if tellMe != nil {
		s.me.conn.Relay(tellMe)
	}
	if tellOther != nil {
		other.conn.Relay(tellOther)
	}
	sc.mu.Unlock()
	if swap != nil {
		s.finishTrade(swap, other)
	}
	return nil
}

// finishTrade swaps both offers (both characters locked in GID order) and tells both.
func (s *session) finishTrade(t *tradeState, other *player) {
	pa, pb := s.me, other
	if t.a != s.gid {
		pa, pb = other, s.me
	}
	ca, cb := pa.char.Load(), pb.char.Load()
	if ca == nil || cb == nil {
		return
	}
	first, second := ca, cb
	if pb.gid < pa.gid {
		first, second = cb, ca
	}
	first.mu.Lock()
	second.mu.Lock()
	changes, err := pa.r.TradeSwap(ca.st, cb.st, *t.offers[t.a], *t.offers[t.b])
	var out [2][][]byte
	if err == nil {
		for i, p := range []*player{pa, pb} {
			c := []*character{ca, cb}[i]
			for _, ch := range changes {
				if ch.ToA != (i == 0) {
					continue
				}
				if ch.Add {
					out[i] = append(out[i], c0106Add(p.r, p.disc, p.gid, p.lid(), ch.Item))
				} else {
					x := ch.Item
					x.Qty = ch.N
					out[i] = append(out[i], c0106Remove(p.r, p.disc, p.gid, p.lid(), x))
				}
			}
			c.bank(time.Now())
			out[i] = append(out[i], c0115(p.gid, p.lid(), noSource, rules.LevelBlock(c.st, p.disc)))
			s.srv.saver.queue(p.gid, c.slot, c.st)
			p.snap.Store(c.snapshot(p.r))
		}
	}
	second.mu.Unlock()
	first.mu.Unlock()
	for i, p := range []*player{pa, pb} {
		peer := pb.gid
		if i == 1 {
			peer = pa.gid
		}
		if err == nil {
			p.conn.Relay(c1902(p, peer, 6, 1))
		} else {
			p.conn.Relay(c1902(p, peer, 4, 6))
		}
		for _, pkt := range out[i] {
			p.conn.Relay(pkt)
		}
	}
	if err != nil {
		s.c.Log.Info("trade failed", "err", err)
	}
}

// tradeItem is 0x1903: +0x24 sender, +0x28 receiver, +0x2C UUID (-1 meseta), +0x30 count,
// +0x34 op (1 add, 2 remove, 3 meseta). Echoed to the sender; the partner's copy carries
// the item record. A changed offer un-confirms both.
func (s *session) tradeItem(b []byte) error {
	if s.me == nil || s.char == nil {
		return nil
	}
	partner, uuid, count := binary.LittleEndian.Uint32(b[0x28:]), binary.LittleEndian.Uint32(b[0x2C:]), int(binary.LittleEndian.Uint32(b[0x30:]))
	op := b[0x34]
	other := s.srv.world.find(partner)
	sc := s.srv.social
	sc.mu.Lock()
	t := sc.trades[s.gid]
	sc.mu.Unlock()
	if other == nil || t == nil {
		return nil
	}
	s.char.mu.Lock()
	var rec []byte
	isNew, ok := false, true
	sc.mu.Lock()
	offer := t.offers[s.gid]
	switch op {
	case 3:
		offer.Meseta = min(count, int(s.char.st.Meseta))
	default:
		var slot *rules.Item
		for i := range s.char.st.Inv {
			if s.char.st.Inv[i].UUID == uuid {
				slot = &s.char.st.Inv[i]
			}
		}
		if slot == nil {
			ok = false
			break
		}
		if op == 1 {
			_, had := offer.Items[uuid]
			isNew = !had
			offer.Items[uuid] = min(count, slot.Qty)
			rec = s.r.TradeRecord(slot, offer.Items[uuid], other.disc)
		} else {
			delete(offer.Items, uuid)
		}
	}
	unconfirm := ok && len(t.confirmed) > 0
	if unconfirm {
		t.confirmed = map[uint32]bool{}
	}
	if ok { // told under sc.mu, like trade's replies
		item := func(d *player, rec []byte) []byte {
			x := append([]byte(nil), b[0x24:0x38]...)
			x[0x11] = 0
			if isNew {
				x[0x11] = 1
			}
			return wire.Encode(0x1904, me(d).Bytes(x).Bytes(rec))
		}
		s.me.conn.Relay(item(s.me, nil))
		other.conn.Relay(item(other, rec))
		if unconfirm {
			s.me.conn.Relay(c1902(s.me, other.gid, 4, 3))
			other.conn.Relay(c1902(other, s.gid, 4, 3))
		}
	}
	sc.mu.Unlock()
	s.char.mu.Unlock()
	return nil
}
