// SPDX-License-Identifier: GPL-3.0-or-later

// Package netx runs the TCP services: an accept loop with per-address limits
// (Limiter), and Conn, which gives every client its own writer goroutine behind a bounded
// queue so that one slow client never holds up the goroutine sending to it.
package netx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"time"
)

// handshakeTimeout bounds an SSL handshake (the PS2 is slow, but not this slow).
const handshakeTimeout = 20 * time.Second

type Server struct {
	Log    *slog.Logger
	Idle   time.Duration // close a client silent this long; 0 = never
	Limit  *Limiter      // per-address limits, shared across listeners; nil = none
	Queue  int           // unsent writes per client; 0 = DefaultQueue
	Handle func(ctx context.Context, c *Conn)
}

// Serve accepts connections until ctx ends. Each runs Handle in its own goroutine;
// when Handle returns, queued writes are flushed and the connection is closed.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			s.Log.Warn("accept", "err", err)
			continue
		}
		go s.handle(ctx, nc)
	}
}

func (s *Server) handle(ctx context.Context, nc net.Conn) {
	peer := nc.RemoteAddr().String()
	addr := peerAddr(nc)
	if why := s.Limit.Admit(addr); why != admitted {
		s.Log.Warn(string(why), "peer", peer)
		nc.Close()
		return
	}
	defer s.Limit.Release(addr)
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
	}
	log := s.Log.With("peer", peer)
	if hs, ok := nc.(interface {
		HandshakeContext(context.Context) error
	}); ok {
		hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
		err := hs.HandshakeContext(hctx)
		cancel()
		if err != nil {
			log.Info("ssl handshake failed", "err", err)
			nc.Close()
			return
		}
	}
	c := newConn(nc, s.Idle, s.Queue, log)
	defer c.finish()
	stop := context.AfterFunc(ctx, c.Close)
	defer stop()
	s.Handle(ctx, c)
}

func peerAddr(nc net.Conn) netip.Addr {
	ap, err := netip.ParseAddrPort(nc.RemoteAddr().String())
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}
