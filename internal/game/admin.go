// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/store"
)

// What the admin API (internal/admin, wired in cmd/psud) asks the running game: the same
// operations as the GM chat commands, by GID or name.

// Status is what status.json and the admin API's /status hold.
func (s *Server) Status() any {
	s.init()
	online := s.world.players()
	unis, areas, games := map[string]int{}, map[string]int{}, map[string]int{}
	for _, p := range online {
		unis[strconv.Itoa(int(p.uni.Load()))]++
		games[p.disc]++
		key := "-"
		if w := p.where.Load(); w != nil {
			key = fmt.Sprintf("%d/%d", w.area.Quest, w.area.Zone)
		}
		areas[key]++
	}
	s.world.mu.Lock()
	zones := len(s.world.zones)
	s.world.mu.Unlock()
	now := time.Now()
	running := []string{}
	for _, r := range s.calendar().Running(now, "") {
		running = append(running, r.E.Key)
	}
	return map[string]any{"time": now.UTC().Format(time.RFC3339), "uptime_s": int64(now.Sub(s.ops.started).Seconds()),
		"online": len(online), "games": games, "universes": unis, "areas": areas, "zones": zones,
		"season": nilIfEmpty(s.calendar().Current(now, s.Season)), "events": running}
}

// Players is everyone online: character, account, disc, universe, area.
func (s *Server) Players() any {
	s.init()
	rows := []map[string]any{}
	for _, p := range s.world.players() {
		row := map[string]any{"name": charName(p), "account": p.user, "gid": fmt.Sprintf("%#08x", p.gid),
			"game": p.disc, "uni": p.uni.Load()}
		if w := p.where.Load(); w != nil {
			row["area"] = []uint32{w.area.Quest, w.area.Zone, w.area.Map}
		}
		s.world.partyMu.Lock()
		row["party"] = p.party != nil
		s.world.partyMu.Unlock()
		_, row["muted"] = s.muted(p.gid)
		rows = append(rows, row)
	}
	return rows
}

var noticeKinds = map[string]uint32{"center": noticeCentre, "ticker": noticeTicker, "right": noticeRight, "dialog": noticeDialog}

// Announce sends a notice: scope "all" (or ""), "uni:N", "area:QUEST/ZONE" or
// "player:NAME"; kind ticker, right, dialog or center. It returns how many got it.
func (s *Server) Announce(text, scope, kind string, secs int) (int, error) {
	s.init()
	k, ok := noticeKinds[cmpStr(kind, "ticker")]
	if !ok {
		return 0, fmt.Errorf("kind: ticker, right, dialog or center")
	}
	var pick func(*player) bool
	what, arg, _ := strings.Cut(scope, ":")
	switch what {
	case "", "all":
	case "uni":
		n, err := strconv.Atoi(arg)
		if err != nil {
			return 0, fmt.Errorf("scope uni:N")
		}
		pick = func(p *player) bool { return int(p.uni.Load()) == n }
	case "area":
		q, z, ok := strings.Cut(arg, "/")
		qn, err1 := strconv.Atoi(q)
		zn, err2 := strconv.Atoi(z)
		if !ok || err1 != nil || err2 != nil {
			return 0, fmt.Errorf("scope area:QUEST/ZONE")
		}
		pick = func(p *player) bool {
			w := p.where.Load()
			return w != nil && int(w.area.Quest) == qn && int(w.area.Zone) == zn
		}
	case "player":
		_, p, _ := s.findPlayer(arg)
		if p == nil {
			return 0, fmt.Errorf("%s is not online", arg)
		}
		pick = func(o *player) bool { return o == p }
	default:
		return 0, fmt.Errorf("scope: all, uni:N, area:QUEST/ZONE or player:NAME")
	}
	if secs <= 0 && k == noticeDialog {
		secs = 10
	}
	n := s.broadcast(text, k, uint32(max(secs, 0)), pick)
	s.Log.Info("announce", "scope", cmpStr(scope, "all"), "kind", kind, "to", n)
	return n, nil
}

// Find resolves a character online, an account or a character name.
func (s *Server) Find(name string) (gid uint32, online, ok bool) {
	s.init()
	gid, p, ok := s.findPlayer(name)
	return gid, p != nil, ok
}

// Online reports whether gid is in the game.
func (s *Server) Online(gid uint32) bool { s.init(); return s.world.find(gid) != nil }

// Kick tells gid why and drops it; false when it was not online.
func (s *Server) Kick(gid uint32, reason string) bool {
	s.init()
	p := s.world.find(gid)
	if p != nil {
		s.kick(p, reason)
	}
	return p != nil
}

// Mute keeps gid's chat from being relayed for minutes (0: until unmuted).
func (s *Server) Mute(gid uint32, minutes int, reason, by string) error {
	s.init()
	m := store.Mute{GID: gid, Reason: reason, By: by}
	if minutes > 0 {
		m.Until = time.Now().Add(time.Duration(minutes) * time.Minute)
	}
	return s.mute(m)
}

func (s *Server) Unmute(gid uint32) bool { s.init(); return s.unmute(gid) }

// SeasonPreview is !season for gid (a name, none or off); "" shows the season now.
func (s *Server) SeasonPreview(gid uint32, value string) string {
	s.init()
	if value == "" {
		return s.seasonCommand(gid, nil)
	}
	return s.seasonCommand(gid, []string{value})
}

// ReloadEvents re-reads the calendar now; it returns the specs it skipped and why.
func (s *Server) ReloadEvents(ctx context.Context) []string {
	s.init()
	return s.loadCalendar(ctx).Problems
}
