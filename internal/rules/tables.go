// SPDX-License-Identifier: GPL-3.0-or-later

// Package rules is the game logic the PS2 client expects the server to own: levels and
// stats, character records, inventory records, and what one disc may show a player of
// the other. Pure functions over the disc's tables; no I/O, no wire framing.
package rules

import (
	"fmt"

	"github.com/princebyyourlogic/gurhal/internal/gamedata"
)

const (
	PSU  = "psu"
	AotI = "aoti"

	psuClasses = 10 // a PSU session offers types 0..9
	psuLevels  = 100
)

// Set holds the rules of both discs. Without AotI data its tables fall back to the
// shared (PSU) files, as the Python server's do.
type Set struct {
	psu, aoti *Rules
}

// Rules are one disc's tables.
type Rules struct {
	disc    string
	set     *Set
	ply     plyTables
	items   map[ItemID]*ItemDef
	pa      map[ItemID]paParam
	bullets [10][]byte
	parts   partsTable
	partSet map[uint32]bool

	enemies      *enemyTables // nil: generic rows only
	enemyLevels  [][]int
	paThresholds map[ItemID][]int // PA (level 0) -> EXP per level-up
	paTop        int              // the longest PA threshold list (PSU 29)
	missions     *MissionData
	drops        *dropTables
	weapons      map[string]weaponDef
	ppRegen      map[ItemID]int
	shops        *shopTables
	grind        *grindTables
	lumilass     [][][]uint32 // [race*2+gender] lists of part words
	probo        *proboTables
	synth        *synthTables
}

type weaponDef struct {
	Sound    int        `json:"sound"`
	Targets  int        `json:"targets"`
	Hitboxes []HexBytes `json:"hitboxes"`
}

type plyTables struct {
	Levels       [][][]int `json:"levels"`       // [race*2+gender][level-1] = EXP, HP, 8 stats
	Types        [][][]int `json:"types"`        // [type][type level-1] = type EXP, HP %, 8 stat %, 4 race %
	Requirements [][]int8  `json:"requirements"` // [type] = type level needed per type
	TypeCaps     []int     `json:"type_caps"`
}

// ItemDef is an items.json entry.
type ItemDef struct {
	ID        ItemID   `json:"id"`
	Cat       int      `json:"cat"`
	Name      string   `json:"name"`
	Price     uint32   `json:"price"`
	Consts    HexBytes `json:"consts"` // 20-byte constants block
	Rarity0   uint8    `json:"rarity0"`
	Rec       HexBytes `json:"rec"` // traps, line shields, units: their table record
	ATP       int      `json:"atp"`
	ATA       int      `json:"ata"`
	ValueBase int      `json:"value_base"` // NPC shop buy price
}

type paParam struct {
	Base []HexBytes `json:"base"`
	Pct  [][2]int16 `json:"pct"`
}

type partsTable struct {
	Words    []uint32   `json:"words"`
	Defaults [][]uint32 `json:"defaults"` // [race*2+gender][slot]
}

// Load builds both discs' rules from the game data.
func Load(d *gamedata.Data) (*Set, error) {
	s := &Set{}
	var err error
	if s.psu, err = load(d, PSU, s); err != nil {
		return nil, err
	}
	if s.aoti, err = load(d, AotI, s); err != nil {
		return nil, err
	}
	return s, nil
}

func load(d *gamedata.Data, disc string, s *Set) (*Rules, error) {
	r := &Rules{disc: disc, set: s}
	var items map[string]*ItemDef
	var pp struct {
		Bullets []HexBytes         `json:"bullet_default"`
		PA      map[string]paParam `json:"pa"`
	}
	var levels struct {
		Levels [][]int `json:"levels"`
	}
	var pa map[string]struct {
		Thr []int `json:"thr"`
	}
	var regen map[string]int
	var lumi struct {
		Selectable [][][]uint32 `json:"selectable"`
	}
	r.missions, r.drops, r.enemies = &MissionData{}, &dropTables{}, &enemyTables{}
	r.shops, r.grind, r.probo, r.synth = &shopTables{}, &grindTables{}, &proboTables{}, &synthTables{}
	for name, v := range map[string]any{"shops": r.shops, "grind": r.grind, "lumilass": &lumi, "probo": r.probo,
		"synthesis": r.synth, "plylevel": &r.ply, "items": &items, "paparam": &pp, "parts": &r.parts,
		"enemylevel": &levels, "enemies": r.enemies, "pa": &pa, "missions": r.missions, "drops": r.drops,
		"weapons": &r.weapons, "ppregen": &regen} {
		if err := d.Table(disc, name, v); err != nil {
			return nil, err
		}
	}
	r.enemyLevels, r.lumilass = levels.Levels, lumi.Selectable
	if len(r.enemies.Variants) == 0 {
		r.enemies = nil
	}
	r.paThresholds, r.paTop = map[ItemID][]int{}, 0
	for k, v := range pa {
		var id ItemID
		if id.UnmarshalText([]byte(k)) == nil {
			r.paThresholds[id] = v.Thr
			r.paTop = max(r.paTop, len(v.Thr))
		}
	}
	if r.paTop == 0 {
		r.paTop = 29
	}
	r.ppRegen = map[ItemID]int{}
	for k, v := range regen {
		var id ItemID
		if id.UnmarshalText([]byte(k)) == nil {
			r.ppRegen[id] = v
		}
	}
	r.items = make(map[ItemID]*ItemDef, len(items))
	for _, it := range items {
		r.items[it.ID] = it
	}
	r.pa = map[ItemID]paParam{}
	for k, v := range pp.PA {
		var id ItemID
		if err := id.UnmarshalText([]byte(k)); err != nil {
			return nil, fmt.Errorf("rules: paparam: %w", err)
		}
		r.pa[id] = v
	}
	for i := range min(len(pp.Bullets), len(r.bullets)) {
		r.bullets[i] = pp.Bullets[i]
	}
	if len(r.parts.Words) > 0 {
		r.partSet = make(map[uint32]bool, len(r.parts.Words))
		for _, w := range r.parts.Words {
			r.partSet[w] = true
		}
	}
	if len(r.ply.Levels) < 8 || len(r.ply.Types) < psuClasses {
		return nil, fmt.Errorf("rules: %s plylevel table is incomplete", disc)
	}
	return r, nil
}

// For returns a disc's rules ("psu" or "aoti").
func (s *Set) For(disc string) *Rules {
	if disc == AotI {
		return s.aoti
	}
	return s.psu
}

func (r *Rules) Disc() string { return r.disc }

// Item looks up an item definition.
func (r *Rules) Item(id ItemID) (*ItemDef, bool) {
	it, ok := r.items[id]
	return it, ok
}

// ClassCount: types 0..n-1 exist for this disc (PSU 10; AotI 15, Acromaster was never retail).
func (r *Rules) ClassCount() int {
	if r.disc == AotI {
		return 15
	}
	return psuClasses
}

// typeTable is the table that defines class cls: AotI's for its own classes 10..15.
func (r *Rules) typeTable(cls int) *plyTables {
	if cls >= psuClasses {
		return &r.set.aoti.ply
	}
	return &r.ply
}

func (r *Rules) typeRows(cls int) [][]int { return r.typeTable(cls).Types[cls] }

// retailCurve: both discs' tables are the retail ones with the same type EXP for 0..9,
// the condition for marking a state's type EXP as on the retail curve.
func (s *Set) retailCurve() bool {
	p, a := s.psu.ply, s.aoti.ply
	if p.TypeCaps == nil || a.TypeCaps == nil {
		return false
	}
	for c := range psuClasses {
		if len(p.Types[c]) != len(a.Types[c]) {
			return false
		}
		for i := range p.Types[c] {
			if p.Types[c][i][0] != a.Types[c][i][0] {
				return false
			}
		}
	}
	return true
}
