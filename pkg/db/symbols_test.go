package db

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func openSymbolsDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(Config{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// addFile registers a file with a single chunk and returns their IDs
func addFile(t *testing.T, db *DB, path string) (fileID, chunkID int64) {
	t.Helper()
	fileID, err := db.UpsertFile(path, 1, "hash")
	if err != nil {
		t.Fatalf("Failed to upsert file: %v", err)
	}
	chunkID, err = db.InsertChunk(fileID, 1, 10)
	if err != nil {
		t.Fatalf("Failed to insert chunk: %v", err)
	}
	return fileID, chunkID
}

type edge struct {
	Target string
	Type   string
	Weight float64
}

// edgesFrom returns the edges of one relation leaving the named entity
func edgesFrom(t *testing.T, db *DB, source, relation string) []edge {
	t.Helper()
	rows, err := db.conn.Query(`
		SELECT e2.name, e2.entity_type, g.weight
		FROM graph_edges g
		JOIN entities e1 ON e1.id = g.source_entity_id
		JOIN entities e2 ON e2.id = g.target_entity_id
		WHERE e1.name = ? AND g.relation_type = ?
	`, source, relation)
	if err != nil {
		t.Fatalf("Failed to query edges: %v", err)
	}
	defer rows.Close()

	var edges []edge
	for rows.Next() {
		var e edge
		if err := rows.Scan(&e.Target, &e.Type, &e.Weight); err != nil {
			t.Fatalf("Failed to scan edge: %v", err)
		}
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].Target < edges[j].Target })
	return edges
}

func TestStoreFileSymbols_ResolvesAcrossFiles(t *testing.T) {
	db := openSymbolsDB(t)

	// The caller is indexed before the file that defines what it calls
	callerFile, callerChunk := addFile(t, db, "/proj/app/main.go")
	callerDefs := []SymbolDef{
		{Name: "main", EntityType: "PACKAGE", ChunkID: callerChunk, Parent: -1},
		{Name: "Run", EntityType: "FUNCTION", ChunkID: callerChunk, Parent: 0},
	}
	callerRefs := []SymbolRef{
		{Source: 0, RelationType: "imports", Name: "store", ImportBase: "store", ExternalName: "example.com/proj/store"},
		{Source: 1, RelationType: "calls", Name: "Open", ImportBase: "store", ExternalName: "store.Open", Member: true},
		{Source: 1, RelationType: "calls", Name: "Println", ImportBase: "fmt", ExternalName: "fmt.Println", Member: true},
		{Source: 1, RelationType: "calls", Name: "Close", Container: "s", ExternalName: "Close", Member: true},
	}
	if err := db.StoreFileSymbols(callerFile, "/proj", callerDefs, callerRefs); err != nil {
		t.Fatalf("StoreFileSymbols failed: %v", err)
	}

	unresolved := []edge{
		{"Close", EntityTypeExternal, weightExact},
		{"fmt.Println", EntityTypeExternal, weightExact},
		{"store.Open", EntityTypeExternal, weightExact},
	}
	if got := edgesFrom(t, db, "Run", "calls"); !reflect.DeepEqual(got, unresolved) {
		t.Errorf("calls before definitions are indexed = %v, want %v", got, unresolved)
	}
	if got, want := edgesFrom(t, db, "main", "defines"), []edge{{"Run", "FUNCTION", weightExact}}; !reflect.DeepEqual(got, want) {
		t.Errorf("defines = %v, want %v", got, want)
	}

	storeFile, storeChunk := addFile(t, db, "/proj/store/store.go")
	storeDefs := []SymbolDef{
		{Name: "store", EntityType: "PACKAGE", ChunkID: storeChunk, Parent: -1},
		{Name: "Open", EntityType: "FUNCTION", ChunkID: storeChunk, Parent: 0},
		{Name: "Close", Container: "Store", EntityType: "METHOD", ChunkID: storeChunk, Parent: 0},
	}
	if err := db.StoreFileSymbols(storeFile, "/proj", storeDefs, nil); err != nil {
		t.Fatalf("StoreFileSymbols failed: %v", err)
	}

	resolved := []edge{
		{"Open", "FUNCTION", weightExact}, // Through the import
		{"fmt.Println", EntityTypeExternal, weightExact},
	}
	if got := edgesFrom(t, db, "Run", "calls"); !reflect.DeepEqual(got, resolved) {
		t.Errorf("calls after definitions are indexed = %v, want %v", got, resolved)
	}
	// The receiver's type is unknown, so this is a match by name only
	if got, want := edgesFrom(t, db, "Run", RelationMayCall), []edge{{"Store.Close", "METHOD", weightByName}}; !reflect.DeepEqual(got, want) {
		t.Errorf("may_call = %v, want %v", got, want)
	}
	if got, want := edgesFrom(t, db, "main", "imports"), []edge{{"store", "PACKAGE", weightExact}}; !reflect.DeepEqual(got, want) {
		t.Errorf("imports = %v, want %v", got, want)
	}

	// Removing the definitions turns the calls back into external ones
	if err := db.DeleteChunksForFile(storeFile); err != nil {
		t.Fatalf("DeleteChunksForFile failed: %v", err)
	}
	if err := db.StoreFileSymbols(storeFile, "/proj", nil, nil); err != nil {
		t.Fatalf("StoreFileSymbols failed: %v", err)
	}
	if got := edgesFrom(t, db, "Run", "calls"); !reflect.DeepEqual(got, unresolved) {
		t.Errorf("calls after definitions are removed = %v, want %v", got, unresolved)
	}
	if got := edgesFrom(t, db, "Run", RelationMayCall); len(got) != 0 {
		t.Errorf("may_call after definitions are removed = %v, want none", got)
	}
}

func TestDeleteChunksForFile_RemovesGraph(t *testing.T) {
	db := openSymbolsDB(t)

	fileID, chunkID := addFile(t, db, "/proj/a.py")
	defs := []SymbolDef{
		{Name: "a", EntityType: "PACKAGE", ChunkID: chunkID, Parent: -1},
		{Name: "f", EntityType: "FUNCTION", ChunkID: chunkID, Parent: 0, StartLine: 1, EndLine: 5},
		{Name: "g", EntityType: "FUNCTION", ChunkID: chunkID, Parent: 0, StartLine: 7, EndLine: 9},
	}
	refs := []SymbolRef{{Source: 1, RelationType: "calls", Name: "g", ExternalName: "g"}}
	if err := db.StoreFileSymbols(fileID, "/proj", defs, refs); err != nil {
		t.Fatalf("StoreFileSymbols failed: %v", err)
	}
	if got, want := edgesFrom(t, db, "f", "calls"), []edge{{"g", "FUNCTION", weightExact}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}

	// What f calls can be looked up by where f is
	callees, err := db.Callees("/proj/a.py", 1, 5)
	if err != nil {
		t.Fatalf("Callees failed: %v", err)
	}
	if want := []Location{{Name: "g", Type: "FUNCTION", Path: "/proj/a.py", StartLine: 7, EndLine: 9}}; !reflect.DeepEqual(callees, want) {
		t.Errorf("callees = %+v, want %+v", callees, want)
	}

	if err := db.DeleteChunksForFile(fileID); err != nil {
		t.Fatalf("DeleteChunksForFile failed: %v", err)
	}

	for _, table := range []string{"chunks", "entities", "graph_edges", "symbol_defs", "symbol_refs"} {
		var count int
		if err := db.conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("Failed to count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s has %d rows after deleting the file's chunks, want 0", table, count)
		}
	}
}

// edgeRows returns the row of each may_call edge leaving the named entity, by
// the container of its target
func edgeRows(t *testing.T, db *DB, source string) map[string]int64 {
	t.Helper()
	rows, err := db.conn.Query(`
		SELECT d.container, g.rowid
		FROM graph_edges g
		JOIN entities e ON e.id = g.source_entity_id
		JOIN symbol_defs d ON d.entity_id = g.target_entity_id
		WHERE e.name = ? AND g.relation_type = ?
	`, source, RelationMayCall)
	if err != nil {
		t.Fatalf("Failed to query edges: %v", err)
	}
	defer rows.Close()

	found := make(map[string]int64)
	for rows.Next() {
		var container string
		var rowid int64
		if err := rows.Scan(&container, &rowid); err != nil {
			t.Fatalf("Failed to scan edge: %v", err)
		}
		found[container] = rowid
	}
	return found
}

func TestStoreFileSymbols_KeepsEdgesThatDoNotChange(t *testing.T) {
	db := openSymbolsDB(t)

	// Two types with a method of the same name, and a call that can be to either
	methodIn := func(path, container string) (int64, func()) {
		fileID, _ := addFile(t, db, path)
		return fileID, func() {
			chunkID, err := db.InsertChunk(fileID, 1, 10)
			if err != nil {
				t.Fatalf("Failed to insert chunk: %v", err)
			}
			defs := []SymbolDef{{Name: "Get", Container: container, EntityType: "METHOD", ChunkID: chunkID, Parent: -1}}
			if err := db.StoreFileSymbols(fileID, "/proj", defs, nil); err != nil {
				t.Fatalf("StoreFileSymbols failed: %v", err)
			}
		}
	}
	_, storeCache := methodIn("/proj/cache.go", "Cache")
	storeCache()
	queueFile, storeQueue := methodIn("/proj/queue.go", "Queue")
	storeQueue()

	callerFile, callerChunk := addFile(t, db, "/proj/main.go")
	callerDefs := []SymbolDef{{Name: "Run", EntityType: "FUNCTION", ChunkID: callerChunk, Parent: -1}}
	callerRefs := []SymbolRef{{Source: 0, RelationType: "calls", Name: "Get", ExternalName: "Get", Member: true}}
	if err := db.StoreFileSymbols(callerFile, "/proj", callerDefs, callerRefs); err != nil {
		t.Fatalf("StoreFileSymbols failed: %v", err)
	}
	// A later edge, so that a row that is written again cannot get the
	// number it had
	otherFile, otherChunk := addFile(t, db, "/proj/other.go")
	otherDefs := []SymbolDef{
		{Name: "other", EntityType: "PACKAGE", ChunkID: otherChunk, Parent: -1},
		{Name: "Helper", EntityType: "FUNCTION", ChunkID: otherChunk, Parent: 0},
	}
	if err := db.StoreFileSymbols(otherFile, "/proj", otherDefs, nil); err != nil {
		t.Fatalf("StoreFileSymbols failed: %v", err)
	}

	before := edgeRows(t, db, "Run")
	if len(before) != 2 {
		t.Fatalf("may_call edges = %v, want one to Cache and one to Queue", before)
	}

	// Indexing one of the two again resolves the call again. Its edge to the
	// other is as it was and is not written a second time.
	if err := db.DeleteChunksForFile(queueFile); err != nil {
		t.Fatalf("DeleteChunksForFile failed: %v", err)
	}
	storeQueue()

	after := edgeRows(t, db, "Run")
	if len(after) != 2 {
		t.Fatalf("may_call edges after indexing again = %v, want one to Cache and one to Queue", after)
	}
	if after["Cache"] != before["Cache"] {
		t.Errorf("edge to Cache.Get was written again: row %d became %d", before["Cache"], after["Cache"])
	}
	want := []edge{{"Cache.Get", "METHOD", weightByName}, {"Queue.Get", "METHOD", weightByName}}
	if got := edgesFrom(t, db, "Run", RelationMayCall); !reflect.DeepEqual(got, want) {
		t.Errorf("may_call after indexing again = %v, want %v", got, want)
	}
}

func TestSelectTargets(t *testing.T) {
	cands := []candidate{
		{entityID: 1, entityType: "FUNCTION", path: "/proj/app/util.go", root: "/proj"},
		{entityID: 2, entityType: "FUNCTION", path: "/proj/lib/util.go", root: "/proj"},
		{entityID: 3, entityType: "METHOD", container: "Server", path: "/proj/app/server.go", root: "/proj"},
		{entityID: 4, entityType: "METHOD", container: "Client", path: "/proj/lib/client.go", root: "/proj"},
		{entityID: 5, entityType: "PACKAGE", path: "/proj/lib/util.go", root: "/proj"},
		{entityID: 6, entityType: "FUNCTION", path: "/other/lib/util.go", root: "/other"},
	}

	tests := []struct {
		name       string
		ref        pendingRef
		follows    []string // Directories the project of the reference follows
		wantIDs    []int64
		wantWeight float64
	}{
		{
			name:       "plain call prefers the same directory",
			ref:        pendingRef{relationType: "calls", path: "/proj/app/main.go", root: "/proj"},
			wantIDs:    []int64{1},
			wantWeight: weightExact,
		},
		{
			name:       "plain call elsewhere is a match by name",
			ref:        pendingRef{relationType: "calls", path: "/proj/cmd/main.go", root: "/proj"},
			wantIDs:    []int64{1, 2},
			wantWeight: weightByName,
		},
		{
			name:       "import selects the directory",
			ref:        pendingRef{relationType: "calls", importBase: "lib", member: true, path: "/proj/app/main.go", root: "/proj"},
			wantIDs:    []int64{2, 4},
			wantWeight: weightExact,
		},
		{
			name:       "import within another project",
			ref:        pendingRef{relationType: "calls", importBase: "lib", path: "/other/main.go", root: "/other"},
			wantIDs:    []int64{6},
			wantWeight: weightExact,
		},
		{
			name:       "import does not lead out of the project",
			ref:        pendingRef{relationType: "calls", importBase: "lib", path: "/third/main.go", root: "/third"},
			wantIDs:    nil,
			wantWeight: weightExact,
		},
		{
			name:       "import leads into a directory the project follows",
			ref:        pendingRef{relationType: "calls", importBase: "lib", path: "/third/main.go", root: "/third"},
			follows:    []string{"/other"},
			wantIDs:    []int64{6},
			wantWeight: weightExact,
		},
		{
			name:       "type checker leads into a directory the project follows",
			ref:        pendingRef{relationType: "calls", targetPath: "/other/lib/util.go", path: "/third/main.go", root: "/third"},
			follows:    []string{"/other"},
			wantIDs:    []int64{6},
			wantWeight: weightExact,
		},
		{
			name:       "type checker does not lead out of the project",
			ref:        pendingRef{relationType: "calls", targetPath: "/other/lib/util.go", path: "/third/main.go", root: "/third"},
			wantIDs:    nil,
			wantWeight: weightExact,
		},
		{
			name:       "a name alone never leaves the project",
			ref:        pendingRef{relationType: "calls", member: true, path: "/third/main.go", root: "/third"},
			follows:    []string{"/other", "/proj"},
			wantIDs:    nil,
			wantWeight: weightByName,
		},
		{
			name:       "plain call stays within the project",
			ref:        pendingRef{relationType: "calls", path: "/other/main.go", root: "/other"},
			wantIDs:    []int64{6},
			wantWeight: weightByName,
		},
		{
			name:       "import of something that is not indexed",
			ref:        pendingRef{relationType: "calls", importBase: "fmt", member: true, path: "/proj/app/main.go", root: "/proj"},
			wantIDs:    nil,
			wantWeight: weightExact,
		},
		{
			name:       "known container",
			ref:        pendingRef{relationType: "calls", container: "Client", member: true, path: "/proj/app/main.go", root: "/proj"},
			wantIDs:    []int64{4},
			wantWeight: weightExact,
		},
		{
			name:       "unknown receiver matches everything by that name",
			ref:        pendingRef{relationType: "calls", container: "c", member: true, path: "/proj/lib/x.go", root: "/proj"},
			wantIDs:    []int64{1, 2, 3, 4},
			wantWeight: weightByName,
		},
		{
			name:       "type-checked target is that definition only",
			ref:        pendingRef{relationType: "calls", targetPath: "/proj/lib/util.go", path: "/proj/app/main.go", root: "/proj"},
			wantIDs:    []int64{2},
			wantWeight: weightExact,
		},
		{
			name:       "type-checked target outside the index",
			ref:        pendingRef{relationType: "calls", targetPath: "/usr/lib/go/src/fmt/print.go", path: "/proj/app/main.go", root: "/proj"},
			wantIDs:    nil,
			wantWeight: weightExact,
		},
		{
			name:       "imports only match packages",
			ref:        pendingRef{relationType: "imports", importBase: "lib", path: "/proj/app/main.go", root: "/proj"},
			wantIDs:    []int64{5},
			wantWeight: weightExact,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, weight := selectTargets(tt.ref, cands, tt.follows)
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("targets = %v, want %v", ids, tt.wantIDs)
			}
			if weight != tt.wantWeight {
				t.Errorf("weight = %v, want %v", weight, tt.wantWeight)
			}
		})
	}
}

func TestSelectTargets_CodeBeforeData(t *testing.T) {
	// BCPL: a routine is declared as a global in a header and defined in a source
	cands := []candidate{
		{entityID: 1, entityType: "VARIABLE", path: "/proj/com/cghdr", root: "/proj"},
		{entityID: 2, entityType: "FUNCTION", path: "/proj/com/mcg5", root: "/proj"},
	}
	ref := pendingRef{relationType: "calls", path: "/proj/com/mcg2", root: "/proj"}
	if ids, weight := selectTargets(ref, cands, nil); !reflect.DeepEqual(ids, []int64{2}) || weight != weightExact {
		t.Errorf("targets = %v (weight %v), want the routine and not its declaration", ids, weight)
	}
	// A routine that is only declared, as those of a library are
	if ids, _ := selectTargets(ref, cands[:1], nil); !reflect.DeepEqual(ids, []int64{1}) {
		t.Errorf("targets = %v, want the declaration when there is nothing else", ids)
	}
}

func TestMatchesImport_AnyCase(t *testing.T) {
	if !matchesImport("/proj/g/libhdr.h", "LIBHDR") || !matchesImport("/proj/LCGHDR", "lcghdr") || matchesImport("/proj/g/libhdr.h", "lib") {
		t.Error("an import should name its file whatever the case, and no other")
	}
}
