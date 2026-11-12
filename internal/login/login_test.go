// SPDX-License-Identifier: GPL-3.0-or-later

package login

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/princebyyourlogic/gurhal/internal/auth"
	"github.com/princebyyourlogic/gurhal/internal/netx"
	"github.com/princebyyourlogic/gurhal/internal/store"
	"github.com/princebyyourlogic/gurhal/internal/wire"
)

// What prettystellaruniverse's login.py builds for the same inputs.
var python = map[string]string{
	"hello":       "b000000002020300ffff00000000000000000000000000000000000000000000000000000000000000000000010000100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
	"auth_info":   "3400000002230300000000000000000000000000000000000000000000000f007856341200000000000000007856341201020304",
	"auth_error":  "9400000002230300000000000000000000000000000000000000000000000f000100001000000000000000000000000000000000030000000000000000002a004c006f00670069006e0020006600610069006c00650064003a002000770072006f006e0067002000750073006500720020004900440020006f0072002000700061007300730077006f00720064002e0000000000",
	"game_server": "3400000002160300ffff00000000000000000000000000000000000000000f00785634120000000000000000cb0071071d2f0000",
	"motd0":       "5c00000002250300ffff0000000000000000000000000000000000000000000000000000000000000000000001001600570065006c0063006f006d0065000a005300650063006f006e00640020006c0069006e0065000a0000000000",
	"motd1":       "8400000002250300ffff0000000000000000000000000000000000000000000000000000000000000000000002012a006c0069006e0065002000310035000a006c0069006e0065002000310036000a006c0069006e0065002000310037000a006c0069006e0065002000310038000a006c0069006e0065002000310039000a0000000000",
}

func TestPacketsMatchPython(t *testing.T) {
	twenty := make([]string, 20)
	for i := range twenty {
		twenty[i] = "line " + string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	text := strings.ReplaceAll(strings.Join(twenty, "\n"), "line 0", "line ")
	got := map[string][]byte{
		"hello":       wire.Hello(0x10000001),
		"auth_info":   authInfo(0x12345678, [4]byte{1, 2, 3, 4}),
		"auth_error":  authError(0x10000001, "Login failed: wrong user ID or password."),
		"game_server": gameServer(0x12345678, netip.MustParseAddr("203.0.113.7"), 12061),
		"motd0":       motdPage("Welcome\nSecond line", 0),
		"motd1":       motdPage(text, 1),
	}
	for name, b := range got {
		if hex.EncodeToString(b) != python[name] {
			t.Errorf("%s:\n got %x\nwant %s", name, b, python[name])
		}
	}
	if hex.EncodeToString(motdPage(text, 9)) != python["motd1"] {
		t.Error("a page past the end must give the last page")
	}
}

func TestWrapMatchesTextwrap(t *testing.T) {
	got := wrap("Maintenance on Sunday at 18:00 UTC for about one hour, so plan accordingly please.", 50)
	want := []string{"Maintenance on Sunday at 18:00 UTC for about one", "hour, so plan accordingly please."}
	if !slices.Equal(got, want) {
		t.Fatalf("wrap = %q", got)
	}
	if got := wrap("", 50); !slices.Equal(got, []string{""}) {
		t.Fatalf("empty line = %q", got)
	}
	if got := wrap(strings.Repeat("x", 120), 50); len(got) != 3 || len(got[0]) != 50 || len(got[2]) != 20 {
		t.Fatalf("long word = %q", got)
	}
	if got := motdText([]string{"one", "two"}); got != "one\n\ntwo" {
		t.Fatalf("motdText = %q", got)
	}
}

func loginBody(name, password string) []byte {
	b := make([]byte, 0x54)
	copy(b[0x24:0x3C], name)
	copy(b[0x3C:0x54], password)
	return b
}

func TestLoginFlow(t *testing.T) {
	st := store.NewMemory()
	keys := auth.NewKeys()
	log := slog.New(slog.DiscardHandler)
	svc := &Service{Disc: "psu", Keys: keys, Store: st, MOTD: "Welcome", GameAddr: netip.MustParseAddr("203.0.113.7"),
		GamePort: 12061, Auth: &auth.Service{Store: st, AutoRegister: true, Log: log}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&netx.Server{Log: log, Handle: svc.Handle}).Serve(ctx, ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	read := func(cmd uint16) wire.Packet {
		t.Helper()
		p, err := wire.Read(c, 0x10000)
		if err != nil || p.Cmd != cmd {
			t.Fatalf("want %#04x, got %#04x (%v)", cmd, p.Cmd, err)
		}
		return p
	}
	send := func(cmd uint16, body []byte) { c.Write(wire.EncodeOn(wire.ChanClient, cmd, body)) }

	read(wire.CmdHello)
	motdReq := make([]byte, 0x28)
	send(cmdMOTDReq, motdReq)
	if p := read(cmdMOTD); !strings.Contains(string(p.Body), "W\x00e\x00l\x00c\x00o\x00m\x00e") {
		t.Fatal("MOTD page does not hold the default text")
	}

	send(cmdLogin, loginBody("Ethan", "wrong!"))
	read(cmdAuth) // registers Ethan with "wrong!"
	send(cmdLogin, loginBody("ethan", "nope"))
	if p := read(cmdAuth); binary.LittleEndian.Uint32(p.Body[0x2C:]) != authErrCode {
		t.Fatal("a wrong password was not refused")
	}
	send(cmdLogin, loginBody("ETHAN", "wrong!"))
	p := read(cmdAuth)
	gid := binary.LittleEndian.Uint32(p.Body[0x18:])
	if gid != auth.PreferredGID("ethan") || binary.LittleEndian.Uint32(p.Body[0x24:]) != gid {
		t.Fatalf("auth info GID %#x", gid)
	}
	key := p.Body[0x28:0x2C]

	send(cmdGameReq, make([]byte, 0x24))
	p = read(cmdGameServer)
	if !slices.Equal(p.Body[0x24:0x28], []byte{203, 0, 113, 7}) || binary.LittleEndian.Uint16(p.Body[0x28:]) != 12061 {
		t.Fatalf("game server % x", p.Body[0x24:0x2C])
	}
	if _, err := wire.Read(c, 0x10000); err == nil {
		t.Fatal("the connection stayed open after 0x0216")
	}
	if d, ok := keys.Take(gid, key); !ok || d != "psu" {
		t.Fatal("the key login sent is not redeemable")
	}
}
