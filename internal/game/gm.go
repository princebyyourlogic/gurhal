// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/calendar"
	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/store"
)

// Chat commands: a "!" line is a command, never relayed; replies are timed dialogs.
//
//	everyone  !help !who !news !gm <message to the staff>
//	admins    !announce / !uni / !zone <text> (ticker), !notice <text> (dialog to all),
//	          !online !stats !spawns, !kick <name> [reason], !ban <name> [hours] [reason]
//	          (0 = 10 years), !unban, !mute <name> [minutes] [reason] (0 = until unmuted),
//	          !unmute, !summon / !goto <name> (lobbies, same universe), !give <item> [qty]
//	          [name], !meseta <n | +n | -n> [name], !level <n>, !warp <quest> <zone> <map>
//	          [entry], !season [name | none | off], !clear
//
// <name> is a character online, an account or a character name. Admins are the accounts
// in Server.Admins; every admin command is logged.

const (
	helpAll   = "!who !news !gm <message to the staff>"
	helpAdmin = "!announce !uni !zone !notice !online !stats !spawns !kick !ban !unban !mute !unmute !summon !goto " +
		"!give !meseta !level !warp !news !season !gm !clear"
	noEnemy = 0xFFFFFFFE // an EXP source that is no enemy: the level-up effect without a figure
)

func charName(p *player) string {
	if snap := p.snap.Load(); snap != nil {
		return rules.UTF16Text(snap.raw[rules.RawName:rules.RawRace])
	}
	return "?"
}

// findPlayer is a character name online, else an account or character name: its GID and
// the player when online.
func (s *Server) findPlayer(name string) (uint32, *player, bool) {
	for _, p := range s.world.players() {
		if strings.EqualFold(charName(p), name) {
			return p.gid, p, true
		}
	}
	gid, err := s.Store.FindGID(s.ctx(), name)
	if err != nil {
		return 0, nil, false
	}
	return gid, s.world.find(gid), true
}

func (s *Server) kick(p *player, reason string) {
	msg := "You have been disconnected by a GM."
	if reason != "" {
		msg += " Reason: " + reason
	}
	say(p, msg, 5)
	time.AfterFunc(1500*time.Millisecond, p.conn.Close)
}

func (s *Server) mute(m store.Mute) error {
	if err := s.Store.SetMute(s.ctx(), m); err != nil {
		return err
	}
	s.ops.mu.Lock()
	s.ops.mutes[m.GID] = m
	s.ops.mu.Unlock()
	if p := s.world.find(m.GID); p != nil {
		text := mutedText(m)
		if m.Reason != "" {
			text += " Reason: " + m.Reason
		}
		say(p, text, 5)
	}
	return nil
}

func (s *Server) unmute(gid uint32) bool {
	s.ops.mu.Lock()
	delete(s.ops.mutes, gid)
	s.ops.mu.Unlock()
	ok, _ := s.Store.Unmute(s.ctx(), gid)
	return ok
}

// gmCommand runs a "!" chat line from s.
func (s *session) gmCommand(text string) {
	fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(text), "!"))
	if len(fields) == 0 {
		return
	}
	cmd, args := strings.ToLower(fields[0]), fields[1:]
	srv, me := s.srv, s.me
	admin := srv.admin(s.user)
	reply := func(line string, secs uint32) { say(me, line, secs) }
	if admin {
		s.c.Log.Info("gm", "user", s.user, "cmd", cmd, "args", args[:min(len(args), 6)])
	}

	switch {
	case cmd == "help":
		reply(map[bool]string{false: helpAll, true: helpAdmin}[admin], 5)
		return
	case cmd == "news" && !(admin && len(args) > 0 && args[0] == "reload"):
		if n := srv.notices(s.ctx); len(n) > 0 {
			reply(strings.Join(n, "\n\n"), 15)
		} else {
			reply("No news.", 15)
		}
		return
	case cmd == "gm":
		s.char.mu.Lock()
		name := rules.UTF16Text(s.char.raw[rules.RawName:rules.RawRace])
		s.char.mu.Unlock()
		reply(srv.helpAsk(s.ctx, s.gid, name, strings.Join(args, " ")), 8)
		return
	case cmd == "who" && !admin:
		reply(fmt.Sprintf("%d player(s) online.", len(srv.world.players())), 5)
		return
	case !admin:
		reply("Unknown command.", 5)
		return
	}

	rest := strings.Join(args, " ")
	switch cmd {
	case "who", "online":
		var rows []string
		for _, p := range srv.world.players() {
			q := "-"
			if w := p.where.Load(); w != nil {
				q = strconv.Itoa(int(w.area.Quest))
			}
			rows = append(rows, fmt.Sprintf("%s (%s %s U%d %s)", charName(p), p.user, p.disc, p.uni.Load(), q))
		}
		reply(fmt.Sprintf("%d online: %s", len(rows), strings.Join(rows, ", ")), 10)
	case "news":
		srv.loadCalendar(s.ctx)
		reply(fmt.Sprintf("News: %d notice(s).", len(srv.notices(s.ctx))), 5)
	case "stats":
		reply(srv.stats(), 8)
	case "spawns":
		if s.zone == nil {
			reply("Not in a zone.", 8)
			return
		}
		s.zone.spawns(me)
	case "announce", "uni", "zone", "notice":
		if rest == "" {
			break
		}
		kind, secs, pick := uint32(noticeTicker), uint32(0), func(*player) bool { return true }
		switch cmd {
		case "notice":
			kind, secs = noticeDialog, 10
		case "uni":
			uni := me.uni.Load()
			pick = func(p *player) bool { return p.uni.Load() == uni }
		case "zone":
			if s.zone == nil {
				reply("Not in a zone.", 3)
				return
			}
			s.zone.do(func(zs *zoneState) {
				zs.broadcast(func(d *member) []byte { return c0228(d.p.gid, rest, noticeTicker, 0) })
				say(me, fmt.Sprintf("Sent to %d.", len(zs.members)), 3)
			})
			return
		}
		if n := srv.broadcast(rest, kind, secs, pick); cmd != "announce" || n == 0 {
			reply(fmt.Sprintf("Sent to %d.", n), 3)
		}
	case "kick", "ban", "unban", "mute", "unmute", "summon", "goto":
		if len(args) == 0 {
			break
		}
		s.gmTarget(cmd, args)
	case "season":
		reply(srv.seasonCommand(s.gid, args), 10)
	case "give":
		if len(args) == 0 {
			break
		}
		qty, target := 1, me
		if len(args) > 1 {
			qty, _ = strconv.Atoi(args[1])
		}
		if len(args) > 2 {
			if _, target, _ = srv.findPlayer(strings.Join(args[2:], " ")); target == nil {
				reply(args[2]+" is not online.", 5)
				return
			}
		}
		reply(srv.giveItem(target, strings.ToLower(fmt.Sprintf("%08s", args[0])), max(1, min(qty, 99))), 5)
	case "meseta":
		if len(args) == 0 {
			break
		}
		target := me
		if len(args) > 1 {
			if _, target, _ = srv.findPlayer(strings.Join(args[1:], " ")); target == nil {
				reply(args[1]+" is not online.", 5)
				return
			}
		}
		reply(srv.setMeseta(target, args[0]), 5)
	case "level":
		n, err := strconv.Atoi(rest)
		if err != nil {
			break
		}
		reply(s.setLevel(n), 5)
	case "warp":
		var v [4]uint64
		for i := range min(len(args), 4) {
			v[i], _ = strconv.ParseUint(args[i], 10, 32)
		}
		if len(args) < 3 {
			break
		}
		a := resolve(s.disc, uint32(v[0]), uint32(v[1]), uint32(v[2]))
		if !s.available(a.Quest, a.Zone) {
			reply(fmt.Sprintf("No archives for %d/%d", a.Quest, a.Zone), 5)
			return
		}
		s.c.Log.Info("gm warp", "to", fmt.Sprintf("%d/%d/%d", a.Quest, a.Zone, a.Map))
		s.goZone(a, uint16(v[3]), s.zoneKeyFor(a), lobbyZone, a.Quest != s.area.Quest)
	case "clear":
		if s.mission == nil || !s.mission.in || s.zone == nil {
			reply("Not on a mission.", 5)
			return
		}
		s.zone.forceClear(s.gid)
	default:
		reply("Unknown command. !help", 5)
	}
}

// gmTarget runs the commands about another player.
func (s *session) gmTarget(cmd string, args []string) {
	srv, me := s.srv, s.me
	reply := func(line string) { say(me, line, 5) }
	gid, p, ok := srv.findPlayer(args[0])
	if !ok {
		reply("No such player: " + args[0])
		return
	}
	name := args[0]
	if p != nil {
		name = charName(p)
	}
	reason := func(from int) string {
		if len(args) <= from {
			return ""
		}
		r := []rune(strings.Join(args[from:], " "))
		return string(r[:min(len(r), 200)])
	}
	switch cmd {
	case "unban":
		srv.Store.SetBan(s.ctx, gid, store.Ban{})
		reply("Unbanned " + name + ".")
	case "ban":
		hours := 24.0
		if len(args) > 1 {
			if h, err := strconv.ParseFloat(args[1], 64); err == nil && h >= 0 {
				hours = h
			}
		}
		until := time.Now().Add(time.Duration(hours * float64(time.Hour)))
		if hours == 0 {
			until = time.Now().AddDate(10, 0, 0)
		}
		if err := srv.Store.SetBan(s.ctx, gid, store.Ban{Until: until, Reason: reason(2), By: s.user}); err != nil {
			reply("ban failed: " + err.Error())
			return
		}
		s.c.Log.Info("gm ban", "target", fmt.Sprintf("%#08x", gid), "hours", hours, "reason", reason(2))
		if p != nil && p != me {
			srv.kick(p, cmpStr(reason(2), "banned"))
			reply("ban " + name + ": done")
		} else {
			reply("ban " + name + ": done (offline)")
		}
	case "kick":
		if p == nil || p == me {
			reply(map[bool]string{true: name + " is not online.", false: "Not yourself."}[p == nil])
			return
		}
		srv.kick(p, reason(1))
		reply("kick " + name + ": done")
	case "mute":
		minutes := 60
		if len(args) > 1 {
			if n, err := strconv.Atoi(args[1]); err == nil {
				minutes = n
			}
		}
		m := store.Mute{GID: gid, Reason: reason(2), By: s.user}
		if minutes > 0 {
			m.Until = time.Now().Add(time.Duration(minutes) * time.Minute)
		}
		if err := srv.mute(m); err != nil {
			reply("mute failed: " + err.Error())
			return
		}
		if minutes > 0 {
			reply(fmt.Sprintf("Muted %s for %d min.", name, minutes))
		} else {
			reply("Muted " + name + ".")
		}
	case "unmute":
		if srv.unmute(gid) {
			reply("Unmuted " + name + ".")
		} else {
			reply(name + " was not muted.")
		}
	case "summon", "goto":
		if p == nil {
			reply(name + " is not online.")
			return
		}
		if p == me {
			reply("Not yourself.")
			return
		}
		mover, to := p, me
		if cmd == "goto" {
			mover, to = me, p
		}
		if err := srv.bring(mover, to, me); err != nil {
			reply(err.Error())
		}
	}
}

func cmpStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// bring sends mover to to's lobby spot, on mover's session goroutine; a refusal there is
// told to gm.
func (s *Server) bring(mover, to, gm *player) error {
	w := to.where.Load()
	if w == nil || !lobbyQuests[w.area.Quest] {
		return errors.New(charName(to) + " is not in a lobby.")
	}
	if mover.uni.Load() != to.uni.Load() {
		return fmt.Errorf("%s is in universe %d, %s in %d.", charName(mover), mover.uni.Load(), charName(to), to.uni.Load())
	}
	area, entry := mapFor(w.area, mover.disc, to.disc), w.entry
	ok := mover.job(func(ms *session) {
		fail := func(why string) {
			if s.world.find(gm.gid) == gm {
				say(gm, why, 5)
			}
		}
		switch {
		case ms.char == nil || ms.me == nil:
			fail(charName(mover) + " cannot be moved now.")
		case ms.mission != nil:
			fail(charName(mover) + " is on a mission.")
		case !ms.available(area.Quest, area.Zone):
			fail(fmt.Sprintf("No archives for %d/%d on %s.", area.Quest, area.Zone, ms.disc))
		default:
			ms.c.Log.Info("gm move", "to", fmt.Sprintf("%d/%d/%d", area.Quest, area.Zone, area.Map))
			ms.goZone(area, entry, ms.zoneKeyFor(area), lobbyZone, area.Quest != ms.area.Quest)
		}
	})
	if !ok {
		return errors.New(charName(mover) + " cannot be moved now.")
	}
	return nil
}

// giveItem adds an item to p's inventory (its character locked) and tells p.
func (s *Server) giveItem(p *player, hex string, qty int) string {
	var id rules.ItemID
	if id.UnmarshalText([]byte(hex)) != nil {
		return "No item " + hex
	}
	def, ok := p.r.Item(id)
	if !ok {
		return "No item " + hex + " on " + p.disc
	}
	c := p.char.Load()
	if c == nil {
		return "No character."
	}
	c.mu.Lock()
	added, err := p.r.AddItem(c.st, id, qty, 0)
	if err == nil {
		s.saveChar(p, c)
	}
	c.mu.Unlock()
	if err != nil {
		return err.Error()
	}
	p.conn.Relay(c0106Add(p.r, p.disc, p.gid, p.lid(), added))
	return fmt.Sprintf("Gave %s x%d to %s", def.Name, added.Qty, charName(p))
}

// setMeseta sets (n) or changes (+n / -n) p's meseta.
func (s *Server) setMeseta(p *player, arg string) string {
	n, err := strconv.Atoi(arg)
	if err != nil {
		return "meseta: a number, +n or -n"
	}
	c := p.char.Load()
	if c == nil {
		return "No character."
	}
	c.mu.Lock()
	v := n
	if arg[0] == '+' || arg[0] == '-' {
		v = int(c.st.Meseta) + n
	}
	c.st.Meseta = uint32(max(0, min(v, 999_999_999)))
	s.saveChar(p, c)
	block, m := rules.LevelBlock(c.st, p.disc), c.st.Meseta
	c.mu.Unlock()
	p.conn.Relay(c0115(p.gid, p.lid(), noSource, block))
	return fmt.Sprintf("%s: meseta %d", charName(p), m)
}

// saveChar queues another session's character for saving; hold c.mu.
func (s *Server) saveChar(p *player, c *character) {
	c.bank(time.Now())
	s.saver.queue(p.gid, c.slot, c.st)
	p.snap.Store(c.snapshot(p.r))
}

// setLevel is !level on oneself: the level, its EXP and full HP, shown as a level-up.
func (s *session) setLevel(n int) string {
	c, r := s.char, s.r
	c.mu.Lock()
	c.st.Level = max(1, min(n, r.LevelCap()))
	c.st.EXP = r.ExpForLevel(c.raw, c.st.Level)
	c.st.HP = r.Derived(c.raw, c.st).MaxHP
	s.saveLocked()
	level := c.st.Level
	user := r.User(s.gid, s.lid, c.raw, c.st, s.area, s.entry, rules.Pos{}, s.disc, "")
	c.mu.Unlock()
	s.c.Send(c010d(s.lid, s.gid, s.gid, s.lid, user, noEnemy, true))
	return fmt.Sprintf("Level %d", level)
}

// seasonCommand is !season: no argument shows what runs; a name previews it on the GM's
// next lobby load, "none" previews no season, "off" follows the server again.
func (s *Server) seasonCommand(gid uint32, args []string) string {
	staged := s.Data.Seasons()
	if len(args) == 0 {
		cur := s.calendar().Current(time.Now(), s.Season)
		return fmt.Sprintf("Season %s (PSU_SEASON=%s). Staged: %s.", cmpStr(cur, "none"), calendar.Mode(s.Season),
			cmpStr(strings.Join(staged, ", "), "nothing"))
	}
	v := strings.ToLower(args[0])
	s.ops.mu.Lock()
	defer s.ops.mu.Unlock()
	switch v {
	case "off", "clear", "server":
		delete(s.ops.previews, gid)
		return "Season preview off: the server's lobbies again on the next lobby load."
	case "none", "no":
		s.ops.previews[gid] = ""
		return "Previewing no season on the next lobby load."
	}
	name := calendar.NameOf(v)
	if name == "" {
		return "season: one of " + strings.Join(calendar.Seasons, ", ") + ", none or off"
	}
	s.ops.previews[gid] = name
	for _, st := range staged {
		if st == name {
			return "Previewing " + name + " on the next lobby load."
		}
	}
	return "Previewing " + name + " on the next lobby load (nothing staged for it: no change)."
}

func (s *Server) stats() string {
	online := s.world.players()
	games, unis := map[string]int{}, map[uint32]int{}
	for _, p := range online {
		games[p.disc]++
		unis[p.uni.Load()]++
	}
	s.world.mu.Lock()
	zones := len(s.world.zones)
	s.world.mu.Unlock()
	parties := map[*party]bool{}
	s.world.partyMu.Lock()
	for _, p := range online {
		if p.party != nil {
			parties[p.party] = true
		}
	}
	s.world.partyMu.Unlock()
	s.social.mu.Lock()
	trades := map[*tradeState]bool{}
	for _, t := range s.social.trades {
		trades[t] = true
	}
	s.social.mu.Unlock()
	s.ops.mu.Lock()
	mutes := len(s.ops.mutes)
	s.ops.mu.Unlock()
	up := time.Since(s.ops.started)
	line := fmt.Sprintf("Up %dh%02dm, %d online", int(up.Hours()), int(up.Minutes())%60, len(online))
	for _, g := range []string{rules.AotI, rules.PSU} {
		if games[g] > 0 {
			line += fmt.Sprintf(" %s:%d", g, games[g])
		}
	}
	line += " | unis"
	for _, u := range s.Universes {
		if unis[u.ID] > 0 {
			line += fmt.Sprintf(" U%d:%d", u.ID, unis[u.ID])
		}
	}
	return line + fmt.Sprintf(" | zones %d parties %d trades %d mutes %d", zones, len(parties), len(trades), mutes)
}

// spawns tells gm what this zone holds.
func (z *zone) spawns(gm *player) {
	z.do(func(zs *zoneState) {
		rm := &zs.room
		alive := 0
		for _, e := range rm.enemies {
			if !e.Dead {
				alive++
			}
		}
		owner := ""
		if z.key.owner != 0 {
			owner = fmt.Sprintf(" owner %#x", z.key.owner)
		}
		say(gm, fmt.Sprintf("U%d %d/%d%s: %d player(s), enemies %d spawned / %d alive / %d in the set data, %d ground item(s), %d box(es)",
			z.key.uni, z.key.quest, z.key.zone, owner, len(zs.members), len(rm.enemies), alive, len(rm.roster),
			len(rm.ground), len(rm.boxes)), 8)
	})
}
