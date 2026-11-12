// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Every Store passes the same contract. Postgres runs when PSU_TEST_DATABASE_URL names a
// throwaway database (make test-db); its tables are dropped first.
func TestMemory(t *testing.T) { contract(t, NewMemory()) }

func TestPostgres(t *testing.T) {
	url := os.Getenv("PSU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PSU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	p, err := OpenPostgres(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	if p, err = OpenPostgres(ctx, url); err != nil { // migrations are applied once
		t.Fatal(err)
	}
	defer p.Close()
	contract(t, p)
}

func contract(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)
	a := Account{GID: 0x3965b242, Username: "Ethan", Salt: []byte("salt"), Hash: []byte("hash"), Created: now}

	if _, err := s.Account(ctx, "ethan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Account before create: %v", err)
	}
	if err := s.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccount(ctx, Account{GID: 7, Username: "ETHAN", Salt: a.Salt, Hash: a.Hash}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same name in other case: %v", err)
	}
	if err := s.CreateAccount(ctx, Account{GID: a.GID, Username: "other", Salt: a.Salt, Hash: a.Hash}); !errors.Is(err, ErrGIDTaken) {
		t.Fatalf("same GID: %v", err)
	}
	got, err := s.Account(ctx, "eThAn")
	if err != nil || got.GID != a.GID || got.Username != "Ethan" || string(got.Hash) != "hash" || !got.LastLogin.IsZero() {
		t.Fatalf("Account = %+v, %v", got, err)
	}

	if err := s.SetPassword(ctx, a.GID, []byte("s2"), []byte("h2"), true); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchLogin(ctx, a.GID, now); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Account(ctx, "ethan")
	if string(got.Salt) != "s2" || string(got.Hash) != "h2" || !got.ResetPending || !got.LastLogin.Equal(now) {
		t.Fatalf("after SetPassword/TouchLogin: %+v", got)
	}
	if err := s.SetPassword(ctx, 12345, nil, nil, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetPassword on a missing GID: %v", err)
	}

	if _, ok, err := s.ActiveBan(ctx, a.GID, now); ok || err != nil {
		t.Fatalf("ban before any: %v %v", ok, err)
	}
	if err := s.SetBan(ctx, a.GID, Ban{Until: now.Add(time.Hour), Reason: "spam", By: "test"}); err != nil {
		t.Fatal(err)
	}
	if b, ok, err := s.ActiveBan(ctx, a.GID, now); !ok || err != nil || b.Reason != "spam" {
		t.Fatalf("active ban: %+v %v %v", b, ok, err)
	}
	if _, ok, _ := s.ActiveBan(ctx, a.GID, now.Add(2*time.Hour)); ok {
		t.Fatal("an expired ban is still in force")
	}

	if cs, err := s.Characters(ctx, a.GID); err != nil || len(cs) != 0 {
		t.Fatalf("characters before any: %v %v", cs, err)
	}
	for _, slot := range []int{2, 0} {
		if err := s.CreateCharacter(ctx, a.GID, Character{Slot: slot, Name: "Ethan", Raw: []byte{1, 2, 3}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateCharacter(ctx, 999, Character{Slot: 0, Name: "x", Raw: []byte{1}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("character for a missing account: %v", err)
	}
	if err := s.SaveState(ctx, a.GID, 2, []byte(`{"level": 5}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveState(ctx, a.GID, 3, []byte(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("state for an empty slot: %v", err)
	}
	cs, err := s.Characters(ctx, a.GID)
	if err != nil || len(cs) != 2 || cs[0].Slot != 0 || cs[1].Slot != 2 || cs[0].State != nil ||
		string(cs[0].Raw) != "\x01\x02\x03" || !strings.Contains(string(cs[1].State), `"level": 5`) {
		t.Fatalf("characters = %+v, %v", cs, err)
	}
	if err := s.CreateCharacter(ctx, a.GID, Character{Slot: 2, Name: "New", Raw: []byte{9}}); err != nil {
		t.Fatal(err)
	}
	if cs, _ := s.Characters(ctx, a.GID); cs[1].Name != "New" || cs[1].State != nil {
		t.Fatalf("a recreated slot keeps its old state: %+v", cs[1])
	}

	if err := s.SetRaw(ctx, a.GID, 2, []byte{7, 7}); err != nil {
		t.Fatal(err)
	}
	if cs, _ := s.Characters(ctx, a.GID); string(cs[1].Raw) != "\x07\x07" || cs[1].Name != "New" {
		t.Fatalf("SetRaw: %+v", cs[1])
	}
	s.SaveState(ctx, a.GID, 0, []byte(`{"level": 1, "shop": {"open": true}}`))
	s.SaveState(ctx, a.GID, 2, []byte(`{"level": 1, "shop": {"open": false}}`))
	if sh, err := s.OpenShops(ctx); err != nil || len(sh) != 1 || sh[0].GID != a.GID || sh[0].Slot != 0 {
		t.Fatalf("open shops %+v, %v", sh, err)
	}
	s.SetCard(ctx, a.GID, 3, []byte("card3"))
	s.SetCard(ctx, a.GID, 1, []byte("card1"))
	s.SetCard(ctx, a.GID, 3, nil)
	if c, err := s.Cards(ctx, a.GID); err != nil || len(c) != 1 || string(c[1]) != "card1" {
		t.Fatalf("cards %v %v", c, err)
	}
	for _, m := range []string{"m1", "m2", "m3"} {
		if err := s.AddMail(ctx, a.GID, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := s.Mail(ctx, a.GID, 2)
	if err != nil || len(ms) != 2 || string(ms[0].Rec) != "m2" || string(ms[1].Rec) != "m3" {
		t.Fatalf("mail %v %v", ms, err)
	}
	if err := s.SetMail(ctx, ms[0].ID, []byte("m2read")); err != nil {
		t.Fatal(err)
	}
	if ms, _ := s.Mail(ctx, a.GID, 5); string(ms[1].Rec) != "m2read" {
		t.Fatalf("mail after SetMail %v", ms)
	}
	for _, w := range []uint32{9, 4, 9} {
		s.SetBlacklist(ctx, a.GID, w, []byte{byte(w)})
	}
	s.SetBlacklist(ctx, a.GID, 4, nil)
	if bl, err := s.Blacklist(ctx, a.GID); err != nil || len(bl) != 1 || bl[0][0] != 9 {
		t.Fatalf("blacklist %v %v", bl, err)
	}

	for _, n := range []string{"first", "second"} {
		if err := s.AddNotice(ctx, n, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.Notices(ctx); err != nil || !slices.Equal(n, []string{"first", "second"}) {
		t.Fatalf("Notices = %q, %v", n, err)
	}
	if err := s.Audit(ctx, "register", a.GID, a.Username, "test"); err != nil {
		t.Fatal(err)
	}
	ops(t, s, a.GID, now)
	admin(t, s, a.GID)
}

func admin(t *testing.T, s Store, gid uint32) {
	ctx := context.Background()
	other := Account{GID: 0x1234, Username: "Zed", Salt: []byte("s"), Hash: []byte("h")}
	if err := s.CreateAccount(ctx, other); err != nil {
		t.Fatal(err)
	}
	s.SetBan(ctx, gid, Ban{Until: time.Now().Add(time.Hour), Reason: "test"})
	rows, err := s.Accounts(ctx, "", 10)
	if err != nil || len(rows) != 2 || rows[0].Username != "Ethan" || rows[1].Username != "Zed" ||
		len(rows[0].Characters) != 2 || rows[0].BanReason != "test" || rows[0].BannedUntil.IsZero() {
		t.Fatalf("Accounts %+v %v", rows, err)
	}
	if rows, _ := s.Accounts(ctx, "NE", 10); len(rows) != 1 || rows[0].GID != gid { // a character "New"
		t.Fatalf("Accounts by character %+v", rows)
	}
	if err := s.RenameAccount(ctx, other.GID, "ethan"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename onto a taken name: %v", err)
	}
	if err := s.RenameAccount(ctx, other.GID, "Zara"); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Account(ctx, "zara"); err != nil || a.GID != other.GID {
		t.Fatalf("after rename %+v %v", a, err)
	}

	ns, err := s.NoticeList(ctx)
	if err != nil || len(ns) != 2 || ns[0].Text != "first" || ns[0].By != "test" {
		t.Fatalf("NoticeList %+v %v", ns, err)
	}
	if ok, _ := s.DeleteNotice(ctx, ns[0].ID); !ok {
		t.Fatal("DeleteNotice")
	}
	if n, _ := s.Notices(ctx); !slices.Equal(n, []string{"second"}) {
		t.Fatalf("after delete %q", n)
	}

	s.HelpAsk(ctx, other.GID, "Zchar", "lost", "I am lost", time.Now())
	th, err := s.HelpThreads(ctx, true, 10)
	if err != nil || len(th) != 1 || th[0].GID != other.GID || th[0].Subject != "lost" || th[0].Name != "Zchar" ||
		!th[0].Waiting || th[0].Messages != 1 {
		t.Fatalf("open HelpThreads %+v %v", th, err)
	}
	if all, _ := s.HelpThreads(ctx, false, 10); len(all) != 2 {
		t.Fatalf("all HelpThreads %+v", all)
	}
	s.HelpReply(ctx, th[0].ID, "look up", "staff", false, time.Now())
	one, msgs, err := s.HelpThread(ctx, th[0].ID)
	if err != nil || one.Waiting || len(msgs) != 2 || msgs[0].Staff || !msgs[1].Staff || msgs[1].By != "staff" {
		t.Fatalf("HelpThread %+v %+v %v", one, msgs, err)
	}
	if au, err := s.AuditTail(ctx, 5); err != nil || len(au) != 1 || au[0].Op != "register" {
		t.Fatalf("AuditTail %+v %v", au, err)
	}

	if err := s.DeleteAccount(ctx, other.GID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AccountByGID(ctx, other.GID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted account %v", err)
	}
	if th, _ := s.HelpThreads(ctx, false, 10); len(th) != 1 {
		t.Fatalf("the deleted account's help threads remain: %+v", th)
	}
	if err := s.DeleteAccount(ctx, other.GID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice %v", err)
	}
}

func ops(t *testing.T, s Store, gid uint32, now time.Time) {
	ctx := context.Background()
	if acc, err := s.AccountByGID(ctx, gid); err != nil || acc.Username != "Ethan" {
		t.Fatalf("AccountByGID %+v %v", acc, err)
	}
	for _, name := range []string{"ethan", "nEw"} {
		if g, err := s.FindGID(ctx, name); err != nil || g != gid {
			t.Fatalf("FindGID(%s) = %#x, %v", name, g, err)
		}
	}
	if _, err := s.FindGID(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindGID(nobody) %v", err)
	}
	if n, err := s.CountAccounts(ctx); err != nil || n != 1 {
		t.Fatalf("CountAccounts %d %v", n, err)
	}
	if cs, err := s.AllCharacters(ctx); err != nil || len(cs) != 2 {
		t.Fatalf("AllCharacters %d %v", len(cs), err)
	}
	s.SetBan(ctx, gid, Ban{Until: now.Add(time.Hour)})
	if b, err := s.BannedGIDs(ctx, now); err != nil || !slices.Equal(b, []uint32{gid}) {
		t.Fatalf("BannedGIDs %v %v", b, err)
	}
	s.SetBan(ctx, gid, Ban{})
	if b, _ := s.BannedGIDs(ctx, now); len(b) != 0 {
		t.Fatalf("BannedGIDs after unban %v", b)
	}

	c := Clear{At: now, Game: "psu", Uni: 1, Quest: 110000, Secs: 300, Rank: 3, Score: 90, Kills: 40,
		Party: []Member{{gid, 0, "New"}}}
	if err := s.RecordClear(ctx, c); err != nil {
		t.Fatal(err)
	}
	if cl, err := s.Clears(ctx); err != nil || len(cl) != 1 || !cl[0].At.Equal(now) || cl[0].Quest != 110000 ||
		cl[0].Rank != 3 || len(cl[0].Party) != 1 || cl[0].Party[0].Name != "New" {
		t.Fatalf("Clears %+v %v", cl, err)
	}

	s.SetEvent(ctx, "b", []byte(`{"key": "b"}`))
	s.SetEvent(ctx, "a", []byte(`{"key": "a"}`))
	s.SetEvent(ctx, "b", nil)
	if ev, err := s.Events(ctx); err != nil || len(ev) != 1 || ev[0].Key != "a" {
		t.Fatalf("Events %+v %v", ev, err)
	}

	s.SetMute(ctx, Mute{GID: gid, Reason: "spam", By: "gm"})
	if ms, err := s.Mutes(ctx); err != nil || len(ms) != 1 || !ms[0].Until.IsZero() || ms[0].Reason != "spam" {
		t.Fatalf("Mutes %+v %v", ms, err)
	}
	if ok, err := s.Unmute(ctx, gid); !ok || err != nil {
		t.Fatalf("Unmute %v %v", ok, err)
	}
	if ok, _ := s.Unmute(ctx, gid); ok {
		t.Fatal("unmuted twice")
	}

	if _, ok, _ := s.HelpOpen(ctx, gid); ok {
		t.Fatal("a help thread before asking")
	}
	t1, err := s.HelpAsk(ctx, gid, "New", "stuck", "I am stuck", now)
	if err != nil {
		t.Fatal(err)
	}
	if t2, _ := s.HelpAsk(ctx, gid, "New", "", "still stuck", now.Add(time.Second)); t2 != t1 {
		t.Fatalf("a second thread %d while %d is open", t2, t1)
	}
	if n, _ := s.HelpSent(ctx, gid, now.Add(-time.Minute)); n != 2 {
		t.Fatalf("HelpSent %d", n)
	}
	if _, ok, _ := s.HelpLatestAnswer(ctx, t1); ok {
		t.Fatal("an answer before replying")
	}
	if g, err := s.HelpReply(ctx, t1, "try again", "staff", false, now); err != nil || g != gid {
		t.Fatalf("HelpReply %#x %v", g, err)
	}
	if text, ok, _ := s.HelpLatestAnswer(ctx, t1); !ok || text != "try again" {
		t.Fatalf("HelpLatestAnswer %q", text)
	}
	un, err := s.HelpUndelivered(ctx, gid)
	if err != nil || len(un) != 1 || un[0].Text != "try again" {
		t.Fatalf("HelpUndelivered %+v %v", un, err)
	}
	s.HelpDelivered(ctx, un[0].ID)
	if un, _ := s.HelpUndelivered(ctx, gid); len(un) != 0 {
		t.Fatalf("undelivered after delivery %+v", un)
	}
	if g, was, err := s.HelpClose(ctx, t1, now); err != nil || was || g != gid {
		t.Fatalf("HelpClose %#x %v %v", g, was, err)
	}
	if _, was, _ := s.HelpClose(ctx, t1, now); !was {
		t.Fatal("closed twice without noticing")
	}
	if _, ok, _ := s.HelpOpen(ctx, gid); ok {
		t.Fatal("open after close")
	}
}
