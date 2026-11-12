// SPDX-License-Identifier: GPL-3.0-or-later

// Package archtest enforces the package layering of production code (tests may wire
// the whole stack): the protocol and data layers never depend on a service, the pure
// game rules depend on nothing but game data, the services never depend on each other,
// and the bot client knows only the wire.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/princebyyourlogic/gurhal/"

var services = []string{"internal/patch", "internal/login", "internal/game", "internal/store", "internal/admin"}

var rules = []struct {
	from      string
	forbidden []string
}{
	{"internal/wire", services},
	{"internal/netx", services},
	{"internal/ssl3", services},
	{"internal/disc", services},
	{"internal/gamedata", services},
	{"internal/calendar", append([]string{"internal/wire", "internal/netx", "internal/rules", "internal/gamedata"}, services...)},
	{"internal/rules", append([]string{"internal/wire", "internal/netx", "internal/ssl3"}, services...)},
	{"internal/patch", []string{"internal/login", "internal/game", "internal/admin"}},
	{"internal/login", []string{"internal/patch", "internal/game", "internal/admin"}},
	{"internal/game", []string{"internal/patch", "internal/login", "internal/admin"}},
	{"internal/admin", []string{"internal/patch", "internal/login", "internal/game"}},
	{"internal/bot", append([]string{"internal/netx", "internal/rules", "internal/gamedata", "internal/auth"}, services...)},
}

func TestLayering(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			dep, ok := strings.CutPrefix(p, module)
			if !ok {
				continue
			}
			for _, r := range rules {
				if !within(rel, r.from) {
					continue
				}
				for _, bad := range r.forbidden {
					if within(dep, bad) {
						t.Errorf("%s imports %s", rel, dep)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func within(path, pkg string) bool { return path == pkg || strings.HasPrefix(path, pkg+"/") }
