package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	if err := idx.IndexFile(ctx, path, true); err != nil {
		t.Fatal(err)
	}
	if embedder.Calls == calls || mockDB.DeleteChunksCount == deleted {
		t.Error("forcing did not index an unchanged file again")
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
