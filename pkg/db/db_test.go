package db

import (
	"errors"
	"fmt"
	"github.com/mattn/go-sqlite3"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpen_NewDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sub", "test.db")

	db, err := Open(Config{Path: dbPath})
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	if db.Path() != dbPath {
		t.Errorf("Path() = %s, want %s", db.Path(), dbPath)
	}
	if version, _ := db.GetMeta(MetaKeySchemaVersion); version != SchemaVersion {
		t.Errorf("schema version = %q, want %q", version, SchemaVersion)
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("Database file not created: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("Database permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestOpen_InvalidConfig(t *testing.T) {
	if _, err := Open(Config{}); err == nil {
		t.Error("Open accepted an empty path")
	}
}

func TestOpen_ExistingDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(Config{Path: dbPath})
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	if err := db.SetMeta("test_key", "test_value"); err != nil {
		t.Fatalf("Failed to set meta: %v", err)
	}
	db.Close()

	db, err = Open(Config{Path: dbPath})
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer db.Close()

	if value, _ := db.GetMeta("test_key"); value != "test_value" {
		t.Errorf("meta after reopen = %q, want test_value", value)
	}
}

func TestOpen_OtherSchemaVersion(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(Config{Path: dbPath})
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	if err := db.SetMeta(MetaKeySchemaVersion, "2.7.0"); err != nil {
		t.Fatalf("Failed to set meta: %v", err)
	}
	db.Close()

	if _, err := Open(Config{Path: dbPath}); err == nil {
		t.Error("Open accepted a database with another schema version")
	}
}

func TestOpen_UpgradesSchema(t *testing.T) {
	for _, from := range []string{"3.0.0", "3.1.0"} {
		t.Run(from, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "test.db")

			db, err := Open(Config{Path: dbPath})
			if err != nil {
				t.Fatalf("Failed to open database: %v", err)
			}
			fileID, chunkID := addFile(t, db, "/src/a.go")
			defs := []SymbolDef{{Name: "f", EntityType: "FUNCTION", ChunkID: chunkID, Parent: -1}}
			refs := []SymbolRef{{Source: 0, RelationType: "calls", Name: "g", ExternalName: "g"}}
			if err := db.StoreFileSymbols(fileID, "", defs, refs); err != nil {
				t.Fatalf("StoreFileSymbols failed: %v", err)
			}
			for _, module := range []string{"graph", "embeddings"} {
				if err := db.SetFileModule(fileID, module, 42); err != nil {
					t.Fatalf("SetFileModule failed: %v", err)
				}
			}
			if err := db.SetMeta(MetaKeySchemaVersion, from); err != nil {
				t.Fatalf("Failed to set meta: %v", err)
			}
			db.Close()

			// The graph of before is built anew: its files are still known,
			// and are indexed again. What other kinds of index hold stays.
			db, err = Open(Config{Path: dbPath})
			if err != nil {
				t.Fatalf("Open refused a %s database: %v", from, err)
			}
			defer db.Close()
			if version, _ := db.GetMeta(MetaKeySchemaVersion); version != SchemaVersion {
				t.Errorf("schema version = %s, want %s", version, SchemaVersion)
			}
			if modules, _ := db.FileModules(fileID); len(modules) != 1 || modules["embeddings"] != 42 {
				t.Errorf("modules after the upgrade = %v, want embeddings at 42 only", modules)
			}
			for _, table := range []string{"chunks", "entities", "graph_edges", "symbol_defs", "symbol_refs"} {
				var count int
				if err := db.conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
					t.Fatalf("Failed to count %s: %v", table, err)
				}
				if count != 0 {
					t.Errorf("%s has %d rows after the upgrade, want 0", table, count)
				}
			}
			if n, _ := db.CountFiles(); n != 1 {
				t.Errorf("files after the upgrade = %d, want 1", n)
			}
		})
	}
}

func TestRawQuery_Regexp(t *testing.T) {
	db, err := Open(Config{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	var matched bool
	rows, err := db.RawQuery("SELECT 'IndexFile' REGEXP '^Index[A-Z]'")
	if err != nil {
		t.Fatalf("REGEXP failed: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		rows.Scan(&matched)
	}
	if !matched {
		t.Error("REGEXP did not match")
	}
}

func TestWritesWaitForEachOther(t *testing.T) {
	db := openSymbolsDB(t)

	// A write that is going on, such as the symbols of a file being stored
	db.conn.writing.Lock()
	done := make(chan error, 1)
	go func() { done <- db.MarkFilePending("/proj/a.go", 1, "hash") }()

	select {
	case err := <-done:
		t.Fatalf("a second write did not wait for the first: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	// Reading goes on meanwhile
	if _, err := db.CountFiles(); err != nil {
		t.Errorf("CountFiles during a write failed: %v", err)
	}

	db.conn.writing.Unlock()
	if err := <-done; err != nil {
		t.Errorf("MarkFilePending after the first write failed: %v", err)
	}
}

func TestWriters_OfTwoProcesses(t *testing.T) {
	// Two handles on one file stand for two processes: the daemon and a query
	path := filepath.Join(t.TempDir(), "graph.db")
	daemon, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	query, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer query.Close()

	// A transaction that reads first and writes later, as resolving
	// references does. The other process writes meanwhile, and has to wait:
	// were it let through, what the transaction read would be stale and its
	// own write would fail with "database is locked", at once and for good.
	wrote := make(chan error, 1)
	err = func() error {
		query.conn.writing.Lock()
		defer query.conn.writing.Unlock()
		tx, err := query.conn.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM entities").Scan(&n); err != nil {
			return err
		}

		go func() { wrote <- daemon.SetMeta("who", "daemon") }()
		select {
		case err := <-wrote:
			wrote <- err
			return fmt.Errorf("the other process wrote while a transaction that writes was open (%v)", err)
		case <-time.After(200 * time.Millisecond):
		}

		if _, err := tx.Exec("INSERT INTO entities (name, entity_type, chunk_id) VALUES ('x', 'EXTERNAL', 0)"); err != nil {
			return fmt.Errorf("the transaction could not write after reading: %w", err)
		}
		return tx.Commit()
	}()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-wrote; err != nil {
		t.Errorf("the other process could not write once the transaction was done: %v", err)
	}
}

func TestIsBusy(t *testing.T) {
	if !IsBusy(sqlite3.Error{Code: sqlite3.ErrBusy}) || !IsBusy(fmt.Errorf("failed to insert external entity: %w", sqlite3.Error{Code: sqlite3.ErrBusy})) {
		t.Error("a busy database is not told")
	}
	if !IsBusy(errors.New("graph index: failed to insert entity: database is locked")) {
		t.Error("a busy database is not told from its message")
	}
	if IsBusy(nil) || IsBusy(errors.New("no such table")) || IsBusy(sqlite3.Error{Code: sqlite3.ErrConstraint}) {
		t.Error("another error is taken for a busy database")
	}
}

func TestOpen_UpgradesFrom320(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(Config{Path: dbPath})
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	fileID, _ := addFile(t, db, "/src/a.go")
	if err := db.SetFileModule(fileID, "graph", 42); err != nil {
		t.Fatalf("SetFileModule failed: %v", err)
	}
	if _, err := db.conn.Exec("DROP TABLE embedding_keys"); err != nil {
		t.Fatalf("Failed to drop embedding_keys: %v", err)
	}
	if err := db.SetMeta(MetaKeySchemaVersion, "3.2.0"); err != nil {
		t.Fatalf("Failed to set meta: %v", err)
	}
	db.Close()

	// Only a table is added: what every kind of index holds stays
	db, err = Open(Config{Path: dbPath})
	if err != nil {
		t.Fatalf("Open refused a 3.2.0 database: %v", err)
	}
	defer db.Close()
	if version, _ := db.GetMeta(MetaKeySchemaVersion); version != SchemaVersion {
		t.Errorf("schema version = %s, want %s", version, SchemaVersion)
	}
	if modules, _ := db.FileModules(fileID); modules["graph"] != 42 {
		t.Errorf("modules after the upgrade = %v, want graph at 42", modules)
	}
	if _, err := db.conn.Exec("SELECT COUNT(*) FROM embedding_keys"); err != nil {
		t.Errorf("no embedding_keys after the upgrade: %v", err)
	}
}
