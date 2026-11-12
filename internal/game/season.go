// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"time"

	"github.com/princebyyourlogic/gurhal/internal/calendar"
	"github.com/princebyyourlogic/gurhal/internal/rules"
)

// Seasonal lobbies. AotI takes the season id from 0x020F +0x2D; PSU from the quest NBL's
// zone record, so PSU gets a seasonal zone only together with its seasonal quest NBL
// (else the set file would place objects whose models were never loaded). Both read the
// "seasonal" flag 0x0205 +0x43. A lobby zone with nothing staged for the disc gets
// nothing.

const noSeason = 0xFF

// seasonOf is the lobby season for gid: its GM preview, else the running event's, else
// the server's setting ("" none).
func (s *Server) seasonOf(gid uint32) string {
	s.ops.mu.Lock()
	v, ok := s.ops.previews[gid]
	s.ops.mu.Unlock()
	if ok {
		return v
	}
	return s.calendar().Current(time.Now(), s.Season)
}

// staged is a lobby zone's seasonal archives for the session: the season id, the quest
// NBL (nil: not staged) and the zone NBL; ok false without a staged zone.
func (s *session) staged(q, z uint32) (id uint8, quest, zone []byte, ok bool) {
	name := s.srv.seasonOf(s.gid)
	if name == "" {
		return noSeason, nil, nil, false
	}
	zone, ok = s.srv.Data.SeasonNBL(name, s.disc, zoneNBL(q, z))
	if !ok {
		return noSeason, nil, nil, false
	}
	quest, _ = s.srv.Data.SeasonNBL(name, s.disc, questNBL(q))
	return uint8(calendar.ID(name)), quest, zone, true
}

// zoneSeason is staged for a zone change: PSU needs the quest NBL with it, sent only when
// the quest changes.
func (s *session) zoneSeason(a rules.Area, questChange bool) (id uint8, quest, zone []byte, ok bool) {
	id, quest, zone, ok = s.staged(a.Quest, a.Zone)
	if ok && s.disc != rules.AotI && !(questChange && quest != nil) {
		return noSeason, nil, nil, false
	}
	return id, quest, zone, ok
}

// seasonDrop is a season event item for a kill that rolled nothing, at 1 in rate (0: the
// server's SeasonDrop) while a season is on; only items the disc has.
func (s *Server) seasonDrop(rng rules.Rand, r *rules.Rules, now time.Time, rate int) (rules.Drop, bool) {
	ids := calendar.EventItems[s.calendar().Current(now, s.Season)]
	if rate <= 0 {
		rate = s.SeasonDrop
	}
	if len(ids) == 0 || rate <= 0 || rng.Intn(rate) != 0 {
		return rules.Drop{}, false
	}
	var have []rules.ItemID
	for _, hex := range ids {
		var id rules.ItemID
		if id.UnmarshalText([]byte(hex)) == nil {
			if _, ok := r.Item(id); ok {
				have = append(have, id)
			}
		}
	}
	if len(have) == 0 {
		return rules.Drop{}, false
	}
	return rules.Drop{Item: have[rng.Intn(len(have))]}, true
}
