// SPDX-License-Identifier: GPL-3.0-or-later

package netx

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/wire"
)

const (
	DefaultQueue = 1024

	sendWait     = 5 * time.Second  // how long Send waits for queue room before giving up on the client
	writeTimeout = 30 * time.Second // one socket write
	ackWait      = 5 * time.Second  // a fragment's 0x0B05
	drainWait    = 2 * time.Second  // flushing the queue when the handler returns
	pacePiece    = 1024             // SendPaced writes this much at a time,
	paceGap      = 5 * time.Millisecond
	batchMax     = 64 << 10 // plain writes already queued go out together, up to this much
)

// ErrSlow means the client stopped reading and was disconnected.
var ErrSlow = errors.New("netx: client not reading")

type item struct {
	data  []byte
	paced bool
	close bool
	after func() // run by the writer when it gets here
}

// Conn is one client. The handler goroutine reads from R; everything written goes
// through the queue to the connection's writer goroutine.
type Conn struct {
	R   *bufio.Reader
	Log *slog.Logger

	nc    net.Conn
	idle  *atomic.Int64 // read timeout in nanoseconds; 0 = none
	out   chan item
	acks  chan struct{}
	done  chan struct{}
	wrote chan struct{} // closed when the writer exits
	once  sync.Once
}

func newConn(nc net.Conn, idle time.Duration, queue int, log *slog.Logger) *Conn {
	if queue <= 0 {
		queue = DefaultQueue
	}
	c := &Conn{Log: log, nc: nc, idle: new(atomic.Int64), out: make(chan item, queue),
		acks: make(chan struct{}, 1), done: make(chan struct{}), wrote: make(chan struct{})}
	c.idle.Store(int64(idle))
	c.R = bufio.NewReader(idleReader{nc, c.idle})
	go c.writer()
	return c
}

// Send queues a reply. If the queue stays full for sendWait the client is
// disconnected and ErrSlow returned.
func (c *Conn) Send(b []byte) error { return c.enqueue(item{data: b}) }

// SendPaced queues a framed packet that goes out as wire fragments, one at a time,
// each after the client acknowledged the last (see Acked), in small pieces: PCSX2's
// sockets backend copes badly with large bursts.
func (c *Conn) SendPaced(pkt []byte) error { return c.enqueue(item{data: pkt, paced: true}) }

// After runs f on the writer goroutine once everything queued before it is written
// (f must not block). The caller goes on reading meanwhile: paced writes wait for acks
// that only its reads deliver.
func (c *Conn) After(f func()) error { return c.enqueue(item{after: f}) }

// Relay queues something another player caused (a move, a chat line) without waiting.
// A client whose queue is full is disconnected: dropping state would desync it.
func (c *Conn) Relay(b []byte) bool {
	select {
	case c.out <- item{data: b}:
		return true
	case <-c.done:
		return false
	default:
		c.Log.Warn("relay queue full, disconnecting")
		c.Close()
		return false
	}
}

// Acked tells the writer the client acknowledged a fragment (0x0B05).
func (c *Conn) Acked() {
	select {
	case c.acks <- struct{}{}:
	default:
	}
}

// Close drops the connection at once, discarding queued writes.
func (c *Conn) Close() {
	c.once.Do(func() {
		close(c.done)
		c.nc.Close()
	})
}

// Done is closed when the connection is.
func (c *Conn) Done() <-chan struct{} { return c.done }

// SetIdle changes how long the client may stay silent (0: forever, for a client that
// is kept alive by the server's own writes).
func (c *Conn) SetIdle(d time.Duration) {
	c.idle.Store(int64(d))
	if d == 0 {
		c.nc.SetReadDeadline(time.Time{})
	}
}

// Addr is the client's address.
func (c *Conn) Addr() netip.Addr { return peerAddr(c.nc) }

func (c *Conn) enqueue(it item) error {
	select {
	case c.out <- it:
		return nil
	case <-c.done:
		return net.ErrClosed
	default:
	}
	t := time.NewTimer(sendWait)
	defer t.Stop()
	select {
	case c.out <- it:
		return nil
	case <-c.done:
		return net.ErrClosed
	case <-t.C:
		c.Log.Warn("client not reading, disconnecting")
		c.Close()
		return ErrSlow
	}
}

// finish flushes what is queued (up to drainWait) and closes the connection.
func (c *Conn) finish() {
	t := time.NewTimer(drainWait)
	defer t.Stop()
	select {
	case c.out <- item{close: true}:
		select {
		case <-c.wrote:
		case <-t.C:
		}
	case <-c.done:
	case <-t.C:
	}
	c.Close()
}

func (c *Conn) writer() {
	defer close(c.wrote)
	var batch []byte
	var next *item // taken from the queue while batching, handled next
	for {
		var it item
		if next != nil {
			it, next = *next, nil
		} else {
			select {
			case it = <-c.out:
			case <-c.done:
				return
			}
		}
		if it.close {
			return
		}
		if it.after != nil {
			it.after()
			continue
		}
		var err error
		if it.paced {
			err = c.writePaced(it.data)
		} else {
			// one write for this and the plain writes queued behind it (a crowded zone
			// relays many small packets to everyone)
			batch = append(batch[:0], it.data...)
		more:
			for len(batch) < batchMax {
				select {
				case n := <-c.out:
					if n.paced || n.close || n.after != nil {
						next = &n
						break more
					}
					batch = append(batch, n.data...)
				default:
					break more
				}
			}
			err = c.write(batch)
		}
		if err != nil {
			c.Log.Debug("write failed", "err", err)
			c.Close()
			return
		}
	}
}

func (c *Conn) write(b []byte) error {
	c.nc.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := c.nc.Write(b)
	return err
}

func (c *Conn) writePaced(pkt []byte) error {
	select { // an ack that arrived after its fragment timed out must not release the next one
	case <-c.acks:
	default:
	}
	frags := wire.Fragments(pkt)
	for i, f := range frags {
		for o := 0; o < len(f); o += pacePiece {
			if o > 0 {
				if err := c.sleep(paceGap); err != nil {
					return err
				}
			}
			if err := c.write(f[o:min(o+pacePiece, len(f))]); err != nil {
				return err
			}
		}
		if len(frags) == 1 {
			break
		}
		t := time.NewTimer(ackWait)
		select {
		case <-c.acks:
		case <-t.C:
			c.Log.Warn("fragment not acknowledged", "fragment", i+1, "of", len(frags))
		case <-c.done:
			t.Stop()
			return net.ErrClosed
		}
		t.Stop()
	}
	return nil
}

func (c *Conn) sleep(d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-c.done:
		return net.ErrClosed
	}
}

// idleReader renews the read deadline before every read, so a client silent for idle
// is dropped.
type idleReader struct {
	nc   net.Conn
	idle *atomic.Int64
}

func (r idleReader) Read(p []byte) (int, error) {
	if d := time.Duration(r.idle.Load()); d > 0 {
		r.nc.SetReadDeadline(time.Now().Add(d))
	}
	return r.nc.Read(p)
}
