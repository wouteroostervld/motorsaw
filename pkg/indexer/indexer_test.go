package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/wouteroostervld/motorsaw/pkg/config"
)

func TestIndexFile(t *testing.T) {
	testFile := filepath.Join(t.TempDir(), "calc.py")
	content := `import math


# Adds two numbers.
def add(a, b):
    total = a + b
    total = total + 0
    total = int(total)
    return total


class Calc:
    def area(self, r):
        sq = self.square(r)
        scale = math.pi
        result = scale * sq
        return result

    def square(self, x):
        y = add(x, 0)
        y = y * y
        y = y + 0
        return y
`
	if err := os.WriteFile(testFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	mockDB := NewMockDatabase()
	idx := New(mockDB, nil, NewGraph(mockDB))
	if err := idx.IndexFile(context.Background(), testFile, false); err != nil {
		t.Fatalf("IndexFile failed: %v", err)
	}
	if mockDB.UpsertFileCount != 1 || mockDB.DeleteChunksCount != 1 {
		t.Errorf("file upserted %d times, old chunks deleted %d times, want 1 and 1", mockDB.UpsertFileCount, mockDB.DeleteChunksCount)
	}

	// Where the chunks are: the file, add, Calc, Calc.area, Calc.square. Their
	// text is not stored.
	wantChunks := [][2]int{{1, 24}, {4, 9}, {12, 23}, {13, 17}, {19, 23}}
	if len(mockDB.Chunks) != len(wantChunks) {
		t.Fatalf("got %d chunks, want %d", len(mockDB.Chunks), len(wantChunks))
	}
	for i, want := range wantChunks {
		if c := mockDB.Chunks[i]; c.StartLine != want[0] || c.EndLine != want[1] {
			t.Errorf("chunk %d = lines %d-%d, want %d-%d", i, c.StartLine, c.EndLine, want[0], want[1])
		}
	}

	// Each definition is an entity in the chunk that holds it; the module comes first
	wantDefs := []struct {
		name, container, etype, signature string
		chunk                             int64
		parent, start, end                int
	}{
		{"calc", "", "PACKAGE", "", 1, -1, 1, 24},
		{"add", "", "FUNCTION", "def add(a, b):", 2, 0, 4, 9},
		{"Calc", "", "CLASS", "class Calc:", 3, 0, 12, 23},
		{"area", "Calc", "METHOD", "def area(self, r):", 4, 2, 13, 17},
		{"square", "Calc", "METHOD", "def square(self, x):", 5, 2, 19, 23},
	}
	if len(mockDB.SymbolDefs) != len(wantDefs) {
		t.Fatalf("got %d definitions, want %d: %+v", len(mockDB.SymbolDefs), len(wantDefs), mockDB.SymbolDefs)
	}
	for i, want := range wantDefs {
		got := mockDB.SymbolDefs[i]
		if got.Name != want.name || got.Container != want.container || got.EntityType != want.etype ||
			got.Signature != want.signature || got.ChunkID != want.chunk || got.Parent != want.parent ||
			got.StartLine != want.start || got.EndLine != want.end {
			t.Errorf("definition %d = %+v, want %+v", i, got, want)
		}
	}

	calls := map[string]bool{}
	for _, ref := range mockDB.SymbolRefs {
		calls[mockDB.SymbolDefs[ref.Source].Name+" "+ref.RelationType+" "+ref.ExternalName] = true
	}
	for _, want := range []string{"calc imports math", "area calls square", "square calls add"} {
		if !calls[want] {
			t.Errorf("missing reference %q in %v", want, calls)
		}
	}
}

func TestIndexFile_Unparseable(t *testing.T) {
	testFile := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(testFile, []byte("just some notes\n"), 0600); err != nil {
		t.Fatal(err)
	}

	mockDB := NewMockDatabase()
	if err := New(mockDB, nil, NewGraph(mockDB)).IndexFile(context.Background(), testFile, false); err != nil {
		t.Fatalf("IndexFile failed: %v", err)
	}
	if len(mockDB.Chunks) != 0 || len(mockDB.SymbolDefs) != 0 {
		t.Errorf("a text file produced %d chunks and %d definitions, want none", len(mockDB.Chunks), len(mockDB.SymbolDefs))
	}
}

func TestIndexFile_Markup(t *testing.T) {
	testFile := filepath.Join(t.TempDir(), "guide.md")
	content := "# Guide\n\nIntro.\n\n## Install\n\nRun it.\n\nThen wait.\n\nDone.\n"
	if err := os.WriteFile(testFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	mockDB := NewMockDatabase()
	if err := New(mockDB, nil, NewGraph(mockDB)).IndexFile(context.Background(), testFile, false); err != nil {
		t.Fatalf("IndexFile failed: %v", err)
	}
	if len(mockDB.Chunks) < 2 || len(mockDB.SymbolDefs) != 0 {
		t.Errorf("a document produced %d chunks and %d definitions, want sections and no definitions", len(mockDB.Chunks), len(mockDB.SymbolDefs))
	}
}

func TestIndexFile_FoldsCase(t *testing.T) {
	// BCPL of the 1980s, in a file without an extension: CGERROR and cgerror
	// are one name, and are stored as one
	testFile := filepath.Join(t.TempDir(), "MCG1")
	content := `SECTION "MCG1"
GET "CGHDR"

LET CgStart(n) BE
$( CGERROR(n)
   cgerror(n+1)
$)

AND cgerror(n) BE
$( abort(n)
$)
`
	if err := os.WriteFile(testFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	mockDB := NewMockDatabase()
	if err := New(mockDB, nil, NewGraph(mockDB)).IndexFile(context.Background(), testFile, false); err != nil {
		t.Fatalf("IndexFile failed: %v", err)
	}
	var defs, refs []string
	for _, d := range mockDB.SymbolDefs {
		defs = append(defs, d.EntityType+" "+d.Name)
	}
	for _, r := range mockDB.SymbolRefs {
		refs = append(refs, r.RelationType+" "+r.Name)
	}
	if got, want := strings.Join(defs, ", "), "PACKAGE mcg1, FUNCTION cgstart, FUNCTION cgerror"; got != want {
		t.Errorf("definitions: %s; want %s", got, want)
	}
	if got, want := strings.Join(refs, ", "), "imports cghdr, calls cgerror, calls abort"; got != want {
		t.Errorf("references: %s; want %s", got, want)
	}
	if sig := mockDB.SymbolDefs[1].Signature; sig != "LET CgStart(n) BE" {
		t.Errorf("signature %q, want it as written", sig)
	}
}

// source writes a small source file and returns its path
func source(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calc.py")
	content := "def add(a, b):\n    total = a + b\n    total = total + 0\n    total = int(total)\n    return total\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func selector(kinds map[string]bool) Selector {
	return func(string) (map[string]bool, error) { return kinds, nil }
}

func TestModules_SwitchedPerFile(t *testing.T) {
	path := source(t)
	ctx := context.Background()
	mockDB := NewMockDatabase()
	embedder := &MockEmbedder{}
	kinds := map[string]bool{config.IndexGraph: true}
	idx := New(mockDB, selector(kinds), NewGraph(mockDB), NewEmbeddings(mockDB, embedder))

	// Only what is switched on runs
	if err := idx.IndexFile(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	if len(mockDB.SymbolDefs) == 0 || embedder.Calls != 0 || len(mockDB.Modules) != 1 {
		t.Fatalf("graph only: %d definitions, %d embedding calls, held by %v", len(mockDB.SymbolDefs), embedder.Calls, mockDB.Modules)
	}

	// Switching another kind on builds that one, and leaves the first alone
	kinds[config.IndexEmbeddings] = true
	deleted := mockDB.DeleteChunksCount
	if err := idx.IndexFile(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	if len(mockDB.Embeddings) == 0 || mockDB.DeleteChunksCount != deleted {
		t.Errorf("embeddings switched on: %d embeddings, graph rebuilt %d times", len(mockDB.Embeddings), mockDB.DeleteChunksCount-deleted)
	}

	// Nothing changed: nothing runs, unless forced
	calls := embedder.Calls
	if err := idx.IndexFile(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	if embedder.Calls != calls || mockDB.DeleteChunksCount != deleted {
		t.Error("an unchanged file was indexed again")
	}
	inserts := mockDB.EmbeddingInserts
	if err := idx.IndexFile(ctx, path, true); err != nil {
		t.Fatal(err)
	}
	if mockDB.EmbeddingInserts == inserts || mockDB.DeleteChunksCount == deleted {
		t.Error("forcing did not index an unchanged file again")
	}
	// The text is what it was, so the model is not asked again
	if embedder.Calls != calls {
		t.Errorf("forcing asked the model %d times for text it had embedded", embedder.Calls-calls)
	}

	// Switching a kind off forgets what it held, and only that
	kinds[config.IndexGraph] = false
	if err := idx.IndexFile(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	if len(mockDB.SymbolDefs) != 0 || len(mockDB.Embeddings) == 0 {
		t.Errorf("graph switched off: %d definitions, %d embeddings", len(mockDB.SymbolDefs), len(mockDB.Embeddings))
	}
	if _, held := mockDB.Modules[config.IndexGraph]; held || len(mockDB.Modules) != 1 {
		t.Errorf("held by %v, want embeddings only", mockDB.Modules)
	}
}

func TestModules_Only(t *testing.T) {
	path := source(t)
	ctx := context.Background()
	mockDB := NewMockDatabase()
	embedder := &MockEmbedder{}
	kinds := map[string]bool{config.IndexGraph: true, config.IndexEmbeddings: true}
	idx := New(mockDB, selector(kinds), NewGraph(mockDB), NewEmbeddings(mockDB, embedder))

	// A command that needs the graph does not wait for a model
	if err := idx.Only(config.IndexGraph).IndexFile(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	if embedder.Calls != 0 || len(mockDB.SymbolDefs) == 0 {
		t.Fatalf("graph only: %d embedding calls, %d definitions", embedder.Calls, len(mockDB.SymbolDefs))
	}

	// The rest is still to be done, and is all that is done later
	plan, err := idx.Plan(path, mockDB.Modules[config.IndexGraph], mockDB.Modules, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Update) != 1 || plan.Update[0] != config.IndexEmbeddings || len(plan.Forget) != 0 {
		t.Errorf("plan = %+v, want embeddings to update", plan)
	}

	// Out of scope is left alone, even when it is switched off
	kinds[config.IndexGraph] = false
	if err := idx.Only(config.IndexEmbeddings).IndexFile(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	if len(mockDB.SymbolDefs) == 0 || len(mockDB.Embeddings) == 0 {
		t.Errorf("embeddings only: %d definitions, %d embeddings", len(mockDB.SymbolDefs), len(mockDB.Embeddings))
	}

	// A file that is gone is forgotten by all, whatever the scope
	if err := idx.Only(config.IndexGraph).ForgetFile(1, path); err != nil {
		t.Fatal(err)
	}
	if len(mockDB.SymbolDefs) != 0 || len(mockDB.Embeddings) != 0 || len(mockDB.Forgotten) != 1 {
		t.Errorf("forgotten: %d definitions, %d embeddings, %v", len(mockDB.SymbolDefs), len(mockDB.Embeddings), mockDB.Forgotten)
	}
}

func TestModules_FailureIsKeptApart(t *testing.T) {
	path := source(t)
	ctx := context.Background()
	mockDB := NewMockDatabase()
	embedder := &MockEmbedder{Err: errors.New("model is away")}
	idx := New(mockDB, nil, NewGraph(mockDB), NewEmbeddings(mockDB, embedder))

	// The graph is built and kept; the failure is reported
	err := idx.IndexFile(ctx, path, false)
	if err == nil || !strings.Contains(err.Error(), "embeddings index: model is away") {
		t.Fatalf("err = %v", err)
	}
	if len(mockDB.SymbolDefs) == 0 || len(mockDB.Modules) != 1 {
		t.Fatalf("%d definitions, held by %v", len(mockDB.SymbolDefs), mockDB.Modules)
	}

	// The next run does only what failed
	embedder.Err = nil
	deleted := mockDB.DeleteChunksCount
	if err := idx.IndexFile(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	if mockDB.DeleteChunksCount != deleted || len(mockDB.Embeddings) == 0 {
		t.Errorf("retry: graph rebuilt %d times, %d embeddings", mockDB.DeleteChunksCount-deleted, len(mockDB.Embeddings))
	}
}

func TestEmbeddings_NoModel(t *testing.T) {
	mockDB := NewMockDatabase()
	idx := New(mockDB, nil, NewGraph(mockDB), NewEmbeddings(mockDB, nil))

	// Asked before a run, so that it is said once
	if err := idx.Ready([]string{config.IndexGraph}); err != nil {
		t.Errorf("the graph needs nothing: %v", err)
	}
	if err := idx.Ready([]string{config.IndexGraph, config.IndexEmbeddings}); !errors.Is(err, ErrNoEmbedder) {
		t.Errorf("err = %v, want ErrNoEmbedder", err)
	}
	if err := idx.IndexFile(context.Background(), source(t), false); !errors.Is(err, ErrNoEmbedder) {
		t.Errorf("err = %v, want ErrNoEmbedder", err)
	}
	// Without a model it can still forget
	if err := idx.ForgetFile(1, "gone.py"); err != nil {
		t.Errorf("forgetting without a model: %v", err)
	}
}

func TestEmbeddings_Chunks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "calc.py")
	var body strings.Builder
	for i := 0; i < 40; i++ {
		body.WriteString("    total = total + 1\n")
	}
	content := "import math\n\n\ndef add(a, b):\n    total = a + b\n" + body.String() + "    return total\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	mockDB := NewMockDatabase()
	embedder := &MockEmbedder{}
	module := NewEmbeddings(mockDB, embedder)
	module.MaxChunkSize = 300
	module.BatchSize = 2
	if err := New(mockDB, nil, module).IndexFile(context.Background(), path, false); err != nil {
		t.Fatal(err)
	}

	if mockDB.EmbeddingModel != "mock-model" {
		t.Errorf("model = %q", mockDB.EmbeddingModel)
	}
	// The file folded to its signatures, and the function cut into pieces that fit
	if len(mockDB.Embeddings) < 4 {
		t.Fatalf("%d embeddings, want the skeleton and several pieces of add", len(mockDB.Embeddings))
	}
	if want := (len(mockDB.Embeddings) + 1) / 2; embedder.Calls != want {
		t.Errorf("%d requests for %d chunks in batches of 2, want %d", embedder.Calls, len(mockDB.Embeddings), want)
	}
	for i, c := range mockDB.Embeddings {
		if len(c.Vector) != 2 {
			t.Errorf("embedding %d has no vector", i)
		}
		if c.StartLine < 1 || c.EndLine > 47 || c.StartLine > c.EndLine {
			t.Errorf("embedding %d covers lines %d-%d", i, c.StartLine, c.EndLine)
		}
	}
	last := mockDB.Embeddings[len(mockDB.Embeddings)-1]
	if last.Title == "" || last.EndLine != 46 || last.StartLine <= 4 {
		t.Errorf("last piece = %q lines %d-%d, want the end of add", last.Title, last.StartLine, last.EndLine)
	}
	// What is embedded says where it is from, and stays within the limit
	prefix := filepath.Base(dir) + "/calc.py"
	for i, text := range embedder.Texts {
		if !strings.HasPrefix(text, prefix) {
			t.Errorf("text %d does not start with %q: %q", i, prefix, text[:min(len(text), 60)])
		}
		if len(text) > 300+len(prefix)+40 {
			t.Errorf("text %d is %d bytes", i, len(text))
		}
	}
}

func TestEmbeddings_ReusesVectors(t *testing.T) {
	function := func(name, line string) string {
		var body strings.Builder
		for i := 0; i < 12; i++ {
			body.WriteString("    " + line + "\n")
		}
		return "def " + name + "(total):\n" + body.String() + "    return total\n\n\n"
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "one", "calc.py")
	write(path, function("add", "total = total + 1")+function("sub", "total = total - 1"))

	mockDB := NewMockDatabase()
	embedder := &MockEmbedder{}
	module := NewEmbeddings(mockDB, embedder)
	module.MaxChunkSize = 300
	idx := New(mockDB, nil, module)
	ctx := context.Background()
	if err := idx.IndexFile(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	all := len(embedder.Texts)
	if all < 3 || len(mockDB.Embeddings) != all {
		t.Fatalf("%d texts embedded for %d chunks, want the skeleton and both functions", all, len(mockDB.Embeddings))
	}

	// An edit in one function sends that function, not the rest of the file
	write(path, function("add", "total = total + 1")+function("sub", "total = total - 2"))
	if err := idx.IndexFile(ctx, path, true); err != nil {
		t.Fatal(err)
	}
	sent := embedder.Texts[all:]
	if len(sent) == 0 || len(sent) >= all {
		t.Errorf("an edit in sub sent %d of %d chunks", len(sent), all)
	}
	for _, text := range sent {
		if !strings.Contains(text, "total - 2") {
			t.Errorf("sent a chunk that did not change: %q", text[:min(len(text), 60)])
		}
	}
	if len(mockDB.Embeddings) != all {
		t.Errorf("%d chunks stored after the edit, want %d", len(mockDB.Embeddings), all)
	}
	for i, c := range mockDB.Embeddings {
		if len(c.Vector) == 0 {
			t.Errorf("chunk %d has no vector", i)
		}
	}

	// The same file in another place, a worktree or a virtual environment,
	// is not sent at all
	before := len(embedder.Texts)
	copied := filepath.Join(dir, "two", "calc.py")
	write(copied, function("add", "total = total + 1")+function("sub", "total = total - 2"))
	if err := idx.IndexFile(ctx, copied, false); err != nil {
		t.Fatal(err)
	}
	if len(embedder.Texts) != before || len(mockDB.Embeddings) != all {
		t.Errorf("a copy sent %d texts and stored %d chunks, want 0 and %d", len(embedder.Texts)-before, len(mockDB.Embeddings), all)
	}
}

func TestEmbeddings_EditSendsItsBranch(t *testing.T) {
	body := strings.Repeat("        x = x + 1\n", 10)
	source := func(signature, extra string) string {
		return "class A:\n    " + signature + ":\n" + extra + body + "        return x\n\n" +
			"    def g(self, x):\n" + body + "        return x\n\n\n" +
			"class B:\n    def h(self, x):\n" + body + "        return x\n"
	}
	path := filepath.Join(t.TempDir(), "a.py")
	if err := os.WriteFile(path, []byte(source("def f(self, x)", "")), 0600); err != nil {
		t.Fatal(err)
	}

	mockDB := NewMockDatabase()
	embedder := &MockEmbedder{}
	module := NewEmbeddings(mockDB, embedder)
	module.MaxChunkSize = 300
	idx := New(mockDB, nil, module)
	if err := idx.IndexFile(context.Background(), path, false); err != nil {
		t.Fatal(err)
	}

	// Folded definitions are named, not placed: their lines are in the index
	for _, text := range embedder.Texts {
		if strings.Contains(text, "chunk: lines") {
			t.Errorf("line numbers sent to the model: %q", text)
		}
	}
	held := map[string][2]int{}
	for _, c := range mockDB.Embeddings {
		held[c.Title] = [2]int{c.StartLine, c.EndLine}
	}
	if held["A.f"] != [2]int{2, 13} || held["B.h"] != [2]int{30, 41} {
		t.Errorf("lines in the index = %v, want A.f at 2-13 and B.h at 30-41", held)
	}

	edit := func(signature, extra string) []string {
		t.Helper()
		before := len(embedder.Texts)
		if err := os.WriteFile(path, []byte(source(signature, extra)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := idx.IndexFile(context.Background(), path, true); err != nil {
			t.Fatal(err)
		}
		var sent []string
		for _, text := range embedder.Texts[before:] {
			head, _, _ := strings.Cut(text, "\n")
			_, title, _ := strings.Cut(head, ": ")
			sent = append(sent, title)
		}
		sort.Strings(sent)
		return sent
	}

	// A line more in the body of A.f moves everything below it, but only
	// A.f reads differently: A and the file show it folded to its signature
	if sent := edit("def f(self, x)", "        x = x * 2\n"); !reflect.DeepEqual(sent, []string{"A.f"}) {
		t.Errorf("an edit in a body sent again %q, want A.f", sent)
	}
	// A new signature shows in A as well, which folds it
	if sent := edit("def f(self, x, y)", "        x = x * 2\n"); !reflect.DeepEqual(sent, []string{"A", "A.f"}) {
		t.Errorf("a new signature sent again %q, want A and A.f", sent)
	}
	// What was reused is stored where it is now
	for _, c := range mockDB.Embeddings {
		if c.Title == "B.h" && c.StartLine != 31 {
			t.Errorf("B.h stored at line %d, want 31", c.StartLine)
		}
	}
}

// Forgetting a file asks the embeddings to forget it only when some were
// made of it: finding a file's rows in the vector table scans every vector,
// which made forgetting a file without any take a fifth of a second
func TestForgetFile_EmbeddingsOnlyWhenMade(t *testing.T) {
	path := source(t)
	ctx := context.Background()
	mockDB := NewMockDatabase()
	kinds := map[string]bool{config.IndexGraph: true}
	idx := New(mockDB, selector(kinds), NewGraph(mockDB), NewEmbeddings(mockDB, &MockEmbedder{}))

	if err := idx.IndexFile(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	if err := idx.ForgetFile(1, path); err != nil {
		t.Fatal(err)
	}
	if mockDB.EmbeddingDeletes != 0 {
		t.Errorf("embeddings asked to forget a file of which none were made (%d times)", mockDB.EmbeddingDeletes)
	}
	if mockDB.DeleteChunksCount == 0 || len(mockDB.Forgotten) != 1 {
		t.Errorf("the graph and the file must still be forgotten: %d chunk deletes, forgotten %v", mockDB.DeleteChunksCount, mockDB.Forgotten)
	}

	kinds[config.IndexEmbeddings] = true
	if err := idx.IndexFile(ctx, path, true); err != nil {
		t.Fatal(err)
	}
	before := mockDB.EmbeddingDeletes
	if err := idx.ForgetFile(1, path); err != nil {
		t.Fatal(err)
	}
	if mockDB.EmbeddingDeletes != before+1 || len(mockDB.Embeddings) != 0 {
		t.Errorf("a file with embeddings must have them forgotten: %d deletes, %d left", mockDB.EmbeddingDeletes-before, len(mockDB.Embeddings))
	}
}
