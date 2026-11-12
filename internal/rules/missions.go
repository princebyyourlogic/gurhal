// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

// Missions from the disc's offline free missions (missions.json, extract/missiondata.py).

type MissionData struct {
	Counters map[string]Counter  `json:"counters"`
	Missions map[string]*Mission `json:"missions"`
}

type Counter struct {
	Groups int      `json:"groups"`
	Quests []uint32 `json:"quests"`
}

type Mission struct {
	Name     string                       `json:"name"`
	Enter    [3]int                       `json:"enter"` // zone, map, entry
	Exit     []int                        `json:"exit"`
	Zones    map[string][]uint32          `json:"zones"` // zone -> map ids (index = map index in flags)
	Objects  map[string][]json.RawMessage `json:"objects"`
	Gateways map[string][]Gateway         `json:"gateways"`
	Quest    *Quest                       `json:"quest"`
}

type Gateway struct {
	Map      uint32      `json:"map"`
	MapIndex int         `json:"map_index"`
	ID       uint32      `json:"id"`
	Flags    []int       `json:"flags"` // [0] = the event set when its waves are cleared
	Enemies  []EnemySpec `json:"enemies"`
}

type Quest struct {
	Start   []int             `json:"start"`
	Clear   []int             `json:"clear"`
	Rank    *Rank             `json:"rank"`
	Rewards map[string]Reward `json:"rewards"`
}

type Rank struct {
	Sections   []Section           `json:"sections"`
	Thresholds []int               `json:"thresholds"`
	TypePoints []int               `json:"type_points"`
	Rewards    [][]json.RawMessage `json:"rewards"` // [reward id, condition tokens]
}

type Section struct {
	Type  int      `json:"type"`
	Kind  int      `json:"kind"`
	Arg   int      `json:"arg"`
	Max   int      `json:"max"`
	Min   int      `json:"min"`
	Par   int      `json:"par"`
	Mult  int      `json:"mult"`
	Flags [][3]int `json:"flags"`
}

type Reward struct {
	Item   string `json:"item"`
	Qty    int    `json:"qty"`
	Flags  int    `json:"flags"`
	Meseta int    `json:"meseta"`
}

// SetObject is a mission zone's set object: map index, group, object, type, params.
type SetObject struct {
	MapIndex, Group, Object, Type int
	Params                        []byte
	Target                        int // box target id (150 + set index), -1 none
	OID                           int // map-local object id
	Pos                           []float64
}

// Mission looks up a quest of this disc.
func (r *Rules) Mission(qid uint32) *Mission {
	if r.missions == nil {
		return nil
	}
	return r.missions.Missions[strconv.FormatUint(uint64(qid), 10)]
}

// Counter is a counter's table, if this disc has it.
func (r *Rules) Counter(id uint32) (Counter, bool) {
	if r.missions == nil {
		return Counter{}, false
	}
	c, ok := r.missions.Counters[strconv.FormatUint(uint64(id), 10)]
	return c, ok
}

// CounterOptions are the 0x0C10 option bytes: groups 3, quests 3 selectable, 0 hidden.
func (r *Rules) CounterOptions(c Counter) []byte {
	out := make([]byte, 0, c.Groups+len(c.Quests))
	for range c.Groups {
		out = append(out, 3)
	}
	for _, q := range c.Quests {
		if r.Mission(q) != nil && r.shown(q) {
			out = append(out, 3)
		} else {
			out = append(out, 0)
		}
	}
	return out
}

// shown: AotI's tables list every map layout of a mission; only layout 0 is offered.
func (r *Rules) shown(q uint32) bool { return r.disc != AotI || q < 300000 || (q/10)%10 == 0 }

// Maps are a mission's zone -> map ids.
func (m *Mission) Maps(zone uint32) []uint32 { return m.Zones[strconv.FormatUint(uint64(zone), 10)] }

// HasMap reports whether the mission has map m in zone z.
func (m *Mission) HasMap(z, mp uint32) bool {
	for _, x := range m.Maps(z) {
		if x == mp {
			return true
		}
	}
	return false
}

// Roster is a mission zone's enemies by target id, with their gateway and map.
func (m *Mission) Roster(zone uint32) (map[uint32]*EnemySpec, []Gateway) {
	gates := m.Gateways[strconv.FormatUint(uint64(zone), 10)]
	out := map[uint32]*EnemySpec{}
	for _, g := range gates {
		for i := range g.Enemies {
			e := g.Enemies[i]
			e.Gateway, e.Map = g.ID, g.Map
			out[e.ID] = &e
		}
	}
	return out, gates
}

// SetObjects are a mission zone's set objects.
func (m *Mission) SetObjects(zone uint32) []SetObject {
	var out []SetObject
	for _, raw := range m.Objects[strconv.FormatUint(uint64(zone), 10)] {
		var v []json.RawMessage
		if json.Unmarshal(raw, &v) != nil || len(v) < 5 {
			continue
		}
		var o SetObject
		var params string
		json.Unmarshal(v[0], &o.MapIndex)
		json.Unmarshal(v[1], &o.Group)
		json.Unmarshal(v[2], &o.Object)
		json.Unmarshal(v[3], &o.Type)
		json.Unmarshal(v[4], &params)
		o.Params, _ = hex.DecodeString(params)
		o.Target, o.OID = -1, -1
		if len(v) > 5 {
			json.Unmarshal(v[5], &o.Target)
			o.OID = o.Target
		}
		if len(v) > 6 {
			json.Unmarshal(v[6], &o.OID)
		}
		if len(v) > 7 {
			json.Unmarshal(v[7], &o.Pos)
		}
		out = append(out, o)
	}
	return out
}

// --- the judge (start / clear rules, rank, rewards: the offline TQuestJudge) ---------------------------

// Evaluate runs an RPN rule: 0..6 AND OR EQ LT LE GT GE, 100 n push, 120 zone map event =
// flag set, 134 / 135 quest zone map = the player is there, 147 = rank. Empty = never;
// an unknown command = never.
func Evaluate(tokens []int, flag func(zone, mapID, ev int) bool, rank *int, at *Area) bool {
	if len(tokens) == 0 {
		return false
	}
	var st []int
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	for i := 0; i < len(tokens); {
		t := tokens[i]
		switch {
		case t >= 0 && t <= 6:
			if len(st) < 2 {
				return false
			}
			b, a := st[len(st)-1], st[len(st)-2]
			st = st[:len(st)-2]
			var v bool
			switch t {
			case 0:
				v = a != 0 && b != 0
			case 1:
				v = a != 0 || b != 0
			case 2:
				v = a == b
			case 3:
				v = a < b
			case 4:
				v = a <= b
			case 5:
				v = a > b
			case 6:
				v = a >= b
			}
			st = append(st, b2i(v))
			i++
		case t == 100 && i+1 < len(tokens):
			st = append(st, tokens[i+1])
			i += 2
		case t == 120 && i+3 < len(tokens):
			st = append(st, b2i(flag(tokens[i+1], tokens[i+2], tokens[i+3])))
			i += 4
		case (t == 134 || t == 135) && i+3 < len(tokens):
			st = append(st, b2i(atPlace(at, tokens[i+1], tokens[i+2], tokens[i+3])))
			i += 4
		case t == 147:
			v := -1
			if rank != nil {
				v = *rank
			}
			st = append(st, v)
			i++
		default:
			return false
		}
	}
	return len(st) > 0 && st[len(st)-1] != 0
}

func atPlace(at *Area, quest, zone, mapID int) bool {
	if at == nil || quest == -1 || int64(at.Quest) == -1 || int(at.Quest) != quest {
		return false
	}
	return (zone < 0 || int(at.Zone) == zone) && (mapID < 0 || int(at.Map) == mapID)
}

// Run is a mission run's judge counters.
type Run struct {
	Kills, Deaths int
	APCDowns      map[int]int
}

// SectionScore is one rank section's value and score.
type SectionScore struct {
	S            Section
	Value, Score int
}

// RankOf (0x3cbe70): score = clamp(max - mult * (par - value), min, max) per section;
// rank = the first threshold the total is within, else 3 (best).
func RankOf(q *Quest, elapsed int, run Run, flag func(zone, mapID, ev int) bool) (int, int, []SectionScore) {
	var rk Rank
	if q.Rank != nil {
		rk = *q.Rank
	}
	var secs []SectionScore
	total := 0
	for _, s := range rk.Sections {
		var v int
		switch s.Type {
		case 0, 1:
			v = elapsed
		case 2:
			v = run.Kills
		case 3:
			v = run.Deaths
		case 4:
			v = run.APCDowns[s.Arg]
		case 6:
			for _, f := range s.Flags {
				if flag(f[0], f[1], f[2]) {
					v++
				}
			}
		}
		score := max(s.Min, min(s.Max, s.Max-s.Mult*(s.Par-v)))
		secs = append(secs, SectionScore{s, v, score})
		total += score
	}
	th := rk.Thresholds
	if th == nil {
		th = []int{399, 699, 999, 1000}
	}
	rank := 3
	for i, t := range th {
		if total <= t {
			rank = i
			break
		}
	}
	return rank, total, secs
}

// Rewards are the quest rewards whose condition holds for rank.
func Rewards(q *Quest, rank int, flag func(zone, mapID, ev int) bool) []Reward {
	if q.Rank == nil {
		return nil
	}
	var out []Reward
	for _, rw := range q.Rank.Rewards {
		if len(rw) < 2 {
			continue
		}
		var id int
		var cond []int
		json.Unmarshal(rw[0], &id)
		json.Unmarshal(rw[1], &cond)
		if Evaluate(cond, flag, &rank, nil) {
			if x, ok := q.Rewards[strconv.Itoa(id)]; ok {
				out = append(out, x)
			}
		}
	}
	return out
}

// ResultBlock is the 0x628-byte 0x0C09 result block: rank, participation, sections,
// total, the sections, meseta, items.
func (r *Rules) ResultBlock(rank, total int, secs []SectionScore, meseta int, got []Item) []byte {
	b := make([]byte, 0x628)
	b[0], b[1], b[2] = byte(rank), 100, byte(len(secs))
	binary.LittleEndian.PutUint32(b[4:], uint32(int32(total)))
	binary.LittleEndian.PutUint32(b[8:], uint32(int32(total)))
	for i, s := range secs[:min(len(secs), 6)] {
		o := 0xC + 16*i
		b[o], b[o+1] = byte(int8(s.S.Type)), byte(int8(s.S.Kind))
		binary.LittleEndian.PutUint32(b[o+4:], uint32(int32(s.S.Par)))
		binary.LittleEndian.PutUint32(b[o+8:], uint32(int32(s.Value)))
		binary.LittleEndian.PutUint32(b[o+12:], uint32(int32(s.Score)))
	}
	binary.LittleEndian.PutUint32(b[0x8C:], uint32(meseta))
	b[0x91] = byte(min(len(got), 10))
	for i, s := range got[:min(len(got), 10)] {
		o := 0x98 + 0x44*i
		b[o], b[o+1] = 0xFF, byte(s.Qty)
		rec := append(r.VarRec(&s, r.disc)[4:], r.ConstRec(&s)[:0x20]...)
		copy(b[o+4:o+0x44], rec[:0x40])
	}
	b[0x624] = 1
	if len(got) > 0 {
		b[0x624] |= 4
	}
	return b
}
