// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/store"
)

var home = netip.MustParseAddr("192.0.2.1")

func service(autoRegister bool) (*Service, *store.Memory) {
	st := store.NewMemory()
	return &Service{Store: st, AutoRegister: autoRegister, Log: slog.New(slog.DiscardHandler)}, st
}

func TestRegisterThenSignIn(t *testing.T) {
	s, st := service(true)
	ctx := context.Background()
	gid, err := s.SignIn(ctx, "Ethan", "hunter2", home)
	if err != nil || gid != PreferredGID("ethan") {
		t.Fatalf("register: %#x %v", gid, err)
	}
	if got, err := s.SignIn(ctx, "ETHAN", "hunter2", home); err != nil || got != gid {
		t.Fatalf("sign in, other case: %#x %v", got, err)
	}
	if _, err := s.SignIn(ctx, "ethan", "wrong", home); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("wrong password: %v", err)
	}
	if a, _ := st.Account(ctx, "ethan"); a.Username != "Ethan" || a.LastLogin.IsZero() {
		t.Fatalf("stored account %+v", a)
	}
}

func TestNoAutoRegister(t *testing.T) {
	s, _ := service(false)
	if _, err := s.SignIn(context.Background(), "nobody", "password", home); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("unknown name without auto-register: %v", err)
	}
}

func TestBadNamesAndShortPasswordsDoNotRegister(t *testing.T) {
	s, _ := service(true)
	for _, c := range [][2]string{{" lead", "password"}, {"x\x01y", "password"}, {"twentyfourcharacterslong", "password"}, {"ok", "abc"}} {
		if _, err := s.SignIn(context.Background(), c[0], c[1], home); !errors.Is(err, ErrBadLogin) {
			t.Errorf("%q / %q registered: %v", c[0], c[1], err)
		}
	}
}

func TestGIDClashMovesOn(t *testing.T) {
	s, st := service(true)
	ctx := context.Background()
	salt, hash := NewPassword("x")
	st.CreateAccount(ctx, store.Account{GID: PreferredGID("ethan"), Username: "squatter", Salt: salt, Hash: hash})
	if gid, err := s.SignIn(ctx, "ethan", "hunter2", home); err != nil || gid != PreferredGID("ethan")+1 {
		t.Fatalf("clash: %#x %v", gid, err)
	}
}

func TestResetCode(t *testing.T) {
	s, st := service(false)
	ctx := context.Background()
	salt, hash := NewPassword("ABC234")
	st.CreateAccount(ctx, store.Account{GID: 5, Username: "p", Salt: salt, Hash: hash, ResetPending: true})
	if gid, err := s.SignIn(ctx, "p", "ABC234", home); err != nil || gid != 5 {
		t.Fatalf("code alone: %v", err)
	}
	if _, err := s.SignIn(ctx, "p", "ABC234newpass", home); err != nil {
		t.Fatalf("code + new password: %v", err)
	}
	if _, err := s.SignIn(ctx, "p", "newpass", home); err != nil {
		t.Fatalf("new password: %v", err)
	}
	if _, err := s.SignIn(ctx, "p", "ABC234", home); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("the code still works after the new password was set: %v", err)
	}
}

func TestBan(t *testing.T) {
	s, st := service(true)
	ctx := context.Background()
	gid, _ := s.SignIn(ctx, "p", "password", home)
	st.SetBan(ctx, gid, store.Ban{Until: time.Now().Add(time.Hour)})
	var b BannedError
	if _, err := s.SignIn(ctx, "p", "password", home); !errors.As(err, &b) {
		t.Fatalf("banned account signed in: %v", err)
	}
}

func TestThrottle(t *testing.T) {
	s, _ := service(true)
	ctx := context.Background()
	s.SignIn(ctx, "p", "password", home)
	for range failsPerName {
		s.SignIn(ctx, "p", "wrong", home)
	}
	if _, err := s.SignIn(ctx, "p", "password", home); !errors.Is(err, ErrThrottled) {
		t.Fatalf("after %d failures: %v", failsPerName, err)
	}
	later := time.Now().Add(failWindow + time.Second)
	s.now = func() time.Time { return later }
	if _, err := s.SignIn(ctx, "p", "password", home); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestKeys(t *testing.T) {
	k := NewKeys()
	key := k.Issue(7, "aoti")
	wrong := [4]byte{key[0] ^ 1, key[1], key[2], key[3]}
	if _, ok := k.Take(7, wrong[:]); ok {
		t.Fatal("wrong key accepted")
	}
	key = k.Issue(7, "aoti")
	if d, ok := k.Take(7, key[:]); !ok || d != "aoti" {
		t.Fatalf("Take = %q %v", d, ok)
	}
	if _, ok := k.Take(7, key[:]); ok {
		t.Fatal("key used twice")
	}
	key = k.Issue(8, "psu")
	k.now = func() time.Time { return time.Now().Add(keyTTL + time.Second) }
	if _, ok := k.Take(8, key[:]); ok {
		t.Fatal("expired key accepted")
	}
}

// A GM name is never auto-registered: the operator makes it with a reset code.
func TestReservedNames(t *testing.T) {
	s, _ := service(true)
	s.Reserved = []string{"Admin"}
	ctx := context.Background()
	if _, err := s.SignIn(ctx, "ADMIN", "password", home); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("a reserved name registered: %v", err)
	}
	code := ResetCode()
	gid, err := s.Create(ctx, "Admin", code, true, "psuctl")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.SignIn(ctx, "admin", code+"realpass", home); err != nil || got != gid {
		t.Fatalf("the operator-made account %v %v", got, err)
	}
	if _, err := s.Create(ctx, "admin", "x", true, "psuctl"); !errors.Is(err, store.ErrNameTaken) {
		t.Fatalf("creating a taken name: %v", err)
	}
}

func TestRegistrationsPerHour(t *testing.T) {
	s, _ := service(true)
	s.RegsPerHour = 2
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	ctx := context.Background()
	for i, name := range []string{"one", "two"} {
		if _, err := s.SignIn(ctx, name, "password", home); err != nil {
			t.Fatalf("registration %d: %v", i, err)
		}
	}
	if _, err := s.SignIn(ctx, "three", "password", home); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("a third account within the hour: %v", err)
	}
	if _, err := s.SignIn(ctx, "one", "password", home); err != nil {
		t.Fatalf("an existing account is still allowed: %v", err)
	}
	if _, err := s.SignIn(ctx, "three", "password", netip.MustParseAddr("198.51.100.9")); err != nil {
		t.Fatalf("another address: %v", err)
	}
	now = now.Add(61 * time.Minute)
	if _, err := s.SignIn(ctx, "four", "password", home); err != nil {
		t.Fatalf("after the hour: %v", err)
	}
}
