// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// stream is tools/golden_combat.py's scripted dice: value k is (k * 7919 + 13) % 1000003.
type stream struct{ k int }

func (s *stream) next() int {
	v := (s.k*7919 + 13) % 1000003
	s.k++
	return v
}
func (s *stream) Intn(n int) int   { return s.next() % n }
func (s *stream) Float64() float64 { return float64(s.next()%1000000) / 1e6 }

type combatGolden struct {
	Battles []struct {
		Quest   uint32 `json:"quest"`
		Zone    uint32 `json:"zone"`
		Raw     string `json:"raw"`
		Start   int    `json:"start"`
		Weapon  int    `json:"weapon"`
		Batches []struct {
			Body    string            `json:"body"`
			Entries []string          `json:"entries"`
			Kills   []uint32          `json:"kills"`
			HP      int               `json:"hp"`
			SE2     uint32            `json:"se2"`
			EnemyHP map[string]int    `json:"enemy_hp"`
			PAs     []PA              `json:"pas"`
			Grown   []json.RawMessage `json:"grown"`
		} `json:"batches"`
	} `json:"battles"`
	Enemies []struct {
		Spec    *EnemySpec `json:"spec"`
		Generic int        `json:"generic"`
		HP      int        `json:"hp"`
		Stats   [8]int     `json:"stats"`
		EXP     int        `json:"exp"`
		Element uint8      `json:"element"`
		Pct     uint8      `json:"pct"`
	} `json:"enemies"`
	EXP []struct {
		Raw   string `json:"raw"`
		Steps []struct {
			Amount int    `json:"amount"`
			Up     bool   `json:"up"`
			Level  int    `json:"level"`
			EXP    uint32 `json:"exp"`
			HP     int    `json:"hp"`
			TL     [2]int `json:"tl"`
			TEXP   int    `json:"texp"`
			Types  string `json:"types"`
			Status string `json:"status"`
		} `json:"steps"`
	} `json:"exp"`
	Drops struct {
		Start int                 `json:"start"`
		Rolls [][]json.RawMessage `json:"rolls"`
		Zones [][4]int            `json:"zones"`
	} `json:"drops"`
	Judge []struct {
		Quest   uint32   `json:"quest"`
		Elapsed int      `json:"elapsed"`
		Kills   int      `json:"kills"`
		Flags   [][3]int `json:"flags"`
		Start   bool     `json:"start"`
		Clear   bool     `json:"clear"`
		Rank    int      `json:"rank"`
		Total   int      `json:"total"`
		Rewards []Reward `json:"rewards"`
		Block   string   `json:"block"`
	} `json:"judge"`
	Counters []struct {
		Table   uint32 `json:"table"`
		Options string `json:"options"`
	} `json:"counters"`
}

func loadCombat(t *testing.T) (*Rules, combatGolden) {
	set, _ := golden(t)
	raw, err := os.ReadFile(filepath.Join(os.Getenv("PSU_TEST_GOLDEN"), "combat.json"))
	if err != nil {
		t.Skip("no combat.json: python3 tools/golden_combat.py prettystellaruniverse data work/golden/combat.json")
	}
	var g combatGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return set.For(PSU), g
}

func TestBattles(t *testing.T) {
	r, g := loadCombat(t)
	for bi, bt := range g.Battles {
		raw := unhex(t, bt.Raw)
		st := r.NewState(raw)
		st.Inv[bt.Weapon].Active = 1
		f := &Fighter{R: r, GID: 0x01000000 + uint32(raw[RawRace]), LID: 0, Raw: raw, St: st, Status: &Status{}}
		m := r.Mission(bt.Quest)
		roster, _ := m.Roster(bt.Zone)
		boxes := map[uint32]*Box{}
		for _, o := range m.SetObjects(bt.Zone) {
			if o.Type == 12 && o.Target >= 0 {
				boxes[uint32(o.Target)] = &Box{MapIndex: o.MapIndex, Params: o.Params, OID: o.OID, Pos: o.Pos}
			}
		}
		b := &Battle{R: r, Rng: &stream{bt.Start}, Now: 1000, Players: map[uint16]*Fighter{0: f},
			Enemies: map[uint32]*Enemy{}, Roster: roster, Boxes: boxes, Self: f.GID}
		for i, want := range bt.Batches {
			b.Level = max(1, st.Level)
			got := b.Resolve(unhex(t, want.Body))
			if len(got.Entries) != len(want.Entries) {
				t.Fatalf("battle %d batch %d: %d entries, want %d", bi, i, len(got.Entries), len(want.Entries))
			}
			for k, e := range got.Entries {
				if hex.EncodeToString(e) != want.Entries[k] {
					t.Fatalf("battle %d batch %d entry %d:\n got %x\nwant %s", bi, i, k, e, want.Entries[k])
				}
			}
			var kills []uint32
			for _, e := range got.Kills {
				kills = append(kills, e.ID)
			}
			if !reflect.DeepEqual(kills, want.Kills) && len(kills)+len(want.Kills) > 0 {
				t.Fatalf("battle %d batch %d kills %v, want %v", bi, i, kills, want.Kills)
			}
			if st.HP != want.HP || f.Status.SE2(1000) != want.SE2 {
				t.Fatalf("battle %d batch %d: hp %d se2 %#x, want %d %#x", bi, i, st.HP, f.Status.SE2(1000), want.HP, want.SE2)
			}
			for id, hp := range want.EnemyHP {
				n, _ := strconv.Atoi(id)
				if e := b.Enemies[uint32(n)]; e == nil || e.HP != hp {
					t.Fatalf("battle %d batch %d: enemy %s hp %v, want %d", bi, i, id, e, hp)
				}
			}
			if !reflect.DeepEqual(st.PAs, want.PAs) {
				t.Fatalf("battle %d batch %d: PAs %v, want %v", bi, i, st.PAs, want.PAs)
			}
			if len(got.Grown) != len(want.Grown) {
				t.Fatalf("battle %d batch %d: %d PA growths, want %d", bi, i, len(got.Grown), len(want.Grown))
			}
		}
	}
}

func TestEnemies(t *testing.T) {
	r, g := loadCombat(t)
	for _, want := range g.Enemies {
		var e *Enemy
		if want.Spec != nil {
			e = r.NewEnemy(want.Spec.ID, want.Spec, want.Spec.Level)
		} else {
			e = r.NewEnemy(9999, nil, want.Generic)
		}
		if e.MaxHP != want.HP || e.Stats != want.Stats || e.EXP != want.EXP || e.Element != want.Element || e.ElementPct != want.Pct {
			t.Errorf("enemy %+v: got hp %d stats %v exp %d el %d/%d, want %+v", want.Spec, e.MaxHP, e.Stats, e.EXP,
				e.Element, e.ElementPct, want)
		}
	}
}

func TestEXP(t *testing.T) {
	r, g := loadCombat(t)
	for _, c := range g.EXP {
		raw := unhex(t, c.Raw)
		st := r.NewState(raw)
		for _, s := range c.Steps {
			up := r.AddEXP(raw, st, s.Amount)
			old, nw := r.AddTypeEXP(st, s.Amount/10)
			if up != s.Up || st.Level != s.Level || st.EXP != s.EXP || st.HP != s.HP || [2]int{old, nw} != s.TL ||
				st.TypeEXP[st.Cls] != s.TEXP {
				t.Fatalf("+%d: up %v level %d exp %d hp %d tl %d/%d texp %d, want %+v", s.Amount, up, st.Level, st.EXP,
					st.HP, old, nw, st.TypeEXP[st.Cls], s)
			}
			if hex.EncodeToString(r.TypesTable(raw, st)) != s.Types {
				t.Fatalf("+%d: types table differs", s.Amount)
			}
			if hex.EncodeToString(r.StatusBlock(raw, st, PSU)) != s.Status {
				t.Fatalf("+%d: status block differs", s.Amount)
			}
		}
	}
}

func TestDrops(t *testing.T) {
	r, g := loadCombat(t)
	rng := &stream{g.Drops.Start}
	for _, roll := range g.Drops.Rolls {
		var kind string
		var a, b, c, d int
		json.Unmarshal(roll[0], &kind)
		json.Unmarshal(roll[1], &a)
		json.Unmarshal(roll[2], &b)
		json.Unmarshal(roll[3], &c)
		var res json.RawMessage
		var drop Drop
		var ok bool
		if kind == "enemy" {
			res = roll[4]
			drop, ok = r.RollEnemy(rng, a, b, c)
		} else {
			json.Unmarshal(roll[4], &d)
			res = roll[5]
			drop, ok = r.RollObject(rng, a, b, c, d)
		}
		var want []json.RawMessage
		json.Unmarshal(res, &want)
		switch {
		case want == nil && !ok:
		case want == nil || !ok:
			t.Fatalf("%s %v: got %+v %v, want %s", kind, roll[1:4], drop, ok, res)
		default:
			var wk string
			json.Unmarshal(want[0], &wk)
			if wk == "meseta" {
				var m int
				json.Unmarshal(want[1], &m)
				if drop.Meseta != m {
					t.Fatalf("%s %v: meseta %d, want %d", kind, roll[1:4], drop.Meseta, m)
				}
			} else {
				var id ItemID
				json.Unmarshal(want[1], &id)
				if drop.Item != id {
					t.Fatalf("%s %v: item %v, want %v", kind, roll[1:4], drop.Item, id)
				}
			}
		}
	}
	for _, z := range g.Drops.Zones {
		area, lvl, _ := r.ZoneArea(uint32(z[0]), uint32(z[1]))
		if area != z[2] || lvl != z[3] {
			t.Errorf("zone %v: area %d level %d", z, area, lvl)
		}
	}
}

func TestJudge(t *testing.T) {
	r, g := loadCombat(t)
	for _, c := range g.Judge {
		q := r.Mission(c.Quest).Quest
		on := map[[3]int]bool{}
		for _, f := range c.Flags {
			on[f] = true
		}
		flag := func(z, m, e int) bool { return on[[3]int{z, m, e}] }
		if Evaluate(q.Start, flag, nil, nil) != c.Start || Evaluate(q.Clear, flag, nil, &Area{c.Quest, 0, 0}) != c.Clear {
			t.Errorf("%d: start/clear differ", c.Quest)
		}
		rank, total, secs := RankOf(q, c.Elapsed, Run{Kills: c.Kills, Deaths: 1}, flag)
		got := Rewards(q, rank, flag)
		if rank != c.Rank || total != c.Total || !reflect.DeepEqual(got, c.Rewards) && len(got)+len(c.Rewards) > 0 {
			t.Fatalf("%d: rank %d total %d rewards %v, want %+v", c.Quest, rank, total, got, c)
		}
		var slots []Item
		for n, rw := range got {
			var id ItemID
			if id.UnmarshalText([]byte(rw.Item)) != nil {
				continue
			}
			if _, ok := r.Item(id); ok {
				slots = append(slots, Item{UUID: uint32(7 + n), ID: id, Qty: max(1, rw.Qty)})
			}
		}
		if hex.EncodeToString(r.ResultBlock(rank, total, secs, 1234, slots)) != c.Block {
			t.Fatalf("%d: result block differs", c.Quest)
		}
	}
	for _, c := range g.Counters {
		ct, _ := r.Counter(c.Table)
		if hex.EncodeToString(r.CounterOptions(ct)) != c.Options {
			t.Errorf("counter %d options differ", c.Table)
		}
	}
}

func TestStatuses(t *testing.T) {
	r, _ := loadCombat(t)
	raw, _ := os.ReadFile(filepath.Join(os.Getenv("PSU_TEST_GOLDEN"), "combat.json"))
	var doc struct {
		Status struct {
			Raw   string              `json:"raw"`
			Rod   ItemID              `json:"rod"`
			Start int                 `json:"start"`
			Log   [][]json.RawMessage `json:"log"`
		} `json:"status"`
	}
	json.Unmarshal(raw, &doc)
	g := doc.Status
	craw := unhex(t, g.Raw)
	st := r.NewState(craw)
	st.Inv = append(st.Inv, Item{UUID: 50, ID: g.Rod, Qty: 1, Active: 1, Techs: [][2]int{{41, 12}, {1, 3}, {0, 0}, {0, 0}}})
	st.PAs = append(st.PAs, PA{5, ItemID{6, 5, 0, 0x12}}, PA{0, ItemID{6, 1, 0, 3}})
	f := &Fighter{R: r, GID: 0x02000000, Raw: craw, St: st, Status: &Status{}}
	rng, now := &stream{g.Start}, 1000.0
	var got [][]any
	note := func(what string, vals ...any) { got = append(got, append([]any{what, now}, vals...)) }
	s := f.Status
	note("inflict poison", s.Inflict(rng, 0x2, 1, 100, 10, false, 0, now), s.SE2(now))
	note("inflict burn sure", s.Inflict(rng, 0x1, 2, 100, 10, true, 0, now), s.SE2(now))
	note("inflict freeze sure", s.Inflict(rng, 0x20, 0, 100, 10, true, 0, now), s.SE2(now))
	note("buffs", s.ApplyBuffs(0x800000, 2, now), s.SE2(now))
	mx := f.MaxHP()
	note("buffed stats", buffedStats(s, [8]int{100, 200, 300, 400, 500, 600, 700, 800}, now))
	for _, tm := range []float64{1001, 1002.5, 1003, 1004, 1006, 1012, 1030} {
		now = tm
		ev, hp := s.Step(st.HP, mx, now)
		st.HP = hp
		evs := [][]any{}
		for _, e := range ev {
			evs = append(evs, []any{e.Kind, e.Amount, e.HP})
		}
		note("step", evs, hp, s.SE2(now))
	}
	now = 1031
	note("inflict freeze again", s.Inflict(rng, 0x20, 1, 100, 10, true, 0, now), s.SE2(now))
	for range 4 {
		s.breakOnHit()
		note("break", s.SE2(now))
	}
	note("cure", s.Cure(0x3C3FF, now), s.SE2(now))
	d := unhex(t, "04120000000000003200000090010000"+"01320000")
	dmg, se := Damage(rng, d, [8]int{10, 10, 10, 10, 10, 10, 10, 10}, 500, nil, f, PSU, now)
	note("damage frozen", dmg, se)
	for _, c := range [][4]uint8{{1, 50, 9, 30}, {1, 100, 1, 100}, {2, 10, 10, 0}, {4, 20, 1, 0}, {12, 60, 4, 80}} {
		note("element", c[0], c[1], c[2], c[3], elementMult(c[0], c[1], c[2], c[3], PSU))
	}
	sup, _ := f.supportAttack(50, make([]byte, 20), now)
	note("support", hex.EncodeToString(sup.d), sup.flags, sup.bits, sup.power, sup.level)
	st.HP = 10
	amount, se1, _ := f.applySupport(sup, now)
	note("apply support", amount, se1, st.HP)
	note("foie D", hex.EncodeToString(f.playerAttack(50|1<<29, make([]byte, 20), now)))
	pg, _ := f.PAHit(50)
	note("pa hit", []any{pg.PA.EXP, pg.PA.ID.String()}, pg.Up)

	if len(got) != len(g.Log) {
		t.Fatalf("%d log lines, want %d", len(got), len(g.Log))
	}
	for i, line := range got {
		b, _ := json.Marshal(line)
		var gotVals, wantVals []any
		json.Unmarshal(b, &gotVals)
		wb, _ := json.Marshal(g.Log[i])
		json.Unmarshal(wb, &wantVals)
		if !reflect.DeepEqual(normBool(gotVals), normBool(wantVals)) {
			t.Errorf("%s:\n got %s\nwant %s", line[0], b, wb)
		}
	}
}

// normBool makes Python's 0/1 results and Go's bools compare equal.
func normBool(v []any) []any {
	out := make([]any, len(v))
	for i, x := range v {
		if b, ok := x.(bool); ok {
			x = 0.0
			if b {
				x = 1.0
			}
		}
		out[i] = x
	}
	return out
}
