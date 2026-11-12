// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type econGolden struct {
	Shops [][]json.RawMessage `json:"shops"`
	Trade struct {
		Ops [][]json.RawMessage `json:"ops"`
		Inv []Item              `json:"inv"`
	} `json:"trade"`
	Grind        [][]json.RawMessage `json:"grind"`
	GrindCommits struct {
		Start int                 `json:"start"`
		Rows  [][]json.RawMessage `json:"rows"`
	} `json:"grind_commits"`
	Learn struct {
		Disks []ItemID            `json:"disks"`
		Rows  [][]json.RawMessage `json:"rows"`
		PAs   []PA                `json:"pas"`
		Inv   []ItemID            `json:"inv"`
	} `json:"learn"`
	Lumilass [][]json.RawMessage `json:"lumilass"`
	Probo    [][]json.RawMessage `json:"probo"`
	Synth    struct {
		Start int                 `json:"start"`
		Rows  [][]json.RawMessage `json:"rows"`
	} `json:"synth"`
}

func loadEcon(t *testing.T) (*Rules, econGolden) {
	set, _ := golden(t)
	raw, err := os.ReadFile(filepath.Join(os.Getenv("PSU_TEST_GOLDEN"), "econ.json"))
	if err != nil {
		t.Skip("no econ.json: python3 tools/golden_econ.py prettystellaruniverse data work/golden/econ.json")
	}
	var g econGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return set.For(PSU), g
}

func jv[T any](t *testing.T, m json.RawMessage) T {
	var v T
	if err := json.Unmarshal(m, &v); err != nil {
		t.Fatalf("%s: %v", m, err)
	}
	return v
}

func sampleState(r *Rules) (*State, []byte) {
	raw := append([]byte(nil), mustSample()...)
	return r.NewState(raw), raw
}

// mustSample is testplay's sample 0x0D02 body as the golden scripts use it (race 0,
// gender 0, type 0).
func mustSample() []byte {
	b, _ := hex.DecodeString("0000000000000000000000000000000000000000000000000000000000000000000000000000000054006500730074000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000017f000000000600000006000100060002000000030101000400280005000000000000000000000000ff7f00003cb60200ff7f0000ff7f000000000000ef8d0000ffff020079d400009a3f0100da1d0100ffff0000ffff00000f070000136a000001000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001000000010000000100000001000000010000000100000001000000010000000100000001000000010000000100000001000000010000000100000001000000")
	b[RawRace], b[RawGender], b[RawClass] = 0, 0, 0
	return b
}

func TestShops(t *testing.T) {
	r, g := loadEcon(t)
	for _, row := range g.Shops {
		npc, lvl, cat := jv[int](t, row[0]), jv[int](t, row[1]), jv[*int](t, row[2])
		want := jv[[][2]string](t, row[3])
		lst, c, ok := r.ShopList(npc, lvl)
		if !ok || (cat != nil && c != *cat) || len(lst) != len(want) {
			t.Fatalf("npc %d level %d: %d entries cat %d, want %d", npc, lvl, len(lst), c, len(want))
		}
		var recs []byte
		for i, e := range lst {
			if e.Key.String() != want[i][0] || hex.EncodeToString(e.DiscID[:]) != want[i][1] {
				t.Fatalf("npc %d level %d entry %d: %v", npc, lvl, i, e)
			}
			recs = append(recs, r.ShopRecord(e)...)
		}
		pkt := jv[string](t, row[4])
		if wantRecs := pkt[2*(8+0x34):]; hex.EncodeToString(recs) != wantRecs {
			t.Fatalf("npc %d level %d: records differ", npc, lvl)
		}
	}
	st, _ := sampleState(r)
	st.Meseta = 50000
	for _, op := range g.Trade.Ops {
		var meseta, n int
		switch jv[string](t, op[0]) {
		case "buy":
			key := jv[ItemID](t, op[1])
			s, err := r.Buy(st, key, jv[int](t, op[2]))
			if wantErr := jv[*string](t, op[4]); (err != nil) != (wantErr != nil) || (err == nil && s.UUID != jv[uint32](t, op[3])) {
				t.Fatalf("buy %v: %+v %v", key, s, err)
			}
		case "sell":
			got, gain := r.Sell(st, jv[uint32](t, op[1]), jv[int](t, op[2]))
			if got != jv[int](t, op[3]) || gain != jv[int](t, op[4]) {
				t.Fatalf("sell: %d %d", got, gain)
			}
		}
		meseta, n = jv[int](t, op[5]), jv[int](t, op[6])
		if int(st.Meseta) != meseta || len(st.Inv) != n {
			t.Fatalf("%s: meseta %d items %d, want %d %d", op[0], st.Meseta, len(st.Inv), meseta, n)
		}
	}
	if !reflect.DeepEqual(st.Inv, g.Trade.Inv) {
		t.Fatalf("inventory after trading:\n got %+v\nwant %+v", st.Inv, g.Trade.Inv)
	}
}

func TestGrinding(t *testing.T) {
	r, g := loadEcon(t)
	for _, row := range g.Grind {
		w, gr, want := jv[ItemID](t, row[0]), jv[ItemID](t, row[1]), jv[*int](t, row[2])
		c, ok := r.GrindChance(&Item{ID: gr}, &Item{UUID: 2, ID: w, Qty: 1})
		if ok != (want != nil) || (ok && c != *want) {
			t.Fatalf("%v on %v: %d %v, want %v", gr, w, c, ok, want)
		}
	}
	rng := &stream{g.GrindCommits.Start}
	var st *State
	var last ItemID
	for _, row := range g.GrindCommits.Rows {
		w := jv[ItemID](t, row[0])
		if w != last {
			gr := jv[ItemID](t, g.Grind[len(g.Grind)-1][1])
			_ = gr
			st = &State{Inv: []Item{{UUID: 1, ID: grinderOf(t, g), Qty: 99}, {UUID: 2, ID: w, Qty: 1}}}
			last = w
		}
		_, ok, valid := r.Grind(rng, st, 1, 1)
		wantErr := jv[*string](t, row[2])
		if valid != (wantErr == nil) || (valid && ok != jv[bool](t, row[1])) {
			t.Fatalf("grind %v: ok %v valid %v, want %s %s", w, ok, valid, row[1], row[2])
		}
		want := jv[Item](t, row[3])
		if st.Inv[1].Grind != want.Grind || st.Inv[1].PPBonus != want.PPBonus || st.Inv[1].ATPBonus != want.ATPBonus {
			t.Fatalf("grind %v: %+v, want %+v", w, st.Inv[1], want)
		}
	}
}

// grinderOf is the grinder the commits used: the last of the quote table's three.
func grinderOf(t *testing.T, g econGolden) ItemID {
	return jv[ItemID](t, g.Grind[2][1])
}

func TestLearnDisks(t *testing.T) {
	r, g := loadEcon(t)
	st, _ := sampleState(r)
	for n, d := range g.Learn.Disks {
		st.Inv = append(st.Inv, Item{UUID: uint32(100 + n), ID: d, Qty: 1})
	}
	for _, row := range g.Learn.Rows {
		req, _ := hex.DecodeString(jv[string](t, row[0]))
		pos, entry, cat, err := r.LearnDisk(st, req)
		wantErr := jv[*string](t, row[4])
		if (err != nil) != (wantErr != nil) {
			t.Fatalf("learn %x: %v, want %v", req, err, wantErr)
		}
		if err == nil && (pos != jv[int](t, row[1]) || entry != jv[PA](t, row[2]) || cat != jv[int](t, row[3])) {
			t.Fatalf("learn: %d %v %d", pos, entry, cat)
		}
	}
	var inv []ItemID
	for _, x := range st.Inv {
		inv = append(inv, x.ID)
	}
	if !reflect.DeepEqual(st.PAs, g.Learn.PAs) || !reflect.DeepEqual(inv, g.Learn.Inv) {
		t.Fatalf("after learning: %v / %v", st.PAs, inv)
	}
}

func TestLumilassAndProbo(t *testing.T) {
	r, g := loadEcon(t)
	for _, row := range g.Lumilass {
		raw, _ := hex.DecodeString(jv[string](t, row[0]))
		body, _ := hex.DecodeString(jv[string](t, row[3]))
		if got := r.HairList(raw); !reflect.DeepEqual(got, jv[[]uint32](t, row[1])) && len(got) > 0 {
			t.Fatalf("hair list %v", got)
		}
		if hex.EncodeToString(r.ApplyLumilass(raw, body)) != jv[string](t, row[2]) {
			t.Fatal("lumilass record differs")
		}
	}
	st, _ := sampleState(r)
	for _, row := range g.Probo {
		id := jv[ItemID](t, row[0])
		ev, deltas := r.ProboGift(st, Item{UUID: 1, ID: id, Qty: 1})
		if ev != jv[bool](t, row[1]) || st.Probo.Type != jv[int](t, row[2]) || st.Probo.Stats != jv[[5]int](t, row[3]) ||
			deltas != jv[[5]int](t, row[4]) {
			t.Fatalf("gift %v: %v type %d stats %v deltas %v, want %s", id, ev, st.Probo.Type, st.Probo.Stats, deltas, row)
		}
	}
}

func TestSynthesis(t *testing.T) {
	r, g := loadEcon(t)
	rng := &stream{g.Synth.Start}
	st, _ := sampleState(r)
	st.Probo = &Probo{Type: 3, Stats: [5]int{250, 120, 400, 30, 0}}
	for _, row := range g.Synth.Rows {
		bid := jv[string](t, row[0])
		b := r.SynthBoard(bid)
		e := &SynthEntry{Board: bid, FailID: -1, Slots: b.Recipe(0)}
		st.RoomStock = nil
		for n := range 4 {
			m := b.Materials[n][0]
			var id ItemID
			if id.UnmarshalText([]byte(m.ID)) == nil {
				if _, ok := r.Item(id); ok {
					st.RoomStock = append(st.RoomStock, Item{UUID: uint32(900 + n), ID: id, Qty: 20})
				}
			}
		}
		if r.SynthRate(st, b, 0) != jv[int](t, row[1]) || r.OutputMax(st, e, b) != jv[int](t, row[2]) {
			t.Fatalf("board %s: rate %d max %d, want %s %s", bid, r.SynthRate(st, b, 0), r.OutputMax(st, e, b), row[1], row[2])
		}
		res := r.SynthRoll(rng, st, e, b)
		if want := jv[SynthResult](t, row[3]); *res != want {
			t.Fatalf("board %s: result %+v, want %+v", bid, *res, want)
		}
		r.SynthConsume(st, e, b, 0, 1)
		var stock [][2]any
		for _, x := range st.RoomStock {
			stock = append(stock, [2]any{x.ID.String(), float64(x.Qty)})
		}
		var want [][2]any
		json.Unmarshal(row[4], &want)
		if len(stock)+len(want) > 0 && !reflect.DeepEqual(stock, want) {
			t.Fatalf("board %s: stock %v, want %v", bid, stock, want)
		}
	}
}
