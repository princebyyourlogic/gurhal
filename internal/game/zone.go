// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/rules"
)

const (
	maxLIDs    = 1024
	zoneInbox  = 256
	statusTick = 200 * time.Millisecond // statuses tick and expire on this beat

	cmdMove = 0x0102
)

// casts are the channel-1 commands relayed to the zone (EGS's whitelist): movement and
// animation.
var casts = map[uint16]bool{0x0101: true, 0x0102: true, 0x0104: true, 0x0107: true, 0x010F: true,
	0x050F: true, 0x0503: true, 0x0514: true}

// member is a player in a zone. Only the zone's goroutine touches it after join.
type member struct {
	p      *player
	lid    uint16
	area   rules.Area
	entry  uint16
	pos    rules.Pos
	areanb uint32 // the member's own area counter (others' 0x0205 about a newcomer use it)
	loaded bool   // its own zone load is sent: casts and chat reach it from then on
}

func (m *member) user(recipient string) []byte {
	return m.p.snap.Load().user(m.p.gid, m.lid, m.area, m.entry, m.pos, recipient)
}

// zone is one goroutine owning everything in a zone instance: its members, and its room
// (enemies, flags, objects, ground items and, in a mission, the judge). Sessions talk to
// it by queueing functions that run on that goroutine; it answers through members'
// connections without ever blocking (Relay).
type zone struct {
	key   zoneKey
	inbox chan func(*zoneState)
	done  chan struct{} // closed when the last member has left; later requests are dropped
	refs  int           // members, counted under world.mu
}

type zoneState struct {
	z       *zone
	w       *world
	members map[uint32]*member
	room    room
	held    []*character // character locks a closure holds (lock / unlock)
}

func startZone(w *world, key zoneKey, r *rules.Rules, run *missionRun) *zone {
	z := &zone{key: key, inbox: make(chan func(*zoneState), zoneInbox), done: make(chan struct{})}
	zs := &zoneState{z: z, w: w, members: map[uint32]*member{}}
	zs.room.init(w, key, r, run)
	go func() {
		defer zs.room.close()
		tick := time.NewTicker(statusTick)
		defer tick.Stop()
		for {
			select {
			case f := <-z.inbox:
				zs.safely("zone", func() { f(zs) })
			case <-tick.C:
				zs.safely("zone tick", zs.statusTick)
			case <-z.done:
				return
			}
		}
	}()
	return z
}

// do runs f on the zone's goroutine. A zone that has stopped drops it: a session's
// write callback (show after a load) can arrive after the player left.
func (z *zone) do(f func(*zoneState)) {
	select {
	case z.inbox <- f:
	case <-z.done:
	}
}

func (z *zone) stop() { close(z.done) }

func (z *zone) join(m *member) (uint16, [][]byte) {
	type reply struct {
		lid    uint16
		others [][]byte
	}
	ch := make(chan reply, 1)
	z.do(func(zs *zoneState) {
		used := make([]bool, maxLIDs)
		var others [][]byte
		for _, o := range zs.sorted() {
			used[o.lid] = true
			others = append(others, o.user(m.p.disc))
		}
		for m.lid = 0; int(m.lid) < maxLIDs-1 && used[m.lid]; m.lid++ {
		}
		zs.members[m.p.gid] = m
		ch <- reply{m.lid, others}
	})
	r := <-ch
	return r.lid, r.others
}

// show spawns gid for everyone else in the zone (after its own load). Until then it gets
// no casts or chat: a client in a load cannot drain them, and dozens of players moving
// would fill its queue.
func (z *zone) show(gid uint32) {
	z.do(func(zs *zoneState) {
		p, ok := zs.members[gid]
		if !ok {
			return
		}
		p.loaded = true
		for _, d := range zs.others(gid) {
			user := p.user(d.p.disc)
			for _, pkt := range [][]byte{
				c0111(d.lid, d.p.gid, gid, p.lid, 6, 0),
				c010d(d.lid, d.p.gid, gid, p.lid, user, noSource, false),
				c0205Map(d.lid, d.p.gid, mapFor(p.area, d.p.disc, p.p.disc), p.entry, d.areanb, p.lid, 0),
				c0203(d.lid, d.p.gid, gid, p.lid),
				c0201(d.lid, d.p.gid, gid, user),
			} {
				d.p.conn.Relay(pkt)
			}
		}
	})
}

func (z *zone) leave(gid uint32) {
	z.do(func(zs *zoneState) {
		p, ok := zs.members[gid]
		if !ok {
			return
		}
		delete(zs.members, gid)
		for _, d := range zs.members {
			d.p.conn.Relay(c0204Unspawn(d.lid, d.p.gid, gid, p.lid))
		}
	})
}

// locate records where gid stands after a map change inside the zone.
func (z *zone) locate(gid uint32, a rules.Area, entry uint16, areanb uint32) {
	z.do(func(zs *zoneState) {
		if p, ok := zs.members[gid]; ok {
			p.area, p.entry, p.areanb = a, entry, areanb
		}
	})
}

// cast relays a channel-1 command; a move also updates where the player stands.
func (z *zone) cast(gid uint32, cmd uint16, body []byte) {
	z.do(func(zs *zoneState) {
		p, ok := zs.members[gid]
		if !ok {
			return
		}
		if cmd == cmdMove && len(body) >= 0x4C {
			p.pos = rules.Pos{
				X:   math.Float32frombits(binary.LittleEndian.Uint32(body[0x34:])),
				Y:   math.Float32frombits(binary.LittleEndian.Uint32(body[0x38:])),
				Z:   math.Float32frombits(binary.LittleEndian.Uint32(body[0x3C:])),
				Dir: rules.DirFromWire(binary.LittleEndian.Uint16(body[0x2E:])),
			}
		}
		pkt := castTo(cmd, body, gid, p.lid)
		for _, d := range zs.others(gid) {
			if d.loaded {
				d.p.conn.Relay(pkt)
			}
		}
	})
}

// chat sends a line to everyone in the zone, the sender's echo included.
func (z *zone) chat(gid uint32, body []byte) {
	z.do(func(zs *zoneState) {
		if _, ok := zs.members[gid]; !ok {
			return
		}
		for _, d := range zs.members {
			if d.loaded || d.p.gid == gid {
				d.p.conn.Relay(c0304Chat(d.lid, d.p.gid, gid, body))
			}
		}
	})
}

// broadcast sends build(d) to every member (the sender included).
func (zs *zoneState) broadcast(build func(d *member) []byte) {
	for _, d := range zs.members {
		if pkt := build(d); pkt != nil {
			d.p.conn.Relay(pkt)
		}
	}
}

func (zs *zoneState) others(gid uint32) []*member {
	out := make([]*member, 0, len(zs.members))
	for g, m := range zs.members {
		if g != gid {
			out = append(out, m)
		}
	}
	return out
}

// sorted is the members in GID order (the lock order for their characters).
func (zs *zoneState) sorted() []*member {
	out := make([]*member, 0, len(zs.members))
	for _, m := range zs.members {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].p.gid < out[j].p.gid })
	return out
}

// mapFor is an area of a player of disc owner as a recipient sees it: the shared
// Spaceport is map 900 on PSU and 901 on AotI.
func mapFor(a rules.Area, recipient, owner string) rules.Area {
	if owner != recipient && a.Quest == spaceportQuest && (a.Map == 900 || a.Map == 901) {
		a.Map = 900
		if recipient == rules.AotI {
			a.Map = 901
		}
	}
	return a
}

// monoNow is monotonic seconds for combat statuses.
var epoch = time.Now()

func monoNow() float64 { return time.Since(epoch).Seconds() }

// dice is the server's randomness for combat and drops.
type dice struct{}

func (dice) Intn(n int) int   { return rand.IntN(n) }
func (dice) Float64() float64 { return rand.Float64() }
