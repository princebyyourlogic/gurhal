// SPDX-License-Identifier: GPL-3.0-or-later

// Package gamedata loads the data directory the extractor builds from the owner's discs
// (prettystellaruniverse's `python -m extract`; nothing of the discs ships with psud):
//
//	.built               which discs were built, and when
//	nbl/*.nbl            archives the server sends as they are (quests, zones, counters)
//	nbl/missions.json    mission and zone tables
//	gamedata/*.json      items, enemies, drops, levels, shops, synthesis, ...
//	captures/*.bin       tables captured from the original servers
//	nbl/season-staging/<season>/*.nbl  a season's lobby archives (extract/seasonbuild.py)
//
// A disc other than PSU may have its own copy of any file under <dir>/<disc>/, which wins
// over the shared one. Everything is read into memory once and never changes.
package gamedata

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The files the services need. Load fails, naming what is missing, without them.
var (
	requiredTables   = []string{"drops", "enemies", "enemylevel", "items", "pa", "paparam", "parts", "plylevel", "ppregen", "shops", "synthesis", "weapons", "missions", "grind", "lumilass", "probo"}
	requiredCaptures = []string{"packet170c.bin", "ppcube.bin"}
)

const sharedDisc = "psu" // the disc whose files sit at the top level

type Built struct {
	Discs   map[string]string `json:"discs"` // disc key -> serial
	BuiltAt string            `json:"built_at"`
}

type Data struct {
	Built    Built
	nbl      map[string][]byte          // "name" or "<disc>/name"
	tables   map[string]json.RawMessage // "items" or "<disc>/items"
	captures map[string][]byte
	seasons  map[string]map[string][]byte // season -> "name" or "<disc>/name"
}

// Load reads dir and checks it was built for every disc in discs (nil: whatever it was
// built for; see Discs).
func Load(dir string, discs []string) (*Data, error) {
	d := &Data{nbl: map[string][]byte{}, tables: map[string]json.RawMessage{}, captures: map[string][]byte{}}
	raw, err := os.ReadFile(filepath.Join(dir, ".built"))
	if err != nil {
		return nil, fmt.Errorf("gamedata: %s has no built data (run make import): %w", dir, err)
	}
	if err := json.Unmarshal(raw, &d.Built); err != nil {
		return nil, fmt.Errorf("gamedata: .built: %w", err)
	}
	for _, disc := range discs {
		if _, ok := d.Built.Discs[disc]; !ok {
			return nil, fmt.Errorf("gamedata: %s was built without the %s disc (run make import with it)", dir, disc)
		}
	}

	overlays := []string{""}
	for disc := range d.Built.Discs {
		if disc != sharedDisc {
			overlays = append(overlays, disc+"/")
		}
	}
	for _, o := range overlays {
		if err := readDir(filepath.Join(dir, "nbl", o), ".nbl", func(name string, b []byte) { d.nbl[o+name] = b }); err != nil {
			return nil, err
		}
		if err := readDir(filepath.Join(dir, "gamedata", o), ".json", func(name string, b []byte) {
			d.tables[o+strings.TrimSuffix(name, ".json")] = b
		}); err != nil {
			return nil, err
		}
		if b, err := os.ReadFile(filepath.Join(dir, "nbl", o, "missions.json")); err == nil {
			d.tables[o+"missions"] = b
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := readDir(filepath.Join(dir, "captures"), ".bin", func(name string, b []byte) { d.captures[name] = b }); err != nil {
		return nil, err
	}
	if err := d.loadSeasons(filepath.Join(dir, "nbl", "season-staging"), overlays); err != nil {
		return nil, err
	}

	var missing []string
	for _, t := range requiredTables {
		if _, ok := d.tables[t]; !ok {
			missing = append(missing, t+".json")
		}
	}
	for _, c := range requiredCaptures {
		if _, ok := d.captures[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("gamedata: %s is missing %s (run make import again)", dir, strings.Join(missing, ", "))
	}
	return d, nil
}

// readDir hands every file in dir with the extension to add; a missing dir is empty.
func readDir(dir, ext string, add func(name string, b []byte)) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gamedata: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ext {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("gamedata: %w", err)
		}
		add(e.Name(), b)
	}
	return nil
}

func (d *Data) loadSeasons(dir string, overlays []string) error {
	d.seasons = map[string]map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gamedata: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files := map[string][]byte{}
		for _, o := range overlays {
			if err := readDir(filepath.Join(dir, e.Name(), o), ".nbl", func(name string, b []byte) { files[o+name] = b }); err != nil {
				return err
			}
		}
		if len(files) > 0 {
			d.seasons[e.Name()] = files
		}
	}
	return nil
}

// SeasonNBL returns a season's staged copy of an archive for a disc.
func (d *Data) SeasonNBL(season, disc, name string) ([]byte, bool) {
	files := d.seasons[season]
	if disc != sharedDisc {
		if b, ok := files[disc+"/"+name]; ok {
			return b, true
		}
	}
	b, ok := files[name]
	return b, ok
}

// Seasons are the seasons with staged archives, sorted.
func (d *Data) Seasons() []string {
	out := make([]string, 0, len(d.seasons))
	for s := range d.seasons {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// NBL returns an archive by file name for a disc.
func (d *Data) NBL(disc, name string) ([]byte, bool) {
	if disc != sharedDisc {
		if b, ok := d.nbl[disc+"/"+name]; ok {
			return b, true
		}
	}
	b, ok := d.nbl[name]
	return b, ok
}

// Table decodes a JSON table ("items", "missions", ...) for a disc into v.
func (d *Data) Table(disc, name string, v any) error {
	raw, ok := d.tables[disc+"/"+name]
	if !ok || disc == sharedDisc {
		if raw, ok = d.tables[name]; !ok {
			return fmt.Errorf("gamedata: no table %s", name)
		}
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("gamedata: %s: %w", name, err)
	}
	return nil
}

// Capture returns a captured table by file name.
func (d *Data) Capture(name string) ([]byte, bool) {
	b, ok := d.captures[name]
	return b, ok
}

// Discs lists the discs the data was built for, sorted.
func (d *Data) Discs() []string {
	var discs []string
	for k := range d.Built.Discs {
		discs = append(discs, k)
	}
	slices.Sort(discs)
	return discs
}

// Counts summarizes what was loaded, for the startup log.
func (d *Data) Counts() (nbls, tables int) { return len(d.nbl), len(d.tables) }
