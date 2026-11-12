// SPDX-License-Identifier: GPL-3.0-or-later

package calendar

import (
	"slices"
	"testing"
	"time"
)

// Expected values come from prettystellaruniverse's psu/events.py and psu/season.py.
func TestCalendar(t *testing.T) {
	c := New(map[string][]byte{
		"hw":  []byte(`{"key":"hw","name":"Halloween","start":"2026-10-28T00:00Z","end":"2026-11-08T00:00Z","season":"halloween","exp":1.5,"drops":2,"notice":"Boo."}`),
		"wk":  []byte(`{"key":"wk","name":"Weekend","weekly":{"dow":[5,6],"from":"18:30","hours":30},"meseta":2,"game":"aoti","event_drop":20}`),
		"bad": []byte(`{"key":"bad","name":"Bad","start":"2026-01-02T00:00Z","end":"2026-01-01T00:00Z"}`),
	})
	if len(c.Events) != 2 || len(c.Problems) != 1 {
		t.Fatalf("events %d, problems %q", len(c.Events), c.Problems)
	}
	now := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	want := []string{"Halloween is on until Nov 8 0:00 UTC. Boo.", "Weekend is on until Nov 9 0:30 UTC.",
		"Weekend starts Nov 8 18:30 UTC.", "Weekend starts Nov 14 18:30 UTC."}
	if got := c.Notices(now); !slices.Equal(got, want) {
		t.Fatalf("notices\n got %q\nwant %q", got, want)
	}
	var up [][3]int64
	for _, r := range c.Upcoming(now, 10) {
		up = append(up, [3]int64{int64(len(r.E.Key)), r.Start.Unix(), r.End.Unix()})
	}
	wantUp := [][3]int64{{2, 1793145600, 1794096000}, {2, 1794076200, 1794184200}, {2, 1794162600, 1794270600},
		{2, 1794681000, 1794789000}, {2, 1794767400, 1794875400}}
	if !slices.Equal(up, wantUp) {
		t.Fatalf("upcoming %v", up)
	}
	if fx := c.Effects(now, "psu"); fx != (Effects{EXP: 1.5, Meseta: 1, DropRolls: 2}) {
		t.Fatalf("psu effects %+v", fx)
	}
	if fx := c.Effects(now, "aoti"); fx != (Effects{EXP: 1.5, Meseta: 2, DropRolls: 2, EventDrop: 20}) {
		t.Fatalf("aoti effects %+v", fx)
	}
	if s := c.Current(now, "none"); s != "halloween" {
		t.Fatalf("season %q", s)
	}
	started, ended := Changes(c.Running(now, ""), c.Running(now.Add(5*time.Hour), ""))
	if len(started) != 0 || len(ended) != 1 || ended[0].E.Key != "hw" {
		t.Fatalf("changes %v %v", started, ended)
	}
	if len(c.BoardRows(now)) != 2 {
		t.Fatalf("board rows %+v", c.BoardRows(now))
	}
}

func TestSeasons(t *testing.T) {
	if e := Easter(2026); e.Month() != 4 || e.Day() != 5 {
		t.Fatalf("easter %v", e)
	}
	for day, want := range map[string]string{"2026-04-01": "easter", "2026-10-30": "halloween", "2026-05-01": ""} {
		d, _ := time.Parse("2006-01-02", day)
		if got := ByDate(d); got != want {
			t.Errorf("%s: %q, want %q", day, got, want)
		}
	}
	if NameOf("Autumn") != "redleaves" || ID("snow") != 14 || Mode("off") != "none" || Mode("bogus") != "" {
		t.Fatal("names")
	}
}
