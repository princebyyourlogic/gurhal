// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/store"
)

// saveEvery coalesces a character's saves: a burst of changes is one write.
const saveEvery = time.Second

type charKey struct {
	gid  uint32
	slot int
}

// saver writes character states in the background so the game never waits on the
// database. A failed write keeps the latest state and is retried.
type saver struct {
	st  store.Store
	log *slog.Logger

	mu      sync.Mutex
	pending map[charKey][]byte
}

func newSaver(st store.Store, log *slog.Logger) *saver {
	return &saver{st: st, log: log, pending: map[charKey][]byte{}}
}

// queue snapshots st (JSON, now) to be written soon.
func (s *saver) queue(gid uint32, slot int, st *rules.State) {
	b, err := json.Marshal(st)
	if err != nil {
		s.log.Error("save: encode", "gid", gid, "err", err)
		return
	}
	s.mu.Lock()
	s.pending[charKey{gid, slot}] = b
	s.mu.Unlock()
}

// flush writes one character's pending state now (logout, reselect).
func (s *saver) flush(ctx context.Context, gid uint32, slot int) {
	k := charKey{gid, slot}
	s.mu.Lock()
	b, ok := s.pending[k]
	delete(s.pending, k)
	s.mu.Unlock()
	if ok {
		s.write(ctx, k, b)
	}
}

// run writes pending states every saveEvery until ctx ends, then writes the rest.
func (s *saver) run(ctx context.Context) {
	t := time.NewTicker(saveEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.writeAll(ctx)
		case <-ctx.Done():
			s.writeAll(context.Background())
			return
		}
	}
}

func (s *saver) writeAll(ctx context.Context) {
	s.mu.Lock()
	batch := s.pending
	s.pending = map[charKey][]byte{}
	s.mu.Unlock()
	for k, b := range batch {
		s.write(ctx, k, b)
	}
}

func (s *saver) write(ctx context.Context, k charKey, b []byte) {
	err := s.st.SaveState(ctx, k.gid, k.slot, b)
	switch {
	case err == nil:
		return
	case errors.Is(err, store.ErrNotFound): // the slot was emptied or recreated meanwhile
		s.log.Warn("save: character gone", "gid", k.gid, "slot", k.slot)
		return
	}
	s.log.Error("save failed, will retry", "gid", k.gid, "slot", k.slot, "err", err)
	s.mu.Lock()
	if _, newer := s.pending[k]; !newer {
		s.pending[k] = b
	}
	s.mu.Unlock()
}
