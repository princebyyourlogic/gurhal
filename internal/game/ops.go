// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/princebyyourlogic/gurhal/internal/calendar"
	"github.com/princebyyourlogic/gurhal/internal/store"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// The operator's side: notices (0x0228) and the news ticker, the event calendar, chat
// mutes, and the background jobs that keep them current.
//
//	S>C 0x0228 (PSU 0xd9f00, AotI 0xd3390): +0x24 type, +0x28 duration, UTF-16 text + NUL.
//	Type 0 centre dialog (duration is a style, 0..2), 1 top ticker, 2 right-hand ticker,
//	3 timed dialog (closes after duration s). A 0x0304 cannot carry a sender name on PS2.

const (
	noticeCentre = 0
	noticeTicker = 1
	noticeRight  = 2
	noticeDialog = 3

	welcomeDelay  = 6 * time.Second // after the first lobby load: the HUD must be up for a ticker
	helpDelay     = 8 * time.Second // staff answers, after the welcome
	calendarEvery = 30 * time.Second
	noticeMax     = 511 // UTF-16 units 0x0228 carries
)

func c0228(gid uint32, text string, kind, secs uint32) []byte {
	u := utf16.Encode([]rune(text))
	return wire.Encode(0x0228, wire.Body{}.Head(wire.NoLID, gid).U32(kind).U32(secs).UTF16(u[:min(len(u), noticeMax)]).U16(0))
}

// say is a timed dialog to p.
func say(p *player, text string, secs uint32) { p.conn.Relay(c0228(p.gid, text, noticeDialog, secs)) }

// ops is the operator state the sessions read.
type ops struct {
	mu       sync.Mutex
	cal      *calendar.Calendar
	mutes    map[uint32]store.Mute
	previews map[uint32]string // GM season previews: gid -> season ("" none)
	started  time.Time
}

func (s *Server) calendar() *calendar.Calendar {
	s.ops.mu.Lock()
	defer s.ops.mu.Unlock()
	return s.ops.cal
}

// loadCalendar re-reads the events from the store; the problems are logged once per load
// that changes them.
func (s *Server) loadCalendar(ctx context.Context) *calendar.Calendar {
	rows, err := s.Store.Events(ctx)
	if err != nil {
		s.Log.Error("events", "err", err)
		return s.calendar()
	}
	specs := map[string][]byte{}
	for _, r := range rows {
		specs[r.Key] = r.Spec
	}
	c := calendar.New(specs)
	s.ops.mu.Lock()
	old := s.ops.cal
	s.ops.cal = c
	s.ops.mu.Unlock()
	if old == nil || strings.Join(old.Problems, "\n") != strings.Join(c.Problems, "\n") {
		for _, p := range c.Problems {
			s.Log.Warn("events: skipped", "problem", p)
		}
	}
	return c
}

func (s *Server) loadMutes(ctx context.Context) {
	ms, err := s.Store.Mutes(ctx)
	if err != nil {
		s.Log.Error("mutes", "err", err)
		return
	}
	s.ops.mu.Lock()
	defer s.ops.mu.Unlock()
	for _, m := range ms {
		s.ops.mutes[m.GID] = m
	}
}

// muted is gid's mute in force, if any.
func (s *Server) muted(gid uint32) (store.Mute, bool) {
	s.ops.mu.Lock()
	defer s.ops.mu.Unlock()
	m, ok := s.ops.mutes[gid]
	if ok && !m.Until.IsZero() && !m.Until.After(time.Now()) {
		delete(s.ops.mutes, gid)
		return store.Mute{}, false
	}
	return m, ok
}

func mutedText(m store.Mute) string {
	if m.Until.IsZero() {
		return "You are muted."
	}
	return "You are muted for " + strconv.Itoa(int(time.Until(m.Until)/time.Minute)+1) + " more minute(s)."
}

// notices are the operator's news, then a line per running or soon starting event.
func (s *Server) notices(ctx context.Context) []string {
	n, err := s.Store.Notices(ctx)
	if err != nil {
		s.Log.Error("notices", "err", err)
	}
	return append(n, s.calendar().Notices(time.Now())...)
}

// currentNotice is the one the rotation is on ("" none): one per TickerSecs.
func (s *Server) currentNotice(ctx context.Context, now time.Time) string {
	n := s.notices(ctx)
	if len(n) == 0 {
		return ""
	}
	period := int64(s.TickerSecs)
	if period <= 0 {
		period = 1800
	}
	return oneLine(n[now.Unix()/period%int64(len(n))])
}

func oneLine(text string) string {
	r := []rune(strings.Join(strings.Fields(text), " "))
	return string(r[:min(len(r), 500)])
}

// broadcast sends a notice to everyone online that pick accepts (nil: everyone).
func (s *Server) broadcast(text string, kind, secs uint32, pick func(*player) bool) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	if kind == noticeCentre {
		secs = 2
	}
	n := 0
	for _, p := range s.world.players() {
		if pick == nil || pick(p) {
			p.conn.Relay(c0228(p.gid, text, kind, secs))
			n++
		}
	}
	return n
}

// runOps keeps the calendar current and announces event starts and ends, rotates the
// news ticker and writes the board, until ctx ends.
func (s *Server) runOps(ctx context.Context) {
	s.loadMutes(ctx)
	running := s.loadCalendar(ctx).Running(time.Now(), "") // already running: not announced again
	cal := time.NewTicker(calendarEvery)
	defer cal.Stop()
	var ticker, board <-chan time.Time
	if s.TickerSecs > 0 {
		t := time.NewTicker(time.Duration(s.TickerSecs) * time.Second)
		defer t.Stop()
		ticker = t.C
	}
	if s.BoardFile != "" {
		every := time.Duration(max(s.BoardSecs, 10)) * time.Second
		t := time.NewTicker(every)
		defer t.Stop()
		board = t.C
		s.writeBoard(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-cal.C:
			s.guard("calendar", func() {
				next := s.loadCalendar(ctx).Running(now, "")
				started, ended := calendar.Changes(running, next)
				for _, r := range started {
					s.broadcast(strings.TrimSpace(r.E.Name+" has begun! It runs until "+calendar.Stamp(r.End)+". "+r.E.Notice),
						noticeTicker, 0, nil)
				}
				for _, r := range ended {
					s.broadcast(r.E.Name+" has ended. Thank you for playing!", noticeTicker, 0, nil)
				}
				running = next
			})
		case now := <-ticker:
			s.guard("news ticker", func() {
				if text := s.currentNotice(ctx, now); text != "" {
					s.broadcast(text, noticeTicker, 0, nil)
				}
			})
		case <-board:
			s.guard("board", func() { s.writeBoard(ctx) })
		}
	}
}

// welcome is p's first lobby load: the current notice as a ticker, then any staff
// answers that came while p was away.
func (s *Server) welcome(p *player) {
	time.AfterFunc(welcomeDelay, func() {
		s.guard("welcome", func() {
			if s.world.find(p.gid) == p {
				if text := s.currentNotice(s.ctx(), time.Now()); text != "" {
					p.conn.Relay(c0228(p.gid, text, noticeTicker, 0))
				}
			}
		})
	})
	time.AfterFunc(helpDelay, func() {
		s.guard("help delivery", func() {
			if s.world.find(p.gid) == p {
				s.deliverHelp(p.gid)
			}
		})
	})
}
