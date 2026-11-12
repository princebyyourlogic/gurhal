// SPDX-License-Identifier: GPL-3.0-or-later

package netx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/wire"
)

func serve(t *testing.T, s *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if s.Log == nil {
		s.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.Serve(ctx, ln)
	return ln.Addr().String()
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return c
}

func TestQueuedWritesFlushWhenHandlerReturns(t *testing.T) {
	addr := serve(t, &Server{Handle: func(_ context.Context, c *Conn) {
		for _, b := range []string{"one ", "two ", "three"} {
			c.Send([]byte(b))
		}
	}})
	got, err := io.ReadAll(dial(t, addr))
	if err != nil || string(got) != "one two three" {
		t.Fatalf("read %q, %v", got, err)
	}
}

func TestAfterRunsOncePacedWritesAreOut(t *testing.T) {
	pkt := bytes.Repeat([]byte{1}, 10*pacePiece) // one fragment: no acks to wait for
	ran := make(chan time.Duration, 1)
	addr := serve(t, &Server{Handle: func(_ context.Context, c *Conn) {
		start := time.Now()
		c.SendPaced(pkt)
		c.After(func() { ran <- time.Since(start) })
	}})
	if _, err := io.ReadAll(dial(t, addr)); err != nil {
		t.Fatal(err)
	}
	if d := <-ran; d < 9*paceGap {
		t.Fatalf("After ran after %v, before the paced write could finish", d)
	}
}

func TestRelayToStalledClientDisconnects(t *testing.T) {
	done := make(chan bool, 1)
	addr := serve(t, &Server{Queue: 2, Handle: func(_ context.Context, c *Conn) {
		big := make([]byte, 8<<20) // the writer blocks on this; the client never reads
		ok := true
		for i := 0; i < 8 && ok; i++ {
			ok = c.Relay(big)
		}
		select {
		case <-c.Done():
			done <- !ok
		case <-time.After(time.Second):
			done <- false
		}
	}})
	dial(t, addr)
	if !<-done {
		t.Fatal("a client whose queue filled up was not disconnected")
	}
}

func TestPacedFragmentsWaitForAck(t *testing.T) {
	pkt := wire.Encode(0x0D03, bytes.Repeat([]byte{7}, wire.FragmentLimit+100))
	addr := serve(t, &Server{Handle: func(_ context.Context, c *Conn) {
		c.SendPaced(pkt)
		for {
			p, err := wire.Read(c.R, 0x10000)
			if err != nil {
				return
			}
			if p.IsFragmentAck() {
				c.Acked()
			}
		}
	}})
	c := dial(t, addr)
	first := make([]byte, 16+wire.FragmentLimit)
	if _, err := io.ReadFull(c, first); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("second fragment came before the ack (n=%d, err=%v)", n, err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write(wire.EncodeOn(wire.ChanClient, wire.CmdFragmentAck, nil))
	second := make([]byte, 16+len(pkt)-wire.FragmentLimit)
	if _, err := io.ReadFull(c, second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(first[16:], second[16:]...), pkt) {
		t.Fatal("fragments do not reassemble to the packet")
	}
}

func TestMaxPerIP(t *testing.T) {
	addr := serve(t, &Server{Limit: &Limiter{MaxOpen: 1}, Handle: func(ctx context.Context, c *Conn) {
		c.Send([]byte("hi"))
		<-ctx.Done()
	}})
	first := dial(t, addr)
	if _, err := io.ReadFull(first, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	if n, err := dial(t, addr).Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("second connection from the same address was served (n=%d, err=%v)", n, err)
	}
}

func TestLimiterRate(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &Limiter{MaxOpen: 10, PerMinute: 3, now: func() time.Time { return now }}
	a := netip.MustParseAddr("192.0.2.1")
	for i := range 3 {
		if why := l.Admit(a); why != admitted {
			t.Fatalf("connection %d refused: %s", i, why)
		}
		l.Release(a)
	}
	if why := l.Admit(a); why != TooOften {
		t.Fatalf("a fourth connection within the minute: %q", why)
	}
	if why := l.Admit(netip.MustParseAddr("192.0.2.2")); why != admitted {
		t.Fatalf("another address: %q", why)
	}
	now = now.Add(20 * time.Second) // one token back
	if why := l.Admit(a); why != admitted {
		t.Fatalf("after 20 s: %q", why)
	}
	if why := l.Admit(a); why != TooOften {
		t.Fatalf("only one token came back: %q", why)
	}
	l.Release(a)
	full := &Limiter{MaxTotal: 1}
	if full.Admit(a) != admitted || full.Admit(netip.MustParseAddr("192.0.2.3")) != Full {
		t.Fatal("MaxTotal does not cap everyone together")
	}
	full.Release(a)
	if full.Admit(netip.MustParseAddr("192.0.2.3")) != admitted {
		t.Fatal("a released slot is not free again")
	}
	now = now.Add(time.Hour)
	l.Release(netip.MustParseAddr("192.0.2.2"))
	if len(l.peers) != 0 {
		t.Fatalf("idle addresses kept: %d", len(l.peers))
	}
}

func TestIdleClientIsDropped(t *testing.T) {
	addr := serve(t, &Server{Idle: 100 * time.Millisecond, Handle: func(_ context.Context, c *Conn) {
		c.R.ReadByte()
	}})
	start := time.Now()
	if _, err := io.ReadAll(dial(t, addr)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("idle client not dropped")
	}
}
