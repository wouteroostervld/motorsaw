package chunker

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// captured is the text of what a query captures in a file, as "name: text"
func captured(t *testing.T, query, language, path, src string) []string {
	t.Helper()
	q, err := NewQuery(query, language)
	if err != nil {
		t.Fatalf("NewQuery failed: %v", err)
	}
	defer q.Close()
	var out []string
	for _, c := range q.Find(path, []byte(src)) {
		text, _, _ := strings.Cut(src[c.Start:c.End], "\n")
		out = append(out, c.Name+": "+text)
	}
	return out
}

func TestQuery_Find(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"a capture", `(method_declaration name: (field_identifier) @name)`, []string{"name: load"}},
		{"nothing captured", `(method_declaration)`, []string{"match: func (s *Store) load() error {"}},
		{"several patterns, nothing captured", "(method_declaration) ; methods\n(type_declaration)",
			[]string{"match: type Store struct {", "match: func (s *Store) load() error {"}},
		{"an underscore keeps a capture to the query",
			`(function_declaration name: (identifier) @_name result: (_) @result (#match? @_name "^[a-z]"))`,
			[]string{"result: error"}},
		{"a node captured by two matches is found once",
			`(function_declaration parameters: (parameter_list (parameter_declaration)) result: (parameter_list (_)) @result)`,
			[]string{"result: (*Store, error)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := captured(t, tt.query, "", "store.go", goSource); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQuery_Languages(t *testing.T) {
	// Kinds are those of a grammar: this one is Go's, and no other's
	q, err := NewQuery(`(short_var_declaration)`, "")
	if err != nil {
		t.Fatalf("NewQuery failed: %v", err)
	}
	defer q.Close()
	if got := q.Languages(); !reflect.DeepEqual(got, []string{"go"}) {
		t.Errorf("Languages() = %v, want go alone", got)
	}
	if !q.Fits("a.go") || q.Fits("a.py") || q.Fits("a.txt") {
		t.Error("Fits does not go by the language of the file")
	}
	if got := q.Find("a.py", []byte("def f(): pass\n")); got != nil {
		t.Errorf("found %v in a language the query does not fit", got)
	}

	// An identifier is one in many grammars
	if got := captured(t, `((identifier) @id (#eq? @id "p3"))`, "", "seg.b", "LET start(p1, p2, p3) BE RETURN\n"); !reflect.DeepEqual(got, []string{"id: p3"}) {
		t.Errorf("BCPL: got %q", got)
	}

	// Of several patterns, a language takes the ones in its own words
	both := `(function_declaration name: (_) @name) (function_definition name: (_) @name) ; Go, Python`
	if got := captured(t, both, "", "a.go", "package a\n\nfunc f() {}\n"); !reflect.DeepEqual(got, []string{"name: f"}) {
		t.Errorf("Go: got %q", got)
	}
	if got := captured(t, both, "", "a.py", "def g(): pass\n"); !reflect.DeepEqual(got, []string{"name: g"}) {
		t.Errorf("Python: got %q", got)
	}

	for name, tt := range map[string]struct{ query, language, want string }{
		"no language has the kind":  {`(no_such_kind)`, "", "fits no language"},
		"the named language hasn't": {`(method_declaration)`, "python", "python: "},
		"not a query":               {`(method_declaration`, "", "Invalid syntax"},
		"not a language":            {`(identifier)`, "fortran", "unknown language"},
	} {
		if _, err := NewQuery(tt.query, tt.language); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want one with %q", name, err, tt.want)
		}
	}
}

func TestSplitPatterns(t *testing.T) {
	source := `(a (b) @x) @y ; one (
"(" @paren
[(c) "d;"]+ @e ((f) @g (#eq? @g "h)"))`
	want := []string{`(a (b) @x) @y`, `"(" @paren`, `[(c) "d;"]+ @e`, `((f) @g (#eq? @g "h)"))`}
	if got := splitPatterns(source); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParse_SignatureRange(t *testing.T) {
	f := parse(t, "store.go", goSource)
	for _, s := range f.Symbols {
		if got := strings.Join(strings.Fields(goSource[s.SigStart:s.SigEnd]), " "); got != s.Signature {
			t.Errorf("%s: range holds %q, signature is %q", s.Name, got, s.Signature)
		}
	}
	_, open := findSymbol(t, f, "Open")
	param, body := strings.Index(goSource, "path string)"), strings.Index(goSource, "s := &Store")
	if !f.InSignature(param) || f.InSignature(body) || f.InSignature(strings.Index(goSource, "// Open opens")) {
		t.Errorf("InSignature takes more or less than the signature %d-%d", open.SigStart, open.SigEnd)
	}
}

func TestSyntaxTree(t *testing.T) {
	line := strings.Count(goSource[:strings.Index(goSource, "func check")], "\n") + 1
	got, ok := SyntaxTree("store.go", []byte(goSource), line, line)
	if !ok {
		t.Fatal("SyntaxTree does not know Go")
	}
	want := strings.NewReplacer("N", strconv.Itoa(line)).Replace(`(source_file)  [1-N]
  (function_declaration)  [N]
    "func"  [N]
    name: (identifier) check  [N]
    parameters: (parameter_list)  [N]
      "("  [N]
      (parameter_declaration)  [N]
        name: (identifier) data  [N]
        type: (slice_type)  [N]
          "["  [N]
          "]"  [N]
          element: (type_identifier) byte  [N]
      ")"  [N]
    result: (type_identifier) error  [N]
    body: (block)  [N]
      "{"  [N]
      (statement_list)  [N]
        (return_statement)  [N]
          "return"  [N]
          (expression_list)  [N]
            (nil) nil  [N]
      "}"  [N]
`)
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	if _, ok := SyntaxTree("notes.md", []byte("# Notes\n"), 0, 0); ok {
		t.Error("SyntaxTree made a tree of Markdown, which has none")
	}
}
