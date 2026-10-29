package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
	"github.com/wouteroostervld/motorsaw/pkg/config"
	"github.com/wouteroostervld/motorsaw/pkg/db"
	"github.com/wouteroostervld/motorsaw/pkg/gotypes"
)

// GraphStore is where the graph module keeps what it derives from a file
type GraphStore interface {
	InsertChunk(fileID int64, startLine, endLine int) (int64, error)
	DeleteChunksForFile(fileID int64) error
	StoreFileSymbols(fileID int64, root string, defs []db.SymbolDef, refs []db.SymbolRef) error
	SetProjectFollows(root string, dirs []string) error
}

// Graph is the module that builds the code graph: it parses a file, stores
// the chunks it folds into, and records its definitions, calls and imports.
// It needs nothing but the file, and takes milliseconds.
type Graph struct {
	GoTypes bool // Resolve the calls in Go files with the Go type checker
	// Which project a file is in and what the project follows. Without it a
	// file is in its repository, or else its directory, and follows nothing.
	Projects *Projects

	store   GraphStore
	goTypes *gotypes.Resolver

	mu       sync.Mutex
	followed map[string]bool // Projects whose follow setting is stored
}

// NewGraph creates the graph module
func NewGraph(store GraphStore) *Graph {
	return &Graph{GoTypes: true, store: store, goTypes: gotypes.NewResolver(), followed: make(map[string]bool)}
}

func (g *Graph) Name() string { return config.IndexGraph }

func (g *Graph) Ready() error { return nil }

func (g *Graph) Forget(fileID int64) error {
	return g.store.DeleteChunksForFile(fileID)
}

// Index stores the graph of a file. A file in a language that cannot be
// parsed contributes nothing to it.
func (g *Graph) Index(ctx context.Context, src *Source) error {
	if err := g.store.DeleteChunksForFile(src.FileID); err != nil {
		return fmt.Errorf("failed to delete old chunks: %w", err)
	}

	parsed, ok := src.Parse(0)
	if !ok {
		slog.Debug("Not a parseable file", "file", src.Path)
		return nil
	}

	var err error
	chunkIDs := make([]int64, len(parsed.Chunks))
	for i, c := range parsed.Chunks {
		if chunkIDs[i], err = g.store.InsertChunk(src.FileID, c.StartLine, c.EndLine); err != nil {
			return fmt.Errorf("failed to insert chunk: %w", err)
		}
	}
	slog.Debug("Stored chunks", "file", src.Path, "count", len(chunkIDs))

	// Documents fold into chunks but define no symbols
	if parsed.Markup {
		return nil
	}

	if parsed.Language == "go" && g.GoTypes {
		// Without it the calls stay as the syntax tree gives them
		if err := g.goTypes.Apply(parsed, src.Path); err != nil {
			slog.Debug("Go type check failed", "file", src.Path, "error", err)
		}
	}

	if err := g.storeSymbols(src.FileID, src.Path, parsed, chunkIDs); err != nil {
		return fmt.Errorf("failed to store symbols: %w", err)
	}
	return nil
}

// follow stores where the references of a project may lead outside it, the
// first time a file of the project is indexed
func (g *Graph) follow(root string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.followed[root] {
		return nil
	}
	dirs, err := g.Projects.Follows(root)
	if err != nil {
		return err
	}
	if err := g.store.SetProjectFollows(root, dirs); err != nil {
		return err
	}
	g.followed[root] = true
	return nil
}

// storeSymbols stores the definitions, calls and imports of a parsed file as
// graph entities and edges
func (g *Graph) storeSymbols(fileID int64, filePath string, parsed *chunker.File, chunkIDs []int64) error {
	// Where case does not tell names apart they are kept in lower case, so
	// that a call of CGERROR finds the definition of cgerror
	name := func(s string) string {
		if parsed.FoldCase {
			return strings.ToLower(s)
		}
		return s
	}

	// The module comes first; it defines everything at file level and is the
	// source of imports and of calls outside any definition
	defs := make([]db.SymbolDef, 0, len(parsed.Symbols)+1)
	defs = append(defs, db.SymbolDef{
		Name:       name(parsed.Module),
		EntityType: chunker.TypePackage,
		ChunkID:    chunkIDs[0],
		Parent:     -1,
		StartLine:  parsed.Chunks[0].StartLine,
		EndLine:    parsed.Chunks[0].EndLine,
	})
	for _, sym := range parsed.Symbols {
		defs = append(defs, db.SymbolDef{
			Name:       name(sym.Name),
			Container:  sym.Container,
			Signature:  sym.Signature,
			StartLine:  sym.StartLine,
			EndLine:    sym.EndLine,
			EntityType: sym.Type,
			ChunkID:    chunkIDs[sym.Chunk],
			Parent:     sym.Parent + 1,
		})
	}

	refs := make([]db.SymbolRef, 0, len(parsed.Refs))
	seen := make(map[db.SymbolRef]bool)
	for _, ref := range parsed.Refs {
		r := db.SymbolRef{
			Source:       ref.Source + 1,
			RelationType: ref.Relation,
			Name:         name(ref.Name),
			Container:    ref.Container,
			ImportBase:   name(ref.ImportBase),
			TargetPath:   ref.TargetPath,
			ExternalName: ref.External,
			Member:       ref.Member,
		}
		if ref.Relation == chunker.RelationCalls || ref.Relation == chunker.RelationMayCall {
			r.ExternalName = name(ref.External)
		}
		// Two spellings of one name are one reference
		if seen[r] {
			continue
		}
		seen[r] = true
		refs = append(refs, r)
	}

	root := g.Projects.Root(filePath)
	if err := g.follow(root); err != nil {
		return err
	}
	if err := g.store.StoreFileSymbols(fileID, root, defs, refs); err != nil {
		return err
	}
	slog.Debug("Stored symbols", "file_id", fileID, "definitions", len(defs), "references", len(refs))
	return nil
}
