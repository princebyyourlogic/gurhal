// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/netx"
	"github.com/princebyyourlogic/gurhal/internal/rules"
)

// player is one logged-in character as the rest of the world sees it.
type player struct {
	gid   uint32
	disc  string
	r     *rules.Rules
	conn  *netx.Conn
	uni   atomic.Uint32
	char  atomic.Pointer[character]
	snap  atomic.Pointer[snapshot] // replaced by its session when the character changes
	where atomic.Pointer[place]    // where it stands, for party windows
	jobs  chan func(*session)      // work for its session goroutine (a GM moving it)
	user  string                   // the account name

	// under world.partyMu
	party   *party
	invites byte // "receive invites" setting (0x101D)
}

// job runs f on p's session goroutine; false when the queue is full or the session ended.
func (p *player) job(f func(*session)) bool {
	select {
	case p.jobs <- f:
		return true
	case <-p.conn.Done():
		return false
	default:
		return false
	}
}

// place is where a player stands, as its session last set it.
type place struct {
	area  rules.Area
	entry uint16
	lid   uint16
}

// zoneKey names a zone instance: lobby zones are per universe, mission zones per
// universe and mission owner (the party leader).
type zoneKey struct {
	uni, quest, zone uint32
	owner            uint32
}

// world tracks who is online, which zones exist, mission runs and parties. Zone
// membership changes go through mu, so a zone is stopped exactly when its last member
// has left.
type world struct {
	srv    *Server
	mu     sync.Mutex
	online map[uint32]*player
	zones  map[zoneKey]*zone
	runs   map[runKey]*missionRun

	partyMu sync.Mutex
	pending map[uint32]*player // invitee GID -> inviter
}

func newWorld(srv *Server) *world {
	return &world{srv: srv, online: map[uint32]*player{}, zones: map[zoneKey]*zone{}, runs: map[runKey]*missionRun{},
		pending: map[uint32]*player{}}
}

// claim puts p online, returning the session already online with that GID (to drop).
func (w *world) claim(p *player) *player {
	w.mu.Lock()
	defer w.mu.Unlock()
	old := w.online[p.gid]
	w.online[p.gid] = p
	return old
}

// release takes p offline unless a newer session has claimed its GID.
func (w *world) release(p *player) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.online[p.gid] == p {
		delete(w.online, p.gid)
	}
}

func (w *world) find(gid uint32) *player {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.online[gid]
}

// players is everyone online.
func (w *world) players() []*player {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*player, 0, len(w.online))
	for _, p := range w.online {
		out = append(out, p)
	}
	return out
}

// population counts players online per universe.
func (w *world) population() map[uint32]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[uint32]int{}
	for _, p := range w.online {
		out[p.uni.Load()]++
	}
	return out
}

// join adds m to the zone key names, starting it if needed, and returns the zone with
// m's LID and the user records of everyone already there (for m's disc).
func (w *world) join(key zoneKey, m *member) (*zone, uint16, [][]byte) {
	w.mu.Lock()
	z, ok := w.zones[key]
	if !ok {
		var run *missionRun
		if key.owner != 0 && m.p.r.Mission(key.quest) != nil { // a My Room is per owner too, without a run
			run = w.runLocked(runKey{key.uni, key.quest, key.owner})
		}
		z = startZone(w, key, m.p.r, run)
		w.zones[key] = z
	}
	z.refs++
	w.mu.Unlock()
	lid, others := z.join(m)
	return z, lid, others // the zone cannot stop meanwhile: m holds a reference
}

// leave removes gid from z; the last one out stops it.
func (w *world) leave(z *zone, gid uint32) {
	w.mu.Lock()
	z.refs--
	last := z.refs == 0
	if last {
		delete(w.zones, z.key)
	}
	w.mu.Unlock()
	z.leave(gid)
	if last {
		z.stop()
	}
}

// --- mission runs ------------------------------------------------------------------------------------

type runKey struct{ uni, quest, owner uint32 }

// missionRun is one accepted mission: the judge's counters and the flags of each of its
// zones (the start / clear rules read flags across zones).
type missionRun struct {
	mu      sync.Mutex
	t0      time.Time
	started bool
	cleared bool
	run     rules.Run
	flags   map[uint32]*flagSet // zone -> its instance flags while the zone exists
}

// startRun makes a fresh run for an accepted mission (a second run starts over).
func (w *world) startRun(k runKey) *missionRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := &missionRun{flags: map[uint32]*flagSet{}, run: rules.Run{APCDowns: map[int]int{}}}
	w.runs[k] = r
	return r
}

// runLocked is the run of a mission zone, made if nobody accepted it here (a member who
// follows after the owner left). Hold w.mu.
func (w *world) runLocked(k runKey) *missionRun {
	r, ok := w.runs[k]
	if !ok {
		r = &missionRun{flags: map[uint32]*flagSet{}, run: rules.Run{APCDowns: map[int]int{}}}
		w.runs[k] = r
	}
	return r
}

// flagSet is a zone instance's event flags: set A 20 maps x 256, set B 256.
type flagSet struct {
	mu sync.Mutex
	a  [640]byte
	b  [32]byte
}

func setBit(bits []byte, fid, mode uint32) {
	i, b := fid>>3, byte(1)<<(fid&7)
	if int(i) >= len(bits) {
		return
	}
	switch mode {
	case 0:
		bits[i] |= b
	case 1:
		bits[i] &^= b
	case 2:
		bits[i] ^= b
	}
}

func (f *flagSet) set(fid, mode uint32, b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b {
		setBit(f.b[:], fid, mode)
	} else {
		setBit(f.a[:], fid, mode)
	}
}

func (f *flagSet) isSet(fid uint32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := fid >> 3
	return int(i) < len(f.a) && f.a[i]&(1<<(fid&7)) != 0
}

func (f *flagSet) copyAll() ([640]byte, [32]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.a, f.b
}

// --- parties -----------------------------------------------------------------------------------------

// party is up to six players; its fields are under world.partyMu.
type party struct {
	slots   [6]*player
	leader  int
	mission *partyMission // once the leader accepts: members depart into it
}

type partyMission struct{ quest, owner uint32 }

func (pt *party) slotOf(p *player) int {
	for i, m := range pt.slots {
		if m == p {
			return i
		}
	}
	return -1
}

func (pt *party) members() []*player {
	var out []*player
	for _, m := range pt.slots {
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}
