// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/princebyyourlogic/gurhal/internal/store"
)

const (
	pbkdf2Rounds = 200_000
	saltSize     = 16

	NameMax     = 23 // the 0x0219 name field is 24 bytes
	MinPassword = 4
	ResetLen    = 6 // an admin reset code; code + new password must fit the password field

	failWindow   = 10 * time.Minute
	failsPerName = 8
	failsPerAddr = 30
)

var (
	ErrBadLogin  = errors.New("wrong user ID or password")
	ErrThrottled = errors.New("too many failed logins")
)

// BannedError refuses a suspended account.
type BannedError struct{ Until time.Time }

func (e BannedError) Error() string {
	return "suspended until " + e.Until.UTC().Format("2006-01-02 15:04 MST")
}

// Service signs players in against the store.
type Service struct {
	Store        store.Store
	AutoRegister bool     // an unknown name registers with the password it was given
	Reserved     []string // names auto-register refuses (the GM accounts: made with psuctl)
	RegsPerHour  int      // new accounts one address may auto-register an hour; 0 = unlimited
	Log          *slog.Logger

	now  func() time.Time
	mu   sync.Mutex
	fail map[string][]time.Time // "name:..." / "addr:..." -> recent failures
	regs map[string][]time.Time // "addr:..." -> recent registrations
}

// SignIn checks a 0x0219 login. Names are Latin-1 as the client sends them. Passwords
// are never logged or stored in clear.
//
// After an admin reset the password is a ResetLen-character code: the code alone logs
// in, and the code immediately followed by a new password makes that the password (the
// client has no change-password screen).
func (s *Service) SignIn(ctx context.Context, name, password string, from netip.Addr) (uint32, error) {
	nameKey, addrKey := "name:"+strings.ToLower(name), "addr:"+from.String()
	if s.throttled(nameKey, failsPerName) || s.throttled(addrKey, failsPerAddr) {
		return 0, ErrThrottled
	}
	regKey := addrKey
	if from.IsLoopback() { // the plain test ports: bots and the diff register freely
		regKey = ""
	}
	gid, err := s.signIn(ctx, name, password, regKey)
	if errors.Is(err, ErrBadLogin) {
		s.failed(nameKey, addrKey)
	}
	if err != nil {
		return 0, err
	}
	if b, banned, err := s.Store.ActiveBan(ctx, gid, s.clock()); err != nil {
		return 0, err
	} else if banned {
		return 0, BannedError{b.Until}
	}
	return gid, s.Store.TouchLogin(ctx, gid, s.clock())
}

func (s *Service) signIn(ctx context.Context, name, password, addrKey string) (uint32, error) {
	a, err := s.Store.Account(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		if !s.AutoRegister || !ValidName(name) || len(password) < MinPassword || s.reserved(name) {
			return 0, ErrBadLogin
		}
		if !s.mayRegister(addrKey) {
			s.Log.Warn("auto-register refused: too many new accounts from one address", "addr", strings.TrimPrefix(addrKey, "addr:"))
			return 0, ErrBadLogin
		}
		return s.Create(ctx, name, password, false, "auto-register")
	}
	if err != nil {
		return 0, err
	}
	if matches(password, a.Salt, a.Hash) {
		return a.GID, nil
	}
	if a.ResetPending && len(password) >= ResetLen+MinPassword && matches(password[:ResetLen], a.Salt, a.Hash) {
		salt, hash := NewPassword(password[ResetLen:])
		if err := s.Store.SetPassword(ctx, a.GID, salt, hash, false); err != nil {
			return 0, err
		}
		s.Store.Audit(ctx, "password_set", a.GID, a.Username, "player: reset code + new password")
		return a.GID, nil
	}
	return 0, ErrBadLogin
}

// Create registers an account (auto-register, or an admin's with a reset code as the
// password: reset true). A name taken meanwhile is store.ErrNameTaken.
func (s *Service) Create(ctx context.Context, name, password string, reset bool, by string) (uint32, error) {
	salt, hash := NewPassword(password)
	gid := PreferredGID(name)
	for range 64 {
		err := s.Store.CreateAccount(ctx, store.Account{GID: gid, Username: name, Salt: salt, Hash: hash,
			ResetPending: reset, Created: s.clock()})
		switch {
		case err == nil:
			s.Store.Audit(ctx, "register", gid, name, by)
			s.Log.Info("account registered", "user", name, "gid", fmt.Sprintf("%#08x", gid), "by", by)
			return gid, nil
		case errors.Is(err, store.ErrGIDTaken):
			if gid = (gid + 1) & 0x7FFFFFFF; gid == 0 {
				gid = 1
			}
		case errors.Is(err, store.ErrNameTaken) && !reset: // registered meanwhile by another connection
			return s.signIn(ctx, name, password, "")
		default:
			return 0, err
		}
	}
	return 0, errors.New("auth: no free GID near the preferred one")
}

func (s *Service) reserved(name string) bool {
	for _, r := range s.Reserved {
		if strings.EqualFold(r, name) {
			return true
		}
	}
	return false
}

// mayRegister counts a registration from an address, refusing past RegsPerHour.
func (s *Service) mayRegister(addrKey string) bool {
	if s.RegsPerHour <= 0 || addrKey == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.clock().Add(-time.Hour)
	if s.regs == nil {
		s.regs = map[string][]time.Time{}
	}
	recent := s.regs[addrKey][:0]
	for _, t := range s.regs[addrKey] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= s.RegsPerHour {
		s.regs[addrKey] = recent
		return false
	}
	s.regs[addrKey] = append(recent, s.clock())
	for k, ts := range s.regs { // forget addresses with nothing recent
		if len(ts) > 0 && !ts[len(ts)-1].After(cutoff) {
			delete(s.regs, k)
		}
	}
	return true
}

// PreferredGID derives an account's GID from its name: 31 bits of SHA-1, never 0. A
// clash moves to the next free one.
func PreferredGID(name string) uint32 {
	h := sha1.Sum([]byte(strings.ToLower(name)))
	if g := binary.LittleEndian.Uint32(h[:]) & 0x7FFFFFFF; g != 0 {
		return g
	}
	return 1
}

// ValidName: 1..NameMax printable Latin-1 characters, no leading or trailing space.
func ValidName(name string) bool {
	n := 0
	for _, r := range name {
		if r > 0xFF || !unicode.IsPrint(r) {
			return false
		}
		n++
	}
	return n > 0 && n <= NameMax && name == strings.TrimSpace(name)
}

// NewPassword returns a fresh salt and the password's hash.
func NewPassword(password string) (salt, hash []byte) {
	salt = make([]byte, saltSize)
	rand.Read(salt)
	return salt, hashPassword(password, salt)
}

// hashSlots bounds the PBKDF2 work in flight to the CPUs: a login flood queues instead
// of starving the game.
var hashSlots = make(chan struct{}, runtime.GOMAXPROCS(0))

func hashPassword(password string, salt []byte) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	h, err := pbkdf2.Key(sha256.New, latin1Bytes(password), salt, pbkdf2Rounds, sha256.Size)
	if err != nil {
		panic(err) // only for invalid parameters, which are constants here
	}
	return h
}

func matches(password string, salt, hash []byte) bool {
	return subtle.ConstantTimeCompare(hashPassword(password, salt), hash) == 1
}

// latin1Bytes turns a password back into the bytes the client typed.
func latin1Bytes(s string) string {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		b = append(b, byte(r))
	}
	return string(b)
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Service) throttled(key string, limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.clock().Add(-failWindow)
	recent := s.fail[key][:0]
	for _, t := range s.fail[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(s.fail, key)
	} else {
		s.fail[key] = recent
	}
	return len(recent) >= limit
}

func (s *Service) failed(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail == nil {
		s.fail = map[string][]time.Time{}
	}
	for _, k := range keys {
		s.fail[k] = append(s.fail[k], s.clock())
	}
}

// resetAlphabet leaves out 0/O and 1/I, which players misread.
const resetAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// ResetCode is a fresh one-time code for an admin password reset.
func ResetCode() string {
	b := make([]byte, ResetLen)
	rand.Read(b)
	for i := range b {
		b[i] = resetAlphabet[int(b[i])%len(resetAlphabet)]
	}
	return string(b)
}
