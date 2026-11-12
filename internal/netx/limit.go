// SPDX-License-Identifier: GPL-3.0-or-later

package netx

import (
	"net/netip"
	"sync"
	"time"
)

// Limiter caps what one address may hold across every listener that shares it: open
// connections at once, and new connections a minute (a token bucket that refills
// PerMinute a minute, PerMinute deep); and the open connections of everyone together.
// Zero fields are unlimited; a nil Limiter admits everything.
type Limiter struct {
	MaxOpen   int
	PerMinute int
	MaxTotal  int // open connections from everyone together

	mu    sync.Mutex
	total int
	peers map[netip.Addr]*peer
	now   func() time.Time // tests
}

type peer struct {
	open   int
	tokens float64
	at     time.Time
}

// Refusal is why a connection was refused ("" admitted).
type Refusal string

const (
	TooMany  Refusal = "too many connections from one address"
	TooOften Refusal = "connecting too often from one address"
	Full     Refusal = "the server is full"
	admitted Refusal = ""
)

// Admit takes a connection slot for a, or says why not; Release gives it back.
func (l *Limiter) Admit(a netip.Addr) Refusal {
	if l == nil {
		return admitted
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if l.peers == nil {
		l.peers = map[netip.Addr]*peer{}
	}
	p := l.peers[a]
	if p == nil {
		p = &peer{tokens: float64(l.PerMinute), at: now}
		l.peers[a] = p
	}
	if l.PerMinute > 0 {
		p.tokens = min(float64(l.PerMinute), p.tokens+now.Sub(p.at).Minutes()*float64(l.PerMinute))
		p.at = now
	}
	switch {
	case l.MaxTotal > 0 && l.total >= l.MaxTotal:
		return Full
	case l.MaxOpen > 0 && p.open >= l.MaxOpen:
		return TooMany
	case l.PerMinute > 0 && p.tokens < 1:
		return TooOften
	}
	p.tokens--
	p.open++
	l.total++
	return admitted
}

func (l *Limiter) Release(a netip.Addr) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.peers[a]
	if p == nil {
		return
	}
	p.open--
	l.total--
	l.prune()
}

// prune forgets idle addresses whose bucket has refilled; hold mu.
func (l *Limiter) prune() {
	now := l.clock()
	for a, p := range l.peers {
		if p.open <= 0 && (l.PerMinute == 0 || p.tokens+now.Sub(p.at).Minutes()*float64(l.PerMinute) >= float64(l.PerMinute)) {
			delete(l.peers, a)
		}
	}
}

func (l *Limiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}
