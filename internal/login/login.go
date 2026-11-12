// SPDX-License-Identifier: GPL-3.0-or-later

// Package login is the login server (12110 PSU, 12140 AotI; SSLv3 is terminated in
// front of it). Protocol from EGS's documentation; the PS2 client speaks the PC login:
//
//	S>C 0x0202  hello: target ffff, a temporary GID
//	C>S 0x080E  client version (language, entrance, platform 0 = PS2, version)
//	C>S 0x0226 / 0x023F  notice page request   -> 0x0225 page
//	C>S 0x0219  name[24], password[24]         -> 0x0223 GID + key, or an error text
//	C>S 0x0217  game server request            -> 0x0216 address and port; close
//
// The client then connects to the game server and sends 0x020D with the GID and key.
package login

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/auth"
	"github.com/princebyyourlogic/gurhal/internal/calendar"
	"github.com/princebyyourlogic/gurhal/internal/netx"
	"github.com/princebyyourlogic/gurhal/internal/store"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

const maxPacket = 0x1000

// Service serves the login port of one disc.
type Service struct {
	Disc     string // the disc key travels with the login key to the game server
	Auth     *auth.Service
	Keys     *auth.Keys
	Store    store.Store // notices for the login box
	MOTD     string      // shown when there are no notices
	GameAddr netip.Addr  // IPv4 the client is sent to
	GamePort uint16
	Idle     time.Duration // once signed in, how long the client may stay silent (before: the listener's)
}

func (s *Service) Handle(ctx context.Context, c *netx.Conn) {
	gid := tempGID()
	if c.Send(wire.Hello(gid)) != nil {
		return
	}
	for {
		p, err := wire.Read(c.R, maxPacket)
		if err != nil {
			return
		}
		b := p.Body
		switch {
		case p.Cmd == cmdLogin && len(b) >= 0x54: // the body holds a password: never log it
			gid = s.login(ctx, c, gid, wire.Latin1(b[0x24:0x3C]), wire.Latin1(b[0x3C:0x54]))
		case p.Cmd == cmdVersion && len(b) >= 0x30:
			c.Log.Debug("client version", "language", b[0x25], "entrance", b[0x27], "platform", b[0x28],
				"version", fmt.Sprintf("%#08x", binary.LittleEndian.Uint32(b[0x2C:])))
		case (p.Cmd == cmdMOTDReq || p.Cmd == cmdMOTDReq2) && len(b) >= 0x28:
			c.Send(motdPage(s.motd(ctx, c), int(b[0x24])))
		case p.Cmd == cmdGameReq:
			c.Send(gameServer(gid, s.GameAddr, s.GamePort))
			return
		case p.Cmd != cmdLogin:
			c.Log.Debug("login: command ignored", "cmd", fmt.Sprintf("%#04x", p.Cmd), "size", len(b))
		}
	}
}

// login answers 0x0219 and returns the GID the session goes on with.
func (s *Service) login(ctx context.Context, c *netx.Conn, gid uint32, name, password string) uint32 {
	acct, err := s.Auth.SignIn(ctx, name, password, c.Addr())
	var banned auth.BannedError
	switch {
	case err == nil:
		c.Log.Info("login", "user", name, "gid", fmt.Sprintf("%#08x", acct), "disc", s.Disc)
		if s.Idle > 0 {
			c.SetIdle(s.Idle)
		}
		c.Send(authInfo(acct, s.Keys.Issue(acct, s.Disc)))
		return acct
	case errors.Is(err, auth.ErrBadLogin):
		c.Log.Info("login refused", "user", name, "reason", err)
		c.Send(authError(gid, "Login failed: wrong user ID or password."))
	case errors.Is(err, auth.ErrThrottled):
		c.Log.Warn("login refused", "user", name, "reason", err)
		c.Send(authError(gid, "Too many login attempts. Please wait a few minutes."))
	case errors.As(err, &banned):
		c.Log.Info("login refused", "user", name, "reason", err)
		c.Send(authError(gid, "This account is "+banned.Error()+"."))
	default:
		c.Log.Error("login failed", "user", name, "err", err)
		c.Send(authError(gid, "The server could not check your login. Please try again later."))
	}
	return gid
}

func (s *Service) motd(ctx context.Context, c *netx.Conn) string {
	n, err := s.Store.Notices(ctx)
	if err != nil {
		c.Log.Error("notices", "err", err)
	}
	if events, err := s.Store.Events(ctx); err == nil && len(events) > 0 { // a line per event on or soon
		specs := map[string][]byte{}
		for _, e := range events {
			specs[e.Key] = e.Spec
		}
		n = append(n, calendar.New(specs).Notices(time.Now())...)
	}
	if len(n) == 0 {
		return s.MOTD
	}
	return motdText(n)
}

// tempGID stands in until the client logs in, as EGS does.
func tempGID() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return 0x10000000 | binary.LittleEndian.Uint32(b[:])&0xFFFFFF
}
