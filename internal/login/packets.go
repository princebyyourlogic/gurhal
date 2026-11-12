// SPDX-License-Identifier: GPL-3.0-or-later

package login

import (
	"net/netip"
	"strings"

	"github.com/princebyyourlogic/gurhal/internal/wire"
)

const (
	cmdGameServer = 0x0216 // S>C game server address, then the connection closes
	cmdGameReq    = 0x0217 // C>S
	cmdLogin      = 0x0219 // C>S name[24], password[24]
	cmdAuth       = 0x0223 // S>C GID + key, or an error text
	cmdMOTD       = 0x0225 // S>C one page of the notice box
	cmdMOTDReq    = 0x0226 // C>S page request (also 0x023F)
	cmdMOTDReq2   = 0x023F
	cmdVersion    = 0x080E // C>S language, entrance, platform, version

	target      = wire.NoLID
	motdLines   = 15 // lines per notice-box page
	motdWidth   = 50 // characters that fit one line of the box
	authErrCode = 3  // EGS system_auth_error
)

func authInfo(gid uint32, key [4]byte) []byte {
	return wire.Encode(cmdAuth, wire.Body{}.Zero(20).Bytes(wire.Tag).U32(gid).Zero(8).U32(gid).Bytes(key[:]))
}

// authError is the refusal the client shows: code 3 and a UTF-16 message.
func authError(gid uint32, text string) []byte {
	msg := wire.UTF16(text)
	return wire.Encode(cmdAuth, wire.Body{}.Zero(20).Bytes(wire.Tag).U32(gid).Zero(16).U32(authErrCode).Zero(6).
		U16(uint16(len(msg)+2)).UTF16(msg).Zero(2))
}

func gameServer(gid uint32, addr netip.Addr, port uint16) []byte {
	ip := addr.As4()
	return wire.Encode(cmdGameServer, wire.Body{}.U16(target).Zero(18).Bytes(wire.Tag).U32(gid).Zero(8).
		Bytes(ip[:]).U16(port).Zero(2))
}

// motdPage is one page of text for the login notice box, motdLines lines a page.
func motdPage(text string, page int) []byte {
	lines := strings.Split(text, "\n")
	pages := max(1, (len(lines)+motdLines-1)/motdLines)
	page = min(page, pages-1)
	var b strings.Builder
	for _, l := range lines[page*motdLines : min(page*motdLines+motdLines, len(lines))] {
		b.WriteString(l + "\n")
	}
	units := wire.UTF16(b.String())
	return wire.Encode(cmdMOTD, wire.Body{}.U16(target).Zero(34).U8(uint8(pages)).U8(uint8(page)).
		U16(uint16(len(units)+2)).UTF16(units).Zero(2))
}

// motdText joins notices for the box: each wrapped to its width, a blank line between.
func motdText(notices []string) string {
	var lines []string
	for i, n := range notices {
		if i > 0 {
			lines = append(lines, "")
		}
		for _, l := range strings.Split(n, "\n") {
			lines = append(lines, wrap(l, motdWidth)...)
		}
	}
	return strings.Join(lines, "\n")
}

// wrap breaks a line at spaces into lines of at most width characters, splitting words
// longer than that.
func wrap(line string, width int) []string {
	var out []string
	cur := ""
	for _, w := range strings.Fields(line) {
		for r := []rune(w); len(r) > width; r = []rune(w) {
			if cur != "" {
				out, cur = append(out, cur), ""
			}
			out, w = append(out, string(r[:width])), string(r[width:])
		}
		switch {
		case cur == "":
			cur = w
		case len([]rune(cur))+1+len([]rune(w)) <= width:
			cur += " " + w
		default:
			out, cur = append(out, cur), w
		}
	}
	if cur != "" || len(out) == 0 {
		out = append(out, cur)
	}
	return out
}
