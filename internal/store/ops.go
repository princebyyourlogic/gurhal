// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"context"
	"slices"
	"strings"
	"time"
)

// Clear is one mission clear: who, how fast, how well.
type Clear struct {
	At                       time.Time
	Game                     string
	Uni, Quest               uint32
	Secs, Rank, Score, Kills int
	Party                    []Member
}

type Member struct {
	GID  uint32 `json:"gid"`
	Slot int    `json:"slot"`
	Name string `json:"name"`
}

type Event struct {
	Key  string
	Spec []byte
}

// Mute keeps a player's chat from being relayed until Until (zero: until unmuted).
type Mute struct {
	GID    uint32
	Until  time.Time
	Reason string
	By     string
}

type HelpMessage struct {
	ID, Thread int64
	Text       string
	At         time.Time
}

// --- Memory --------------------------------------------------------------------------------------------

type memThread struct {
	id, gid          int64
	name, subject    string
	closed, waiting  bool
	created, updated time.Time
}

type memHelp struct {
	id, thread int64
	staff      bool
	text, by   string
	at         time.Time
	delivered  bool
}

type memOps struct {
	clears  []Clear
	events  map[string][]byte
	mutes   map[uint32]Mute
	threads []memThread
	help    []memHelp
	helpSeq int64
}

func (m *Memory) AccountByGID(_ context.Context, gid uint32) (Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[gid]
	if !ok {
		return Account{}, ErrNotFound
	}
	return clone(a), nil
}

func (m *Memory) FindGID(ctx context.Context, name string) (uint32, error) {
	if a, err := m.Account(ctx, name); err == nil {
		return a.GID, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for gid, cs := range m.chars {
		for _, c := range cs {
			if strings.EqualFold(c.Name, name) {
				return gid, nil
			}
		}
	}
	return 0, ErrNotFound
}

func (m *Memory) CountAccounts(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.accounts), nil
}

func (m *Memory) AllCharacters(context.Context) ([]Owned, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Owned
	for gid, cs := range m.chars {
		for _, c := range cs {
			out = append(out, Owned{gid, Character{c.Slot, c.Name, slices.Clone(c.Raw), slices.Clone(c.State)}})
		}
	}
	return out, nil
}

func (m *Memory) BannedGIDs(_ context.Context, now time.Time) ([]uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []uint32
	for gid, b := range m.bans {
		if b.Until.After(now) {
			out = append(out, gid)
		}
	}
	return out, nil
}

func (m *Memory) RecordClear(_ context.Context, c Clear) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.Party = slices.Clone(c.Party)
	m.ops.clears = append(m.ops.clears, c)
	return nil
}

func (m *Memory) Clears(context.Context) ([]Clear, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.ops.clears), nil
}

func (m *Memory) Events(context.Context) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for k, v := range m.ops.events {
		out = append(out, Event{k, slices.Clone(v)})
	}
	slices.SortFunc(out, func(a, b Event) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

func (m *Memory) SetEvent(_ context.Context, key string, spec []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ops.events == nil {
		m.ops.events = map[string][]byte{}
	}
	if spec == nil {
		delete(m.ops.events, key)
	} else {
		m.ops.events[key] = slices.Clone(spec)
	}
	return nil
}

func (m *Memory) Mutes(context.Context) ([]Mute, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Mute
	for _, x := range m.ops.mutes {
		out = append(out, x)
	}
	return out, nil
}

func (m *Memory) SetMute(_ context.Context, x Mute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ops.mutes == nil {
		m.ops.mutes = map[uint32]Mute{}
	}
	m.ops.mutes[x.GID] = x
	return nil
}

func (m *Memory) Unmute(_ context.Context, gid uint32) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.ops.mutes[gid]
	delete(m.ops.mutes, gid)
	return ok, nil
}

func (m *Memory) thread(id int64) *memThread {
	for i := range m.ops.threads {
		if m.ops.threads[i].id == id {
			return &m.ops.threads[i]
		}
	}
	return nil
}

func (m *Memory) HelpOpen(_ context.Context, gid uint32) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.ops.threads) - 1; i >= 0; i-- {
		if t := m.ops.threads[i]; t.gid == int64(gid) && !t.closed {
			return t.id, true, nil
		}
	}
	return 0, false, nil
}

func (m *Memory) HelpSent(_ context.Context, gid uint32, since time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.ops.help {
		if t := m.thread(h.thread); t != nil && t.gid == int64(gid) && !h.staff && h.at.After(since) {
			n++
		}
	}
	return n, nil
}

func (m *Memory) HelpAsk(ctx context.Context, gid uint32, name, subject, text string, at time.Time) (int64, error) {
	id, ok, _ := m.HelpOpen(ctx, gid)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !ok {
		id = 1
		if n := len(m.ops.threads); n > 0 {
			id = m.ops.threads[n-1].id + 1
		}
		m.ops.threads = append(m.ops.threads, memThread{id: id, gid: int64(gid), subject: subject, created: at})
	}
	t := m.thread(id)
	t.waiting, t.updated, t.name = true, at, name
	m.ops.helpSeq++
	m.ops.help = append(m.ops.help, memHelp{id: m.ops.helpSeq, thread: id, text: text, at: at, delivered: true})
	return id, nil
}

func (m *Memory) HelpLatestAnswer(_ context.Context, thread int64) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.ops.help) - 1; i >= 0; i-- {
		if h := m.ops.help[i]; h.thread == thread && h.staff {
			return h.text, true, nil
		}
	}
	return "", false, nil
}

func (m *Memory) HelpUndelivered(_ context.Context, gid uint32) ([]HelpMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []HelpMessage
	for _, h := range m.ops.help {
		if t := m.thread(h.thread); t != nil && t.gid == int64(gid) && h.staff && !h.delivered {
			out = append(out, HelpMessage{h.id, h.thread, h.text, h.at})
		}
	}
	return out, nil
}

func (m *Memory) HelpDelivered(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.ops.help {
		if m.ops.help[i].id == id {
			m.ops.help[i].delivered = true
		}
	}
	return nil
}

func (m *Memory) HelpReply(_ context.Context, thread int64, text, by string, closeIt bool, at time.Time) (uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.thread(thread)
	if t == nil {
		return 0, ErrNotFound
	}
	m.ops.helpSeq++
	m.ops.help = append(m.ops.help, memHelp{id: m.ops.helpSeq, thread: thread, staff: true, text: text, by: by, at: at})
	t.waiting, t.updated, t.closed = false, at, t.closed || closeIt
	return uint32(t.gid), nil
}

func (m *Memory) HelpClose(_ context.Context, thread int64, at time.Time) (uint32, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.thread(thread)
	if t == nil {
		return 0, false, ErrNotFound
	}
	was := t.closed
	t.closed, t.waiting, t.updated = true, false, at
	return uint32(t.gid), was, nil
}
