// SPDX-License-Identifier: GPL-3.0-or-later

package gamedata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes a minimal synthetic data directory (no disc content).
func fixture(t *testing.T, discs string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".built", `{"discs": {`+discs+`}, "built_at": "2026-11-12T00:00:00Z"}`)
	for _, tb := range requiredTables {
		if tb != "missions" {
			write("gamedata/"+tb+".json", `{"from": "shared"}`)
		}
	}
	write("nbl/missions.json", `{"from": "shared"}`)
	write("gamedata/aoti/items.json", `{"from": "aoti"}`)
	write("nbl/zone-1.nbl", "shared zone")
	write("nbl/aoti/zone-1.nbl", "aoti zone")
	write("nbl/season-staging/snow/x.nbl", "snow x")
	write("nbl/season-staging/snow/aoti/x.nbl", "aoti snow x")
	for _, c := range requiredCaptures {
		write("captures/"+c, "cap")
	}
	return dir
}

func TestOverlays(t *testing.T) {
	d, err := Load(fixture(t, `"psu": "SLUS-21194", "aoti": "SLUS-21631"`), []string{"psu", "aoti"})
	if err != nil {
		t.Fatal(err)
	}
	for disc, want := range map[string]string{"psu": "shared zone", "aoti": "aoti zone"} {
		if b, ok := d.NBL(disc, "zone-1.nbl"); !ok || string(b) != want {
			t.Errorf("%s zone = %q", disc, b)
		}
	}
	var v struct{ From string }
	for disc, want := range map[string]string{"psu": "shared", "aoti": "aoti"} {
		if err := d.Table(disc, "items", &v); err != nil || v.From != want {
			t.Errorf("%s items from %q (%v)", disc, v.From, err)
		}
	}
	if err := d.Table("aoti", "missions", &v); err != nil || v.From != "shared" {
		t.Errorf("aoti falls back to the shared missions: %q %v", v.From, err)
	}
	if _, ok := d.NBL("psu", "x.nbl"); ok {
		t.Error("season staging files are not top-level archives")
	}
	for disc, want := range map[string]string{"psu": "snow x", "aoti": "aoti snow x"} {
		if b, ok := d.SeasonNBL("snow", disc, "x.nbl"); !ok || string(b) != want {
			t.Errorf("%s staged x = %q", disc, b)
		}
	}
	if s := d.Seasons(); len(s) != 1 || s[0] != "snow" {
		t.Errorf("seasons %q", s)
	}
}

func TestRefusesDataWithoutTheDisc(t *testing.T) {
	_, err := Load(fixture(t, `"psu": "SLUS-21194"`), []string{"psu", "aoti"})
	if err == nil || !strings.Contains(err.Error(), "aoti") {
		t.Fatalf("err = %v", err)
	}
}

func TestNamesMissingFiles(t *testing.T) {
	dir := fixture(t, `"psu": "SLUS-21194"`)
	os.Remove(filepath.Join(dir, "gamedata", "shops.json"))
	if _, err := Load(dir, []string{"psu"}); err == nil || !strings.Contains(err.Error(), "shops.json") {
		t.Fatalf("err = %v", err)
	}
}

// PSU_TEST_DATA points at a real data directory built from your discs.
func TestRealData(t *testing.T) {
	dir := os.Getenv("PSU_TEST_DATA")
	if dir == "" {
		t.Skip("PSU_TEST_DATA not set")
	}
	d, err := Load(dir, []string{"psu"})
	if err != nil {
		t.Fatal(err)
	}
	var items map[string]struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := d.Table("psu", "items", &items); err != nil || len(items) == 0 {
		t.Fatalf("items: %d, %v", len(items), err)
	}
	if _, ok := d.NBL("psu", "quest-0.nbl"); !ok {
		t.Fatal("quest-0.nbl missing")
	}
	n, tb := d.Counts()
	t.Logf("%d archives, %d tables, discs %v, %d items", n, tb, d.Discs(), len(items))
}
