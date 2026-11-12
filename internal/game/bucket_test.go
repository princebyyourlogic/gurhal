// SPDX-License-Identifier: GPL-3.0-or-later

package game

import (
	"testing"
	"time"
)

func TestBucket(t *testing.T) {
	b, now := bucket{burst: 3, per: time.Second}, time.Unix(100, 0)
	for i := range 3 {
		if !b.allow(now) {
			t.Fatalf("burst %d refused", i)
		}
	}
	if b.allow(now) {
		t.Fatal("past the burst")
	}
	if !b.allow(now.Add(time.Second)) || b.allow(now.Add(time.Second)) {
		t.Fatal("one token a second")
	}
	if n := func() int {
		c := 0
		for b.allow(now.Add(time.Hour)) {
			c++
		}
		return c
	}(); n != 3 {
		t.Fatalf("refilled to %d, not the burst", n)
	}
}
