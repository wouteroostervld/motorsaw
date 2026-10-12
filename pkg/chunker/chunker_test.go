package chunker

import (
	"strings"
	"testing"
)

const goSource = `package store

import (
	"fmt"

	"example.com/proj/util"
)

// Store keeps things.
type Store struct {
	path string
	open bool
}

// Open opens the store at path.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	return s, nil
}

// load reads the store from disk.
func (s *Store) load() error {
	data := util.Read(s.path)
	s.open = len(data) > 0
	return check(data)
}

func check(data []byte) error { return nil }
`

func parse(t *testing.T, path, src string) *File {
	t.Helper()
	f, ok := Parse(path, []byte(src), 0)
	if !ok {
		t.Fatalf("Parse(%s) not supported", path)
	}
	return f
}

func findSymbol(t *testing.T, f *File, name string) (int, Symbol) {
	t.Helper()
	for i, s := range f.Symbols {
		if s.QualifiedName() == name {
			return i, s
		}
	}
	t.Fatalf("symbol %s not found in %+v", name, f.Symbols)
	return 0, Symbol{}
}

func hasRef(f *File, want Ref) bool {
	for _, r := range f.Refs {
		if r == want {
			return true
		}
	}
	return false
}

func TestParse_Go(t *testing.T) {
	f := parse(t, "/proj/store/store.go", goSource)

	if f.Module != "store" {
		t.Errorf("Module = %q, want store", f.Module)
	}

	// The skeleton lists signatures and points at the chunks with the bodies
	wantSkeleton := `package store

import (
	"fmt"

	"example.com/proj/util"
)

// Store keeps things.
type Store struct {
	path string
	open bool
}

func Open(path string) (*Store, error) // ... chunk: lines 15-22

func (s *Store) load() error // ... chunk: lines 24-29

func check(data []byte) error { return nil }
`
	if got := f.Chunks[0].Content; got != wantSkeleton {
		t.Errorf("skeleton =\n%s\nwant\n%s", got, wantSkeleton)
	}
	if len(f.Chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(f.Chunks))
	}

	// A definition's chunk carries its doc comment and its full body
	_, open := findSymbol(t, f, "Open")
	chunk := f.Chunks[open.Chunk]
	if chunk.StartLine != 15 || chunk.EndLine != 22 || chunk.Parent != 0 || !chunk.Verbatim {
		t.Errorf("Open chunk = lines %d-%d parent %d verbatim %v", chunk.StartLine, chunk.EndLine, chunk.Parent, chunk.Verbatim)
	}
	if !strings.HasPrefix(chunk.Content, "// Open opens the store at path.\nfunc Open(") || !strings.HasSuffix(chunk.Content, "return s, nil\n}\n") {
		t.Errorf("Open chunk content =\n%s", chunk.Content)
	}

	symbols := map[string]string{
		"Store":      TypeStruct,
		"Open":       TypeFunction,
		"Store.load": TypeMethod,
		"check":      TypeFunction,
	}
	if len(f.Symbols) != len(symbols) {
		t.Errorf("got %d symbols, want %d: %+v", len(f.Symbols), len(symbols), f.Symbols)
	}
	for name, etype := range symbols {
		if _, s := findSymbol(t, f, name); s.Type != etype {
			t.Errorf("%s has type %s, want %s", name, s.Type, etype)
		}
	}

	// Too small to fold: they stay in the skeleton
	for _, name := range []string{"check", "Store"} {
		if _, sym := findSymbol(t, f, name); sym.Chunk != 0 {
			t.Errorf("%s is in chunk %d, want 0", name, sym.Chunk)
		}
	}

	openIdx, _ := findSymbol(t, f, "Open")
	loadIdx, _ := findSymbol(t, f, "Store.load")
	wantRefs := []Ref{
		{Source: -1, Relation: RelationImports, Name: "util", ImportBase: "util", External: "example.com/proj/util"},
		// s is not the receiver here, so only the name is known
		{Source: openIdx, Relation: RelationCalls, Name: "load", Container: "s", External: "load", Member: true},
		{Source: openIdx, Relation: RelationCalls, Name: "Errorf", ImportBase: "fmt", External: "fmt.Errorf", Member: true},
		{Source: loadIdx, Relation: RelationCalls, Name: "Read", ImportBase: "util", External: "util.Read", Member: true},
		{Source: loadIdx, Relation: RelationCalls, Name: "check", External: "check"},
	}
	for _, want := range wantRefs {
		if !hasRef(f, want) {
			t.Errorf("missing ref %+v in %+v", want, f.Refs)
		}
	}
}

const pythonSource = `from .utils import helper as h


class Greeter(Base):
    """Greets."""

    # Says hello.
    @log
    def greet(self, name):
        def shout(text):
            loud = text.upper()
            loud = loud.strip()
            loud = loud + "!"
            return loud
        return shout(self.format(h(name)))

    def format(self, name):
        text = "hello " + name
        return text
`

func TestParse_Python(t *testing.T) {
	f := parse(t, "/proj/greeter.py", pythonSource)

	wantSkeleton := `from .utils import helper as h


class Greeter(Base): # ... chunk: lines 4-19
`
	if got := f.Chunks[0].Content; got != wantSkeleton {
		t.Errorf("skeleton =\n%s\nwant\n%s", got, wantSkeleton)
	}

	// Folds nest: the class lists its methods, a method lists its inner
	// functions. A method of a few lines stays where it is.
	_, class := findSymbol(t, f, "Greeter")
	wantClass := `class Greeter(Base):
    """Greets."""

    @log
    def greet(self, name): # ... chunk: lines 7-15

    def format(self, name):
        text = "hello " + name
        return text
`
	if got := f.Chunks[class.Chunk].Content; got != wantClass {
		t.Errorf("class chunk =\n%s\nwant\n%s", got, wantClass)
	}

	greetIdx, greet := findSymbol(t, f, "Greeter.greet")
	wantGreet := `    # Says hello.
    @log
    def greet(self, name):
        def shout(text): # ... chunk: lines 10-14
        return shout(self.format(h(name)))
`
	greetChunk := f.Chunks[greet.Chunk]
	if greetChunk.Content != wantGreet {
		t.Errorf("greet chunk =\n%s\nwant\n%s", greetChunk.Content, wantGreet)
	}
	if greet.Type != TypeMethod || greetChunk.Parent != class.Chunk || greetChunk.Verbatim {
		t.Errorf("greet: type %s, chunk parent %d, verbatim %v", greet.Type, greetChunk.Parent, greetChunk.Verbatim)
	}

	shoutIdx, shout := findSymbol(t, f, "shout")
	if shout.Type != TypeFunction || shout.Parent != greetIdx || f.Chunks[shout.Chunk].Parent != greet.Chunk {
		t.Errorf("shout = %+v", shout)
	}

	wantRefs := []Ref{
		{Source: greetIdx, Relation: RelationCalls, Name: "shout", External: "shout"},
		{Source: greetIdx, Relation: RelationCalls, Name: "format", Container: "Greeter", External: "format", Member: true},
		// The alias is resolved to what was imported
		{Source: greetIdx, Relation: RelationCalls, Name: "helper", ImportBase: "utils", External: ".utils.helper"},
		{Source: shoutIdx, Relation: RelationCalls, Name: "upper", Container: "text", External: "upper", Member: true},
	}
	for _, want := range wantRefs {
		if !hasRef(f, want) {
			t.Errorf("missing ref %+v in %+v", want, f.Refs)
		}
	}
}

func TestParse_Languages(t *testing.T) {
	tests := []struct {
		path    string
		src     string
		symbols map[string]string
		stub    string // Expected in the skeleton
		ref     Ref
	}{
		{
			path: "a.ts",
			src: `import { load } from './lib/io';

export const run = async (path: string) => {
  const data = load(path);
  return data;
};

export class Runner {
  start(): void {
    this.stop();
    run("x");
  }
  stop(): void {
    console.log("stop");
    return;
  }
}
`,
			symbols: map[string]string{"run": TypeFunction, "Runner": TypeClass, "Runner.start": TypeMethod, "Runner.stop": TypeMethod},
			stub:    "export class Runner // ... chunk: lines 8-17",
			ref:     Ref{Source: 0, Relation: RelationCalls, Name: "load", ImportBase: "io", External: "./lib/io.load"},
		},
		{
			path: "a.rs",
			src: `use crate::db::open;

/// A thing.
#[derive(Debug)]
pub struct Thing {
    a: i32,
}

impl Thing {
    pub fn new(a: i32) -> Self {
        Self::check(a);
        Thing { a }
    }

    fn check(a: i32) -> bool {
        let d = open("x");
        a > 0
    }
}
`,
			symbols: map[string]string{"Thing": TypeStruct, "Thing.new": TypeMethod, "Thing.check": TypeMethod},
			stub:    "impl Thing // ... chunk: lines 9-19",
			ref:     Ref{Source: 1, Relation: RelationCalls, Name: "check", Container: "Thing", External: "check", Member: true},
		},
		{
			path: "A.java",
			src: `import com.x.util.Helper;

public class A {
    public int run(int n) {
        Helper.help(n);
        return init();
    }

    private int init() {
        int n = 0;
        return n;
    }
}
`,
			symbols: map[string]string{"A": TypeClass, "A.run": TypeMethod, "A.init": TypeMethod},
			stub:    "public class A // ... chunk: lines 3-13",
			ref:     Ref{Source: 1, Relation: RelationCalls, Name: "init", Container: "A", External: "init"},
		},
		{
			path: "a.cpp",
			src: `#include "widget.h"

int Widget::size() {
    int v = compute(1);
    v += 1;
    return v;
}

static int *alloc(int n) {
    int *p = (int *)malloc(n);
    return p;
}
`,
			symbols: map[string]string{"Widget.size": TypeMethod, "alloc": TypeFunction},
			stub:    "int Widget::size() // ... chunk: lines 3-7",
			ref:     Ref{Source: -1, Relation: RelationImports, Name: "widget", ImportBase: "widget", External: "widget.h"},
		},
		{
			path: "a.rb",
			src: `class Dog < Animal
  def speak(times)
    puts bark()
    self.wag
  end

  def wag
    @tail = true
    nil
  end
end
`,
			symbols: map[string]string{"Dog": TypeClass, "Dog.speak": TypeMethod, "Dog.wag": TypeMethod},
			stub:    "class Dog < Animal # ... chunk: lines 1-11",
			ref:     Ref{Source: 1, Relation: RelationCalls, Name: "wag", Container: "Dog", External: "wag", Member: true},
		},
		{
			path: "a.b",
			src: `GET "libhdr"

LET fact(n) = n = 0 -> 1, n * fact(n - 1)

AND show(upb) BE
$( FOR i = 0 TO upb DO
     writef("%N*N", fact(i))
   newline()
$)
`,
			symbols: map[string]string{"fact": TypeFunction, "show": TypeFunction},
			stub:    "AND show(upb) BE // ... chunk: lines 5-9",
			ref:     Ref{Source: 1, Relation: RelationCalls, Name: "fact", External: "fact"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			f := parse(t, tt.path, tt.src)

			if len(f.Symbols) != len(tt.symbols) {
				t.Errorf("got %d symbols, want %d: %+v", len(f.Symbols), len(tt.symbols), f.Symbols)
			}
			for name, etype := range tt.symbols {
				if _, s := findSymbol(t, f, name); s.Type != etype {
					t.Errorf("%s has type %s, want %s", name, s.Type, etype)
				}
			}
			if !strings.Contains(f.Chunks[0].Content, tt.stub) {
				t.Errorf("skeleton does not contain %q:\n%s", tt.stub, f.Chunks[0].Content)
			}
			if !hasRef(f, tt.ref) {
				t.Errorf("missing ref %+v in %+v", tt.ref, f.Refs)
			}
		})
	}
}

// The definitions of a BCPL declaration are chained with AND. The comment
// above the declaration is about the first of them.
func TestParse_BCPLDeclaration(t *testing.T) {
	f := parse(t, "/proj/queue.b", `GET "libhdr"

// Puts x at the end of q.
LET append(q, x) BE
$( LET cell = getvec(1)
   link(q, cell)
   cell!1 := x
$)

// Joins a cell to the queue.
AND link(q, cell) BE
$( cell!0 := 0
   !(q!1) := cell
   q!1 := cell
$)
`)

	_, first := findSymbol(t, f, "append")
	if first.StartLine != 3 || first.EndLine != 8 || first.Signature != "LET append(q, x) BE" {
		t.Errorf("append = %+v", first)
	}
	_, second := findSymbol(t, f, "link")
	if second.StartLine != 10 || second.EndLine != 15 || second.Signature != "AND link(q, cell) BE" {
		t.Errorf("link = %+v", second)
	}

	want := Ref{Source: -1, Relation: RelationImports, Name: "libhdr", ImportBase: "libhdr", External: "libhdr"}
	if !hasRef(f, want) {
		t.Errorf("missing ref %+v in %+v", want, f.Refs)
	}
}

func TestParse_FoldsBlocksOfOversizedChunks(t *testing.T) {
	src := `package big

func Process(items []string) int {
	total := 0
	for _, item := range items {
		if item == "" {
			continue
		}
		total += len(item)
		total += len(item) * 2
		total += len(item) * 3
	}
	if total > 100 {
		total = 100
	}
	return total
}
`
	// Without a limit the function is one chunk
	if f := parse(t, "big.go", src); len(f.Chunks) != 2 {
		t.Fatalf("got %d chunks without a limit, want 2", len(f.Chunks))
	}

	f, ok := Parse("big.go", []byte(src), 200)
	if !ok {
		t.Fatal("Parse not supported")
	}
	if len(f.Chunks) != 3 {
		t.Fatalf("got %d chunks, want 3: %+v", len(f.Chunks), f.Chunks)
	}

	// Only the loop had to go to make the function fit
	wantFunc := `func Process(items []string) int {
	total := 0
	for _, item := range items { // ... chunk: lines 5-12
	if total > 100 {
		total = 100
	}
	return total
}
`
	if got := f.Chunks[1].Content; got != wantFunc {
		t.Errorf("function chunk =\n%s\nwant\n%s", got, wantFunc)
	}

	loop := f.Chunks[2]
	if loop.Parent != 1 || loop.StartLine != 5 || loop.EndLine != 12 ||
		!strings.HasPrefix(loop.Content, "\tfor _, item := range items {\n") || !strings.HasSuffix(loop.Content, "\n\t}\n") {
		t.Errorf("loop chunk = parent %d, lines %d-%d:\n%s", loop.Parent, loop.StartLine, loop.EndLine, loop.Content)
	}

	// Expanding the stubs again gives back the source
	fn := Expand(f.Chunks[1].Content, loop.Stub, loop.Content)
	if got := Expand(f.Chunks[0].Content, f.Chunks[1].Stub, fn); got != src {
		t.Errorf("expanded =\n%s\nwant\n%s", got, src)
	}
}

func TestParse_Markdown(t *testing.T) {
	src := "---\ntitle: Guide\n---\n" + `# Guide

Intro text.

## Install

Run the installer:

` + "```sh\n# not a heading\nmake install\n```" + `

### Options

- fast
- slow

## Usage

One line.

Setext
======

More text
here.
`
	f := parse(t, "guide.md", src)

	if !f.Markup || len(f.Symbols) != 0 || len(f.Refs) != 0 {
		t.Errorf("markup = %v with %d symbols and %d refs, want a document without symbols", f.Markup, len(f.Symbols), len(f.Refs))
	}

	wantSkeleton := `---
title: Guide
---
# Guide <!-- ... chunk: lines 4-24 -->

Setext <!-- ... chunk: lines 26-30 -->
`
	if got := f.Chunks[0].Content; got != wantSkeleton {
		t.Errorf("skeleton =\n%s\nwant\n%s", got, wantSkeleton)
	}

	// A section lists its subsections
	wantGuide := `# Guide

Intro text.

## Install <!-- ... chunk: lines 8-20 -->

## Usage

One line.
`
	if got := f.Chunks[1].Content; got != wantGuide {
		t.Errorf("section =\n%s\nwant\n%s", got, wantGuide)
	}

	install := f.Chunks[2]
	if install.Parent != 1 || !strings.Contains(install.Content, "# not a heading\nmake install") ||
		!strings.Contains(install.Content, "### Options\n\n- fast") || install.Title != "Guide > Install" {
		t.Errorf("install section = parent %d:\n%s", install.Parent, install.Content)
	}
}

func TestParse_MarkupFoldsLargeElements(t *testing.T) {
	src := `<html>
<head>
  <title>Page</title>
</head>
<body>
  <nav>
    <a href="/">Home</a>
    <a href="/docs">Docs</a>
    <a href="/about">About</a>
    <a href="/blog">Blog</a>
    <a href="/team">Team</a>
    <a href="/jobs">Jobs</a>
  </nav>
  <p>Short.</p>
</body>
</html>
`
	// Small enough: one chunk
	if f := parse(t, "page.html", src); len(f.Chunks) != 1 || !f.Markup {
		t.Fatalf("got %d chunks without a limit, want 1", len(f.Chunks))
	}

	f, ok := Parse("page.html", []byte(src), 120)
	if !ok {
		t.Fatal("Parse not supported")
	}
	wantSkeleton := `<html>
<head>
  <title>Page</title>
</head>
<body> <!-- ... chunk: lines 5-15 -->
</html>
`
	if got := f.Chunks[0].Content; got != wantSkeleton {
		t.Errorf("skeleton =\n%s\nwant\n%s", got, wantSkeleton)
	}
	wantBody := `<body>
  <nav> <!-- ... chunk: lines 6-13 -->
  <p>Short.</p>
</body>
`
	if got := f.Chunks[1].Content; got != wantBody {
		t.Errorf("body =\n%s\nwant\n%s", got, wantBody)
	}

	xml := `<config>
  <servers>
    <server name="a" port="1"/>
    <server name="b" port="2"/>
    <server name="c" port="3"/>
    <server name="d" port="4"/>
    <server name="e" port="5"/>
    <server name="f" port="6"/>
  </servers>
  <mode>fast</mode>
</config>
`
	x, ok := Parse("config.xml", []byte(xml), 100)
	if !ok {
		t.Fatal("Parse not supported")
	}
	if want := "  <servers> <!-- ... chunk: lines 2-9 -->\n  <mode>fast</mode>"; !strings.Contains(x.Chunks[0].Content, want) {
		t.Errorf("xml skeleton does not contain %q:\n%s", want, x.Chunks[0].Content)
	}
}

const m68kSource = `; Startup
	include	"exec/types.i"

	XREF	_LVOAllocMem

SIZE	equ	12

PUSHM	macro
	movem.l	\\1,-(sp)
	endm

* Entry point
_Start:
	move.l	4.w,a6
	jsr	_LVOAllocMem(a6)
	bsr.s	Helper
	bsr	.local
	PUSHM	d0-d7
.local	moveq	#0,d0
	rts

*
* Counts down
*
Helper
	lea	Table(pc),a0
.loop	dbra	d0,.loop
	bne.s	Helper
	beq.s	Other
	jmp	Other.l

; Where the helpers are
Table:	dc.l	Helper,Other
	dc.w	0

Other::	rts
`

func TestParse_M68k(t *testing.T) {
	f := parse(t, "boot.s", m68kSource)

	// Global labels are the definitions; .local and .loop are places inside them
	want := []struct {
		name, etype string
		start, end  int
	}{
		{"PUSHM", TypeFunction, 8, 10},
		{"_Start", TypeFunction, 13, 20},
		{"Helper", TypeFunction, 22, 30},
		{"Table", TypeStruct, 32, 34},
		{"Other", TypeFunction, 36, 36},
	}
	if len(f.Symbols) != len(want) {
		t.Fatalf("got %d symbols, want %d: %+v", len(f.Symbols), len(want), f.Symbols)
	}
	for i, w := range want {
		s := f.Symbols[i]
		if s.Name != w.name || s.Type != w.etype || s.StartLine != w.start || s.EndLine != w.end {
			t.Errorf("symbol %d is %s %s lines %d-%d, want %s %s lines %d-%d",
				i, s.Type, s.Name, s.StartLine, s.EndLine, w.etype, w.name, w.start, w.end)
		}
	}

	for _, stub := range []string{"_Start: ; ... chunk: lines 13-20", "Helper ; ... chunk: lines 22-30"} {
		if !strings.Contains(f.Chunks[0].Content, stub) {
			t.Errorf("skeleton does not contain %q:\n%s", stub, f.Chunks[0].Content)
		}
	}

	start, _ := findSymbol(t, f, "_Start")
	helper, _ := findSymbol(t, f, "Helper")
	for _, ref := range []Ref{
		{Source: -1, Relation: RelationImports, Name: "types", ImportBase: "types", External: "exec/types.i"},
		{Source: start, Relation: RelationCalls, Name: "_LVOAllocMem", External: "_LVOAllocMem"},
		{Source: start, Relation: RelationCalls, Name: "Helper", External: "Helper"},
		{Source: start, Relation: RelationCalls, Name: "PUSHM", External: "PUSHM"},
		{Source: helper, Relation: RelationCalls, Name: "Other", External: "Other"},
	} {
		if !hasRef(f, ref) {
			t.Errorf("missing ref %+v in %+v", ref, f.Refs)
		}
	}
	// Neither a branch to a local label nor a loop back to the start is a call
	for _, r := range f.Refs {
		if r.Name == ".local" || r.Name == ".loop" || (r.Source == helper && r.Name == "Helper") {
			t.Errorf("unexpected ref %+v", r)
		}
	}
}

func TestParse_OtherAssembly(t *testing.T) {
	src := `	.file	"clib.c"
	.text
	.globl	main
	.type	main, @function
main:
	pushq	%rbp
	movq	%rsp, %rbp
	movl	$0, %eax
	call	puts@PLT
	popq	%rbp
	ret
`
	if _, ok := Parse("clib.s", []byte(src), 0); ok {
		t.Error("Parse took x86 assembly for m68k")
	}
}

func TestParse_Unsupported(t *testing.T) {
	if _, ok := Parse("notes.txt", []byte("Notes\n"), 0); ok {
		t.Error("Parse accepted a text file")
	}
	if Supported("notes.txt") || !Supported("main.GO") {
		t.Error("Supported does not go by file extension")
	}
}

func TestParse_M68kCallsThroughRegisters(t *testing.T) {
	// As a disassembly of compiled BCPL and of calls into libraries reads:
	// nearly every call goes through a register
	f := parse(t, "dos.s", `dos_g23:
	move.l  d2,d1
	movea.l 116(a2),a4	; G29 dos_g29
	moveq   #28,d0
	jsr     (a5)
.loc_1:
	lea     seg5_l12-seg5_entry(a4),a4
	moveq   #12,d0
	jsr     (a5)
	lea     Handler(pc),a0
	jsr     (a0)
	jsr     -198(a6)	; exec AllocMem
	jsr     -30(a6)	; open the thing now
	rts

dos_g24:
	movea.l 120(a2),a4	; G30 dos_g30
	movea.l d1,a4
	jsr     (a5)
	lea     Table(pc),a0	; not called: read
	move.l  (a0)+,d0
	movea.l 124(a2),a4	; G31
	jsr     (a5)
	jsr     (a3)	; cleanup
	bsr.w   dos_g23
	jmp     (rom_entry).l
`)
	first, _ := findSymbol(t, f, "dos_g23")
	second, _ := findSymbol(t, f, "dos_g24")
	if len(f.Symbols) != 2 {
		t.Fatalf("symbols = %+v, want the two global labels", f.Symbols)
	}

	want := []Ref{
		// A comment names the global that was loaded: not checked by anything
		{Source: first, Relation: RelationMayCall, Name: "dos_g29", External: "dos_g29"},
		// The operand names the routine
		{Source: first, Relation: RelationCalls, Name: "seg5_l12", External: "seg5_l12"},
		{Source: first, Relation: RelationCalls, Name: "Handler", External: "Handler"},
		// A library vector, named by its comment: the routine of that
		// library, and failing that anything of the name
		{Source: first, Relation: RelationMayCall, Name: "exec_AllocMem", External: "exec_AllocMem"},
		{Source: first, Relation: RelationMayCall, Name: "AllocMem"},
		// One word may be prose: it counts when something is called that
		{Source: second, Relation: RelationMayCall, Name: "cleanup"},
		{Source: second, Relation: RelationCalls, Name: "dos_g23", External: "dos_g23"},
		{Source: second, Relation: RelationCalls, Name: "rom_entry", External: "rom_entry"},
	}
	if len(f.Refs) != len(want) {
		t.Errorf("got %d refs, want %d: %+v", len(f.Refs), len(want), f.Refs)
	}
	for _, ref := range want {
		if !hasRef(f, ref) {
			t.Errorf("missing ref %+v in %+v", ref, f.Refs)
		}
	}
	// Not: a sentence, a register that was written to since, a label that
	// was loaded and not called, the number of a global without its name
	for _, r := range f.Refs {
		switch r.Name {
		case "open", "now", "dos_g30", "Table", "G31", "not":
			t.Errorf("unexpected ref %+v", r)
		}
	}
}
