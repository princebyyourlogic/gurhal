// SPDX-License-Identifier: GPL-3.0-or-later

// Package admin is psud's loopback admin API (psuctl's server) and the status.json writer.
// HTTP + JSON on 127.0.0.1, every request with "Authorization: Bearer <token>"; the token
// is PSU_ADMIN_TOKEN, else a random one psud writes to <state>/admin.token (mode 0600).
// "X-PSU-By" names the operator for the audit trail.
//
//	GET    /v1/status                   status.json's content
//	GET    /v1/players                  everyone online
//	POST   /v1/announce                 {text, scope, kind, seconds}
//	POST   /v1/players/{name}/kick      {reason}
//	POST   /v1/players/{name}/mute      {minutes, reason}      .../unmute
//	POST   /v1/players/{name}/season    {season}               GET /v1/season
//	GET    /v1/accounts?q=&limit=       accounts, their characters and bans
//	POST   /v1/accounts                 {name}: a new account with a reset code (GM accounts)
//	POST   /v1/accounts/{name}/reset    a one-time reset code, shown once
//	POST   /v1/accounts/{name}/ban      {hours, reason}         .../unban
//	POST   /v1/accounts/{name}/rename   {to}
//	DELETE /v1/accounts/{name}?force=1  the account and everything under it
//	GET    /v1/audit?n=                 the latest account changes
//	GET    /v1/news                     POST {text}, DELETE /v1/news/{id}
//	GET    /v1/events                   specs, upcoming runs, problems
//	PUT    /v1/events/{key}             a spec (checked), DELETE /v1/events/{key}
//	GET    /v1/help?all=1               requests; GET /v1/help/{id}
//	POST   /v1/help/{id}/reply          {text, close}           .../close
//
// Replies are JSON; a failure is {"error": "..."} with a 4xx or 5xx status.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/auth"
	"github.com/princebyyourlogic/gurhal/internal/calendar"
	"github.com/princebyyourlogic/gurhal/internal/store"
)

// Game is what the API asks the running game service (game.Server).
type Game interface {
	Status() any
	Players() any
	Announce(text, scope, kind string, secs int) (int, error)
	Find(name string) (gid uint32, online, ok bool)
	Online(gid uint32) bool
	Kick(gid uint32, reason string) bool
	Mute(gid uint32, minutes int, reason, by string) error
	Unmute(gid uint32) bool
	SeasonPreview(gid uint32, value string) string
	ReloadEvents(ctx context.Context) []string
	HelpReply(ctx context.Context, thread int64, text, by string, closeIt bool) (string, error)
	HelpClose(ctx context.Context, thread int64, by string) (string, error)
}

type Server struct {
	Game  Game
	Auth  *auth.Service
	Store store.Store
	Token string
	Log   *slog.Logger
}

const (
	maxBody   = 64 << 10
	listLimit = 200
)

// LoadToken is env's token, else the one in dir/admin.token (made on first use).
func LoadToken(env, dir string) (string, error) {
	if t := strings.TrimSpace(env); t != "" {
		return t, nil
	}
	path := filepath.Join(dir, "admin.token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	t := hex.EncodeToString(raw)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return t, os.WriteFile(path, []byte(t+"\n"), 0o600)
}

// Serve answers on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	stop := context.AfterFunc(ctx, func() { hs.Close() })
	defer stop()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return ctx.Err()
}

// status is an error with its HTTP status.
type status struct {
	code int
	msg  string
}

func (e status) Error() string { return e.msg }

func bad(format string, a ...any) error {
	return status{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}
func gone(format string, a ...any) error {
	return status{http.StatusNotFound, fmt.Sprintf(format, a...)}
}

type req struct {
	*http.Request
	by string
}

// body decodes the request's JSON into v.
func (r req) body(v any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil && !errors.Is(err, io.EOF) { // no body: the defaults
		return bad("body: %v", err)
	}
	return nil
}

func (r req) intQuery(name string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return n
	}
	return def
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h func(req) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				s.Log.Warn("admin: refused", "path", r.URL.Path, "peer", r.RemoteAddr)
				write(w, http.StatusUnauthorized, map[string]string{"error": "bad token"})
				return
			}
			by := strings.TrimSpace(r.Header.Get("X-PSU-By"))
			if by == "" {
				by = "admin"
			}
			out, err := h(req{r, by})
			if err != nil {
				code := http.StatusInternalServerError
				var st status
				if errors.As(err, &st) {
					code = st.code
				} else if errors.Is(err, store.ErrNotFound) {
					code = http.StatusNotFound
				}
				if code >= 500 {
					s.Log.Error("admin", "path", r.URL.Path, "err", err)
				}
				write(w, code, map[string]string{"error": err.Error()})
				return
			}
			if r.Method != http.MethodGet {
				s.Log.Info("admin", "by", by, "method", r.Method, "path", r.URL.Path)
			}
			write(w, http.StatusOK, out)
		})
	}

	route("GET /v1/status", func(req) (any, error) { return s.Game.Status(), nil })
	route("GET /v1/players", func(req) (any, error) { return s.Game.Players(), nil })
	route("POST /v1/announce", s.announce)
	route("POST /v1/players/{name}/kick", s.kick)
	route("POST /v1/players/{name}/mute", s.mute)
	route("POST /v1/players/{name}/unmute", s.unmute)
	route("POST /v1/players/{name}/season", s.season)
	route("GET /v1/season", func(req) (any, error) { return msg(s.Game.SeasonPreview(0, "")), nil })

	route("GET /v1/accounts", s.accounts)
	route("POST /v1/accounts", s.createAccount)
	route("POST /v1/accounts/{name}/reset", s.reset)
	route("POST /v1/accounts/{name}/ban", s.ban)
	route("POST /v1/accounts/{name}/unban", s.unban)
	route("POST /v1/accounts/{name}/rename", s.rename)
	route("DELETE /v1/accounts/{name}", s.deleteAccount)
	route("GET /v1/audit", func(r req) (any, error) { return s.Store.AuditTail(r.Context(), min(r.intQuery("n", 20), listLimit)) })

	route("GET /v1/news", func(r req) (any, error) { return s.Store.NoticeList(r.Context()) })
	route("POST /v1/news", s.addNews)
	route("DELETE /v1/news/{id}", s.deleteNews)
	route("GET /v1/events", s.events)
	route("PUT /v1/events/{key}", s.putEvent)
	route("DELETE /v1/events/{key}", s.deleteEvent)

	route("GET /v1/help", func(r req) (any, error) {
		return s.Store.HelpThreads(r.Context(), r.URL.Query().Get("all") == "", listLimit)
	})
	route("GET /v1/help/{id}", s.helpThread)
	route("POST /v1/help/{id}/reply", s.helpReply)
	route("POST /v1/help/{id}/close", s.helpClose)
	return mux
}

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func msg(text string) map[string]string { return map[string]string{"message": text} }

// --- players -----------------------------------------------------------------------------------------

func (s *Server) player(r req) (uint32, bool, error) {
	name := r.PathValue("name")
	gid, online, ok := s.Game.Find(name)
	if !ok {
		return 0, false, gone("no such player: %s", name)
	}
	return gid, online, nil
}

func (s *Server) announce(r req) (any, error) {
	var in struct {
		Text    string `json:"text"`
		Scope   string `json:"scope"`
		Kind    string `json:"kind"`
		Seconds int    `json:"seconds"`
	}
	if err := r.body(&in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, bad("text is empty")
	}
	n, err := s.Game.Announce(in.Text, in.Scope, in.Kind, in.Seconds)
	if err != nil {
		return nil, bad("%v", err)
	}
	return map[string]int{"sent": n}, nil
}

func (s *Server) kick(r req) (any, error) {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := r.body(&in); err != nil {
		return nil, err
	}
	gid, online, err := s.player(r)
	if err != nil {
		return nil, err
	}
	if !online || !s.Game.Kick(gid, in.Reason) {
		return nil, status{http.StatusConflict, r.PathValue("name") + " is not online"}
	}
	return msg("kicked"), nil
}

func (s *Server) mute(r req) (any, error) {
	in := struct {
		Minutes int    `json:"minutes"`
		Reason  string `json:"reason"`
	}{Minutes: 60}
	if err := r.body(&in); err != nil {
		return nil, err
	}
	gid, _, err := s.player(r)
	if err != nil {
		return nil, err
	}
	return msg("muted"), s.Game.Mute(gid, in.Minutes, in.Reason, r.by)
}

func (s *Server) unmute(r req) (any, error) {
	gid, _, err := s.player(r)
	if err != nil {
		return nil, err
	}
	if !s.Game.Unmute(gid) {
		return msg("was not muted"), nil
	}
	return msg("unmuted"), nil
}

func (s *Server) season(r req) (any, error) {
	var in struct {
		Season string `json:"season"`
	}
	if err := r.body(&in); err != nil {
		return nil, err
	}
	gid, _, err := s.player(r)
	if err != nil {
		return nil, err
	}
	if in.Season == "" {
		return nil, bad("season: a name, none or off")
	}
	return msg(s.Game.SeasonPreview(gid, in.Season)), nil
}

// --- accounts ----------------------------------------------------------------------------------------

func (s *Server) account(r req) (store.Account, error) {
	a, err := s.Store.Account(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		return a, gone("no account %s", r.PathValue("name"))
	}
	return a, err
}

func (s *Server) accounts(r req) (any, error) {
	return s.Store.Accounts(r.Context(), r.URL.Query().Get("q"), min(r.intQuery("limit", 50), listLimit))
}

func (s *Server) createAccount(r req) (any, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := r.body(&in); err != nil {
		return nil, err
	}
	if !auth.ValidName(in.Name) {
		return nil, bad("%q is not a valid user ID", in.Name)
	}
	code := auth.ResetCode()
	if _, err := s.Auth.Create(r.Context(), in.Name, code, true, r.by); errors.Is(err, store.ErrNameTaken) {
		return nil, status{http.StatusConflict, in.Name + " is taken"}
	} else if err != nil {
		return nil, err
	}
	return map[string]string{"account": in.Name, "code": code, "how": resetHow}, nil
}

const resetHow = "log in with the code alone, or the code followed at once by a new password (4+ characters)"

func (s *Server) reset(r req) (any, error) {
	a, err := s.account(r)
	if err != nil {
		return nil, err
	}
	code := auth.ResetCode()
	salt, hash := auth.NewPassword(code)
	if err := s.Store.SetPassword(r.Context(), a.GID, salt, hash, true); err != nil {
		return nil, err
	}
	s.Store.Audit(r.Context(), "reset", a.GID, a.Username, r.by)
	return map[string]string{"account": a.Username, "code": code, "how": resetHow}, nil
}

func (s *Server) ban(r req) (any, error) {
	in := struct {
		Hours  float64 `json:"hours"`
		Reason string  `json:"reason"`
	}{Hours: 24}
	if err := r.body(&in); err != nil {
		return nil, err
	}
	a, err := s.account(r)
	if err != nil {
		return nil, err
	}
	if in.Hours < 0 {
		return nil, bad("hours: 0 (10 years) or more")
	}
	until := time.Now().Add(time.Duration(in.Hours * float64(time.Hour)))
	if in.Hours == 0 {
		until = time.Now().AddDate(10, 0, 0)
	}
	if err := s.Store.SetBan(r.Context(), a.GID, store.Ban{Until: until, Reason: in.Reason, By: r.by}); err != nil {
		return nil, err
	}
	s.Store.Audit(r.Context(), "ban", a.GID, a.Username, r.by)
	kicked := s.Game.Kick(a.GID, cmpStr(in.Reason, "banned"))
	return map[string]any{"account": a.Username, "until": until.UTC().Format(time.RFC3339), "kicked": kicked}, nil
}

func (s *Server) unban(r req) (any, error) {
	a, err := s.account(r)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetBan(r.Context(), a.GID, store.Ban{}); err != nil {
		return nil, err
	}
	s.Store.Audit(r.Context(), "unban", a.GID, a.Username, r.by)
	return msg("unbanned " + a.Username), nil
}

func (s *Server) rename(r req) (any, error) {
	var in struct {
		To string `json:"to"`
	}
	if err := r.body(&in); err != nil {
		return nil, err
	}
	a, err := s.account(r)
	if err != nil {
		return nil, err
	}
	if !auth.ValidName(in.To) {
		return nil, bad("%q is not a valid user ID", in.To)
	}
	if err := s.Store.RenameAccount(r.Context(), a.GID, in.To); errors.Is(err, store.ErrNameTaken) {
		return nil, status{http.StatusConflict, in.To + " is taken"}
	} else if err != nil {
		return nil, err
	}
	s.Store.Audit(r.Context(), "rename "+a.Username, a.GID, in.To, r.by)
	return msg("renamed " + a.Username + " to " + in.To), nil
}

func (s *Server) deleteAccount(r req) (any, error) {
	a, err := s.account(r)
	if err != nil {
		return nil, err
	}
	if s.Game.Online(a.GID) {
		if r.URL.Query().Get("force") == "" {
			return nil, status{http.StatusConflict, a.Username + " is online (force to kick and delete)"}
		}
		s.Game.Kick(a.GID, "account deleted")
	}
	if err := s.Store.DeleteAccount(r.Context(), a.GID); err != nil {
		return nil, err
	}
	s.Store.Audit(r.Context(), "delete", a.GID, a.Username, r.by)
	return msg("deleted " + a.Username), nil
}

func cmpStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// --- news and events ---------------------------------------------------------------------------------

func (s *Server) addNews(r req) (any, error) {
	var in struct {
		Text string `json:"text"`
	}
	if err := r.body(&in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, bad("text is empty")
	}
	return msg("added"), s.Store.AddNotice(r.Context(), strings.TrimSpace(in.Text), r.by)
}

func (s *Server) deleteNews(r req) (any, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, bad("id: a number")
	}
	if ok, err := s.Store.DeleteNotice(r.Context(), id); err != nil || !ok {
		return nil, cmpErr(err, gone("no notice %d", id))
	}
	return msg("deleted"), nil
}

func cmpErr(err, def error) error {
	if err != nil {
		return err
	}
	return def
}

func (s *Server) events(r req) (any, error) {
	rows, err := s.Store.Events(r.Context())
	if err != nil {
		return nil, err
	}
	specs, raw := map[string][]byte{}, []json.RawMessage{}
	for _, e := range rows {
		specs[e.Key] = e.Spec
		raw = append(raw, e.Spec)
	}
	c := calendar.New(specs)
	return map[string]any{"events": raw, "upcoming": c.BoardRows(time.Now()), "problems": c.Problems}, nil
}

func (s *Server) putEvent(r req) (any, error) {
	spec, err := readAll(r)
	if err != nil {
		return nil, err
	}
	e, err := calendar.Parse(spec)
	if err != nil {
		return nil, bad("event: %v", err)
	}
	if e.Key != r.PathValue("key") {
		return nil, bad("the spec's key %q is not %q", e.Key, r.PathValue("key"))
	}
	if err := s.Store.SetEvent(r.Context(), e.Key, spec); err != nil {
		return nil, err
	}
	s.Game.ReloadEvents(r.Context())
	return msg("saved " + e.Key), nil
}

func readAll(r req) ([]byte, error) {
	var v json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody)).Decode(&v); err != nil {
		return nil, bad("body: %v", err)
	}
	return v, nil
}

func (s *Server) deleteEvent(r req) (any, error) {
	if err := s.Store.SetEvent(r.Context(), r.PathValue("key"), nil); err != nil {
		return nil, err
	}
	s.Game.ReloadEvents(r.Context())
	return msg("deleted " + r.PathValue("key")), nil
}

// --- help desk ---------------------------------------------------------------------------------------

func threadID(r req) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, bad("id: a number")
	}
	return id, nil
}

func (s *Server) helpThread(r req) (any, error) {
	id, err := threadID(r)
	if err != nil {
		return nil, err
	}
	t, msgs, err := s.Store.HelpThread(r.Context(), id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"thread": t, "messages": msgs}, nil
}

func (s *Server) helpReply(r req) (any, error) {
	var in struct {
		Text  string `json:"text"`
		Close bool   `json:"close"`
	}
	if err := r.body(&in); err != nil {
		return nil, err
	}
	id, err := threadID(r)
	if err != nil {
		return nil, err
	}
	line, err := s.Game.HelpReply(r.Context(), id, in.Text, r.by, in.Close)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, bad("%v", err) // an empty answer
	}
	if err != nil {
		return nil, gone("no request %d", id)
	}
	return msg(line), nil
}

func (s *Server) helpClose(r req) (any, error) {
	id, err := threadID(r)
	if err != nil {
		return nil, err
	}
	line, err := s.Game.HelpClose(r.Context(), id, r.by)
	if err != nil {
		return nil, gone("no request %d", id)
	}
	return msg(line), nil
}

// WriteStatus replaces path with the game's status every interval until ctx ends, for
// monitoring.
func WriteStatus(ctx context.Context, path string, every time.Duration, g Game, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		b, err := json.Marshal(g.Status())
		if err == nil {
			if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				if err = os.WriteFile(path+".tmp", b, 0o644); err == nil {
					err = os.Rename(path+".tmp", path)
				}
			}
		}
		if err != nil {
			log.Error("status.json", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
