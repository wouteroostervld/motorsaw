package grep

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
	"github.com/wouteroostervld/motorsaw/pkg/filter"
)

const goSource = `package store

import "fmt"

var defaultPath = "/tmp/store"

// Store keeps things.
type Store struct {
	path string
	open bool
}

// Opener opens stores.
type Opener interface {
	Open(path string) (*Store, error)
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	fmt.Println("open", s.path)
	return s, nil
}

func (s *Store) Close() error {
	s.open = false
	fmt.Println("close", s.path)
	return nil
}
`

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// summary reduces results to "file: symbol (type) line,line"
func summary(dir string, files []File) []string {
	var out []string
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f.Path)
		for _, g := range f.Groups {
			s := rel + ": " + g.Symbol + " (" + g.Type + ")"
			for _, l := range g.Lines {
				s += " " + string(rune('0'+l.Number/10)) + string(rune('0'+l.Number%10))
			}
			out = append(out, s)
		}
	}
	return out
}

func TestSearch_ReportsEnclosingDefinition(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"store.go": goSource})

	files, err := Search(`path`, []string{dir}, Options{IgnoreCase: true})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	want := []string{
		"store.go:  () 05",                   // Outside any definition
		"store.go: Store (STRUCT) 09",        // A field
		"store.go: Opener.Open (METHOD) 15",  // A method of an interface
		"store.go: Open (FUNCTION) 18 19 20", // The signature counts
		"store.go: Store.Close (METHOD) 26",  // Qualified by its receiver
	}
	if got := summary(dir, files); !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}

	open := files[0].Groups[3]
	if open.StartLine != 18 || open.EndLine != 22 || open.Lines[0].Text != "func Open(path string) (*Store, error) {" {
		t.Errorf("Open group = %+v", open)
	}
}

func TestSearch_PCRE(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"store.go": goSource})

	tests := []struct {
		name    string
		pattern string
		opts    Options
		want    []string
	}{
		{"lookbehind", `(?<=fmt\.)Println\("close"`, Options{}, []string{"store.go: Store.Close (METHOD) 26"}},
		{"lookahead", `^func (?!\()`, Options{}, []string{"store.go: Open (FUNCTION) 18"}},
		{"backreference", `(\w+): \1\b`, Options{}, []string{"store.go: Open (FUNCTION) 19"}},
		{"ignore case", `STORE KEEPS`, Options{IgnoreCase: true}, []string{"store.go: Store (STRUCT) 07"}},
		{"fixed string", `(*Store, error)`, Options{Fixed: true}, []string{"store.go: Opener.Open (METHOD) 15", "store.go: Open (FUNCTION) 18"}},
		{"whole word", `open`, Options{Word: true}, []string{"store.go: Store (STRUCT) 10", "store.go: Open (FUNCTION) 20", "store.go: Store.Close (METHOD) 25"}},
		{"end of line", `error \{$`, Options{}, []string{"store.go: Store.Close (METHOD) 24"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, err := Search(tt.pattern, []string{dir}, tt.opts)
			if err != nil {
				t.Fatalf("Search failed: %v", err)
			}
			if got := summary(dir, files); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}

	if _, err := Search(`(unclosed`, []string{dir}, Options{}); err == nil {
		t.Error("Search accepted an invalid pattern")
	}
}

func TestSearch_WhichFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"a.py":              "def f():\n    needle = 1\n    return needle\n",
		"notes.txt":         "a needle in plain text\n",
		"sub/b.py":          "needle = 2\n",
		".hidden/c.py":      "needle = 3\n",
		"node_modules/d.js": "var needle = 4;\n",
		"vendor/e.go":       "package e // needle\n",
		"data.bin":          "needle\x00binary",
	})

	files, err := Search(`needle`, []string{dir}, Options{Exclude: []string{"node_modules", "vendor/"}})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	want := []string{
		"a.py: f (FUNCTION) 02 03",
		"notes.txt:  () 01", // Text files are searched; they just have no definitions
		"sub/b.py:  () 01",
	}
	if got := summary(dir, files); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	// A file named explicitly is searched even inside an excluded directory
	files, err = Search(`needle`, []string{filepath.Join(dir, "vendor", "e.go")}, Options{Exclude: []string{"vendor"}})
	if err != nil || len(files) != 1 {
		t.Errorf("explicit file: got %d files, err %v", len(files), err)
	}
}

const pySource = `import os


class Store:
    def open(self, path):
        self.path = path
        return os.stat(path)


def close(store, path=None):
    store.path = path
`

func TestSearch_Signature(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"store.go": goSource, "store.py": pySource, "notes.txt": "path\n"})

	// path is all over the files; in a signature it is a parameter
	files, err := Search(`\bpath\b`, []string{dir}, Options{Signature: true})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	want := []string{
		"store.go: Opener.Open (METHOD) 15",
		"store.go: Open (FUNCTION) 18",
		"store.py: Store.open (METHOD) 05",
		"store.py: close (FUNCTION) 10",
	}
	if got := summary(dir, files); !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}

	// A file with matches, none of them in a signature, is not reported
	files, err = Search(`Println`, []string{dir}, Options{Signature: true})
	if err != nil || len(files) != 0 {
		t.Errorf("got %v, %v, want nothing", summary(dir, files), err)
	}
}

func TestMatch(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"store.go": goSource, "store.py": pySource, ".git/hook.go": goSource})

	tests := []struct {
		name  string
		query string
		lang  string
		opts  Options
		want  []string
	}{
		{"a field of a node", `(method_declaration result: (type_identifier) @result)`, "", Options{},
			[]string{"store.go: Store.Close (METHOD) 24"}},
		{"nothing captured", `(function_declaration)`, "", Options{},
			[]string{"store.go: Open (FUNCTION) 18"}},
		{"a predicate, in every language it fits", `((identifier) @id (#eq? @id "path"))`, "", Options{},
			[]string{
				"store.go: Opener.Open (METHOD) 15", "store.go: Open (FUNCTION) 18 19",
				"store.py: Store.open (METHOD) 05 06 07", "store.py: close (FUNCTION) 10 11",
			}},
		{"in signatures only", `((identifier) @id (#eq? @id "path"))`, "", Options{Signature: true},
			[]string{
				"store.go: Opener.Open (METHOD) 15", "store.go: Open (FUNCTION) 18",
				"store.py: Store.open (METHOD) 05", "store.py: close (FUNCTION) 10",
			}},
		{"one language", `((identifier) @id (#eq? @id "path"))`, "python", Options{Signature: true},
			[]string{"store.py: Store.open (METHOD) 05", "store.py: close (FUNCTION) 10"}},
		{"a capture for the query alone", `(function_definition name: (identifier) @_name parameters: (parameters (default_parameter) @default) (#match? @_name "^cl"))`, "", Options{},
			[]string{"store.py: close (FUNCTION) 10"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, err := chunker.NewQuery(tt.query, tt.lang)
			if err != nil {
				t.Fatalf("NewQuery failed: %v", err)
			}
			defer query.Close()
			files, err := Match(query, []string{dir}, tt.opts)
			if err != nil {
				t.Fatalf("Match failed: %v", err)
			}
			if got := summary(dir, files); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

// Directories are excluded as the index excludes them: by name, by several
// names in a row and by absolute path
func TestSearch_ExcludedDirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"a.py":                  "needle = 1\n",
		"tools/b.py":            "needle = 2\n",
		"tools/ghidra/c.py":     "needle = 3\n",
		"tools/ghidra/sub/d.py": "needle = 4\n",
		"tools/ghidra2/e.py":    "needle = 5\n",
		"lib/cache/f.py":        "needle = 6\n",
	})
	ghidra := filepath.Join(dir, "tools", "ghidra")
	rest := []string{"a.py:  () 01", "lib/cache/f.py:  () 01", "tools/b.py:  () 01", "tools/ghidra2/e.py:  () 01"}

	for _, exclude := range [][]string{{ghidra}, {ghidra + "/"}, {"tools/ghidra"}, {"ghidra"}} {
		files, err := Search(`needle`, []string{dir}, Options{Exclude: exclude})
		if err != nil {
			t.Fatalf("Search failed: %v", err)
		}
		if got := summary(dir, files); !reflect.DeepEqual(got, rest) {
			t.Errorf("exclude %q: got %q, want %q", exclude, got, rest)
		}
	}

	// From a directory above as well, as when run in the root of a project
	files, err := Search(`needle`, []string{filepath.Join(dir, "tools")}, Options{Exclude: []string{ghidra, "lib/cache"}})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	want := []string{"tools/b.py:  () 01", "tools/ghidra2/e.py:  () 01"}
	if got := summary(dir, files); !reflect.DeepEqual(got, want) {
		t.Errorf("from tools: got %q, want %q", got, want)
	}

	// A directory named explicitly is searched even when it is excluded, and
	// so is what is below it
	for root, want := range map[string]int{ghidra: 2, filepath.Join(ghidra, "sub"): 1} {
		files, err := Search(`needle`, []string{root}, Options{Exclude: []string{ghidra, "ghidra"}})
		if err != nil || len(files) != want {
			t.Errorf("explicit directory %s: got %d files, err %v, want %d", root, len(files), err, want)
		}
	}
}

func TestSearch_Language(t *testing.T) {
	dir := t.TempDir()
	// Nothing in the name or the content says what this is
	path := filepath.Join(dir, "fragment")
	if err := os.WriteFile(path, []byte("LET f(x) =\n   x + 1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	files, err := Search("x . 1", []string{path}, Options{})
	if err != nil || len(files) != 1 || files[0].Groups[0].Symbol != "" {
		t.Fatalf("without a language: %+v, %v; want the match outside any definition", files, err)
	}
	for _, paths := range [][]string{{path}, {dir}} {
		files, err = Search("x . 1", paths, Options{Language: "bcpl"})
		if err != nil || len(files) != 1 || files[0].Groups[0].Symbol != "f" {
			t.Errorf("as BCPL, from %v: %+v, %v; want the match in f", paths, files, err)
		}
	}
}

func TestSearch_Gitignore(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		".git/HEAD":     "ref: refs/heads/main\n",
		".gitignore":    "build/\n*.log\n",
		"main.py":       "needle = 1\n",
		"run.log":       "needle\n",
		"build/out.py":  "needle = 2\n",
		"build/out.log": "needle\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	found := func(paths []string, opts Options) []string {
		t.Helper()
		files, err := Search("needle", paths, opts)
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, f := range files {
			rel, _ := filepath.Rel(dir, f.Path)
			names = append(names, rel)
		}
		return names
	}

	following := Options{Walk: filter.NewWalk(nil, nil, nil, true)}
	if got, want := found([]string{dir}, following), []string{"main.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("following .gitignore: %v, want %v", got, want)
	}
	// A directory that is named is searched though it is ignored; what is
	// ignored inside it is not
	if got, want := found([]string{filepath.Join(dir, "build")}, following), []string{"build/out.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the ignored directory, named: %v, want %v", got, want)
	}
	// And so is a file that is named
	if got, want := found([]string{filepath.Join(dir, "run.log")}, following), []string{"run.log"}; !reflect.DeepEqual(got, want) {
		t.Errorf("an ignored file, named: %v, want %v", got, want)
	}
	if got, want := found([]string{dir}, Options{}), []string{"build/out.log", "build/out.py", "main.py", "run.log"}; !reflect.DeepEqual(got, want) {
		t.Errorf("not following .gitignore: %v, want %v", got, want)
	}
}
