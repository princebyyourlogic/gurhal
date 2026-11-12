// SPDX-License-Identifier: GPL-3.0-or-later

// Command psud is a Phantasy Star Universe server for the US PS2 discs (PSU and
// Ambition of the Illuminus): the patch, login and game servers in one process.
//
// DNS for the discs' segaonline.jp names and DNAS are handled elsewhere.
//
//	psud          serve
//	psud health   exit 0 if the local patch server answers (the container healthcheck)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/admin"
	"github.com/princebyyourlogic/gurhal/internal/auth"
	"github.com/princebyyourlogic/gurhal/internal/calendar"
	"github.com/princebyyourlogic/gurhal/internal/disc"
	"github.com/princebyyourlogic/gurhal/internal/game"
	"github.com/princebyyourlogic/gurhal/internal/gamedata"
	"github.com/princebyyourlogic/gurhal/internal/login"
	"github.com/princebyyourlogic/gurhal/internal/netx"
	"github.com/princebyyourlogic/gurhal/internal/patch"
	"github.com/princebyyourlogic/gurhal/internal/rules"
	"github.com/princebyyourlogic/gurhal/internal/ssl3"
	"github.com/princebyyourlogic/gurhal/internal/store"
)

type config struct {
	Bind         string
	GameAddr     netip.Addr
	Discs        string // comma list; empty = every disc the data was built for
	DataDir      string
	StateDir     string
	DatabaseURL  string
	AutoRegister bool
	MOTD         string
	Universes    []game.Universe
	PlainPorts   bool // also serve login and game in plain TCP on loopback (tests and bots)
	Idle         time.Duration
	MaxPerIP     int
	ConnPerMin   int
	RegsPerHour  int
	MaxConns     int
	AdminAddr    string // the admin API ("off": none); always loopback
	AdminToken   string
	LogLevel     slog.Level
	Admins       []string // PSU_ADMINS: account names allowed the GM commands
	Season       string   // PSU_SEASON: none, auto or a season name
	SeasonDrop   int
	TickerSecs   int
	BoardFile    string
	BoardSecs    int
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	c := config{Bind: env("PSU_BIND", "0.0.0.0"), Discs: env("PSU_DISCS", ""), DataDir: env("PSU_DATA_DIR", "data"),
		StateDir: env("PSU_STATE_DIR", "state"), DatabaseURL: os.Getenv("PSU_DATABASE_URL"),
		AutoRegister: env("PSU_AUTOREGISTER", "1") == "1",
		MOTD:         env("PSU_MOTD", "Welcome to the Phantasy Star Universe server."),
		PlainPorts:   env("PSU_PLAIN_PORTS", "0") == "1"}
	var err error
	if c.Universes, err = game.ParseUniverses(env("PSU_UNIVERSES", "1:Universe 1,2:Universe 2,3:Universe 3")); err != nil {
		return c, fmt.Errorf("PSU_UNIVERSES: %w", err)
	}
	if c.GameAddr, err = netip.ParseAddr(os.Getenv("PSU_GAME_ADDR")); err != nil || !c.GameAddr.Is4() {
		return c, errors.New("PSU_GAME_ADDR must be the IPv4 address players reach this server on")
	}
	idle, err := strconv.Atoi(env("PSU_IDLE_SECS", "600"))
	if err != nil || idle < 0 {
		return c, errors.New("PSU_IDLE_SECS must be a number of seconds")
	}
	c.Idle = time.Duration(idle) * time.Second
	if c.MaxPerIP, err = strconv.Atoi(env("PSU_MAX_PER_IP", "16")); err != nil {
		return c, fmt.Errorf("PSU_MAX_PER_IP: %w", err)
	}
	if c.AdminAddr = env("PSU_ADMIN_ADDR", "127.0.0.1:22190"); c.AdminAddr != "off" {
		host, _, err := net.SplitHostPort(c.AdminAddr)
		if ip, perr := netip.ParseAddr(host); err != nil || perr != nil || !ip.IsLoopback() {
			return c, fmt.Errorf("PSU_ADMIN_ADDR %q: a loopback host:port, or off", c.AdminAddr)
		}
	}
	c.AdminToken = os.Getenv("PSU_ADMIN_TOKEN")
	if c.ConnPerMin, err = strconv.Atoi(env("PSU_CONN_PER_MIN", "60")); err != nil || c.ConnPerMin < 0 {
		return c, fmt.Errorf("PSU_CONN_PER_MIN: a number of at least 0")
	}
	if c.MaxConns, err = strconv.Atoi(env("PSU_MAX_CONNS", "2000")); err != nil || c.MaxConns < 0 {
		return c, fmt.Errorf("PSU_MAX_CONNS: a number of at least 0")
	}
	if c.RegsPerHour, err = strconv.Atoi(env("PSU_REGISTER_PER_HOUR", "3")); err != nil || c.RegsPerHour < 0 {
		return c, fmt.Errorf("PSU_REGISTER_PER_HOUR: a number of at least 0")
	}
	for _, a := range strings.Split(os.Getenv("PSU_ADMINS"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			c.Admins = append(c.Admins, a)
		}
	}
	if c.Season = env("PSU_SEASON", "none"); calendar.Mode(c.Season) == "" {
		return c, fmt.Errorf("PSU_SEASON %q: none, auto or one of %s", c.Season, strings.Join(calendar.Seasons, ", "))
	}
	c.BoardFile = env("PSU_BOARD_FILE", filepath.Join(c.StateDir, "board", "board.json"))
	for _, n := range []struct {
		key, def string
		to       *int
	}{{"PSU_SEASON_DROP", "40", &c.SeasonDrop}, {"PSU_NEWS_TICKER_SECS", "1800", &c.TickerSecs}, {"PSU_BOARD_SECS", "60", &c.BoardSecs}} {
		if *n.to, err = strconv.Atoi(env(n.key, n.def)); err != nil || *n.to < 0 {
			return c, fmt.Errorf("%s: a number of at least 0", n.key)
		}
	}
	if err := c.LogLevel.UnmarshalText([]byte(env("PSU_LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("PSU_LOG_LEVEL: %w", err)
	}
	return c, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		if err := health(); err != nil {
			fmt.Fprintln(os.Stderr, "psud health:", err)
			os.Exit(1)
		}
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "psud:", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err := run(cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

var _ admin.Game = (*game.Server)(nil)

// service is one listening port.
type service struct {
	name   string
	bind   string
	port   int
	ssl    bool
	handle func(context.Context, *netx.Conn)
	limit  *netx.Limiter // nil: the loopback test ports
	idle   time.Duration // until the client is signed in (login, game: then their own)
}

// How long a client that has not signed in may stay silent. Patch and the game's key
// exchange run by themselves; at login the player types the ID and password.
const (
	preAuthIdle  = 30 * time.Second
	loginPreAuth = 2 * time.Minute
)

// plainPort is where a service's plain-TCP test listener goes (PSU_PLAIN_PORTS).
func plainPort(port int) int { return port + 10000 }

func run(cfg config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var want []string
	if cfg.Discs != "" {
		want = strings.Split(cfg.Discs, ",")
	}
	data, err := gamedata.Load(cfg.DataDir, want)
	if err != nil {
		return err
	}
	if want == nil {
		want = data.Discs()
	}
	discs, err := disc.Parse(strings.Join(want, ","))
	if err != nil {
		return fmt.Errorf("PSU_DISCS: %w", err)
	}
	ruleSet, err := rules.Load(data)
	if err != nil {
		return err
	}
	nbls, tables := data.Counts()
	log.Info("game data", "dir", cfg.DataDir, "archives", nbls, "tables", tables, "built", data.Built.BuiltAt)

	var st store.Store = store.NewMemory()
	if cfg.DatabaseURL != "" {
		pg, err := store.OpenPostgres(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		st = pg
	} else {
		log.Warn("PSU_DATABASE_URL not set: accounts and characters are kept in memory only")
	}
	defer st.Close()

	cert, err := ssl3.LoadOrCreate(filepath.Join(cfg.StateDir, "ssl3.pem"))
	if err != nil {
		return err
	}
	signIn := &auth.Service{Store: st, AutoRegister: cfg.AutoRegister, Reserved: cfg.Admins, RegsPerHour: cfg.RegsPerHour,
		Log: log.With("svc", "auth")}
	for _, name := range cfg.Admins { // auto-register refuses them: an operator makes them
		if _, err := st.Account(ctx, name); errors.Is(err, store.ErrNotFound) {
			log.Warn("PSU_ADMINS names an account that does not exist yet: make it with psuctl create", "account", name)
		}
	}
	keys := auth.NewKeys()
	patchSvc := &patch.Service{Log: log.With("svc", "patch")}
	gameSvc := &game.Server{Rules: ruleSet, Data: data, Store: st, Keys: keys, Universes: cfg.Universes,
		Log: log.With("svc", "game"), Admins: cfg.Admins, Season: cfg.Season, SeasonDrop: cfg.SeasonDrop,
		TickerSecs: cfg.TickerSecs, BoardFile: cfg.BoardFile, BoardSecs: cfg.BoardSecs}
	saved := make(chan struct{})
	go func() { gameSvc.Run(ctx); close(saved) }()
	defer func() { <-saved }() // the last character saves are written before exit

	limit := &netx.Limiter{MaxOpen: cfg.MaxPerIP, PerMinute: cfg.ConnPerMin, MaxTotal: cfg.MaxConns} // one budget across every public port
	var services []service
	add := func(name string, port int, handle func(context.Context, *netx.Conn), idle time.Duration) {
		services = append(services, service{name, cfg.Bind, port, true, handle, limit, idle})
		if cfg.PlainPorts {
			services = append(services, service{name + "-plain", "127.0.0.1", plainPort(port), false, handle, nil, idle})
		}
	}
	for _, d := range discs {
		loginSvc := &login.Service{Disc: d.Key, Auth: signIn, Keys: keys, Store: st, MOTD: cfg.MOTD,
			GameAddr: cfg.GameAddr, GamePort: disc.GamePort, Idle: cfg.Idle}
		services = append(services, service{d.Key + "-patch", cfg.Bind, d.PatchPort, false, patchSvc.Handle, limit, preAuthIdle})
		add(d.Key+"-login", d.LoginPort, loginSvc.Handle, loginPreAuth)
	}
	add("game", disc.GamePort, gameSvc.Handle, preAuthIdle)

	errs := make(chan error, len(services)+1)
	go admin.WriteStatus(ctx, filepath.Join(cfg.StateDir, "status.json"), 30*time.Second, gameSvc, log.With("svc", "status"))
	if cfg.AdminAddr != "off" {
		token, err := admin.LoadToken(cfg.AdminToken, cfg.StateDir)
		if err != nil {
			return fmt.Errorf("admin token: %w", err)
		}
		ln, err := net.Listen("tcp", cfg.AdminAddr)
		if err != nil {
			return err
		}
		api := &admin.Server{Game: gameSvc, Auth: signIn, Store: st, Token: token, Log: log.With("svc", "admin")}
		go func() { errs <- api.Serve(ctx, ln) }()
		log.Info("listening", "svc", "admin", "addr", ln.Addr().String())
	}
	for _, s := range services {
		ln, err := net.Listen("tcp4", net.JoinHostPort(s.bind, strconv.Itoa(s.port)))
		if err != nil {
			return err
		}
		if s.ssl {
			ln = ssl3.Listen(ln, cert)
		}
		srv := &netx.Server{Log: log.With("svc", s.name), Idle: s.idle, Limit: s.limit, Handle: s.handle}
		go func() { errs <- srv.Serve(ctx, ln) }()
		log.Info("listening", "svc", s.name, "addr", ln.Addr().String(), "ssl3", s.ssl)
	}
	log.Info("psud ready", "game_addr", cfg.GameAddr.String(), "postgres", cfg.DatabaseURL != "",
		"autoregister", cfg.AutoRegister)
	err = <-errs
	stop()
	return err
}

// health reads the hello of the first local patch server that answers.
func health() error {
	var err error
	for _, d := range disc.All {
		if err = patchHello(d.PatchPort); err == nil {
			return nil
		}
	}
	return err
}

func patchHello(port int) error {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	h := make([]byte, 8)
	if _, err := io.ReadFull(c, h); err != nil {
		return err
	}
	if h[4] != 0x01 {
		return fmt.Errorf("unexpected patch hello % x", h)
	}
	return nil
}
