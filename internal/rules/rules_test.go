// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/princebyyourlogic/gurhal/internal/gamedata"
)

// The golden vectors come from prettystellaruniverse run on your own disc data:
//
//	python3 tools/golden.py prettystellaruniverse data work/golden/rules.json
//	PSU_TEST_DATA=$PWD/data PSU_TEST_GOLDEN=$PWD/work/golden scripts/go test ./internal/rules/
type vector struct {
	Raw         string          `json:"raw"`
	GID         uint32          `json:"gid"`
	State       json.RawMessage `json:"state"`
	CharBin     string          `json:"char_bin"`
	SlotRecord  string          `json:"slot_record"`
	StatusBlock string          `json:"status_block"`
	C0A0A       string          `json:"c0a0a"`
	UserBin     string          `json:"user_bin"`
}

func golden(t *testing.T) (*Set, []vector) {
	t.Helper()
	data, dir := os.Getenv("PSU_TEST_DATA"), os.Getenv("PSU_TEST_GOLDEN")
	if data == "" || dir == "" {
		t.Skip("PSU_TEST_DATA / PSU_TEST_GOLDEN not set")
	}
	d, err := gamedata.Load(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	return set, vs
}

func unhex(t *testing.T, s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGolden(t *testing.T) {
	set, vs := golden(t)
	r := set.For(PSU)
	for _, v := range vs {
		raw := unhex(t, v.Raw)
		var want State
		if err := json.Unmarshal(v.State, &want); err != nil {
			t.Fatal(err)
		}
		name := hex.EncodeToString(raw[RawRace : RawClass+1])
		st := r.NewState(raw)
		if !reflect.DeepEqual(*st, want) {
			t.Errorf("%s NewState:\n got %+v\nwant %+v", name, *st, want)
			continue
		}
		check := func(what string, got []byte, want string) {
			if w := unhex(t, want); !bytes.Equal(got, w) {
				for i := range min(len(got), len(w)) {
					if got[i] != w[i] {
						t.Errorf("%s %s: first difference at %#x (len %d, want %d)", name, what, i, len(got), len(w))
						return
					}
				}
				t.Errorf("%s %s: len %d, want %d", name, what, len(got), len(w))
			}
		}
		check("char_bin", r.CharBin(raw, st, PSU), v.CharBin)
		check("slot_record", r.SlotRecord(raw, st), v.SlotRecord)
		check("status_block", r.StatusBlock(raw, st, PSU), v.StatusBlock)
		check("inventory", r.Inventory(st, PSU), v.C0A0A[2*0x2C:])
		check("user_bin", r.User(v.GID, 3, raw, st, Area{1100000, 0, 4}, 2,
			Pos{1.5, -2.0, 30.25, DirFromDegrees(90)}, PSU, ""), v.UserBin)
	}
}

func TestStateJSONRoundTrip(t *testing.T) {
	pp := 12
	st := State{Cls: 3, Level: 7, Inv: []Item{{UUID: 1, ID: ItemID{1, 9, 0, 0}, Qty: 1, PP: &pp}},
		PAs: []PA{{EXP: 5, ID: ItemID{4, 9, 6, 2}}}, Palette: HexBytes{1, 2}}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"pas":[[5,"04090602"]]`)) || !bytes.Contains(b, []byte(`"id":"01090000"`)) {
		t.Fatalf("json %s", b)
	}
	var back State
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, st) {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}
