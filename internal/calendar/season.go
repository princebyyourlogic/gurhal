// SPDX-License-Identifier: GPL-3.0-or-later

package calendar

import (
	"slices"
	"strings"
	"time"
)

// Seasons, by the client's season id (its name table: PSU 0x57a070, AotI 0x5d5510). Each
// has a decoration archive XF_OB_<NAME>.NBL on both discs; the lobby's set file places
// the objects, so a season needs staged lobby archives (extract/seasonbuild.py).
var Seasons = []string{"party", "newyear", "valentine", "whiteday", "cherryblossoms", "easter", "parumfestival", "sonic",
	"neudaizfestival", "fireworks", "redleaves", "halloween", "moatoobfestival", "christmas", "snow", "wedding"}

var aliases = map[string]string{"spring": "cherryblossoms", "autumn": "redleaves", "winter": "snow",
	"anniversary": "party", "summer": "fireworks"}

// calendar windows (month, day) inclusive, first match wins; Easter (Sunday ±7) first.
// The planet festivals and fireworks have no lobby placements, so auto never picks them.
var dates = []struct {
	lo, hi [2]int
	name   string
}{
	{[2]int{1, 1}, [2]int{1, 14}, "newyear"},
	{[2]int{1, 15}, [2]int{2, 6}, "snow"},
	{[2]int{2, 7}, [2]int{2, 20}, "valentine"},
	{[2]int{3, 7}, [2]int{3, 20}, "whiteday"},
	{[2]int{3, 25}, [2]int{4, 20}, "cherryblossoms"},
	{[2]int{6, 1}, [2]int{6, 14}, "wedding"},
	{[2]int{6, 16}, [2]int{6, 30}, "sonic"},
	{[2]int{9, 22}, [2]int{10, 14}, "redleaves"},
	{[2]int{10, 15}, [2]int{10, 27}, "party"}, // PSU's US launch, 2006-10-24
	{[2]int{10, 28}, [2]int{11, 7}, "halloween"},
	{[2]int{12, 12}, [2]int{12, 26}, "christmas"},
	{[2]int{12, 27}, [2]int{12, 30}, "snow"},
	{[2]int{12, 31}, [2]int{12, 31}, "newyear"},
}

// EventItems are holiday items both item tables carry and nothing else gives (they were
// event handouts): one or two per season that has a fitting item.
var EventItems = map[string][]string{
	"valentine": {"03030400", "03030500"}, // Chocolat, Gourmet Chocolat
	"whiteday":  {"03030900", "03030a00"}, // Milkina Candy, Specina Candy
	"sonic":     {"03031d00", "0b013f00"}, // Sonic Gum, Sonic (room goods)
	"easter":    {"0b010400"},             // Easter Egg
	"wedding":   {"0b010c00"},             // Wedding Cake
	"fireworks": {"0b010600"},             // Fireworks (room goods)
	"halloween": {"0b014400"},             // Cursed Pumpkin
	"christmas": {"0b010b00"},             // Christmas Tree
	"snow":      {"0b010100"},             // Snowman
}

// NameOf is a season name or alias as its name ("" unknown).
func NameOf(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if a, ok := aliases[v]; ok {
		v = a
	}
	if slices.Contains(Seasons, v) {
		return v
	}
	return ""
}

// ID is a season's client id.
func ID(name string) int { return slices.Index(Seasons, name) }

// Easter is Gregorian Easter Sunday.
func Easter(year int) time.Time {
	a, b, c := year%19, year/100, year%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	n := h + l - 7*m + 114
	return time.Date(year, time.Month(n/31), n%31+1, 0, 0, 0, 0, time.UTC)
}

// ByDate is the calendar season of a day ("" none).
func ByDate(day time.Time) string {
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	if diff := day.Sub(Easter(day.Year())); diff >= -7*24*time.Hour && diff <= 7*24*time.Hour {
		return "easter"
	}
	md := [2]int{int(day.Month()), day.Day()}
	less := func(a, b [2]int) bool { return a[0] < b[0] || a[0] == b[0] && a[1] <= b[1] }
	for _, w := range dates {
		if less(w.lo, md) && less(md, w.hi) {
			return w.name
		}
	}
	return ""
}

// Mode is the server's season setting: "none", "auto" or a season name.
func Mode(v string) string {
	switch v = strings.ToLower(strings.TrimSpace(v)); v {
	case "", "none", "off", "0":
		return "none"
	case "auto":
		return "auto"
	}
	return NameOf(v)
}

// Current is the lobby season: a running event's, else the mode's ("" none).
func (c *Calendar) Current(now time.Time, mode string) string {
	if s := c.Season(now); s != "" {
		return s
	}
	switch m := Mode(mode); m {
	case "none", "":
		return ""
	case "auto":
		return ByDate(now)
	default:
		return m
	}
}
