// SPDX-License-Identifier: GPL-3.0-or-later

package patch

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/netx"
)

// The bytes prettystellaruniverse's patch.py sends (HELLO, COMPLETE).
const (
	pyHello    = "28000000010000008b9f2dfa00000000000000000000000001000000000000000000000000000000"
	pyComplete = "0800000013000000"
)

func words(ws ...uint32) []byte {
	b := make([]byte, 4*len(ws))
	for i, w := range ws {
		binary.LittleEndian.PutUint32(b[4*i:], w)
	}
	return b
}

func TestPatchFlow(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&netx.Server{Log: log, Handle: (&Service{Log: log}).Handle}).Serve(ctx, ln)

	hellos := map[string][]byte{
		"psu":  packet(cmdHelloPSU, words(0xe44c0915, 0, 0, 1, 1, 0x000100be, 0, 0)),
		"aoti": packet(cmdHelloPC, append(words(0xe44c0915, 0, 0, 0, 0, 0x20bcc000, 0), make([]byte, 16)...)),
	}
	for name, h := range hellos {
		t.Run(name, func(t *testing.T) {
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			got := make([]byte, len(hello))
			if _, err := io.ReadFull(c, got); err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != pyHello {
				t.Fatalf("hello % x", got)
			}
			c.Write(h)
			got = make([]byte, len(complete))
			if _, err := io.ReadFull(c, got); err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != pyComplete {
				t.Fatalf("complete % x", got)
			}
		})
	}
}

func TestHelloWordAt0x18(t *testing.T) {
	if binary.LittleEndian.Uint32(hello[0x18:]) == 0 || len(hello) != 0x28 {
		t.Fatal("AotI only sends its hello when +0x18 is non-zero")
	}
}

func TestReadRefusesOversize(t *testing.T) {
	b := packet(cmdHelloPSU, nil)
	binary.LittleEndian.PutUint32(b, maxPacket+1)
	if _, _, err := read(bytes.NewReader(b)); err == nil {
		t.Fatal("oversized packet accepted")
	}
}
