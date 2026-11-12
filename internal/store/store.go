// SPDX-License-Identifier: GPL-3.0-or-later

// Package store keeps accounts, characters and everything else that outlives a session.
// Postgres is the real store; Memory is for tests and quick local runs.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound  = errors.New("store: not found")
	ErrNameTaken = errors.New("store: account name taken")
	ErrGIDTaken  = errors.New("store: GID taken")
)

// Account is one login. GID is the game's account id (31 bits, never 0). Names are
// unique ignoring case. Passwords exist only as a PBKDF2 hash.
type Account struct {
	GID          uint32
	Username     string
	Salt, Hash   []byte
	ResetPending bool // the password is an admin-issued reset code (see auth)
	Created      time.Time
	LastLogin    time.Time // zero: never
}

// Character is one of an account's four slots: the raw 0x0D02 body the client sent and
// the state the game keeps (JSON; nil until the character is first selected).
type Character struct {
	Slot  int
	Name  string
	Raw   []byte
	State []byte
}

// Owned is a character and its account.
type Owned struct {
	GID uint32
	Character
}

type MailRec struct {
	ID  int64
	Rec []byte
}

type Ban struct {
	Until  time.Time
	Reason string
	By     string
}

type Store interface {
	// Account finds an account by name, ignoring case.
	Account(ctx context.Context, username string) (Account, error)
	// CreateAccount fails with ErrNameTaken or ErrGIDTaken.
	CreateAccount(ctx context.Context, a Account) error
	SetPassword(ctx context.Context, gid uint32, salt, hash []byte, resetPending bool) error
	TouchLogin(ctx context.Context, gid uint32, at time.Time) error

	// Characters lists an account's characters by slot.
	Characters(ctx context.Context, gid uint32) ([]Character, error)
	// CreateCharacter puts a new character in a slot, replacing any there.
	CreateCharacter(ctx context.Context, gid uint32, c Character) error
	SaveState(ctx context.Context, gid uint32, slot int, state []byte) error
	// SetRaw replaces a character's raw record (an appearance change), keeping its state.
	SetRaw(ctx context.Context, gid uint32, slot int, raw []byte) error
	// OpenShops are the characters whose state has an open My Shop (the shop search).
	OpenShops(ctx context.Context) ([]Owned, error)

	// Cards are an account's partner cards by slot.
	Cards(ctx context.Context, gid uint32) (map[int][]byte, error)
	SetCard(ctx context.Context, gid uint32, slot int, rec []byte) error // nil rec removes it
	// Mail is an account's newest limit mails, oldest first.
	Mail(ctx context.Context, gid uint32, limit int) ([]MailRec, error)
	AddMail(ctx context.Context, gid uint32, rec []byte) error
	SetMail(ctx context.Context, id int64, rec []byte) error
	// Blacklist entries in the order they were added.
	Blacklist(ctx context.Context, gid uint32) ([][]byte, error)
	SetBlacklist(ctx context.Context, gid, who uint32, entry []byte) error // nil entry removes it

	// ActiveBan reports the ban in force on gid at now, if any.
	ActiveBan(ctx context.Context, gid uint32, now time.Time) (Ban, bool, error)
	SetBan(ctx context.Context, gid uint32, b Ban) error

	// Notices are the operator's news, oldest first (the login MOTD and the ticker).
	Notices(ctx context.Context) ([]string, error)
	AddNotice(ctx context.Context, text, by string) error

	// AccountByGID finds an account by GID; FindGID resolves an account name, else a
	// character name, ignoring case.
	AccountByGID(ctx context.Context, gid uint32) (Account, error)
	FindGID(ctx context.Context, name string) (uint32, error)
	CountAccounts(ctx context.Context) (int, error)
	// AllCharacters lists every character (the board).
	AllCharacters(ctx context.Context) ([]Owned, error)
	// BannedGIDs are the accounts with a ban in force at now.
	BannedGIDs(ctx context.Context, now time.Time) ([]uint32, error)

	RecordClear(ctx context.Context, c Clear) error
	// Clears are every mission clear, oldest first.
	Clears(ctx context.Context) ([]Clear, error)

	// Events are the operator's calendar entries (JSON specs, see calendar.Event).
	Events(ctx context.Context) ([]Event, error)
	SetEvent(ctx context.Context, key string, spec []byte) error // nil spec removes it

	Mutes(ctx context.Context) ([]Mute, error)
	SetMute(ctx context.Context, m Mute) error
	Unmute(ctx context.Context, gid uint32) (bool, error)

	// The help desk: one open thread per account at a time.
	HelpOpen(ctx context.Context, gid uint32) (int64, bool, error)
	// HelpSent counts the account's messages since a time (its rate limit).
	HelpSent(ctx context.Context, gid uint32, since time.Time) (int, error)
	// HelpAsk adds a player message to the open thread, opening one (with subject) if none.
	HelpAsk(ctx context.Context, gid uint32, name, subject, text string, at time.Time) (int64, error)
	HelpLatestAnswer(ctx context.Context, thread int64) (string, bool, error)
	// HelpUndelivered are the account's staff answers not yet shown, oldest first.
	HelpUndelivered(ctx context.Context, gid uint32) ([]HelpMessage, error)
	HelpDelivered(ctx context.Context, id int64) error
	// HelpReply adds a staff answer (closing the thread if asked) and names the account.
	HelpReply(ctx context.Context, thread int64, text, by string, closeIt bool, at time.Time) (uint32, error)
	// HelpClose closes a thread; was reports it was closed already.
	HelpClose(ctx context.Context, thread int64, at time.Time) (gid uint32, was bool, err error)

	// Accounts lists accounts whose name or a character's name holds pattern (any case;
	// "" all), by name, at most limit.
	Accounts(ctx context.Context, pattern string, limit int) ([]AccountRow, error)
	RenameAccount(ctx context.Context, gid uint32, name string) error // ErrNameTaken
	// DeleteAccount removes the account and everything under its GID.
	DeleteAccount(ctx context.Context, gid uint32) error
	// NoticeList is Notices with ids, for editing; DeleteNotice reports whether it existed.
	NoticeList(ctx context.Context) ([]Notice, error)
	DeleteNotice(ctx context.Context, id int64) (bool, error)
	// HelpThreads are the newest-updated requests first; HelpThread one with its messages.
	HelpThreads(ctx context.Context, openOnly bool, limit int) ([]HelpThread, error)
	HelpThread(ctx context.Context, id int64) (HelpThread, []HelpEntry, error)
	// AuditTail is the last n audit entries, newest first.
	AuditTail(ctx context.Context, n int) ([]AuditEntry, error)

	// Audit records an account change. Never pass a password or reset code.
	Audit(ctx context.Context, op string, gid uint32, username, by string) error

	Close()
}

// Memory is an in-process Store.
type Memory struct {
	mu       sync.Mutex
	accounts map[uint32]Account
	chars    map[uint32]map[int]Character
	cards    map[uint32]map[int][]byte
	mail     []memMail
	black    map[uint32][]memBlack
	bans     map[uint32]Ban
	notices  []Notice
	audit    []AuditEntry
	ops      memOps
}

func NewMemory() *Memory {
	return &Memory{accounts: map[uint32]Account{}, chars: map[uint32]map[int]Character{}, bans: map[uint32]Ban{},
		cards: map[uint32]map[int][]byte{}, black: map[uint32][]memBlack{}}
}

func (m *Memory) Close() {}

func (m *Memory) Account(_ context.Context, username string) (Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if strings.EqualFold(a.Username, username) {
			return clone(a), nil
		}
	}
	return Account{}, ErrNotFound
}

func (m *Memory) CreateAccount(_ context.Context, a Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[a.GID]; ok {
		return ErrGIDTaken
	}
	for _, o := range m.accounts {
		if strings.EqualFold(o.Username, a.Username) {
			return ErrNameTaken
		}
	}
	if a.Created.IsZero() {
		a.Created = time.Now()
	}
	m.accounts[a.GID] = clone(a)
	return nil
}

func (m *Memory) SetPassword(_ context.Context, gid uint32, salt, hash []byte, resetPending bool) error {
	return m.update(gid, func(a *Account) {
		a.Salt, a.Hash, a.ResetPending = slices.Clone(salt), slices.Clone(hash), resetPending
	})
}

func (m *Memory) TouchLogin(_ context.Context, gid uint32, at time.Time) error {
	return m.update(gid, func(a *Account) { a.LastLogin = at })
}

func (m *Memory) update(gid uint32, f func(*Account)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[gid]
	if !ok {
		return ErrNotFound
	}
	f(&a)
	m.accounts[gid] = a
	return nil
}

func (m *Memory) Characters(_ context.Context, gid uint32) ([]Character, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Character
	for slot := range 4 {
		if c, ok := m.chars[gid][slot]; ok {
			out = append(out, Character{c.Slot, c.Name, slices.Clone(c.Raw), slices.Clone(c.State)})
		}
	}
	return out, nil
}

func (m *Memory) CreateCharacter(_ context.Context, gid uint32, c Character) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[gid]; !ok {
		return ErrNotFound
	}
	if m.chars[gid] == nil {
		m.chars[gid] = map[int]Character{}
	}
	m.chars[gid][c.Slot] = Character{c.Slot, c.Name, slices.Clone(c.Raw), slices.Clone(c.State)}
	return nil
}

func (m *Memory) SaveState(_ context.Context, gid uint32, slot int, state []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.chars[gid][slot]
	if !ok {
		return ErrNotFound
	}
	c.State = slices.Clone(state)
	m.chars[gid][slot] = c
	return nil
}

func (m *Memory) OpenShops(_ context.Context) ([]Owned, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Owned
	for gid, cs := range m.chars {
		for _, c := range cs {
			var v struct {
				Shop *struct {
					Open bool `json:"open"`
				} `json:"shop"`
			}
			if json.Unmarshal(c.State, &v) == nil && v.Shop != nil && v.Shop.Open {
				out = append(out, Owned{gid, Character{c.Slot, c.Name, slices.Clone(c.Raw), slices.Clone(c.State)}})
			}
		}
	}
	return out, nil
}

type memMail struct {
	id  int64
	gid uint32
	rec []byte
}

type memBlack struct {
	who   uint32
	entry []byte
}

func (m *Memory) SetRaw(_ context.Context, gid uint32, slot int, raw []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.chars[gid][slot]
	if !ok {
		return ErrNotFound
	}
	c.Raw = slices.Clone(raw)
	m.chars[gid][slot] = c
	return nil
}

func (m *Memory) Cards(_ context.Context, gid uint32) (map[int][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int][]byte{}
	for k, v := range m.cards[gid] {
		out[k] = slices.Clone(v)
	}
	return out, nil
}

func (m *Memory) SetCard(_ context.Context, gid uint32, slot int, rec []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cards[gid] == nil {
		m.cards[gid] = map[int][]byte{}
	}
	if rec == nil {
		delete(m.cards[gid], slot)
	} else {
		m.cards[gid][slot] = slices.Clone(rec)
	}
	return nil
}

func (m *Memory) Mail(_ context.Context, gid uint32, limit int) ([]MailRec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MailRec
	for _, x := range m.mail {
		if x.gid == gid {
			out = append(out, MailRec{x.id, slices.Clone(x.rec)})
		}
	}
	return out[max(0, len(out)-limit):], nil
}

func (m *Memory) AddMail(_ context.Context, gid uint32, rec []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mail = append(m.mail, memMail{int64(len(m.mail) + 1), gid, slices.Clone(rec)})
	return nil
}

func (m *Memory) SetMail(_ context.Context, id int64, rec []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.mail {
		if m.mail[i].id == id {
			m.mail[i].rec = slices.Clone(rec)
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) Blacklist(_ context.Context, gid uint32) ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [][]byte
	for _, b := range m.black[gid] {
		out = append(out, slices.Clone(b.entry))
	}
	return out, nil
}

func (m *Memory) SetBlacklist(_ context.Context, gid, who uint32, entry []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.black[gid]
	for i, b := range l {
		if b.who == who {
			if entry == nil {
				m.black[gid] = append(l[:i], l[i+1:]...)
			} else {
				l[i].entry = slices.Clone(entry)
			}
			return nil
		}
	}
	if entry != nil {
		m.black[gid] = append(l, memBlack{who, slices.Clone(entry)})
	}
	return nil
}

func (m *Memory) ActiveBan(_ context.Context, gid uint32, now time.Time) (Ban, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bans[gid]
	if !ok || !b.Until.After(now) {
		return Ban{}, false, nil
	}
	return b, true, nil
}

func (m *Memory) SetBan(_ context.Context, gid uint32, b Ban) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bans[gid] = b
	return nil
}

func (m *Memory) Notices(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.notices))
	for i, n := range m.notices {
		out[i] = n.Text
	}
	return out, nil
}

func (m *Memory) AddNotice(_ context.Context, text, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var id int64 = 1
	if n := len(m.notices); n > 0 {
		id = m.notices[n-1].ID + 1
	}
	m.notices = append(m.notices, Notice{id, text, by, time.Now()})
	return nil
}

func (m *Memory) Audit(_ context.Context, op string, gid uint32, username, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, AuditEntry{time.Now(), op, gid, username, by})
	return nil
}

func clone(a Account) Account {
	a.Salt, a.Hash = slices.Clone(a.Salt), slices.Clone(a.Hash)
	return a
}
