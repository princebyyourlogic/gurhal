// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/store"
)

// The public board: a JSON snapshot (prettystellaruniverse's board.json, version 1) for an
// outside board service to show; it reads the file and never the database. Names are
// character names only, never account names; banned accounts are left out of every list.
// Ranks are letters (judge rank 0..3 = C, B, A, S).

const (
	boardTop    = 25
	boardBest   = 5 // distinct parties per mission
	boardRecent = 20
	boardWeek   = 7 * 24 * time.Hour
)

var classNames = []string{"Hunter", "Ranger", "Force", "Fighgunner", "Guntecher", "Wartecher", "Fortefighter",
	"Fortegunner", "Fortetecher", "Protranser", "Acrofighter", "Acrotecher", "Fighmaster", "Gunmaster", "Masterforce",
	"Acromaster"}

func className(cls int) string {
	if cls >= 0 && cls < len(classNames) {
		return classNames[cls]
	}
	return "?"
}

func rankLetter(r int) string {
	if r >= 0 && r < 4 {
		return string("CBAS"[r])
	}
	return "?"
}

// charGame is the disc a character last played on (an AotI-only type says AotI).
func charGame(st *rules.State) string {
	if st.Game != "" {
		return st.Game
	}
	if st.Cls >= 10 {
		return rules.AotI
	}
	return rules.PSU
}

type boardClear struct {
	Quest uint32   `json:"quest,omitempty"`
	Name  string   `json:"name,omitempty"`
	Game  string   `json:"game,omitempty"`
	Secs  int      `json:"secs"`
	Rank  string   `json:"rank"`
	Party []string `json:"party"`
	When  int64    `json:"when"`
}

type boardMission struct {
	Quest  uint32       `json:"quest"`
	Name   string       `json:"name"`
	Game   string       `json:"game,omitempty"`
	Clears int          `json:"clears,omitempty"`
	Best   []boardClear `json:"best"`
}

type boardPlayer struct {
	Name    string `json:"name"`
	Level   int    `json:"level"`
	Cls     string `json:"cls"`
	Game    string `json:"game"`
	Uni     uint32 `json:"uni"`
	Where   string `json:"where"`
	Mission string `json:"mission,omitempty"`
}

func (s *Server) missionName(q uint32, game string) string {
	for _, d := range []string{game, rules.PSU, rules.AotI} {
		if m := s.Rules.For(d).Mission(q); m != nil && m.Name != "" {
			return m.Name
		}
	}
	return fmt.Sprintf("Mission %d", q)
}

// bestClears is the fastest clear of each distinct party (clears sorted fastest first),
// at most n, leaving banned members out.
func bestClears(clears []store.Clear, n int, banned map[uint32]bool) []boardClear {
	out := []boardClear{}
	seen := map[string]bool{}
	for _, c := range clears {
		who, key := partyNames(c.Party, banned), partyKey(c.Party)
		if len(who) == 0 || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, boardClear{Secs: c.Secs, Rank: rankLetter(c.Rank), Party: who, When: c.At.Unix()})
		if len(out) >= n {
			break
		}
	}
	return out
}

func partyNames(party []store.Member, banned map[uint32]bool) []string {
	var out []string
	for _, m := range party {
		if !banned[m.GID] {
			out = append(out, m.Name)
		}
	}
	return out
}

func partyKey(party []store.Member) string {
	keys := make([]string, len(party))
	for i, m := range party {
		keys[i] = fmt.Sprintf("%d/%d", m.GID, m.Slot)
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

// boardSnapshot builds the board from the store and the world.
func (s *Server) boardSnapshot(ctx context.Context, now time.Time) (map[string]any, error) {
	bannedList, err := s.Store.BannedGIDs(ctx, now)
	if err != nil {
		return nil, err
	}
	banned := map[uint32]bool{}
	for _, g := range bannedList {
		banned[g] = true
	}
	all, err := s.Store.AllCharacters(ctx)
	if err != nil {
		return nil, err
	}
	clears, err := s.Store.Clears(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := s.Store.CountAccounts(ctx)
	if err != nil {
		return nil, err
	}

	type char struct {
		name string
		st   rules.State
	}
	var chars []char
	for _, o := range all {
		var st rules.State
		if o.Name == "" || banned[o.GID] || json.Unmarshal(o.State, &st) != nil || st.Level == 0 {
			continue
		}
		chars = append(chars, char{o.Name, st})
	}
	sort.SliceStable(chars, func(i, j int) bool {
		a, b := chars[i], chars[j]
		if a.st.Level != b.st.Level {
			return a.st.Level > b.st.Level
		}
		if a.st.EXP != b.st.EXP {
			return a.st.EXP > b.st.EXP
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
	levels := []map[string]any{}
	for _, c := range chars[:min(len(chars), boardTop)] {
		levels = append(levels, map[string]any{"name": c.name, "level": c.st.Level, "cls": className(c.st.Cls),
			"game": charGame(&c.st), "hours": c.st.Playtime / 3600})
	}
	types := []map[string]any{}
	for t, tname := range classNames {
		type row struct {
			Name  string `json:"name"`
			Level int    `json:"level"`
		}
		var rows []row
		for _, c := range chars {
			if lv := c.st.TypeLevel[t]; lv > 1 {
				rows = append(rows, row{c.name, lv})
			}
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Level != rows[j].Level {
				return rows[i].Level > rows[j].Level
			}
			return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
		})
		if len(rows) > 0 {
			types = append(types, map[string]any{"cls": tname, "rows": rows[:min(len(rows), 10)]})
		}
	}

	fastest := slices.Clone(clears)
	sort.SliceStable(fastest, func(i, j int) bool {
		if fastest[i].Secs != fastest[j].Secs {
			return fastest[i].Secs < fastest[j].Secs
		}
		return fastest[i].At.Before(fastest[j].At)
	})
	byQuest := map[uint32][]store.Clear{}
	var order []uint32
	week, weekClears := map[string]int{}, 0
	for _, c := range fastest {
		if len(partyNames(c.Party, banned)) == 0 {
			continue
		}
		if byQuest[c.Quest] == nil {
			order = append(order, c.Quest)
		}
		byQuest[c.Quest] = append(byQuest[c.Quest], c)
		if now.Sub(c.At) <= boardWeek {
			for _, n := range partyNames(c.Party, banned) {
				week[n]++
			}
		}
	}
	missions := []boardMission{}
	for _, q := range order {
		cs := byQuest[q]
		missions = append(missions, boardMission{Quest: q, Name: s.missionName(q, cs[0].Game), Game: cs[0].Game,
			Clears: len(cs), Best: bestClears(cs, boardBest, banned)})
	}
	sort.SliceStable(missions, func(i, j int) bool {
		if missions[i].Clears != missions[j].Clears {
			return missions[i].Clears > missions[j].Clears
		}
		return missions[i].Quest < missions[j].Quest
	})
	recent := []boardClear{}
	for i := len(clears) - 1; i >= 0 && len(recent) < boardRecent; i-- {
		c := clears[i]
		if who := partyNames(c.Party, banned); len(who) > 0 {
			recent = append(recent, boardClear{Quest: c.Quest, Name: s.missionName(c.Quest, c.Game), Game: c.Game,
				Secs: c.Secs, Rank: rankLetter(c.Rank), Party: who, When: c.At.Unix()})
		}
	}
	for _, c := range clears {
		if now.Sub(c.At) <= boardWeek {
			weekClears++
		}
	}
	weekRows := []map[string]any{}
	names := make([]string, 0, len(week))
	for n := range week {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if week[names[i]] != week[names[j]] {
			return week[names[i]] > week[names[j]]
		}
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
	for _, n := range names[:min(len(names), boardTop)] {
		weekRows = append(weekRows, map[string]any{"name": n, "clears": week[n]})
	}

	cal := s.calendar()
	events := cal.BoardRows(now)
	for i := range events {
		e := &events[i]
		if len(e.Missions) == 0 || e.Start > now.Unix() {
			continue
		}
		var results []boardMission
		for _, q := range e.Missions {
			var during []store.Clear
			for _, c := range fastest {
				if c.Quest == q && c.At.Unix() >= e.Start && c.At.Unix() <= min(e.End, now.Unix()) {
					during = append(during, c)
				}
			}
			results = append(results, boardMission{Quest: q, Name: s.missionName(q, rules.PSU), Best: bestClears(during, 10, banned)})
		}
		e.Results = results
	}
	season := map[string]any{"now": nilIfEmpty(cal.Current(now, s.Season)), "next": nil}

	return map[string]any{
		"v": 1, "updated": now.Unix(), "uptime_s": int64(now.Sub(s.ops.started).Seconds()),
		"online": s.boardOnline(), "levels": levels, "types": types, "missions": missions, "recent": recent,
		"week": weekRows, "story": []any{}, "season": season, "events": events,
		"totals": map[string]any{"accounts": accounts, "characters": len(all), "clears": len(clears), "clears_week": weekClears},
	}, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Server) boardOnline() map[string]any {
	perGame := map[string]int{rules.PSU: 0, rules.AotI: 0}
	perUni := map[uint32]int{}
	players := []boardPlayer{}
	online := s.world.players()
	for _, p := range online {
		perGame[p.disc]++
		perUni[p.uni.Load()]++
		c, snap := p.char.Load(), p.snap.Load()
		if c == nil || snap == nil {
			continue
		}
		bp := boardPlayer{Name: rules.UTF16Text(snap.raw[rules.RawName:rules.RawRace]), Level: snap.st.Level,
			Cls: className(snap.st.Cls), Game: p.disc, Uni: p.uni.Load(), Where: "lobby"}
		if w := p.where.Load(); w != nil && !lobbyQuests[w.area.Quest] && w.area.Quest != 0 {
			if m := p.r.Mission(w.area.Quest); m != nil {
				bp.Where, bp.Mission = "mission", s.missionName(w.area.Quest, p.disc)
			} else {
				bp.Where = "room"
			}
		}
		players = append(players, bp)
	}
	sort.SliceStable(players, func(i, j int) bool {
		if players[i].Level != players[j].Level {
			return players[i].Level > players[j].Level
		}
		return strings.ToLower(players[i].Name) < strings.ToLower(players[j].Name)
	})
	unis := []map[string]any{}
	for _, u := range s.Universes {
		unis = append(unis, map[string]any{"id": u.ID, "name": u.Name, "count": perUni[u.ID]})
	}
	return map[string]any{"total": len(online), "psu": perGame[rules.PSU], "aoti": perGame[rules.AotI],
		"players": players, "universes": unis}
}

// writeBoard replaces the board file atomically.
func (s *Server) writeBoard(ctx context.Context) {
	snap, err := s.boardSnapshot(ctx, time.Now())
	if err == nil {
		var b []byte
		if b, err = json.Marshal(snap); err == nil {
			if err = os.MkdirAll(filepath.Dir(s.BoardFile), 0o755); err == nil {
				tmp := s.BoardFile + ".tmp"
				if err = os.WriteFile(tmp, b, 0o644); err == nil {
					err = os.Rename(tmp, s.BoardFile)
				}
			}
		}
	}
	if err != nil {
		s.Log.Error("board", "err", err)
	}
}
