package indexer

import (
	"context"

	"github.com/wouteroostervld/motorsaw/pkg/db"
)

// MockDatabase is a simple mock for testing. It records what the indexer and
// its modules store; operations they do not use are left to the embedded
// interface and panic when called.
type MockDatabase struct {
	db.Database

	UpsertFileCount   int
	DeleteChunksCount int
	Forgotten         []string

	Modules    map[string]int64 // What holds the one file the mock knows
	Chunks     []MockChunk
	SymbolDefs []db.SymbolDef
	SymbolRefs []db.SymbolRef

	EmbeddingModel   string
	Embeddings       []db.EmbeddingChunk
	EmbeddingInserts int
}

// MockChunk is a chunk the indexer stored
type MockChunk struct {
	ID        int64
	StartLine int
	EndLine   int
}

func NewMockDatabase() *MockDatabase {
	return &MockDatabase{Modules: make(map[string]int64)}
}

func (m *MockDatabase) UpsertFile(path string, modTime int64, contentHash string) (int64, error) {
	m.UpsertFileCount++
	return 1, nil
}

func (m *MockDatabase) ForgetFile(path string) error {
	m.Forgotten = append(m.Forgotten, path)
	m.Modules = make(map[string]int64)
	return nil
}

func (m *MockDatabase) FileModules(fileID int64) (map[string]int64, error) {
	held := make(map[string]int64, len(m.Modules))
	for name, modTime := range m.Modules {
		held[name] = modTime
	}
	return held, nil
}

func (m *MockDatabase) SetFileModule(fileID int64, module string, modTime int64) error {
	m.Modules[module] = modTime
	return nil
}

func (m *MockDatabase) ClearFileModule(fileID int64, module string) error {
	delete(m.Modules, module)
	return nil
}

func (m *MockDatabase) InsertChunk(fileID int64, startLine, endLine int) (int64, error) {
	id := int64(len(m.Chunks) + 1)
	m.Chunks = append(m.Chunks, MockChunk{ID: id, StartLine: startLine, EndLine: endLine})
	return id, nil
}

func (m *MockDatabase) DeleteChunksForFile(fileID int64) error {
	m.DeleteChunksCount++
	m.Chunks, m.SymbolDefs, m.SymbolRefs = nil, nil, nil
	return nil
}

func (m *MockDatabase) SetProjectFollows(root string, dirs []string) error {
	return nil
}

func (m *MockDatabase) StoreFileSymbols(fileID int64, root string, defs []db.SymbolDef, refs []db.SymbolRef) error {
	m.SymbolDefs = append(m.SymbolDefs, defs...)
	m.SymbolRefs = append(m.SymbolRefs, refs...)
	return nil
}

func (m *MockDatabase) InsertEmbeddings(fileID int64, model string, chunks []db.EmbeddingChunk) error {
	m.EmbeddingModel = model
	m.Embeddings = append(m.Embeddings, chunks...)
	m.EmbeddingInserts++
	return nil
}

func (m *MockDatabase) DeleteEmbeddings(fileID int64) error {
	m.Embeddings = nil
	return nil
}

func (m *MockDatabase) CachedEmbeddings(keys []string) (map[string][]float32, error) {
	vectors := make(map[string][]float32)
	for _, key := range keys {
		for _, c := range m.Embeddings {
			if c.Key != "" && c.Key == key {
				vectors[key] = c.Vector
			}
		}
	}
	return vectors, nil
}

// MockEmbedder gives every text a vector without asking a model, and records
// what it was asked
type MockEmbedder struct {
	Texts []string
	Calls int
	Err   error // Returned instead of embeddings when set
}

func (m *MockEmbedder) Model() string { return "mock-model" }

func (m *MockEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	m.Calls++
	if m.Err != nil {
		return nil, m.Err
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		m.Texts = append(m.Texts, text)
		vectors[i] = []float32{float32(len(text)), 1}
	}
	return vectors, nil
}
