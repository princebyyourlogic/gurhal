// SPDX-License-Identifier: GPL-3.0-or-later

package rules

import "strconv"

// Enemy stats (TServerActorEnemy 0x282040): the level row of EnemyLevelBaseParam times
// the variant's Param multipliers (enemies.json); the MST slot gets the TP value.

type enemyTables struct {
	Variants map[string]struct {
		HP, ATP, DFP, ATA, EVP, STA, S5, TP, MST, EXP float64
		X24                                           *float64 `json:"x24"`
		Category                                      int      `json:"category"`
		Bits                                          []struct {
			EXPBonus bool `json:"exp_bonus"`
		} `json:"variant_bits"`
	} `json:"variants"`
	Levels    [][]float64 `json:"levels"`
	EXPBits   []float64   `json:"exp_bits"`
	EXPGlobal float64     `json:"exp_global"`
}

// EnemySpec is one enemy of a mission zone's gateways (missions.json).
type EnemySpec struct {
	ID      uint32 `json:"id"`
	Wave    int    `json:"wave"`
	Type    int    `json:"type"`
	Level   int    `json:"level"`
	Variant int    `json:"variant"`
	Name    string `json:"name"`
	Bits    []int  `json:"bits"`
	Gateway uint32 `json:"-"`
	Map     uint32 `json:"-"`
}

// Enemy is one live enemy, owned by its zone.
type Enemy struct {
	ID         uint32
	Spec       *EnemySpec
	Level      int
	MaxHP, HP  int
	Stats      [8]int // ATP DFP ATA EVP STA S5 TP MST
	EXP        int
	Hitters    map[uint32]bool // GIDs of players that hit it
	Dead       bool
	Element    uint8
	ElementPct uint8
	Status     Status
	Killer     uint32
	KillPos    []byte // 12 bytes xyz
}

func (e *Enemy) status() *Status                { return &e.Status }
func (e *Enemy) defenceElement() (uint8, uint8) { return e.Element, e.ElementPct }
func (e *Enemy) stats(now float64) [8]int       { return buffedStats(&e.Status, e.Stats, now) }

func clampIndex(level, n int) int { return max(1, min(level, n-1)) }

// NewEnemy is an enemy from its spec, or (no spec: not in the set data) the generic row
// at the player's level.
func (r *Rules) NewEnemy(id uint32, spec *EnemySpec, level int) *Enemy {
	e := &Enemy{ID: id, Spec: spec, Hitters: map[uint32]bool{}}
	if spec != nil && r.enemies != nil {
		e.Level = spec.Level
		v := r.enemies.Variants[strconv.Itoa(spec.Variant)]
		row := r.enemies.Levels[clampIndex(spec.Level, len(r.enemies.Levels))]
		e.MaxHP = int(max(1.0, row[0]*v.HP))
		mults := []float64{v.ATP, v.DFP, v.ATA, v.EVP, v.STA, v.S5, v.TP, v.MST}
		for i, m := range mults {
			e.Stats[i] = max(0, min(0x7FFF, int(row[1+i]*m)))
		}
		e.Stats[7] = e.Stats[6]
		exp := max(1, int(row[10]*v.EXP))
		mult := r.enemies.EXPGlobal
		for _, b := range spec.Bits {
			if b < len(v.Bits) && v.Bits[b].EXPBonus {
				mult *= r.enemies.EXPBits[b]
			}
		}
		e.EXP = int(float64(exp) * mult)
		if v.Category&7 != 0 {
			e.Element = uint8(v.Category)
		}
		x24 := 1.0
		if v.X24 != nil {
			x24 = *v.X24
		}
		if len(row) > 9 {
			e.ElementPct = uint8(max(0, min(127, int(row[9]*x24))))
		}
	} else {
		row := r.enemyLevels[clampIndex(level, len(r.enemyLevels))]
		e.Level = level
		e.MaxHP = max(1, row[0])
		copy(e.Stats[:], row[1:9])
		e.EXP = row[10]
	}
	e.HP = e.MaxHP
	return e
}

// KillEXP is who gets EXP for a dead enemy: the killer all of it, other hitters 70 %.
func KillEXP(e *Enemy) map[uint32]int {
	out := map[uint32]int{e.Killer: e.EXP}
	for g := range e.Hitters {
		if g != e.Killer {
			out[g] = e.EXP * 7 / 10
		}
	}
	return out
}
