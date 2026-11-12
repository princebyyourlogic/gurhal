// SPDX-License-Identifier: GPL-3.0-or-later

package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/auth"
	"github.com/princebyyourlogic/gurhal/internal/store"
)

type fakeGame struct {
	online map[uint32]bool
	kicked []uint32
	sent   []string
}

func (g *fakeGame) Status() any  { return map[string]int{"online": len(g.online)} }
func (g *fakeGame) Players() any { return []string{} }
func (g *fakeGame) Announce(text, scope, kind string, secs int) (int, error) {
	g.sent = append(g.sent, scope+"|"+kind+"|"+text)
	return len(g.online), nil
}
func (g *fakeGame) Find(name string) (uint32, bool, bool) {
	if name == "Zed" {
		return 7, g.online[7], true
	}
	return 0, false, false
}
func (g *fakeGame) Online(gid uint32) bool { return g.online[gid] }
func (g *fakeGame) Kick(gid uint32, _ string) bool {
	g.kicked = append(g.kicked, gid)
	return g.online[gid]
}
func (g *fakeGame) Mute(uint32, int, string, string) error { return nil }
func (g *fakeGame) Unmute(uint32) bool                     { return true }
func (g *fakeGame) SeasonPreview(uint32, string) string    { return "Season none" }
func (g *fakeGame) ReloadEvents(context.Context) []string  { return nil }
func (g *fakeGame) HelpReply(context.Context, int64, string, string, bool) (string, error) {
	return "Answered.", nil
}
func (g *fakeGame) HelpClose(context.Context, int64, string) (string, error) { return "Closed.", nil }

func setup(t *testing.T) (*httptest.Server, *fakeGame, *store.Memory) {
	st, g := store.NewMemory(), &fakeGame{online: map[uint32]bool{7: true}}
	st.CreateAccount(context.Background(), store.Account{GID: 7, Username: "Zed", Salt: []byte("s"), Hash: []byte("h")})
	log := slog.New(slog.DiscardHandler)
	srv := httptest.NewServer((&Server{Game: g, Auth: &auth.Service{Store: st, Log: log}, Store: st, Token: "sekrit", Log: log}).Handler())
	t.Cleanup(srv.Close)
	return srv, g, st
}

func call(t *testing.T, srv *httptest.Server, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-PSU-By", "tester")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestToken(t *testing.T) {
	srv, _, _ := setup(t)
	if code, _ := call(t, srv, "wrong", "GET", "/v1/status", ""); code != http.StatusUnauthorized {
		t.Fatalf("a bad token got %d", code)
	}
	if code, out := call(t, srv, "sekrit", "GET", "/v1/status", ""); code != 200 || out["online"] != 1.0 {
		t.Fatalf("status %d %v", code, out)
	}
}

func TestAccounts(t *testing.T) {
	srv, g, st := setup(t)
	ctx := context.Background()
	code, out := call(t, srv, "sekrit", "POST", "/v1/accounts/zed/reset", "")
	if code != 200 || len(out["code"].(string)) != 6 {
		t.Fatalf("reset %d %v", code, out)
	}
	if a, _ := st.Account(ctx, "zed"); !a.ResetPending {
		t.Fatal("no reset pending")
	}
	if code, out = call(t, srv, "sekrit", "POST", "/v1/accounts/zed/ban", `{"hours": 2, "reason": "cheat"}`); code != 200 || out["kicked"] != true {
		t.Fatalf("ban %d %v", code, out)
	}
	if b, banned, _ := st.ActiveBan(ctx, 7, time.Now()); !banned || b.Reason != "cheat" || b.By != "tester" {
		t.Fatalf("ban stored %+v %v", b, banned)
	}
	if code, _ = call(t, srv, "sekrit", "POST", "/v1/accounts/zed/rename", `{"to": "a name far longer than the field allows"}`); code != 400 {
		t.Fatalf("an invalid rename got %d", code)
	}
	if code, out = call(t, srv, "sekrit", "DELETE", "/v1/accounts/zed", ""); code != http.StatusConflict {
		t.Fatalf("deleting an online account %d %v", code, out)
	}
	if code, _ = call(t, srv, "sekrit", "DELETE", "/v1/accounts/zed?force=1", ""); code != 200 {
		t.Fatalf("forced delete %d", code)
	}
	if _, err := st.AccountByGID(ctx, 7); err == nil || len(g.kicked) != 2 {
		t.Fatalf("deleted account still there (kicked %v)", g.kicked)
	}
	if code, _ = call(t, srv, "sekrit", "POST", "/v1/accounts/nobody/ban", ""); code != 404 {
		t.Fatalf("a missing account got %d", code)
	}
	au, _ := st.AuditTail(ctx, 10)
	if len(au) != 3 || au[0].Op != "delete" || au[2].Op != "reset" || au[0].By != "tester" {
		t.Fatalf("audit %+v", au)
	}
}

func TestCreateAccount(t *testing.T) {
	srv, _, st := setup(t)
	code, out := call(t, srv, "sekrit", "POST", "/v1/accounts", `{"name": "GameMaster"}`)
	if code != 200 || len(out["code"].(string)) != 6 {
		t.Fatalf("create %d %v", code, out)
	}
	if a, err := st.Account(context.Background(), "gamemaster"); err != nil || !a.ResetPending {
		t.Fatalf("created %+v %v", a, err)
	}
	if code, _ := call(t, srv, "sekrit", "POST", "/v1/accounts", `{"name": "zed"}`); code != http.StatusConflict {
		t.Fatalf("a taken name got %d", code)
	}
}

func TestNewsEventsHelp(t *testing.T) {
	srv, g, st := setup(t)
	ctx := context.Background()
	if code, _ := call(t, srv, "sekrit", "POST", "/v1/news", `{"text": "  Maintenance at 6  "}`); code != 200 {
		t.Fatalf("add news %d", code)
	}
	if n, _ := st.NoticeList(ctx); len(n) != 1 || n[0].Text != "Maintenance at 6" || n[0].By != "tester" {
		t.Fatalf("news %+v", n)
	}
	if code, _ := call(t, srv, "sekrit", "DELETE", "/v1/news/99", ""); code != 404 {
		t.Fatalf("delete missing notice %d", code)
	}
	if code, out := call(t, srv, "sekrit", "PUT", "/v1/events/x", `{"key":"x","name":"X","start":"2026-01-02T00:00Z","end":"2026-01-01T00:00Z"}`); code != 400 {
		t.Fatalf("a bad event %d %v", code, out)
	}
	if code, _ := call(t, srv, "sekrit", "PUT", "/v1/events/y", `{"key":"x","name":"X","start":"2026-01-01T00:00Z","end":"2026-01-02T00:00Z"}`); code != 400 {
		t.Fatal("a key mismatch was saved")
	}
	if code, _ := call(t, srv, "sekrit", "PUT", "/v1/events/x", `{"key":"x","name":"X","start":"2026-01-01T00:00Z","end":"2099-01-02T00:00Z"}`); code != 200 {
		t.Fatal("a good event was refused")
	}
	if code, out := call(t, srv, "sekrit", "GET", "/v1/events", ""); code != 200 || len(out["upcoming"].([]any)) != 1 {
		t.Fatalf("events %v", out)
	}
	if code, out := call(t, srv, "sekrit", "POST", "/v1/announce", `{"text": "hi", "scope": "uni:1"}`); code != 200 || out["sent"] != 1.0 || g.sent[0] != "uni:1||hi" {
		t.Fatalf("announce %v %v", out, g.sent)
	}
	if code, _ := call(t, srv, "sekrit", "POST", "/v1/help/1/reply", `{"text": "ok", "bogus": 1}`); code != 400 {
		t.Fatal("unknown fields accepted")
	}
}

func TestLoadToken(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadToken("", dir)
	if err != nil || len(a) != 48 {
		t.Fatalf("token %q %v", a, err)
	}
	if b, _ := LoadToken("", dir); b != a {
		t.Fatal("the token changed on restart")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "admin.token")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", fi.Mode())
	}
	if c, _ := LoadToken(" fromenv ", dir); c != "fromenv" {
		t.Fatal("PSU_ADMIN_TOKEN is not used")
	}
}
