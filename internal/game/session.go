// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/netx"
	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

const (
	maxPacket      = 0x8000
	keepaliveEvery = 5 * time.Second
	maxErrors      = 50 // a client that keeps tripping handlers is dropped
)

// handler serves one client command. min is the body length it needs; anything shorter
// is ignored, as the Python server does.
type handler struct {
	min    int
	public bool // allowed before the 0x020D key auth
	fn     func(s *session, body []byte) error
}

var handlers = map[uint16]handler{}

func handle(cmd uint16, min int, fn func(*session, []byte) error) {
	handlers[cmd] = handler{min: min, fn: fn}
}

func handlePublic(cmd uint16, min int, fn func(*session, []byte) error) {
	handlers[cmd] = handler{min: min, public: true, fn: fn}
}

// session is one game connection, run by its own goroutine.
type session struct {
	srv *Server
	c   *netx.Conn
	ctx context.Context

	gid      uint32
	user     string // the account name
	authed   bool
	disc     string
	r        *rules.Rules
	entrance byte
	options  []byte // 0x0D07, sent back in 0x0D01
	uni      uint32

	char   *character // the selected character
	me     *player    // once in the world (0x021C)
	zone   *zone
	lid    uint16
	area   rules.Area
	entry  uint16
	areanb uint32
	lcWork map[int]int32 // SAVE_LC00..05 as the client last wrote them (0x0D04)
	errors int

	lastCmd  uint16            // the command being handled (one handler may serve several)
	shopList []rules.ShopEntry // the NPC shop list last sent (0x010A buy indexes it)
	synthCur synthCursor       // the production entry being worked on

	mission *missionState // the accepted mission
	prev    *spot         // where the counter was entered from (a mission returns there)

	jobs chan func(*session) // run between packets (player.job)

	chatRate, socialRate bucket // flood limits (bucket.go)
}

// spot is an area and entry to return to.
type spot struct {
	area  rules.Area
	entry uint16
}

// missionState is an accepted mission: its owner (the party leader) keys its zones.
type missionState struct {
	quest, owner     uint32
	origin           *spot
	in, startPending bool
}

func (s *session) run() {
	defer s.cleanup()
	s.gid = 0x10000000 | randUint32()&0xFFFFFF // until the key auth
	if s.c.Send(wire.Hello(s.gid)) != nil {
		return
	}
	// Reads go on in their own goroutine (fragment acks reach paced writes at once); this
	// one handles packets and the jobs other goroutines queue, one at a time.
	pkts := make(chan wire.Packet, 16)
	go func() {
		defer close(pkts)
		for {
			p, err := wire.Read(s.c.R, maxPacket)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					s.c.Log.Debug("game: closed", "err", err)
				}
				return
			}
			if p.IsFragmentAck() {
				s.c.Acked()
				continue
			}
			select {
			case pkts <- p:
			case <-s.c.Done():
				return
			}
		}
	}()
	for {
		select {
		case p, ok := <-pkts:
			if !ok || !s.dispatch(p) {
				s.c.Close()
				return
			}
		case job := <-s.jobs:
			job(s)
		}
	}
}

// dispatch runs one packet's handler; false drops the connection.
func (s *session) dispatch(p wire.Packet) (ok bool) {
	if p.Channel == wire.ChanCast && casts[p.Cmd] {
		if s.zone != nil {
			s.zone.cast(s.gid, p.Cmd, p.Body)
			s.ppCast(p.Cmd, p.Body)
		}
		return true
	}
	h, known := handlers[p.Cmd]
	if !known || len(p.Body) < h.min || (!s.authed && !h.public) {
		s.c.Log.Debug("game: command ignored", "cmd", fmt.Sprintf("%#04x", p.Cmd), "size", len(p.Body))
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			s.c.Log.Error("game: handler panic", "cmd", fmt.Sprintf("%#04x", p.Cmd), "panic", r, "stack", string(debug.Stack()))
			s.releaseLeaked()
			ok = false // the handler stopped half way: drop the client rather than trust its state
		}
	}()
	s.lastCmd = p.Cmd
	if err := h.fn(s, p.Body); err != nil {
		if errors.Is(err, errDrop) {
			return false
		}
		s.c.Log.Warn("game: handler error", "cmd", fmt.Sprintf("%#04x", p.Cmd), "err", err)
		s.errors++
		return s.errors <= maxErrors
	}
	return true
}

// releaseLeaked frees the character lock a panicking handler may have left held. Zones
// hold a character lock for well under a millisecond, so one still held 100 ms after the
// panic was this session's own.
func (s *session) releaseLeaked() {
	if s.char == nil {
		return
	}
	for deadline := time.Now().Add(100 * time.Millisecond); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if s.char.mu.TryLock() {
			s.char.mu.Unlock()
			return
		}
	}
	s.c.Log.Warn("game: releasing the character lock a panicking handler held")
	s.char.mu.Unlock()
}

// errDrop ends the session (a refused key, a ban).
var errDrop = errors.New("drop")

// keepalive writes an empty packet every 5 s (a write to a dead peer closes it) and, in a
// mission, regenerates weapon PP on the same beat.
func (s *session) keepalive() {
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if s.c.Send(keepalive) != nil {
				return
			}
			s.srv.guard("pp regen", s.ppRegen)
		case <-s.c.Done():
			return
		}
	}
}

func (s *session) cleanup() {
	s.saveNow()
	if s.me != nil {
		s.srv.social.offline(s.gid)
		s.srv.world.partyLeave(s.me, false)
		s.srv.world.release(s.me)
		s.srv.world.presence(s.me) // card holders: now offline (before the zone sees it go)
	}
	if s.zone != nil {
		s.srv.world.leave(s.zone, s.gid)
		s.zone = nil
	}
}

// --- character state ---------------------------------------------------------------------------

// save queues the selected character's state for the background saver and refreshes
// what others see of it.
func (s *session) save() {
	if s.char == nil {
		return
	}
	s.char.mu.Lock()
	defer s.char.mu.Unlock()
	s.saveLocked()
}

// saveLocked is save with the character lock held.
func (s *session) saveLocked() {
	s.char.bank(time.Now())
	s.srv.saver.queue(s.gid, s.char.slot, s.char.st)
	if s.me != nil {
		s.me.snap.Store(s.char.snapshot(s.r))
	}
}

// refresh updates what others see of the character.
func (s *session) refresh() {
	if s.char == nil || s.me == nil {
		return
	}
	s.char.mu.Lock()
	s.me.snap.Store(s.char.snapshot(s.r))
	s.char.mu.Unlock()
}

// saveNow writes the selected character at once (logout, another character selected).
func (s *session) saveNow() {
	if s.char == nil {
		return
	}
	s.save()
	s.srv.saver.flush(context.Background(), s.gid, s.char.slot)
}

// sendAll queues a load sequence, paced (PCSX2 copes badly with large bursts).
func (s *session) sendAll(pkts [][]byte) {
	for _, p := range pkts {
		if s.c.SendPaced(p) != nil {
			return
		}
	}
}

func randUint32() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.LittleEndian.Uint32(b[:])
}
