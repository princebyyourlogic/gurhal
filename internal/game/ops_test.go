// SPDX-License-Identifier: GPL-3.0-or-later

package game_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/bot"
	"github.com/princebyyourlogic/gurhal/internal/game"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// notice waits for a 0x0228 of kind whose text holds want.
func notice(t *testing.T, p *bot.Player, mark int, kind uint32, want string) {
	t.Helper()
	_, err := p.Game.WaitFor(0x0228, mark, 5*time.Second, func(pk wire.Packet) bool {
		return binary.LittleEndian.Uint32(pk.Body[0x24:]) == kind && bytes.Contains(pk.Body[0x2C:], bot.UTF16(want))
	})
	if err != nil {
		t.Fatalf("no notice %q: %v", want, err)
	}
}

// A GM (alice) and a player (bob): commands, the help desk, a mute, a summon, a kick, a ban.
func TestGMAndHelpDesk(t *testing.T) {
	s := start(t, func(g *game.Server) { g.Admins = []string{"ALICE"} })
	a, b := s.player(t, "alice"), s.player(t, "bob")
	ctx := context.Background()

	mb := b.Game.Mark()
	b.Chat("!who")
	notice(t, b, mb, 3, "2 player(s) online.")
	mb = b.Game.Mark()
	b.Chat("!kick alice")
	notice(t, b, mb, 3, "Unknown command.")

	mb = b.Game.Mark()
	b.Chat("!gm I am stuck in a wall")
	notice(t, b, mb, 3, "Sent to the staff (request #1)")
	mb = b.Game.Mark()
	if line, err := s.gs.HelpReply(ctx, 1, "Try !warp later", "staff", false); err != nil || line != "Answered (shown in game now)." {
		t.Fatalf("reply %q %v", line, err)
	}
	notice(t, b, mb, 3, "Staff: Try !warp later")
	mb = b.Game.Mark()
	b.Chat("!gm")
	notice(t, b, mb, 3, "Request #1, latest answer: Try !warp later")

	ma, mb := a.Game.Mark(), b.Game.Mark()
	a.Chat("!announce Maintenance soon")
	notice(t, a, ma, 1, "Maintenance soon")
	notice(t, b, mb, 1, "Maintenance soon")

	mb = b.Game.Mark()
	a.Chat("!meseta +500 bob")
	if p, err := b.Game.Wait(0x0115, mb, 5*time.Second); err != nil || binary.LittleEndian.Uint32(p.Body[0x44:]) != 1500 {
		t.Fatalf("meseta %v", err)
	}

	mb = b.Game.Mark()
	a.Chat("!mute bob 5 spam")
	notice(t, b, mb, 3, "You are muted for 5 more minute(s). Reason: spam")
	ma, mb = a.Game.Mark(), b.Game.Mark()
	b.Chat("hello?")
	notice(t, b, mb, 3, "You are muted")
	time.Sleep(200 * time.Millisecond)
	for _, p := range a.Game.Since(ma) {
		if p.Cmd == 0x0304 {
			t.Fatal("a muted player's chat was relayed")
		}
	}
	ma = a.Game.Mark()
	a.Chat("!unmute bob")
	notice(t, a, ma, 3, "Unmuted bob.")
	ma = a.Game.Mark()
	b.Chat("hello again")
	if _, err := a.Game.Wait(0x0304, ma, 5*time.Second); err != nil {
		t.Fatal("chat after unmute:", err)
	}

	if _, err := a.Warp(1100000, 0, 1, 0x14); err != nil {
		t.Fatal(err)
	}
	mb = b.Game.Mark()
	a.Chat("!summon bob")
	if _, err := b.Game.Wait(0x0236, mb, 5*time.Second); err != nil {
		t.Fatal("bob was not moved:", err)
	}

	mb = b.Game.Mark()
	a.Chat("!ban bob 1 cheating")
	notice(t, b, mb, 3, "Reason: cheating")
	deadline := time.Now().Add(5 * time.Second)
	for !b.Game.Closed() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !b.Game.Closed() {
		t.Fatal("bob was not dropped")
	}
	if _, banned, _ := s.st.ActiveBan(ctx, b.GID, time.Now()); !banned {
		t.Fatal("bob is not banned")
	}
}

// The calendar and the operator's news reach !news (an admin's !news reload re-reads the
// events at once).
func TestEventNotices(t *testing.T) {
	s := start(t, func(g *game.Server) { g.Admins = []string{"dave"} })
	ctx := context.Background()
	s.st.SetEvent(ctx, "fest", []byte(`{"key":"fest","name":"Test Festival","start":"2020-01-01T00:00Z","end":"2099-01-01T00:00Z","exp":2,"notice":"Double EXP!"}`))
	s.st.AddNotice(ctx, "Welcome back.", "test")
	gm, p := s.player(t, "dave"), s.player(t, "carol")
	m := gm.Game.Mark()
	gm.Chat("!news reload")
	notice(t, gm, m, 3, "News: 2 notice(s).")
	m = p.Game.Mark()
	p.Chat("!news")
	notice(t, p, m, 3, "Welcome back.\n\nTest Festival is on until Jan 1 0:00 UTC. Double EXP!")
}

// With a season on, the lobby load carries the staged archives, the season id in 0x020F
// and the seasonal flag in 0x0205; a GM preview of "none" turns it off for that GM only.
func TestSeasonLobby(t *testing.T) {
	s := start(t, func(g *game.Server) { g.Season, g.Admins = "winter", []string{"erin"} })
	p := s.player(t, "erin")
	z, err := p.Game.Wait(0x020F, 0, 5*time.Second)
	if err != nil || z.Body[0x25] != 14 {
		t.Fatalf("0x020f season %v (%v)", z.Body[0x24:0x26], err)
	}
	if m, err := p.Game.Wait(0x0205, 0, 5*time.Second); err != nil || m.Body[len(m.Body)-1] != 1 {
		t.Fatalf("0x0205 seasonal flag %v", err)
	}
	mark := p.Game.Mark()
	p.Chat("!season none")
	notice(t, p, mark, 3, "Previewing no season")
	mark = p.Game.Mark()
	if _, err := p.Warp(1101000, 2, 4, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Warp(1100000, 0, 1, 0); err != nil {
		t.Fatal(err)
	}
	for _, pk := range p.Game.Since(mark) {
		if pk.Cmd == 0x020F && pk.Body[0x25] != 0xFF {
			t.Fatalf("season %d after the preview none", pk.Body[0x25])
		}
	}
}

// A panic on a zone's goroutine is logged; the zone goes on and the character it had
// locked is free again.
func TestZonePanicIsContained(t *testing.T) {
	s := start(t)
	a, b := s.player(t, "alice"), s.player(t, "bob")
	s.gs.PanicInZoneForTest(a.GID)
	mb := b.Game.Mark()
	a.Chat("still here")
	if _, err := b.Game.Wait(0x0304, mb, 5*time.Second); err != nil {
		t.Fatal("the zone stopped after a panic:", err)
	}
	i, u := weapon(t, a)
	if err := a.Equip(i, u); err != nil { // takes alice's character lock
		t.Fatal("alice's character stayed locked:", err)
	}
}
