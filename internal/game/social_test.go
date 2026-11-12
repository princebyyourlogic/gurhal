// SPDX-License-Identifier: GPL-3.0-or-later

package game_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/bot"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

// Alice gives Bob her card, mails him, then hands over meseta in a trade.
func TestCardsMailAndTrade(t *testing.T) {
	s := start(t)
	a, b := s.player(t, "alice"), s.player(t, "bob")
	const wait = 5 * time.Second

	mb := b.Game.Mark()
	a.Game.Send(0x1514, append(le32(b.GID), 0, 0, 0, 0))
	if _, err := b.Game.Wait(0x1503, mb, wait); err != nil {
		t.Fatal("bob never got the card offer:", err)
	}
	mb = b.Game.Mark()
	b.Game.Send(0x1504, []byte{0xFF, 0xFF, 1})
	if _, err := b.Game.Wait(0x1509, mb, wait); err != nil {
		t.Fatal("bob's card was not stored:", err)
	}
	cards, err := s.st.Cards(context.Background(), b.GID)
	if err != nil || len(cards) != 1 || binary.LittleEndian.Uint32(cards[0][0x48:]) != a.GID {
		t.Fatalf("bob's cards %d, %v", len(cards), err)
	}

	mb = b.Game.Mark()
	mail := make([]byte, 0x1AC+4)
	copy(mail, le32(a.GID))
	binary.LittleEndian.PutUint32(mail[0x1AC:], 1)
	a.Game.Send(0x150B, append(mail, le32(b.GID)...))
	if _, err := b.Game.Wait(0x150F, mb, wait); err != nil {
		t.Fatal("bob never got the mail:", err)
	}

	action := func(p *bot.Player, partner uint32, act byte) {
		p.Game.Send(0x1901, append(le32(partner), act, 0))
	}
	is := func(act byte) func(wire.Packet) bool {
		return func(p wire.Packet) bool { return len(p.Body) > 0x2C && p.Body[0x2C] == act }
	}
	mb = b.Game.Mark()
	action(a, b.GID, 1)
	if _, err := b.Game.Wait(0x1901, mb, wait); err != nil {
		t.Fatal("bob never got the trade request:", err)
	}
	ma := a.Game.Mark()
	action(b, a.GID, 2)
	if _, err := a.Game.WaitFor(0x1902, ma, wait, is(1)); err != nil {
		t.Fatal("alice never saw bob accept:", err)
	}
	offer := append(append(append(le32(a.GID), le32(b.GID)...), le32(0xFFFFFFFF)...), le32(1)...)
	mb = b.Game.Mark()
	a.Game.Send(0x1903, append(offer, 3, 0, 0, 0))
	if _, err := b.Game.Wait(0x1904, mb, wait); err != nil {
		t.Fatal("bob never saw alice's offer:", err)
	}
	ma, mb = a.Game.Mark(), b.Game.Mark()
	action(a, b.GID, 4)
	action(b, a.GID, 4)
	action(a, b.GID, 6)
	action(b, a.GID, 6)
	for who, c := range map[string]*bot.Client{"alice": a.Game, "bob": b.Game} {
		m := map[string]int{"alice": ma, "bob": mb}[who]
		if _, err := c.WaitFor(0x1902, m, wait, is(6)); err != nil {
			t.Fatalf("%s: trade never finished: %v", who, err)
		}
	}
}

// Alice stocks and opens her shop and logs off; Bob finds it, visits and buys; the meseta
// reaches Alice's stored state.
func TestMyShopOffline(t *testing.T) {
	s := start(t)
	a := s.player(t, "alice")
	const wait = 5 * time.Second
	_, uuid := weapon(t, a)

	room := func(p *bot.Player, cmd uint16, sub, x int16, aa, bb uint32, rec []byte) {
		b := append(le32(p.GID), byte(sub), byte(sub>>8), byte(x), byte(x>>8))
		b = append(append(b, le32(aa)...), le32(bb)...)
		b = append(b, make([]byte, 0x40-0x34)...)
		if cmd == 0x130F {
			binary.LittleEndian.PutUint32(b[0x3C-0x24:], uuid)
			b = append(b, append(rec, make([]byte, 0x20-len(rec))...)...)
		}
		p.Game.Send(cmd, b)
	}
	ma := a.Game.Mark()
	room(a, 0x130F, 0, 1, 0, 0, nil) // deposit the weapon
	if _, err := a.Game.Wait(0x1324, ma, wait); err != nil {
		t.Fatal("deposit:", err)
	}
	ma = a.Game.Mark()
	room(a, 0x130F, 0x11, int16(uuid), 100, 1, nil) // into the shop at 100
	p, err := a.Game.Wait(0x1323, ma, wait)
	if err != nil {
		t.Fatal("put:", err)
	}
	seen := append([]byte(nil), p.Body[0x24+0x144:0x24+0x164]...)
	if int32(binary.LittleEndian.Uint32(seen)) != 0 {
		t.Fatalf("shop slot 0 is % x", seen)
	}
	b0 := make([]byte, 0x34-0x24)
	copy(b0, le32(a.GID))
	binary.LittleEndian.PutUint32(b0[4:], 0x12)
	binary.LittleEndian.PutUint32(b0[8:], 1)
	ma = a.Game.Mark()
	a.Game.Send(0x130D, b0) // open
	if _, err := a.Game.Wait(0x130E, ma, wait); err != nil {
		t.Fatal("open:", err)
	}
	a.Game.Close()
	time.Sleep(300 * time.Millisecond)

	b := s.player(t, "bob")
	mb := b.Game.Mark()
	b.Game.Send(0x1313, append([]byte{0, 0, 0, 0, 5, 0, 0, 0}, le32(1)...))
	p, err = b.Game.Wait(0x1314, mb, wait)
	if err != nil || binary.LittleEndian.Uint32(p.Body[0x2C:]) != 1 || binary.LittleEndian.Uint32(p.Body[0x30+0x48:]) != a.GID {
		t.Fatalf("search found %v, %v", p.Body[0x2C:0x30], err)
	}
	mb = b.Game.Mark()
	b.Game.Send(0x1304, append(le32(a.GID), 0, 0, 0, 0))
	if _, err := b.Game.Wait(0x1327, mb, wait); err != nil {
		t.Fatal("visit:", err)
	}
	mb = b.Game.Mark()
	room(b, 0x130F, 0x14, 0, 100, 1, seen)
	p, err = b.Game.WaitFor(0x130E, mb, wait, func(p wire.Packet) bool { return p.Body[0x28] == 0x13 })
	if err != nil || binary.LittleEndian.Uint32(p.Body[0x30:]) != 1 {
		t.Fatalf("buy: %v", err)
	}
	cs, _ := s.st.Characters(context.Background(), a.GID)
	var st struct {
		Meseta   uint32 `json:"meseta"`
		Visitors []struct {
			GID uint32 `json:"gid"`
		} `json:"room_visitors"`
	}
	json.Unmarshal(cs[0].State, &st)
	if st.Meseta != 1100 || len(st.Visitors) != 1 || st.Visitors[0].GID != b.GID {
		t.Fatalf("alice's stored meseta %d, visitors %+v", st.Meseta, st.Visitors)
	}
}
