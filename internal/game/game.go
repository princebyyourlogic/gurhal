// SPDX-License-Identifier: GPL-3.0-or-later

// Package game is the game server (12061, both discs; SSLv3 is terminated in front of
// it): everything after login. Protocol from EGS's documentation as prettystellaruniverse
// laid it out for the PS2:
//
//	S>C 0x0202  hello on connect
//	C>S 0x020D  GID + the login key           -> 0x0D05 account flags; keepalives start
//	C>S 0x0D06  character list request        -> 0x0D03 four slots
//	C>S 0x0D02  create a character            -> stored, no reply
//	C>S 0x020B  select a slot                 -> the character load (0x0D01 .. 0x021B)
//	C>S 0x021C  load done                     -> the lobby (0x0C00 .. 0x0236)
//	C>S 0x0807  area change                   -> a map, zone or quest load
//	C>S 0x0304  chat                          -> to the zone, the sender's echo included
//	C>S 0x021D / 0x021F  Uni-cube             -> universe list / move
//	channel 1   movement and animation casts  -> relayed to the zone
//
// Each connection is a session goroutine; each zone instance is a goroutine owning its
// members (zone.go); character states are written by a background saver.
package game

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/auth"
	"github.com/princebyyourlogic/gurhal/internal/calendar"
	"github.com/princebyyourlogic/gurhal/internal/gamedata"
	"github.com/princebyyourlogic/gurhal/internal/netx"
	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/store"
)

// Universe is one entry of the Uni-cube (server select).
type Universe struct {
	ID   uint32
	Name string
}

type universe = Universe

// ParseUniverses reads "1:Universe 1,2:Universe 2".
func ParseUniverses(spec string) ([]Universe, error) {
	var out []Universe
	for _, part := range strings.Split(spec, ",") {
		id, name, _ := strings.Cut(strings.TrimSpace(part), ":")
		n, err := strconv.ParseUint(strings.TrimSpace(id), 10, 32)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("universe %q: want id:name", part)
		}
		if name = strings.TrimSpace(name); name == "" {
			name = "Universe " + id
		}
		out = append(out, Universe{uint32(n), name})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no universes in %q", spec)
	}
	return out, nil
}

type Server struct {
	Rules     *rules.Set
	Data      *gamedata.Data
	Store     store.Store
	Keys      *auth.Keys
	Universes []Universe
	Log       *slog.Logger

	Admins     []string // account names allowed the GM commands (PSU_ADMINS)
	Season     string   // PSU_SEASON: none, auto or a season name
	SeasonDrop int      // season items at 1 in N kills that rolled nothing (0 off)
	TickerSecs int      // the news rotation (0 off)
	BoardFile  string   // the public board snapshot ("" off)
	BoardSecs  int

	once   sync.Once
	world  *world
	saver  *saver
	ground atomic.Uint32 // the last ground item UUID handed out
	social *social
	shopMu sync.Mutex      // offline My Shop owners' stored states
	root   context.Context // the server's lifetime (background database work)
	dice   rules.Rand      // tests may script it
	ops    ops
}

// nextGroundUUID is a fresh ground item UUID (their own range, never an inventory UUID).
func (s *Server) nextGroundUUID() uint32 {
	s.ground.CompareAndSwap(0, rules.GroundBase)
	return s.ground.Add(1)
}

func (s *Server) init() {
	s.once.Do(func() {
		s.world = newWorld(s)
		s.social = newSocial()
		s.ops = ops{cal: calendar.New(nil), mutes: map[uint32]store.Mute{}, previews: map[uint32]string{}, started: time.Now()}
		if s.root == nil {
			s.root = context.Background()
		}
		if s.dice == nil {
			s.dice = dice{}
		}
		s.saver = newSaver(s.Store, s.Log.With("svc", "save"))
	})
}

func (s *Server) ctx() context.Context { return s.root }

// guard runs f, logging a panic instead of letting it end psud: one bad packet handled on
// a zone's goroutine must not take the server down for everyone.
func (s *Server) guard(what string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("panic", "in", what, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	f()
}

// Run writes character saves and runs the operator jobs (calendar, ticker, board) until
// ctx ends, then writes what is left.
func (s *Server) Run(ctx context.Context) {
	s.init()
	go s.runOps(ctx)
	s.saver.run(ctx)
}

// admin reports whether an account may use the GM commands.
func (s *Server) admin(user string) bool {
	return user != "" && slices.ContainsFunc(s.Admins, func(a string) bool { return strings.EqualFold(a, user) })
}

// Handle serves one game connection.
func (s *Server) Handle(ctx context.Context, c *netx.Conn) {
	s.init()
	(&session{srv: s, c: c, ctx: ctx, uni: s.Universes[0].ID, jobs: make(chan func(*session), 4),
		chatRate: newChatBucket(), socialRate: newSocialBucket()}).run()
}

func (s *Server) universe(id uint32) (Universe, bool) {
	for _, u := range s.Universes {
		if u.ID == id {
			return u, true
		}
	}
	return Universe{}, false
}
