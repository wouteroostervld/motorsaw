package db

import (
	"reflect"
	"strings"
	"testing"
)

func TestEmbeddings(t *testing.T) {
	database := openSymbolsDB(t)

	// Nothing embedded yet: no table, and nothing fails for the lack of it
	if model, dim, err := database.EmbeddingModel(); err != nil || model != "" || dim != 0 {
		t.Fatalf("empty index: model %q, dim %d, err %v", model, dim, err)
	}
	if hits, err := database.SearchEmbeddings([]float32{1, 0, 0}, 5, ""); err != nil || len(hits) != 0 {
		t.Fatalf("search of an empty index: %v, %v", hits, err)
	}
	if err := database.DeleteEmbeddings(1); err != nil {
		t.Fatalf("delete from an empty index: %v", err)
	}

	a, _ := addFile(t, database, "/src/one/a.go")
	b, _ := addFile(t, database, "/src/two/b.go")
	if err := database.InsertEmbeddings(a, "m", []EmbeddingChunk{
		{StartLine: 1, EndLine: 9, Vector: []float32{1, 0, 0}},
		{StartLine: 3, EndLine: 7, Title: "Near", Vector: []float32{0.9, 0.1, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.InsertEmbeddings(b, "m", []EmbeddingChunk{
		{StartLine: 2, EndLine: 4, Title: "Far", Vector: []float32{0, 1, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	if model, dim, _ := database.EmbeddingModel(); model != "m" || dim != 3 {
		t.Errorf("model %q, dim %d, want m and 3", model, dim)
	}

	hits, err := database.SearchEmbeddings([]float32{1, 0, 0}, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Path != "/src/one/a.go" || hits[0].EndLine != 9 || hits[1].Title != "Near" {
		t.Errorf("nearest two = %+v", hits)
	}
	if hits[0].Distance > 0.001 || hits[1].Distance <= hits[0].Distance {
		t.Errorf("distances = %v, %v", hits[0].Distance, hits[1].Distance)
	}

	// A root limits the search to the files under it, however far they are
	hits, err = database.SearchEmbeddings([]float32{1, 0, 0}, 2, "/src/two")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "/src/two/b.go" || hits[0].Title != "Far" {
		t.Errorf("under /src/two = %+v", hits)
	}
	if hits, _ := database.SearchEmbeddings([]float32{1, 0, 0}, 2, "/src/tw"); len(hits) != 0 {
		t.Errorf("a root that is only a prefix of a name matched: %+v", hits)
	}

	// One model, one length
	if err := database.InsertEmbeddings(b, "other", []EmbeddingChunk{{Vector: []float32{1, 0, 0}}}); err == nil || !strings.Contains(err.Error(), "other") {
		t.Errorf("another model: err = %v", err)
	}
	if err := database.InsertEmbeddings(b, "m", []EmbeddingChunk{{Vector: []float32{1, 0}}}); err == nil {
		t.Error("a vector of another length was accepted")
	}

	if err := database.DeleteEmbeddings(a); err != nil {
		t.Fatal(err)
	}
	if n, _ := database.CountEmbeddings(); n != 1 {
		t.Errorf("%d embeddings after deleting a file's, want 1", n)
	}

	// A reset makes room for another model
	if err := database.SetFileModule(b, "embeddings", 1); err != nil {
		t.Fatal(err)
	}
	if err := database.ResetEmbeddings("embeddings"); err != nil {
		t.Fatal(err)
	}
	if modules, _ := database.FileModules(b); len(modules) != 0 {
		t.Errorf("after a reset the file is still held: %v", modules)
	}
	if err := database.InsertEmbeddings(b, "other", []EmbeddingChunk{{Vector: []float32{1, 0}}}); err != nil {
		t.Errorf("another model after a reset: %v", err)
	}
}

func TestCachedEmbeddings(t *testing.T) {
	database := openSymbolsDB(t)

	// Nothing embedded yet: nothing is found, and nothing fails
	if cached, err := database.CachedEmbeddings([]string{"k1"}); err != nil || len(cached) != 0 {
		t.Fatalf("empty index: %v, %v", cached, err)
	}

	a, _ := addFile(t, database, "/src/one/a.go")
	b, _ := addFile(t, database, "/src/two/a.go")
	if err := database.InsertEmbeddings(a, "m", []EmbeddingChunk{
		{StartLine: 1, EndLine: 9, Key: "k1", Vector: []float32{1, 0.5, -2}},
		{StartLine: 3, EndLine: 7, Vector: []float32{0, 1, 0}}, // No key: never reused
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.InsertEmbeddings(b, "m", []EmbeddingChunk{
		{StartLine: 1, EndLine: 9, Key: "k1", Vector: []float32{1, 0.5, -2}},
	}); err != nil {
		t.Fatal(err)
	}

	cached, err := database.CachedEmbeddings([]string{"k1", "k2", "k1", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 1 || !reflect.DeepEqual(cached["k1"], []float32{1, 0.5, -2}) {
		t.Errorf("cached = %v, want k1 only, as stored", cached)
	}

	// A key stays while any chunk with that text is left
	if err := database.DeleteEmbeddings(a); err != nil {
		t.Fatal(err)
	}
	if cached, _ := database.CachedEmbeddings([]string{"k1"}); len(cached) != 1 {
		t.Errorf("after deleting one of two copies: %v, want k1", cached)
	}
	if err := database.DeleteEmbeddings(b); err != nil {
		t.Fatal(err)
	}
	if cached, _ := database.CachedEmbeddings([]string{"k1"}); len(cached) != 0 {
		t.Errorf("after deleting both copies: %v, want nothing", cached)
	}
	var keys int
	if err := database.conn.QueryRow("SELECT COUNT(*) FROM embedding_keys").Scan(&keys); err != nil || keys != 0 {
		t.Errorf("%d keys left without a chunk (%v)", keys, err)
	}

	// A reset forgets every key with the vectors
	if err := database.InsertEmbeddings(a, "m", []EmbeddingChunk{{Key: "k1", Vector: []float32{1, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := database.ResetEmbeddings("embeddings"); err != nil {
		t.Fatal(err)
	}
	if err := database.conn.QueryRow("SELECT COUNT(*) FROM embedding_keys").Scan(&keys); err != nil || keys != 0 {
		t.Errorf("%d keys left after a reset (%v)", keys, err)
	}
}
