// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import (
	"encoding/binary"
	"math"
	"sort"
)

// Combat statuses (PSU / AotI share GA_STATUSCHANGE_TABLE.NBL StatusChangeControlData;
// addresses are PSU's). SE2 bits: 0x1 burn, 0x2 poison, 0x4 virus, 0x8 shock, 0x10
// silence, 0x20 freeze, 0x40 sleep, 0x80 paralysis, 0x100 confusion, 0x200 charm, 0x400
// instant death, 0x1000 / 0x2000 timed (Giresta heal / Megistar drain), 0x4000..0x20000
// debuffs and 0x80000..0x400000 buffs of the four slots (ATP, DFP, TP/MST, ATA/EVP),
// 0x8000000 / 0x10000000 barrier charges.

// Rand is the dice: the server's rolls, scripted in tests.
type Rand interface {
	Intn(n int) int // [0, n)
	Float64() float64
}

var inf = math.Inf(1)

type buff struct {
	end  float64 // monotonic seconds; +Inf: until cured
	mult float64 // slot multiplier (an f32 value)
}

type ailSlot struct {
	next   float64 // next tick; NaN: none
	period float64
	value  int
	mode   int // 1: % of max HP
	hits   int // landed hits that end it
}

// Status is an actor's (player or enemy) active buffs, ailments and barriers.
type Status struct {
	buffs   map[uint32]buff
	ail     map[uint32]*ailSlot
	Barrier [2]int // striking/bullet and technique charges (Dizas, Rentis)
}

func (s *Status) init() {
	if s.buffs == nil {
		s.buffs = map[uint32]buff{}
	}
	if s.ail == nil {
		s.ail = map[uint32]*ailSlot{}
	}
}

// SE2 is the active bits; expired ones are dropped.
func (s *Status) SE2(now float64) uint32 {
	var out uint32
	for b, v := range s.buffs {
		if v.end <= now {
			delete(s.buffs, b)
			continue
		}
		out |= b
	}
	return out
}

// buffMult is slot k's multiplier (0 ATP, 1 DFP, 2 TP/MST, 3 ATA/EVP), 1.0 without one.
func (s *Status) buffMult(k int, now float64) float64 {
	bits := s.SE2(now)
	for _, b := range []uint32{1 << (19 + k), 1 << (14 + k)} {
		if bits&b != 0 {
			return s.buffs[b].mult
		}
	}
	return 1.0
}

// Clear ends every status and barrier (death, 0x1bd610).
func (s *Status) Clear() {
	s.buffs, s.ail = nil, nil
	s.Barrier = [2]int{}
}

// NextEvent is the seconds until the next expiry or tick, or false.
func (s *Status) NextEvent(now float64) (float64, bool) {
	best, ok := inf, false
	for _, v := range s.buffs {
		if v.end != inf && v.end < best {
			best, ok = v.end, true
		}
	}
	for _, a := range s.ail {
		if !math.IsNaN(a.next) && a.next < best {
			best, ok = a.next, true
		}
	}
	return max(0, best-now), ok
}

// --- tables (StatusChangeControlData and the technique records) ---------------------------------------

// buffTable[k][level] = (seconds, f32 bits) of buff slot k; debuffTable the slot's debuffs.
var buffTable = [4][5][2]uint32{
	{{120, 0x3F9EB852}, {180, 0x3FA147AE}, {240, 0x3FA3D70A}, {240, 0x3FA3D70A}, {240, 0x3FA3D70A}},
	{{120, 0x3FF0A3D7}, {180, 0x3FF5C28F}, {240, 0x3FFAE148}, {240, 0x3FFAE148}, {240, 0x3FFAE148}},
	{{120, 0x3F9AE148}, {180, 0x3F9D70A4}, {240, 0x3FA00000}, {240, 0x3FA00000}, {240, 0x3FA00000}},
	{{120, 0x40200000}, {180, 0x40400000}, {240, 0x40600000}, {240, 0x40600000}, {240, 0x40600000}},
}

var debuffTable = [4][5][2]uint32{
	{{30, 0x3F547AE1}, {45, 0x3F4F5C29}, {60, 0x3F4A3D71}, {60, 0x3F4A3D71}, {60, 0x3F4A3D71}},
	{{30, 0x3F19999A}, {45, 0x3F000000}, {60, 0x3ECCCCCD}, {60, 0x3ECCCCCD}, {60, 0x3ECCCCCD}},
	{{30, 0x3F4CCCCD}, {45, 0x3F19999A}, {60, 0x3ECCCCCD}, {60, 0x3ECCCCCD}, {60, 0x3ECCCCCD}},
	{{30, 0x3F000000}, {45, 0x3ECCCCCD}, {60, 0x3E99999A}, {60, 0x3E99999A}, {60, 0x3E99999A}},
}

// timedSecs are the seconds per level of statuses 0x1000 and 0x2000.
var timedSecs = []struct {
	bit  uint32
	secs [5]int
}{{0x1000, [5]int{20, 24, 28, 28, 28}}, {0x2000, [5]int{20, 35, 50, 50, 50}}}

// ailments[i][level] = seconds, tick period, value, chance %, hits that end it, value
// mode (1 % of max HP) of status 1 << i.
var ailments = [14][5][6]int{
	{{11, 2, 2, 24, 0, 1}, {11, 2, 3, 29, 0, 1}, {11, 2, 4, 35, 0, 1}, {11, 2, 5, 42, 0, 1}, {11, 2, 6, 50, 0, 1}},
	{{15, 3, 2, 24, 0, 1}, {21, 3, 2, 29, 0, 1}, {27, 3, 2, 35, 0, 1}, {33, 3, 2, 42, 0, 1}, {39, 3, 2, 50, 0, 1}},
	{{20, 5, 6, 24, 0, 1}, {25, 5, 6, 29, 0, 1}, {30, 5, 6, 35, 0, 1}, {35, 5, 6, 42, 0, 1}, {40, 5, 6, 50, 0, 1}},
	{{5, 0, 0, 24, 0, 1}, {5, 0, 0, 29, 0, 1}, {6, 0, 0, 35, 0, 1}, {6, 0, 0, 42, 0, 1}, {7, 0, 0, 50, 0, 1}},
	{{5, 0, 0, 24, 0, 1}, {5, 0, 0, 29, 0, 1}, {6, 0, 0, 35, 0, 1}, {6, 0, 0, 42, 0, 1}, {7, 0, 0, 50, 0, 1}},
	{{3, 0, 0, 24, 3, 2}, {4, 0, 0, 29, 4, 2}, {5, 0, 0, 35, 5, 2}, {6, 0, 0, 42, 6, 2}, {7, 0, 0, 50, 7, 2}},
	{{4, 0, 40, 24, 2, 2}, {5, 0, 50, 29, 3, 2}, {6, 0, 55, 35, 4, 2}, {8, 0, 60, 42, 5, 2}, {10, 0, 60, 50, 6, 2}},
	{{3, 0, 0, 24, 100, 2}, {3, 0, 0, 29, 100, 2}, {3, 0, 0, 35, 100, 2}, {3, 0, 0, 42, 100, 2}, {3, 0, 0, 50, 100, 2}},
	{{5, 0, 0, 24, 3, 2}, {5, 0, 0, 29, 4, 2}, {6, 0, 0, 35, 5, 2}, {6, 0, 0, 42, 6, 2}, {7, 0, 0, 50, 7, 2}},
	{{5, 0, 0, 24, 1, 2}, {5, 0, 0, 29, 1, 2}, {6, 0, 0, 35, 1, 2}, {8, 0, 0, 42, 1, 2}, {10, 0, 0, 50, 1, 2}},
	{{0, 0, 0, 13, 0, 2}, {0, 0, 0, 16, 0, 2}, {0, 0, 0, 19, 0, 2}, {0, 0, 0, 25, 0, 2}, {0, 0, 0, 30, 0, 2}},
	{{0, 0, 0, 15, 0, 2}, {0, 0, 0, 20, 0, 2}, {0, 0, 0, 25, 0, 2}, {0, 0, 0, 30, 0, 2}, {0, 0, 0, 35, 0, 2}},
	{{20, 4, 8, 100, 0, 1}, {24, 4, 9, 100, 0, 1}, {28, 4, 10, 100, 0, 1}, {28, 4, 10, 100, 0, 1}, {28, 4, 10, 100, 0, 1}},
	{{20, 5, 7, 100, 0, 1}, {35, 7, 7, 100, 0, 1}, {50, 9, 7, 100, 0, 1}, {50, 9, 7, 100, 0, 1}, {50, 9, 7, 100, 0, 1}},
}

// ailBlock[i]: status i can only start while none of these SE2 bits is set.
var ailBlock = [14]uint32{0x426, 0x404, 0x400, 0x408, 0x410, 0x421, 0x440, 0x480, 0x700, 0x700, 0x400, 0xC00, 0, 0}

// effectSE2 maps a GA effect id to its SE2 bits.
var effectSE2 = []uint32{0, 0x1, 0x2, 0x4, 0x8, 0x10, 0x20, 0x40, 0x80, 0x100, 0x200, 0x400, 0x1000000, 0x2000000,
	0x4000000, 0x2000, 0x1000, 0x80000, 0x4000, 0x100000, 0x8000, 0x200000, 0x10000, 0x400000, 0x20000, 0x2000000,
	0x802000, 0x8000000, 0x10000000}

func f32(v float64) float64 { return float64(float32(v)) }

func f32bits(b uint32) float64 { return float64(math.Float32frombits(b)) }

func scaled(v int, m float64) int {
	if m == 1.0 {
		return v
	}
	return int(f32(f32(float64(v)) * m))
}

// --- buffs, cures, ailments --------------------------------------------------------------------------

func (s *Status) startTicks(bit uint32, rec [6]int, now float64) {
	s.init()
	next := math.NaN()
	if rec[1] != 0 {
		next = now + float64(rec[1])
	}
	s.ail[bit] = &ailSlot{next: next, period: float64(rec[1]), value: rec[2], mode: rec[5], hits: rec[4]}
}

// ApplyBuffs (0x1bdc60): 0x800000 means all four buffs; each sets its slot and clears its
// debuff; 0x1000 / 0x2000 their timed statuses. True when anything was applied.
func (s *Status) ApplyBuffs(bits uint32, level int, now float64) bool {
	if bits&0x800000 != 0 {
		bits |= 0x780000
	}
	s.init()
	done := false
	for k := range 4 {
		b := uint32(1) << (19 + k)
		if bits&b != 0 && level < len(buffTable[k]) {
			s.buffs[b] = buff{now + float64(buffTable[k][level][0]), f32bits(buffTable[k][level][1])}
			delete(s.buffs, 1<<(14+k))
			done = true
		}
	}
	for _, t := range timedSecs {
		if bits&t.bit != 0 && level < len(t.secs) {
			s.buffs[t.bit] = buff{now + float64(t.secs[level]), 1.0}
			s.startTicks(t.bit, ailments[bitLen(t.bit)-1][level], now)
			done = true
		}
	}
	return done
}

// Cure (0x1be790): the bits' ailments and debuffs go.
func (s *Status) Cure(bits uint32, now float64) bool {
	cured := s.SE2(now) & bits & 0x2003C3FF
	for b := range s.buffs {
		if b&cured != 0 {
			delete(s.buffs, b)
			delete(s.ail, b)
		}
	}
	return cured != 0
}

// chanceRoll (0x1bd900): rand(100) < mult * (base - target STA) / 100, never when STA >= base.
func chanceRoll(rng Rand, base, mult, sta int, sure bool) bool {
	if sure {
		return true
	}
	r := rng.Intn(100)
	if sta >= base {
		return false
	}
	return r < ((mult&0xFFFF)*(base-sta)/100)&0xFFFF
}

// Inflict tries block D's status (0x1bf8f0: ailments, then debuffs). Returns the bits
// that started.
func (s *Status) Inflict(rng Rand, bits uint32, level, mult, sta int, sure bool, immune uint32, now float64) uint32 {
	s.init()
	bits &^= immune
	cur, got := s.SE2(now), uint32(0)
	able := func(i int) bool { return cur&ailBlock[i] == 0 && level >= 0 && level < len(ailments[i]) }
	put := func(i int, clear uint32) uint32 {
		rec := ailments[i][level]
		for b := range s.buffs {
			if b&clear != 0 {
				delete(s.buffs, b)
				delete(s.ail, b)
			}
		}
		b := uint32(1) << i
		end := inf
		if rec[0] != 0 {
			end = now + float64(rec[0])
		}
		s.buffs[b] = buff{end, 1.0}
		if rec[1] != 0 || rec[4] != 0 {
			s.startTicks(b, rec, now)
		}
		cur = cur&^clear | b
		return b
	}
	if bits&0x400 != 0 && able(10) && chanceRoll(rng, ailments[10][level][3], mult, sta, sure) {
		return 0x400
	}
	for _, g := range []struct {
		idx   []int
		clear uint32
	}{{[]int{0, 1, 2}, 0x7}, {[]int{3}, 0}, {[]int{4}, 0}, {[]int{5, 6, 7, 8, 9}, 0x3E0}, {[]int{12}, 0}, {[]int{13}, 0}} {
		for _, i := range g.idx {
			if bits&(1<<i) != 0 && able(i) && chanceRoll(rng, ailments[i][level][3], mult, sta, sure) {
				got |= put(i, g.clear)
				break
			}
		}
	}
	for k, b := range []uint32{0x8000000, 0x10000000} {
		if bits&b != 0 && s.Barrier[k] < level && level < 5 {
			s.Barrier[k] = level
			got |= b
		}
	}
	if bits&0x40000 != 0 {
		bits |= 0x3C000
	}
	for k := range 4 {
		b := uint32(1) << (14 + k)
		if bits&b != 0 && level >= 0 && level < len(debuffTable[k]) && chanceRoll(rng, 100, mult, sta, sure) {
			s.buffs[b] = buff{now + float64(debuffTable[k][level][0]), f32bits(debuffTable[k][level][1])}
			delete(s.buffs, 1<<(19+k))
			got |= b
		}
	}
	return got
}

// breakOnHit (0x1be5b0): a landed damaging hit counts down the hit-ended statuses.
func (s *Status) breakOnHit() {
	for b, a := range s.ail {
		if _, on := s.buffs[b]; a.hits != 0 && on {
			if a.hits--; a.hits <= 0 {
				delete(s.buffs, b)
				delete(s.ail, b)
			}
		}
	}
}

// Tick is one status tick or expiry: "dmg", "heal" or "end" with its amount (the bits
// that ended for "end") and the HP after it.
type Tick struct {
	Kind   string
	Amount int
	HP     int
}

// Step runs the due ticks and expiries (0x1bf040 damage never below 1 HP, 0x1beac0 heal).
func (s *Status) Step(hp, maxHP int, now float64) ([]Tick, int) {
	var before uint32
	for b := range s.buffs {
		before |= b
	}
	var out []Tick
	bits := make([]uint32, 0, len(s.ail))
	for b := range s.ail {
		bits = append(bits, b)
	}
	sort.Slice(bits, func(i, j int) bool { return bits[i] < bits[j] })
	for _, b := range bits {
		a := s.ail[b]
		bf, on := s.buffs[b]
		if math.IsNaN(a.next) || a.next > now || !on || bf.end < a.next {
			continue
		}
		a.next += a.period
		amount := a.value
		if a.mode == 1 {
			amount = maxHP * a.value / 100
		}
		amount = max(1, min(amount, 0xFFFF))
		if b == 0x1000 {
			if amount = min(amount, maxHP-hp); amount > 0 && hp > 0 {
				hp += amount
				out = append(out, Tick{"heal", amount, hp})
			}
		} else if hp > 1 {
			amount = min(amount, hp-1)
			hp -= amount
			out = append(out, Tick{"dmg", amount, hp})
		}
	}
	for b, v := range s.buffs {
		if v.end <= now {
			delete(s.buffs, b)
		}
	}
	after := s.SE2(now)
	for b := range s.ail {
		if _, on := s.buffs[b]; !on {
			delete(s.ail, b)
		}
	}
	if before&^after != 0 {
		out = append(out, Tick{"end", int(before &^ after), hp})
	}
	return out, hp
}

// --- elements and damage -----------------------------------------------------------------------------

var elementCode = [7]uint8{0, 1, 9, 2, 10, 4, 12} // item / school element 1..6 -> attack and defence code

// elementMult (0x1bd050): the same pair with the other polarity is weak, the same element
// resists; each % counts up to 100. AotI scales by 0.66.
func elementMult(acode, apct, dcode, dpct uint8, disc string) float64 {
	if acode&dcode&7 == 0 {
		return 1.0
	}
	s := 1.0
	if disc == AotI {
		s = 0.66
	}
	a := f32(f32(float64(min(apct, 100)))/100.0) + f32(f32(float64(min(dpct, 100)))/100.0)
	if (acode^dcode)&8 != 0 {
		return f32(1.0 + s*a)
	}
	return f32(1.0 - s*a)
}

// Target is what damage needs of a defender.
type Target interface {
	status() *Status
	defenceElement() (code, pct uint8)
}

// Damage is (damage, SE1) for attack block D against a target's 8 stats (client
// 0x1bfc30). Enemy attacks arrive with power 0; attacker (the server's enemy stats) stands
// in. A damaging block without 0x1000 is status only.
func Damage(rng Rand, d []byte, stats [8]int, hp int, attacker *[8]int, t Target, disc string, now float64) (int, uint32) {
	flags := binary.LittleEndian.Uint32(d)
	acc := int(int16(binary.LittleEndian.Uint16(d[8:])))
	bonus := int(int16(binary.LittleEndian.Uint16(d[0xA:])))
	power := int(int16(binary.LittleEndian.Uint16(d[0xC:])))
	if flags&0x200 == 0 || flags&0x1000 == 0 {
		return 0, 0
	}
	if attacker != nil && power <= 0 {
		if flags&0x8 != 0 {
			power = attacker[7]
		} else {
			power = attacker[0]
		}
		if acc == 0 {
			acc = attacker[2]
		}
	}
	var se2 uint32
	if t != nil {
		se2 = t.status().SE2(now)
	}
	if flags&0x4000 == 0 && se2&0xE0 == 0 && (acc <= 0 || rng.Intn(101) <= stats[3]*10/acc) {
		return 0, 0x10
	}
	if t != nil {
		b := &t.status().Barrier
		k := -1
		if flags&0xC == 0xC {
			k = 1
		} else if flags&0xC != 0 {
			k = 0
		}
		if k >= 0 && b[k] > 0 {
			b[k]--
			return 0, 0
		}
	}
	mult := 1.0
	if t != nil && d[0x10] != 0 {
		code, pct := t.defenceElement()
		mult = elementMult(d[0x10], d[0x11], code, pct, disc)
	}
	def := stats[1]
	if flags&0x8 != 0 {
		def = stats[7]
	}
	var dmg int
	switch {
	case flags&0x40000 != 0:
		dmg = floorDiv(hp*bonus, 100)
	case mult != 1.0:
		base := f32(f32(float64(power+bonus)) * mult)
		if flags&0x10000 == 0 {
			base = f32(base - float64(def))
		}
		if base > 0 {
			dmg = int(f32(base / 5.0))
		}
	default:
		base := power + bonus
		if flags&0x10000 == 0 {
			base -= def
		}
		if base > 0 {
			dmg = base / 5
		}
	}
	se := uint32(0x1)
	if dmg != 0 && rng.Float64() < 0.05 {
		dmg, se = dmg*3/2, se|0x400000
	}
	return min(dmg, 0xFFFF), se
}

// floorDiv is Python's // for ints.
func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func bitLen(v uint32) int {
	n := 0
	for ; v != 0; v >>= 1 {
		n++
	}
	return n
}

// TickEntries are the 0x0E07 entries for an actor's status ticks and expiries, as the
// offline room builds them (0x1bb3a0): attacker 0xFFFFFFFF, both actor blocks the
// actor's own.
func TickEntries(actor uint32, s *Status, ticks []Tick, now float64) [][]byte {
	se1 := map[string]uint32{"dmg": 0x1, "heal": 0x80000, "end": 0}
	var out [][]byte
	for _, t := range ticks {
		amount, bits := t.Amount, 0
		if t.Kind == "end" {
			amount, bits = 0, t.Amount
		}
		blk := actorBlock(se1[t.Kind], s.SE2(now), t.HP, s.Barrier)
		e := binary.LittleEndian.AppendUint32([]byte{1, 0, 0, 0}, uint32(amount))
		e = append(e, make([]byte, 12)...)
		e = binary.LittleEndian.AppendUint32(e, 0xFFFFFFFF)
		e = binary.LittleEndian.AppendUint32(e, actor)
		e = append(e, make([]byte, 8)...)
		e = append(append(append(e, blk...), blk...), make([]byte, 16)...)
		out = append(out, append(e, tickBlock(t.Kind, t.Amount, uint32(bits))...))
	}
	return out
}

// tickBlock is a tick's D: damage 0x115210 with the amount as bonus, heal 0x1400, an
// expiry 0x102400 with the bits that ended.
func tickBlock(kind string, amount int, bits uint32) []byte {
	d := make([]byte, 20)
	switch kind {
	case "dmg", "heal":
		f := uint32(0x115210)
		if kind == "heal" {
			f = 0x1400
		}
		binary.LittleEndian.PutUint32(d, f)
		binary.LittleEndian.PutUint16(d[0xA:], uint16(int16(min(amount, 0x7FFF))))
	default:
		binary.LittleEndian.PutUint32(d, 0x102400)
		binary.LittleEndian.PutUint32(d[4:], bits)
	}
	d[0xE] = 100
	return d
}
