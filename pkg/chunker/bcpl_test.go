package chunker

import (
	"strings"
	"testing"
)

// A source in the BCPL of the 1980s, as the TRIPOS compiler is written in:
// no case told apart, $( $) with tags, conditional compilation, names with
// dots, and the old spellings of the operators.
const triposSource = `SECTION "CG5"

$$TRACE := TRUE
$<HDR'  GET "CGHDR"  $>HDR'

MANIFEST $(
   sw.no  = ('N'<<8) \/ 'O'
   $<BIG   maxsize = 400  $>BIG
   $<BIG'  maxsize = 100  $>BIG'
$)

GLOBAL $( cgapply:120; err.p:121 $)

static $( count = 0 $)

// Calls a routine.
LET cgapply(op, k) BE
$(1 LET sa1 = k+3
    let clear.cmp(a) be
    $( a!0 := 0
       count := count + 1
    $)
    IF op \= 0 /\ k > 0 DO CGERROR("bad*N")
    store(sa1, $<TRIPOS 5 $>TRIPOS $<RSX 6 $>RSX)
    $( clear.cmp(k)
       IF k=0 BREAK
$)1

|| Reports and stops.
AND cgerror(n) BE
$( writef("error %N*N", n)
   cgcondjump.(n)
$)

AND cgcondjump.(b) = valof
$( switchon b into
   $(sw case 1: resultis 2
        default: endcase
   $)sw
   resultis rdn.(b)
$)
`

func TestParse_BCPLOldDialect(t *testing.T) {
	// The sources of that time have no extension: the content says what it is
	f := parse(t, "/proj/bcp/mcg5", triposSource)
	if f.Language != "bcpl" || !f.FoldCase {
		t.Fatalf("language %q, FoldCase %v; want bcpl in which case does not matter", f.Language, f.FoldCase)
	}

	want := []struct {
		name, etype string
		start, end  int
	}{
		{"sw.no", TypeConstant, 7, 7},
		{"maxsize", TypeConstant, 8, 8},
		{"maxsize", TypeConstant, 9, 9},
		{"cgapply", TypeVariable, 12, 12},
		{"err.p", TypeVariable, 12, 12},
		{"count", TypeVariable, 14, 14},
		{"cgapply", TypeFunction, 16, 27},
		{"clear.cmp", TypeFunction, 19, 22},
		{"cgerror", TypeFunction, 29, 33},
		{"cgcondjump.", TypeFunction, 35, 41},
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

	// A definition inside a routine belongs to it
	outer := 6
	if nested := f.Symbols[7]; nested.Parent != outer || nested.Signature != "let clear.cmp(a) be" {
		t.Errorf("clear.cmp = %+v, want it inside cgapply with its signature as written", nested)
	}

	// Calls keep the spelling they have; who stores them folds the case
	for _, ref := range []Ref{
		{Source: outer, Relation: RelationCalls, Name: "CGERROR", External: "CGERROR"},
		{Source: outer, Relation: RelationCalls, Name: "store", External: "store"},
		{Source: outer, Relation: RelationCalls, Name: "clear.cmp", External: "clear.cmp"},
		{Source: 8, Relation: RelationCalls, Name: "cgcondjump.", External: "cgcondjump."},
		{Source: 9, Relation: RelationCalls, Name: "rdn.", External: "rdn."},
		{Source: -1, Relation: RelationImports, Name: "CGHDR", ImportBase: "CGHDR", External: "CGHDR"},
	} {
		if !hasRef(f, ref) {
			t.Errorf("missing ref %+v in %+v", ref, f.Refs)
		}
	}
}

func TestBCPLRead(t *testing.T) {
	src := `$$A := TRUE // set
$<A' let x = "let $(" $>A'
LET f() be $( x := 'a' /\ let.it; resultis 1 $)
/* let */ || let
`
	read := bcplRead([]byte(src))
	want := `            // set
$<A  LET x = "let $(" $>A 
LET f() BE $( x := 'a' /\ let.it; RESULTIS 1 $)
/* let */ || let
`
	if string(read.text) != want {
		t.Errorf("read as:\n%s\nwant:\n%s", read.text, want)
	}
	if !read.old || len(read.arms) != 1 || len(read.text) != len(src) {
		t.Errorf("old %v, %d arms, %d bytes of %d", read.old, len(read.arms), len(read.text), len(src))
	}

	// With braces it is the dialect of today, where let is a name
	modern := "LET f() BE { let := 1 }\n"
	if read := bcplRead([]byte(modern)); read.old || string(read.text) != modern {
		t.Errorf("modern source read as old %v:\n%s", read.old, read.text)
	}
}

func TestParse_BCPLFlatComments(t *testing.T) {
	// "/*/ ... //*/" is a comment where comments do not nest
	f := parse(t, "/proj/syn6.b", `LET f() BE
$( a := 1
//*/
   g(a)
/*/
   h(a)
//*/
$)

AND k() BE
$( f()
$)
`)
	if len(f.Symbols) != 2 || f.Symbols[1].Name != "k" || f.Symbols[1].StartLine != 10 {
		t.Fatalf("symbols = %+v, want f and k", f.Symbols)
	}
	for _, r := range f.Refs {
		if r.Name == "h" {
			t.Errorf("call inside a comment was taken as code: %+v", r)
		}
	}
}

func TestLanguage_Content(t *testing.T) {
	header := `// Standard header
MANIFEST {
  B2Wsh = 2
}
GLOBAL { start: 1; stop: 2 }
`
	tests := []struct {
		path, src, want string
	}{
		{"/proj/bcp/mcg5", triposSource, "bcpl"},
		{"/proj/g/libhdr.h", header, "bcpl"},
		{"/proj/include/api.h", "#ifndef API_H\n#define API_H\nstruct point { int x; };\nstatic const int n = 1;\n#endif\n", "cpp"},
		{"/proj/include/plain.h", "struct point { int x; };\nint get(struct point *p);\n", "cpp"},
		{"/proj/Makefile", "OBJS = $(SRC:.c=.o)\nall: $(OBJS)\n\techo $$(date) $)\n", ""},
		{"/proj/run", "#!/bin/sh\nGLOBAL $( x $)\nLET f() BE\n", ""},
		{"/proj/notes", "GET \"/index.html\" is what the client sends\n", ""},
		{"/proj/queue.b", "x", "bcpl"},
		{"/proj/main.c", header, "c"},
	}
	for _, tt := range tests {
		if got := Language(tt.path, []byte(tt.src)); got != tt.want {
			t.Errorf("Language(%s) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestParseAs(t *testing.T) {
	// A file that says nothing about itself, read as what it is said to be
	src := "LET f(x) = x + 1\n"
	if _, ok := Parse("/proj/fragment", []byte(src), 0); ok {
		t.Fatal("Parse took a fragment without extension for something")
	}
	f, ok := ParseAs("BCPL", "/proj/fragment", []byte(src), 0)
	if !ok || f.Language != "bcpl" || len(f.Symbols) != 1 || f.Symbols[0].Name != "f" {
		t.Fatalf("ParseAs(BCPL) = %+v, %v", f, ok)
	}
	if _, ok := ParseAs("fortran", "/proj/fragment", []byte(src), 0); ok {
		t.Error("ParseAs took a language it does not know")
	}
	if !strings.Contains(strings.Join(Languages(), " "), "m68k") {
		t.Errorf("Languages() = %v", Languages())
	}
}
