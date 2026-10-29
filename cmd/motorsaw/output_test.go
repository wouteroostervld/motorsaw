package main

import (
	"bytes"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
	"github.com/wouteroostervld/motorsaw/pkg/grep"
)

func TestParse_OptionsAnywhere(t *testing.T) {
	for _, tt := range []struct {
		args  string
		given []string
		limit int
		s     bool
	}{
		{"-s --limit 3 pattern pkg", []string{"pattern", "pkg"}, 3, true},
		{"pattern pkg -s --limit 3", []string{"pattern", "pkg"}, 3, true},
		{"pattern --limit 3 pkg cmd -s", []string{"pattern", "pkg", "cmd"}, 3, true},
		{"-s -- --limit 3", []string{"--limit", "3"}, 0, true},
		{"pattern -- -s", []string{"pattern", "-s"}, 0, false},
		{"", nil, 0, false},
	} {
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		limit := flags.Int("limit", 0, "")
		s := flags.Bool("s", false, "")
		given := parse(flags, strings.Fields(tt.args))
		if !reflect.DeepEqual(given, tt.given) || *limit != tt.limit || *s != tt.s {
			t.Errorf("parse(%q) = %q, limit %d, s %v", tt.args, given, *limit, *s)
		}
	}
}

func TestNarrow(t *testing.T) {
	var out bytes.Buffer
	n := &narrow{out: &out, width: 5}
	// A line may come in pieces, and several lines in one piece
	for _, piece := range []string{"abc", "defgh\nab", "\nabcde\nżółwie\nlast line"} {
		if _, err := io.WriteString(n, piece); err != nil {
			t.Fatal(err)
		}
	}
	n.flush()
	want := "abcd…\nab\nabcde\nżółw…\nlast…"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

// matches is what a search found: two files, with 2+1 and 3 matching lines
func matches() []grep.File {
	return []grep.File{
		{Path: "/src/a.go", Groups: []grep.Group{
			{Symbol: "T.one", Type: "METHOD", StartLine: 1, EndLine: 9, Lines: []grep.Line{{Number: 2, Text: "\tx()"}, {Number: 3, Text: "\ty()"}}},
			{Lines: []grep.Line{{Number: 20, Text: "var x"}}},
		}},
		{Path: "/src/b.go", Groups: []grep.Group{
			{Symbol: "two", Type: "FUNCTION", StartLine: 4, EndLine: 8, Lines: []grep.Line{{Number: 5, Text: "x()"}, {Number: 6, Text: "x()"}, {Number: 7, Text: "x()"}}},
		}},
	}
}

func TestFirst(t *testing.T) {
	files := matches()
	if got := count(files, false); got != 6 {
		t.Errorf("%d matching lines, want 6", got)
	}
	if got := count(files, true); got != 3 {
		t.Errorf("%d definitions, want 3", got)
	}

	// Lines: the limit may fall inside a definition
	for limit, want := range map[int][]int{1: {1}, 2: {2}, 3: {2, 1}, 4: {2, 1, 1}, 6: {2, 1, 3}, 9: {2, 1, 3}} {
		var got []int
		for _, f := range first(files, limit, false) {
			for _, g := range f.Groups {
				got = append(got, len(g.Lines))
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("first %d lines: groups of %v, want %v", limit, got, want)
		}
	}

	// Definitions: each one that is kept keeps all its lines
	kept := first(files, 2, true)
	if len(kept) != 1 || len(kept[0].Groups) != 2 || len(kept[0].Groups[0].Lines) != 2 {
		t.Errorf("first 2 definitions: %+v", kept)
	}
	if count(files, false) != 6 {
		t.Error("first changed the files it was given")
	}
}

func TestPrintMatchLines(t *testing.T) {
	var out bytes.Buffer
	printMatchLines(&out, matches(), "/src", false)
	want := `a.go:2: [T.one] x()
a.go:3: [T.one] y()
a.go:20: var x
b.go:5: [two] x()
b.go:6: [two] x()
b.go:7: [two] x()
`
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}

	out.Reset()
	printMatchLines(&out, matches(), "/src", true)
	want = `a.go:1-9: [T.one] 2
a.go: 1
b.go:4-8: [two] 3
`
	if out.String() != want {
		t.Errorf("definitions only, got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestChunksNamed(t *testing.T) {
	source := `package p

type T struct{}

func (t T) Long() int {
	a := 1
	b := 2
	c := 3
	d := 4
	return a + b + c + d
}

func Other() int {
	a := 1
	b := 2
	c := 3
	d := 4
	return a + b + c + d
}
`
	f, ok := chunker.Parse("p.go", []byte(source), 0)
	if !ok {
		t.Fatal("not parsed")
	}
	for _, name := range []string{"T.Long", "Long", "7"} {
		chunks := chunksNamed(f, name)
		if len(chunks) != 1 || f.Chunks[chunks[0]].StartLine != 5 || f.Chunks[chunks[0]].EndLine != 11 {
			t.Errorf("chunksNamed(%q) = %v", name, chunks)
		}
	}
	if chunks := chunksNamed(f, "Missing"); len(chunks) != 0 {
		t.Errorf("chunksNamed of a name that is not there = %v", chunks)
	}
}

func TestChunksNamed_AnyCase(t *testing.T) {
	// In the BCPL of the 1980s FormTree and formtree are one name
	source := "LET FormTree(v) BE\n$( a := 1\n   b := 2\n   c := 3\n   d := 4\n$)\n"
	f, ok := chunker.Parse("syn4", []byte(source), 0)
	if !ok {
		t.Fatal("not parsed")
	}
	if chunks := chunksNamed(f, "formtree"); len(chunks) != 1 || f.Chunks[chunks[0]].StartLine != 1 {
		t.Errorf("chunksNamed(formtree) = %v", chunks)
	}
	// Elsewhere they are two
	g, _ := chunker.Parse("p.go", []byte("package p\n\nfunc Long() int {\n\ta := 1\n\tb := 2\n\tc := 3\n\treturn a + b + c\n}\n"), 0)
	if chunks := chunksNamed(g, "long"); len(chunks) != 0 {
		t.Errorf("chunksNamed(long) in Go = %v", chunks)
	}
}

func TestCell(t *testing.T) {
	for _, tt := range []struct {
		value any
		want  string
	}{
		{nil, ""},
		{int64(3), "3"},
		{"name", "name"},
		{[]byte("a\n\tb\n"), `a\n b`},
	} {
		if got := cell(tt.value); got != tt.want {
			t.Errorf("cell(%#v) = %q, want %q", tt.value, got, tt.want)
		}
	}
}
