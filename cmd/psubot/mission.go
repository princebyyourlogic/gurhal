// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/bot"
	"github.com/princebyyourlogic/gurhal/internal/gamedata"
	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// mission is the first free mission of counter 0 with enemies in its enter zone.
type mission struct {
	qid     uint32
	m       *rules.Mission
	enemies []uint32
	flags   []uint32 // the clear rule's flags (map index * 256 + event)
}

func pickMission(dataDir string) (mission, error) {
	d, err := gamedata.Load(dataDir, nil)
	if err != nil {
		return mission{}, err
	}
	set, err := rules.Load(d)
	if err != nil {
		return mission{}, err
	}
	r := set.For(rules.PSU)
	ct, ok := r.Counter(0)
	if !ok {
		return mission{}, fmt.Errorf("no counter 0 in %s", dataDir)
	}
	for _, q := range ct.Quests {
		m := r.Mission(q)
		if m == nil {
			continue
		}
		roster, _ := m.Roster(uint32(m.Enter[0]))
		if len(roster) == 0 {
			continue
		}
		out := mission{qid: q, m: m}
		for id := range roster {
			out.enemies = append(out.enemies, id)
		}
		sort.Slice(out.enemies, func(i, j int) bool { return out.enemies[i] < out.enemies[j] })
		if m.Quest != nil {
			c := m.Quest.Clear
			for i := 0; i < len(c); {
				switch {
				case c[i] == 120 && i+3 < len(c):
					maps := m.Maps(uint32(c[i+1]))
					for k, mp := range maps {
						if int(mp) == c[i+2] {
							out.flags = append(out.flags, uint32(k*256+c[i+3]))
						}
					}
					i += 4
				case c[i] == 100:
					i += 2
				default:
					i++
				}
			}
		}
		return out, nil
	}
	return mission{}, fmt.Errorf("no mission with enemies at counter 0")
}

// missionScenario: A equips, takes the mission, departs, reports its enemies (all of it
// deterministic: compared byte for byte); then fights it out, picks up a drop, clears and
// leaves (random: checked, not compared).
func missionScenario(login, game, tag string, ms mission) ([]record, []string, error) {
	var out []record
	var problems []string
	check := func(ok bool, what string) {
		if !ok {
			problems = append(problems, what)
		}
	}
	p := &bot.Player{Name: "misa" + tag, Disc: "psu", LoginAddr: login, GameAddr: game}
	if err := p.Login("testplay"); err != nil {
		return nil, nil, err
	}
	if err := p.Enter(true, 0, 0, 0); err != nil {
		return nil, nil, err
	}
	defer p.Game.Close()
	g := p.Game
	inv, err := g.Wait(0x0A0A, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	slot, weapon := -1, uint32(0)
	for i, it := range bot.Inventory(inv) {
		if it[1]>>24 == 1 {
			slot, weapon = i, it[0]
			break
		}
	}
	if slot < 0 {
		return nil, nil, fmt.Errorf("no weapon in the starter pack")
	}
	time.Sleep(300 * time.Millisecond)
	mark := g.Mark()
	snap := func(step string) {
		time.Sleep(400 * time.Millisecond)
		out = append(out, record{step, "A", g.Since(mark)})
		mark = g.Mark()
	}
	if err := p.Equip(slot, weapon); err != nil {
		return nil, nil, err
	}
	snap("equip weapon")
	if _, err := p.Counter(0); err != nil {
		return nil, nil, fmt.Errorf("counter: %w", err)
	}
	snap("counter 0")
	if r, err := p.Accept(ms.qid); err != nil || r.Body[0x24] != 0 {
		return nil, nil, fmt.Errorf("accept %d: %v", ms.qid, err)
	}
	snap(fmt.Sprintf("accept %d", ms.qid))
	if _, err := p.Depart(ms.qid); err != nil {
		return nil, nil, fmt.Errorf("depart: %w", err)
	}
	snap("depart + 0x0808")
	for _, e := range ms.enemies {
		p.ReportEnemy(e)
	}
	snap(fmt.Sprintf("report %d enemies", len(ms.enemies)))

	// the fight: random on both servers, so checked rather than compared
	alive := map[uint32]bool{}
	for _, e := range ms.enemies {
		alive[e] = true
	}
	fight := g.Mark()
	landed := 0
	for round := uint32(0); len(alive) > 0 && round < 200; round++ {
		var targets []uint32
		for _, e := range ms.enemies {
			if alive[e] && len(targets) < 20 {
				targets = append(targets, e)
			}
		}
		k := g.Mark()
		p.Hits(round, targets, weapon)
		r, err := g.Wait(0x0E07, k, 4*time.Second)
		if err != nil {
			break
		}
		n := int(binary.LittleEndian.Uint32(r.Body[0x24:]))
		for i := range n {
			e := r.Body[0x28+0x68*i:]
			if binary.LittleEndian.Uint32(e[4:]) > 0 {
				landed++
			}
			dst, se, hp := binary.LittleEndian.Uint32(e[0x18:]), binary.LittleEndian.Uint32(e[0x34:]), binary.LittleEndian.Uint32(e[0x3C:])
			if se&0x20000 != 0 || hp == 0 {
				delete(alive, dst)
			}
		}
	}
	time.Sleep(500 * time.Millisecond)
	got := g.Since(fight)
	count := func(cmd uint16) int {
		n := 0
		for _, x := range got {
			if x.Cmd == cmd {
				n++
			}
		}
		return n
	}
	check(landed > 0, "hits landed")
	check(len(alive) == 0, fmt.Sprintf("all %d enemies killed (%d left)", len(ms.enemies), len(alive)))
	check(count(0x0115)+count(0x010D) > 0, "kills give EXP (0x0115 / 0x010D)")
	check(count(0x1205) > 0, "a cleared wave sets its flag (0x1205)")
	var drop *wire.Packet
	for i := range got {
		if got[i].Cmd == 0x0A02 {
			drop = &got[i]
		}
	}
	if drop != nil {
		uuid := binary.LittleEndian.Uint32(drop.Body[0x6C:])
		k := g.Mark()
		p.Pickup(uuid)
		pk, err := g.Wait(0x0106, k, 4*time.Second)
		check(err == nil && pk.Body[0x2D] == 6, fmt.Sprintf("pick up drop %#x", uuid))
	}
	clear := g.Mark()
	cleared := func() bool {
		for _, x := range g.Since(fight) {
			if x.Cmd == 0x0C09 && x.Body[0x24] == 1 {
				return true
			}
		}
		return false
	}
	if !cleared() {
		for _, f := range ms.flags {
			p.Flag(f)
		}
		g.Wait(0x0C09, clear, 6*time.Second)
		time.Sleep(300 * time.Millisecond)
	}
	check(cleared(), "the clear rule's flags clear it (0x0C09 with results)")
	ex := ms.m.Exit
	if len(ex) < 3 {
		ex = []int{2, 4, 6}
	}
	k, err := p.Leave(uint16(ex[0]), uint16(ex[1]), uint16(ex[2]))
	left := false
	for _, x := range g.Since(k) {
		if x.Cmd == 0x1006 && x.Body[0x24] == 10 {
			left = true
		}
	}
	check(err == nil && left, "leaving through the exit ends the mission (0x1006 10) and loads the lobby")
	return out, problems, nil
}

// partyScenario: A invites B, B accepts, both see the party; B leaves.
func partyScenario(login, game, tag string) ([]record, error) {
	var out []record
	pa := &bot.Player{Name: "pa" + tag, Disc: "psu", LoginAddr: login, GameAddr: game}
	pb := &bot.Player{Name: "pb" + tag, Disc: "psu", LoginAddr: login, GameAddr: game}
	for _, p := range []*bot.Player{pa, pb} {
		if err := p.Login("testplay"); err != nil {
			return nil, err
		}
		if err := p.Enter(true, 1, 0, 2); err != nil {
			return nil, err
		}
		defer p.Game.Close()
	}
	time.Sleep(300 * time.Millisecond)
	ma, mb := pa.Game.Mark(), pb.Game.Mark()
	snap := func(step string) {
		time.Sleep(400 * time.Millisecond)
		out = append(out, record{step, "A", pa.Game.Since(ma)}, record{step, "B", pb.Game.Since(mb)})
		ma, mb = pa.Game.Mark(), pb.Game.Mark()
	}
	pa.Invite(pb.GID)
	snap("A invites B")
	pb.Answer(pa.GID, true)
	snap("B accepts")
	pb.Game.Send(0x1002, nil)
	snap("B leaves the party")
	return out, nil
}

// missionLoop plays ms over and over until deadline; it returns the clears.
func missionLoop(p *bot.Player, ms mission, deadline time.Time) (int, error) {
	g := p.Game
	inv, err := g.Wait(0x0A0A, 0, 0)
	if err != nil {
		return 0, err
	}
	slot, weapon := -1, uint32(0)
	for i, it := range bot.Inventory(inv) {
		if it[1]>>24 == 1 {
			slot, weapon = i, it[0]
			break
		}
	}
	if err := p.Equip(slot, weapon); err != nil {
		return 0, err
	}
	ex := ms.m.Exit
	if len(ex) < 3 {
		ex = []int{2, 4, 6}
	}
	clears := 0
	for seq := uint32(0); time.Now().Before(deadline); {
		g.Clear()
		if _, err := p.Counter(0); err != nil {
			return clears, err
		}
		if _, err := p.Accept(ms.qid); err != nil {
			return clears, err
		}
		if _, err := p.Depart(ms.qid); err != nil {
			return clears, err
		}
		for _, e := range ms.enemies {
			p.ReportEnemy(e)
		}
		alive := map[uint32]bool{}
		for _, e := range ms.enemies {
			alive[e] = true
		}
		start := g.Mark()
		for round := 0; len(alive) > 0 && round < 300; round++ {
			var targets []uint32
			for _, e := range ms.enemies {
				if alive[e] && len(targets) < 20 {
					targets = append(targets, e)
				}
			}
			k := g.Mark()
			seq++
			p.Hits(seq, targets, weapon)
			r, err := g.WaitFor(0x0E07, k, 5*time.Second, func(x wire.Packet) bool {
				return len(x.Body) >= 0x28+0x68 && binary.LittleEndian.Uint32(x.Body[0x28+0x14:]) == uint32(p.LID)
			})
			if err != nil {
				return clears, err
			}
			for i := range int(binary.LittleEndian.Uint32(r.Body[0x24:])) {
				e := r.Body[0x28+0x68*i:]
				if binary.LittleEndian.Uint32(e[0x34:])&0x20000 != 0 {
					delete(alive, binary.LittleEndian.Uint32(e[0x18:]))
				}
			}
		}
		for _, f := range ms.flags {
			p.Flag(f)
		}
		if _, err := g.WaitFor(0x0C09, start, 6*time.Second, func(x wire.Packet) bool { return x.Body[0x24] == 1 }); err == nil {
			clears++
		}
		if _, err := p.Leave(uint16(ex[0]), uint16(ex[1]), uint16(ex[2])); err != nil {
			return clears, err
		}
	}
	return clears, nil
}
