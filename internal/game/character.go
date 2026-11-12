// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"sync"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/rules"
)

// character is the selected character. Its session owns it, but a mission zone's combat
// (support techniques, shared EXP, clear rewards) and a party touch others' characters
// too, so every access holds mu. Lock order: a goroutine holds at most one character
// lock, except a zone resolving hits, which locks its members in GID order. Never call
// into a zone while holding one.
type character struct {
	mu    sync.Mutex
	gid   uint32
	slot  int
	raw   []byte // the 0x0D02 body as the client sent it
	st    *rules.State
	since time.Time // play time counted up to here; zero while not counting

	status    rules.Status          // live combat status (not saved)
	ppLeft    bool                  // 0x0104 left-hand mode
	ppReady   bool                  // moved in the mission zone since its load: PP regenerates
	inMission bool                  // PP regenerates in missions only
	recentUse map[uint32]rules.Item // items just used: a hit may still name them
}

// bank adds the whole seconds played since the last bank to the state's play time.
func (c *character) bank(now time.Time) {
	if c.since.IsZero() {
		return
	}
	if secs := int64(now.Sub(c.since) / time.Second); secs > 0 {
		c.st.Playtime = uint32(min(int64(c.st.Playtime)+secs, 0xFFFFFFFF))
		c.since = c.since.Add(time.Duration(secs) * time.Second)
	}
}

// fighter is the character in combat. Hold mu while using it.
func (c *character) fighter(r *rules.Rules, lid uint16) *rules.Fighter {
	return &rules.Fighter{R: r, GID: c.gid, LID: lid, Raw: c.raw, St: c.st, Status: &c.status, RecentUse: c.recentUse}
}

// snapshot is what zones may read of a character without its lock: immutable once made.
// Records are built from it per recipient.
type snapshot struct {
	r     *rules.Rules // the owner's disc
	raw   []byte
	st    rules.State // a copy; records read only its scalar fields
	since time.Time   // play time keeps counting from here
}

// snapshot copies the character; hold mu.
func (c *character) snapshot(r *rules.Rules) *snapshot {
	return &snapshot{r: r, raw: c.raw, st: *c.st, since: c.since}
}

// user is the snapshot's user record for a recipient disc, as another player sees it,
// with the play time up to now.
func (s *snapshot) user(gid uint32, lid uint16, a rules.Area, entry uint16, p rules.Pos, recipient string) []byte {
	st := s.st
	(&character{st: &st, since: s.since}).bank(time.Now())
	return s.r.User(gid, lid, s.raw, &st, a, entry, p, recipient, s.r.Disc())
}
