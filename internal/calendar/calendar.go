// SPDX-License-Identifier: GPL-3.0-or-later

// Package calendar is the operator's event calendar and the lobby seasons: limited-time
// events the way Sega ran PSU's network service (seasonal lobbies, EXP / drop campaigns,
// featured missions). Pure: the game service loads the specs from the store and asks.
//
// An event spec (JSON, one per store row):
//
//	{"key": "halloween-2026", "name": "Halloween Festival",
//	 "start": "2026-10-28T00:00Z", "end": "2026-11-08T00:00Z",   one window (UTC), or
//	 "weekly": {"dow": [5, 6], "from": "00:00", "hours": 24},    every week (0 = Monday), UTC
//	 "game": "psu" | "aoti",                                      optional: one disc only
//	 "season": "halloween",                                       optional: the lobby season
//	 "exp": 1.5, "meseta": 2, "drops": 2,                         optional: multipliers, rolls per kill
//	 "event_drop": 20,                                            optional: season items at 1 in N
//	 "missions": [110000],                                        optional: featured on the board
//	 "notice": "Pumpkins are dropping all over Gurhal!"}          optional: shown with it
package calendar

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	NoticeDays  = 7  // news lines for events starting this soon
	HorizonDays = 30 // how far ahead the board lists events
)

type Weekly struct {
	DOW   []int   `json:"dow"`
	From  string  `json:"from"`
	Hours float64 `json:"hours"`
}

type Event struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Start     string   `json:"start,omitempty"`
	End       string   `json:"end,omitempty"`
	Weekly    *Weekly  `json:"weekly,omitempty"`
	Game      string   `json:"game,omitempty"`
	Season    string   `json:"season,omitempty"`
	EXP       float64  `json:"exp,omitempty"`
	Meseta    float64  `json:"meseta,omitempty"`
	Drops     int      `json:"drops,omitempty"`
	EventDrop int      `json:"event_drop,omitempty"`
	Missions  []uint32 `json:"missions,omitempty"`
	Notice    string   `json:"notice,omitempty"`

	start, end time.Time
	from       time.Duration
}

// Run is one window of an event.
type Run struct {
	E          *Event
	Start, End time.Time
}

func when(s string) (time.Time, error) {
	for _, l := range []string{"2006-01-02T15:04Z07:00", "2006-01-02T15:04:05Z07:00", time.RFC3339Nano} {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q (2026-10-28T00:00Z)", s)
}

// Parse reads and checks a spec.
func Parse(spec []byte) (*Event, error) {
	var e Event
	if err := json.Unmarshal(spec, &e); err != nil {
		return nil, err
	}
	if e.Key == "" || e.Name == "" {
		return nil, errors.New("needs key and name")
	}
	if w := e.Weekly; w != nil {
		h, m, ok := strings.Cut(cmp(w.From, "00:00"), ":")
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || w.Hours <= 0 || w.Hours > 168 || len(w.DOW) == 0 {
			return nil, errors.New("weekly: dow, from HH:MM, hours 1..168")
		}
		if slices.ContainsFunc(w.DOW, func(d int) bool { return d < 0 || d > 6 }) {
			return nil, errors.New("weekly dow is 0 (Monday) .. 6")
		}
		e.from = time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute
	} else {
		var err error
		if e.start, err = when(e.Start); err != nil {
			return nil, err
		}
		if e.end, err = when(e.End); err != nil {
			return nil, err
		}
		if !e.end.After(e.start) {
			return nil, errors.New("end is not after start")
		}
	}
	if e.Game != "" && e.Game != "psu" && e.Game != "aoti" {
		return nil, errors.New("game is psu or aoti")
	}
	if e.Season != "" && NameOf(e.Season) == "" {
		return nil, fmt.Errorf("unknown season %q", e.Season)
	}
	for k, v := range map[string]float64{"exp": e.EXP, "meseta": e.Meseta} {
		if v < 0 || v > 10 {
			return nil, fmt.Errorf("%s is a multiplier above 0, at most 10", k)
		}
	}
	if e.Drops < 0 || e.Drops > 5 {
		return nil, errors.New("drops is 1..5")
	}
	return &e, nil
}

func cmp(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Windows are e's runs overlapping [since, until).
func (e *Event) Windows(since, until time.Time) []Run {
	if e.Weekly == nil {
		if e.start.Before(until) && e.end.After(since) {
			return []Run{{e, e.start, e.end}}
		}
		return nil
	}
	length := time.Duration(e.Weekly.Hours * float64(time.Hour))
	d := since.Add(-length).UTC()
	day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	var out []Run
	for ; day.Before(until); day = day.AddDate(0, 0, 1) {
		if slices.Contains(e.Weekly.DOW, (int(day.Weekday())+6)%7) {
			s := day.Add(e.from)
			if s.Before(until) && s.Add(length).After(since) {
				out = append(out, Run{e, s, s.Add(length)})
			}
		}
	}
	return out
}

// Calendar is a set of checked events.
type Calendar struct {
	Events   []*Event
	Problems []string // the specs that did not parse, and why
}

// New parses specs (key -> JSON), skipping and reporting what does not parse.
func New(specs map[string][]byte) *Calendar {
	c := &Calendar{}
	keys := make([]string, 0, len(specs))
	for k := range specs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e, err := Parse(specs[k])
		if err != nil {
			c.Problems = append(c.Problems, fmt.Sprintf("event %s: %v", k, err))
			continue
		}
		c.Events = append(c.Events, e)
	}
	return c
}

// Running are the runs at now, for game when given (the other disc's events left out).
func (c *Calendar) Running(now time.Time, game string) []Run {
	var out []Run
	for _, e := range c.Events {
		if game == "" || e.Game == "" || e.Game == game {
			out = append(out, e.Windows(now, now.Add(time.Second))...)
		}
	}
	return out
}

// Upcoming are the runs on now or starting within days, soonest first.
func (c *Calendar) Upcoming(now time.Time, days int) []Run {
	var out []Run
	for _, e := range c.Events {
		out = append(out, e.Windows(now, now.AddDate(0, 0, days))...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].E.Key < out[j].E.Key
	})
	return out
}

// Effects are what the running events change for a disc.
type Effects struct {
	EXP, Meseta float64 // multipliers
	DropRolls   int     // rolls per kill (another roll when one gave nothing)
	EventDrop   int     // season items at 1 in N (0: the server's default)
}

func (c *Calendar) Effects(now time.Time, game string) Effects {
	fx := Effects{EXP: 1, Meseta: 1, DropRolls: 1}
	for _, r := range c.Running(now, game) {
		fx.EXP = max(fx.EXP, r.E.EXP)
		fx.Meseta = max(fx.Meseta, r.E.Meseta)
		fx.DropRolls = max(fx.DropRolls, r.E.Drops)
		if r.E.EventDrop > 0 && (fx.EventDrop == 0 || r.E.EventDrop < fx.EventDrop) {
			fx.EventDrop = r.E.EventDrop
		}
	}
	return fx
}

// Season is the season a running event puts in the lobbies ("" none).
func (c *Calendar) Season(now time.Time) string {
	for _, r := range c.Running(now, "") {
		if r.E.Season != "" {
			return NameOf(r.E.Season)
		}
	}
	return ""
}

// Stamp is a time as the notices show it: "Oct 28 0:00 UTC".
func Stamp(t time.Time) string {
	return strings.ReplaceAll(t.UTC().Format("Jan 02 15:04 UTC"), " 0", " ")
}

// Notices are news lines: one per running event, one per event starting within
// NoticeDays.
func (c *Calendar) Notices(now time.Time) []string {
	var out []string
	for _, r := range c.Upcoming(now, NoticeDays) {
		line := r.E.Name + " starts " + Stamp(r.Start) + "."
		if !r.Start.After(now) {
			line = r.E.Name + " is on until " + Stamp(r.End) + "."
		}
		if r.E.Notice != "" {
			line += " " + r.E.Notice
		}
		out = append(out, line)
	}
	return out
}

// BoardRow is an event as the board lists it (its next run).
type BoardRow struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Start    int64    `json:"start"`
	End      int64    `json:"end"`
	Running  bool     `json:"running"`
	Game     *string  `json:"game"`
	Season   *string  `json:"season"`
	EXP      *float64 `json:"exp"`
	Meseta   *float64 `json:"meseta"`
	Drops    *int     `json:"drops"`
	Missions []uint32 `json:"missions"`
	Notice   *string  `json:"notice"`
	Results  any      `json:"results,omitempty"`
}

func opt[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

func (c *Calendar) BoardRows(now time.Time) []BoardRow {
	var rows []BoardRow
	seen := map[string]bool{}
	for _, r := range c.Upcoming(now, HorizonDays) {
		if seen[r.E.Key] {
			continue
		}
		seen[r.E.Key] = true
		rows = append(rows, BoardRow{Key: r.E.Key, Name: r.E.Name, Start: r.Start.Unix(), End: r.End.Unix(),
			Running: !r.Start.After(now), Game: opt(r.E.Game), Season: opt(r.E.Season), EXP: opt(r.E.EXP),
			Meseta: opt(r.E.Meseta), Drops: opt(r.E.Drops), Missions: append([]uint32{}, r.E.Missions...),
			Notice: opt(r.E.Notice)})
	}
	return rows
}

// Changes are the event starts and ends between two Running results (keyed by event
// and start), as the ticker announces them.
func Changes(before, after []Run) (started, ended []Run) {
	key := func(r Run) string { return r.E.Key + "@" + r.Start.String() }
	was, is := map[string]bool{}, map[string]bool{}
	for _, r := range before {
		was[key(r)] = true
	}
	for _, r := range after {
		is[key(r)] = true
		if !was[key(r)] {
			started = append(started, r)
		}
	}
	for _, r := range before {
		if !is[key(r)] {
			ended = append(ended, r)
		}
	}
	return started, ended
}
