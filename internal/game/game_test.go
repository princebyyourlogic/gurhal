// SPDX-License-Identifier: GPL-3.0-or-later

package game_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/auth"
	"github.com/princebyyourlogic/gurhal/internal/bot"
	"github.com/princebyyourlogic/gurhal/internal/game"
	"github.com/princebyyourlogic/gurhal/internal/gamedata"
	"github.com/princebyyourlogic/gurhal/internal/login"
	"github.com/princebyyourlogic/gurhal/internal/netx"
	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/store"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// The game tests need the NBLs and tables built from your discs: PSU_TEST_DATA=$PWD/data.
type stack struct {
	login, game string
	st          *store.Memory
	gs          *game.Server
}

// start runs login and game on loopback; opts adjust the game server before it starts.
func start(t *testing.T, opts ...func(*game.Server)) stack {
	t.Helper()
	dir := os.Getenv("PSU_TEST_DATA")
	if dir == "" {
		t.Skip("PSU_TEST_DATA not set")
	}
	data, err := gamedata.Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	set, err := rules.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	if os.Getenv("PSU_TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	st, keys := store.NewMemory(), auth.NewKeys()
	gs := &game.Server{Rules: set, Data: data, Store: st, Keys: keys, Log: log,
		Universes: []game.Universe{{ID: 1, Name: "Universe 1"}, {ID: 2, Name: "Universe 2"}}}
	for _, o := range opts {
		o(gs)
	}
	ls := &login.Service{Disc: rules.PSU, Keys: keys, Store: st, GameAddr: netip.MustParseAddr("127.0.0.1"),
		GamePort: 12061, Auth: &auth.Service{Store: st, AutoRegister: true, Log: log}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go gs.Run(ctx)
	listen := func(h func(context.Context, *netx.Conn)) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go (&netx.Server{Log: log, Handle: h}).Serve(ctx, ln)
		return ln.Addr().String()
	}
	return stack{listen(ls.Handle), listen(gs.Handle), st, gs}
}

func (s stack) player(t *testing.T, name string) *bot.Player {
	t.Helper()
	p := &bot.Player{Name: name, Disc: rules.PSU, LoginAddr: s.login, GameAddr: s.game}
	if err := p.Login("password"); err != nil {
		t.Fatal(err)
	}
	if err := p.Enter(true, 0, 1, 2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Game.Close() })
	return p
}

func TestTwoPlayersMeetChatAndPart(t *testing.T) {
	s := start(t)
	a := s.player(t, "alice")
	mark := a.Game.Mark()
	b := s.player(t, "bob")
	if a.LID == b.LID {
		t.Fatalf("both players have LID %d", a.LID)
	}
	if _, err := a.Game.Wait(0x0201, mark, 5*time.Second); err != nil {
		t.Fatal("alice never saw bob spawn:", err)
	}

	ma, mb := a.Game.Mark(), b.Game.Mark()
	a.Chat("hi bob")
	want := bot.UTF16("hi bob")
	for who, c := range map[string]*bot.Client{"alice (echo)": a.Game, "bob": b.Game} {
		m := map[string]int{"alice (echo)": ma, "bob": mb}[who]
		p, err := c.Wait(0x0304, m, 5*time.Second)
		if err != nil || !bytes.Contains(p.Body, want) {
			t.Fatalf("%s: chat %v", who, err)
		}
	}

	mb = b.Game.Mark()
	a.Move(3, 0, 4, 100)
	p, err := b.Game.Wait(0x0102, mb, 5*time.Second)
	if err != nil || binary.LittleEndian.Uint32(p.Body[0x24:]) != a.GID {
		t.Fatalf("bob: move %v", err)
	}

	mb = b.Game.Mark()
	a.Game.Close()
	if _, err := b.Game.Wait(0x0204, mb, 5*time.Second); err != nil {
		t.Fatal("bob never saw alice leave:", err)
	}
}

func TestCharacterIsSaved(t *testing.T) {
	s := start(t)
	p := s.player(t, "saver")
	p.Game.Close()
	time.Sleep(200 * time.Millisecond)
	cs, err := s.st.Characters(context.Background(), p.GID)
	if err != nil || len(cs) != 1 || !bytes.Contains(cs[0].State, []byte(`"level":1`)) {
		t.Fatalf("stored characters %+v, %v", cs, err)
	}
}

// A crowd entering one lobby at once: everyone gets in and gets a distinct LID.
func TestCrowd(t *testing.T) {
	s := start(t)
	const n = 30
	var wg, entered sync.WaitGroup // everyone stays until all are in: one lobby instance
	lids := make(chan uint16, n)
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		entered.Add(1)
		go func() {
			defer wg.Done()
			p := &bot.Player{Name: fmt.Sprintf("crowd%d", i), Disc: rules.PSU, LoginAddr: s.login, GameAddr: s.game}
			if err := p.Login("password"); err != nil {
				errs <- err
				entered.Done()
				return
			}
			if err := p.Enter(true, byte(i%4), byte(i%2), 0); err != nil {
				errs <- err
				entered.Done()
				return
			}
			lids <- p.LID
			entered.Done()
			entered.Wait()
			for k := range 20 {
				p.Move(float32(k), 0, 0, 0)
				time.Sleep(10 * time.Millisecond)
			}
			p.Game.Close()
		}()
	}
	wg.Wait()
	close(lids)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := map[uint16]bool{}
	for l := range lids {
		if seen[l] {
			t.Errorf("LID %d given twice", l)
		}
		seen[l] = true
	}
}

// firstMission is counter 0's first mission with enemies in its enter zone.
func firstMission(t *testing.T) (uint32, []uint32, []uint32, []int) {
	t.Helper()
	data, err := gamedata.Load(os.Getenv("PSU_TEST_DATA"), nil)
	if err != nil {
		t.Fatal(err)
	}
	set, _ := rules.Load(data)
	r := set.For(rules.PSU)
	ct, _ := r.Counter(0)
	for _, q := range ct.Quests {
		m := r.Mission(q)
		if m == nil {
			continue
		}
		roster, _ := m.Roster(uint32(m.Enter[0]))
		if len(roster) == 0 {
			continue
		}
		var ids []uint32
		for id := range roster {
			ids = append(ids, id)
		}
		var flags []uint32
		c := m.Quest.Clear
		for i := 0; i < len(c); i++ {
			if c[i] == 120 && i+3 < len(c) {
				for k, mp := range m.Maps(uint32(c[i+1])) {
					if int(mp) == c[i+2] {
						flags = append(flags, uint32(k*256+c[i+3]))
					}
				}
				i += 3
			} else if c[i] == 100 {
				i++
			}
		}
		return q, ids, flags, m.Exit
	}
	t.Skip("no mission at counter 0")
	return 0, nil, nil, nil
}

func weapon(t *testing.T, p *bot.Player) (int, uint32) {
	inv, err := p.Game.Wait(0x0A0A, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range bot.Inventory(inv) {
		if it[1]>>24 == 1 {
			return i, it[0]
		}
	}
	t.Fatal("no weapon")
	return 0, 0
}

// fight hits every enemy until all are dead; it returns how many died.
func fight(t *testing.T, p *bot.Player, enemies []uint32, uuid uint32) int {
	alive := map[uint32]bool{}
	for _, e := range enemies {
		alive[e] = true
	}
	for round := uint32(0); len(alive) > 0 && round < 300; round++ {
		var targets []uint32
		for e := range alive {
			if len(targets) < 20 {
				targets = append(targets, e)
			}
		}
		k := p.Game.Mark()
		p.Hits(round, targets, uuid)
		own := func(x wire.Packet) bool { // results go to the whole zone: wait for this player's own
			return len(x.Body) >= 0x28+0x68 && binary.LittleEndian.Uint32(x.Body[0x28+0x14:]) == uint32(p.LID)
		}
		r, err := p.Game.WaitFor(0x0E07, k, 4*time.Second, own)
		if err != nil {
			t.Fatal(err)
		}
		for i := range int(binary.LittleEndian.Uint32(r.Body[0x24:])) {
			e := r.Body[0x28+0x68*i:]
			if binary.LittleEndian.Uint32(e[0x34:])&0x20000 != 0 {
				delete(alive, binary.LittleEndian.Uint32(e[0x18:]))
			}
		}
	}
	return len(enemies) - len(alive)
}

func TestPartyMission(t *testing.T) {
	s := start(t)
	qid, enemies, flags, exit := firstMission(t)
	lead, mem := s.player(t, "leader"), s.player(t, "member")
	lead.Invite(mem.GID)
	if _, err := mem.Game.Wait(0x100A, 0, 5*time.Second); err != nil {
		t.Fatal("no invite:", err)
	}
	mark := mem.Game.Mark()
	mem.Answer(lead.GID, true)
	if _, err := mem.Game.Wait(0x1005, mark, 5*time.Second); err != nil {
		t.Fatal("no party window:", err)
	}
	for _, p := range []*bot.Player{lead, mem} {
		i, u := weapon(t, p)
		if err := p.Equip(i, u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mem.Counter(0); err != nil {
		t.Fatal(err)
	}
	if _, err := lead.Counter(0); err != nil {
		t.Fatal(err)
	}
	mark = mem.Game.Mark()
	if r, err := lead.Accept(qid); err != nil || r.Body[0x24] != 0 {
		t.Fatal("accept:", err)
	}
	if _, err := mem.Game.Wait(0x1015, mark, 5*time.Second); err != nil {
		t.Fatal("the member at the counter did not get the mission:", err)
	}
	if _, err := lead.Depart(qid); err != nil {
		t.Fatal(err)
	}
	mark = lead.Game.Mark()
	if _, err := mem.Depart(qid); err != nil {
		t.Fatal("member depart:", err)
	}
	if _, err := lead.Game.Wait(0x0201, mark, 5*time.Second); err != nil {
		t.Fatal("the member did not join the leader's mission zone:", err)
	}
	for _, e := range enemies {
		lead.ReportEnemy(e)
	}
	_, u := weapon(t, lead)
	_, mu := weapon(t, mem)
	half := len(enemies) / 2
	var wg sync.WaitGroup
	wg.Add(2)
	var killed [2]int
	go func() { defer wg.Done(); killed[0] = fight(t, lead, enemies[:half], u) }()
	go func() { defer wg.Done(); killed[1] = fight(t, mem, enemies[half:], mu) }()
	wg.Wait()
	if killed[0]+killed[1] != len(enemies) {
		t.Fatalf("killed %d of %d", killed[0]+killed[1], len(enemies))
	}
	clearMark := mem.Game.Mark()
	for _, f := range flags {
		lead.Flag(f)
	}
	for who, p := range map[string]*bot.Player{"leader": lead, "member": mem} {
		m := clearMark
		if who == "leader" {
			m = 0
		}
		if _, err := p.Game.Wait(0x0C09, m, 6*time.Second); err != nil {
			t.Fatalf("%s: no clear: %v", who, err)
		}
	}
	if len(exit) < 3 {
		exit = []int{2, 4, 6}
	}
	for _, p := range []*bot.Player{lead, mem} {
		if _, err := p.Leave(uint16(exit[0]), uint16(exit[1]), uint16(exit[2])); err != nil {
			t.Fatal("leave:", err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	cs, _ := s.st.Characters(context.Background(), mem.GID)
	var st rules.State
	if json.Unmarshal(cs[0].State, &st); st.EXP == 0 || st.Meseta == 1000 {
		t.Fatalf("the member's EXP %d / meseta %d were not saved", st.EXP, st.Meseta)
	}
	clears, _ := s.st.Clears(context.Background())
	if len(clears) != 1 || clears[0].Quest != qid || len(clears[0].Party) != 2 || clears[0].Kills != len(enemies) {
		t.Fatalf("clears recorded %+v", clears)
	}
	board := s.gs.BoardForTest(t)
	if ms := board["missions"].([]any); len(ms) != 1 || len(ms[0].(map[string]any)["best"].([]any)) != 1 {
		t.Fatalf("board missions %v", board["missions"])
	}
}
