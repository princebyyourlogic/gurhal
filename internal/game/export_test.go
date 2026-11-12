// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// BoardForTest is the board snapshot as its JSON reader sees it.
func (s *Server) BoardForTest(t *testing.T) map[string]any {
	snap, err := s.boardSnapshot(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(snap)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

// PanicInZoneForTest panics on gid's zone goroutine while holding gid's character lock.
func (s *Server) PanicInZoneForTest(gid uint32) {
	p := s.world.find(gid)
	s.world.mu.Lock()
	var z *zone
	for _, zz := range s.world.zones {
		z = zz
	}
	s.world.mu.Unlock()
	done := make(chan struct{})
	z.do(func(zs *zoneState) {
		defer close(done)
		zs.lock(p.char.Load())
		panic("test panic")
	})
	<-done
}
