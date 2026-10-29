package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
	"github.com/wouteroostervld/motorsaw/pkg/grep"
)

func TestWritesQuery(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.go":  "package p\n\nfunc f(a *A) {\n\ta.db = 1\n\tapp(ctx).Current_sp_database = y\n\tCurrent_sp_database += \"z\"\n\tx := 1\n\tx++\n\tb.m[Current_sp_database] = 2\n\tuse(Current_sp_database)\n}\n",
		"a.php": "<?php\nclass C { function f() {\n  Current::$database = 'x';\n  $this->db = 1;\n  $x .= 'a';\n  $y =& Current::$database;\n  $n++;\n  $a['k'] = 3;\n  echo Current::$database;\n  $database = 'local';\n  \\PhpMyAdmin\\Current::$database .= '!';\n  Other::$database = 1;\n}}\n",
		"a.py":  "def f(o):\n    o.db = 1\n    x += 2\n    d['k'] = 3\n    a, y = 4, 5\n    print(o.db)\n",
		"a.js":  "function f(o) {\n  o.db = 1;\n  x += 2;\n  d['k'] = 3;\n  n++;\n  g(o.db);\n}\n",
		"a.c":   "void f(struct s *o) {\n  o->db = 1;\n  x += 2;\n  d[3] = 3;\n  n++;\n  g(o->db);\n}\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
	}

	for name, want := range map[string][]string{
		// The field and the variable, not the index or the argument
		"Current_sp_database": {"a.go:5", "a.go:6"},
		// PHP: by bare name, the static properties and the local variable;
		// with its class, only that class's static property, assigned,
		// appended to and taken by reference, not read
		"$database":                     {"a.php:10", "a.php:11", "a.php:12", "a.php:3", "a.php:6"},
		"database":                      {"a.php:10", "a.php:11", "a.php:12", "a.php:3", "a.php:6"},
		"Current::$database":            {"a.php:11", "a.php:3", "a.php:6"},
		`\PhpMyAdmin\Current::database`: {"a.php:11", "a.php:3", "a.php:6"},
		// A field or property in every language, not where it is read
		"db": {"a.c:2", "a.go:4", "a.js:2", "a.php:4", "a.py:2"},
		"x":  {"a.c:3", "a.go:7", "a.go:8", "a.js:3", "a.php:5", "a.py:3"},
		// An element written is the variable written
		"d": {"a.c:4", "a.js:4", "a.py:4"},
		"m": {"a.go:9"},
		"a": {"a.php:8", "a.py:5"},
		"y": {"a.php:6", "a.py:5"},
		"n": {"a.c:5", "a.js:5", "a.php:7"},
	} {
		q, err := writesQuery(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		query, err := chunker.NewQuery(q, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found, err := grep.Match(query, []string{dir}, grep.Options{})
		query.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, f := range found {
			for _, g := range f.Groups {
				for _, l := range g.Lines {
					got = append(got, filepath.Base(f.Path)+":"+strconv.Itoa(l.Number))
				}
			}
		}
		sort.Strings(got)
		if len(got) != len(want) {
			t.Errorf("--writes %s = %v, want %v", name, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("--writes %s = %v, want %v", name, got, want)
				break
			}
		}
	}

	if _, err := writesQuery(`a"b`); err == nil {
		t.Error("a name that is not an identifier must be refused")
	}
}
