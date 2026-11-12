// SPDX-License-Identifier: GPL-3.0-or-later

package game

import "time"

// bucket is a token bucket a session keeps for what a client could flood: burst tokens,
// one back every per. Only its session goroutine uses it.
type bucket struct {
	burst  int
	per    time.Duration
	tokens float64
	at     time.Time
}

func (b *bucket) allow(now time.Time) bool {
	if b.at.IsZero() {
		b.tokens, b.at = float64(b.burst), now
	}
	b.tokens = min(float64(b.burst), b.tokens+float64(now.Sub(b.at))/float64(b.per))
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Per-session allowances: chat lines, and messages that reach other players' screens or
// the database (mail, card offers, trade requests).
func newChatBucket() bucket   { return bucket{burst: 10, per: 500 * time.Millisecond} }
func newSocialBucket() bucket { return bucket{burst: 10, per: 6 * time.Second} }
