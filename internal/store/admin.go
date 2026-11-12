// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"context"
	"slices"
	"strings"
	"time"
)

// What the admin API reads and changes besides the game's own tables.

// AccountRow is an account as the operator sees it: no password material.
type AccountRow struct {
	GID          uint32
	Username     string
	ResetPending bool
	Created      time.Time
	LastLogin    time.Time
	Characters   []string  // by slot
	BannedUntil  time.Time // zero: not banned (may be past)
	BanReason    string
}

type Notice struct {
	ID      int64
	Text    string
	By      string
	Created time.Time
}

type HelpThread struct {
	ID               int64
	GID              uint32
	Name, Subject    string
	Created, Updated time.Time
	Closed, Waiting  bool
	Messages         int
}

type HelpEntry struct {
	Staff     bool
	Text, By  string
	At        time.Time
	Delivered bool
}

type AuditEntry struct {
	At       time.Time
	Op       string
	GID      uint32
	Username string
	By       string
}

// --- Memory --------------------------------------------------------------------------------------------

func (m *Memory) Accounts(_ context.Context, pattern string, limit int) ([]AccountRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pat := strings.ToLower(pattern)
	var out []AccountRow
	for gid, a := range m.accounts {
		row := AccountRow{GID: gid, Username: a.Username, ResetPending: a.ResetPending, Created: a.Created, LastLogin: a.LastLogin}
		for slot := range 4 {
			if c, ok := m.chars[gid][slot]; ok {
				row.Characters = append(row.Characters, c.Name)
			}
		}
		if b, ok := m.bans[gid]; ok {
			row.BannedUntil, row.BanReason = b.Until, b.Reason
		}
		if pat == "" || strings.Contains(strings.ToLower(a.Username), pat) ||
			slices.ContainsFunc(row.Characters, func(n string) bool { return strings.Contains(strings.ToLower(n), pat) }) {
			out = append(out, row)
		}
	}
	slices.SortFunc(out, func(a, b AccountRow) int {
		return strings.Compare(strings.ToLower(a.Username), strings.ToLower(b.Username))
	})
	return out[:min(len(out), limit)], nil
}

func (m *Memory) RenameAccount(_ context.Context, gid uint32, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for g, o := range m.accounts {
		if g != gid && strings.EqualFold(o.Username, name) {
			return ErrNameTaken
		}
	}
	a, ok := m.accounts[gid]
	if !ok {
		return ErrNotFound
	}
	a.Username = name
	m.accounts[gid] = a
	return nil
}

func (m *Memory) DeleteAccount(_ context.Context, gid uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[gid]; !ok {
		return ErrNotFound
	}
	delete(m.accounts, gid)
	delete(m.chars, gid)
	delete(m.cards, gid)
	delete(m.black, gid)
	delete(m.bans, gid)
	delete(m.ops.mutes, gid)
	m.mail = slices.DeleteFunc(m.mail, func(x memMail) bool { return x.gid == gid })
	gone := map[int64]bool{}
	m.ops.threads = slices.DeleteFunc(m.ops.threads, func(t memThread) bool {
		gone[t.id] = t.gid == int64(gid)
		return gone[t.id]
	})
	m.ops.help = slices.DeleteFunc(m.ops.help, func(h memHelp) bool { return gone[h.thread] })
	return nil
}

func (m *Memory) NoticeList(context.Context) ([]Notice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.notices), nil
}

func (m *Memory) DeleteNotice(_ context.Context, id int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.notices)
	m.notices = slices.DeleteFunc(m.notices, func(x Notice) bool { return x.ID == id })
	return len(m.notices) < n, nil
}

func (m *Memory) helpThread(t memThread) HelpThread {
	h := HelpThread{ID: t.id, GID: uint32(t.gid), Name: t.name, Subject: t.subject, Created: t.created,
		Updated: t.updated, Closed: t.closed, Waiting: t.waiting && !t.closed}
	for _, x := range m.ops.help {
		if x.thread == t.id {
			h.Messages++
		}
	}
	return h
}

func (m *Memory) HelpThreads(_ context.Context, openOnly bool, limit int) ([]HelpThread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []HelpThread
	for _, t := range m.ops.threads {
		if !openOnly || !t.closed {
			out = append(out, m.helpThread(t))
		}
	}
	slices.SortStableFunc(out, func(a, b HelpThread) int { return b.Updated.Compare(a.Updated) })
	return out[:min(len(out), limit)], nil
}

func (m *Memory) HelpThread(_ context.Context, id int64) (HelpThread, []HelpEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.thread(id)
	if t == nil {
		return HelpThread{}, nil, ErrNotFound
	}
	var msgs []HelpEntry
	for _, x := range m.ops.help {
		if x.thread == id {
			msgs = append(msgs, HelpEntry{x.staff, x.text, x.by, x.at, x.delivered})
		}
	}
	return m.helpThread(*t), msgs, nil
}

func (m *Memory) AuditTail(_ context.Context, n int) ([]AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.audit[max(0, len(m.audit)-n):])
	slices.Reverse(out)
	return out, nil
}
