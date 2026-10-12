package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-sqlite3"
)

// driverName is the SQLite driver with the functions queries rely on
const driverName = "sqlite3_chainsaw"

// busyTimeout is how long SQLite lets a writer wait for the writer of
// another process before it fails it. Who can wait longer asks again; see
// IsBusy.
const busyTimeout = 10 * time.Second

var registerDriver sync.Once

// DB wraps the SQLite database connection with our schema
type DB struct {
	conn *pool
	path string

	// Resolving references in batches; see DeferResolving. Changed only by
	// who is writing.
	deferring int // Callers that put resolving off
	every     int // Files to store before resolving anyway
	stored    int // Files stored since references were last resolved
}

// pool is the connections to the database, with a queue for writing. SQLite
// lets one connection write at a time and fails the others with "database is
// locked" when they have waited for the busy timeout. Storing the symbols of
// one file can take longer than that, so the writers of one process wait for
// each other here, for as long as it takes. Reading does not wait.
//
// Writers in other processes are up to SQLite, which makes one wait for the
// other for the busy timeout, but only when it knows that a transaction is
// going to write. One that starts by reading and writes later is failed at
// once, with "database is locked", when another process wrote in between:
// what it read is no longer what is there. So transactions are begun on a
// connection of their own that takes the write lock at the start.
type pool struct {
	*sql.DB
	writer  *sql.DB // Begins its transactions with BEGIN IMMEDIATE
	writing sync.Mutex
}

// Begin starts a transaction that writes. It waits for a writer in another
// process, for the busy timeout. The caller holds writing.
func (p *pool) Begin() (*sql.Tx, error) {
	return p.writer.Begin()
}

// Close closes the connections
func (p *pool) Close() error {
	err := p.writer.Close()
	if closeErr := p.DB.Close(); err == nil {
		err = closeErr
	}
	return err
}

// IsBusy reports whether an error is SQLite giving up on a database that
// another connection, as a rule another process, is writing to. What was
// asked can be asked again.
func IsBusy(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked
	}
	// Wrapped with %v somewhere on the way up
	return err != nil && strings.Contains(err.Error(), "database is locked")
}

// Exec runs a statement that writes, when no other write is going on
func (p *pool) Exec(query string, args ...any) (sql.Result, error) {
	p.writing.Lock()
	defer p.writing.Unlock()
	return p.DB.Exec(query, args...)
}

// Config holds database configuration
type Config struct {
	Path string // Database file path
}

// DefaultPath returns where the index lives for the current user
func DefaultPath() string {
	return filepath.Join(os.Getenv("HOME"), ".chainsaw", "graph.db")
}

// Open opens or creates a database with the given configuration
func Open(cfg Config) (*DB, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("database path cannot be empty")
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0700); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	registerDriver.Do(func() {
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				// REGEXP is an operator SQLite leaves to the application
				err := conn.RegisterFunc("regexp", func(pattern, s string) (bool, error) {
					return regexp.MatchString(pattern, s)
				}, true)
				if err != nil {
					return err
				}
				// The index holds no source text; code is read from the
				// files when a query asks for it
				return conn.RegisterFunc("file_lines", fileLines, false)
			},
		})
	})

	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_busy_timeout=%d", cfg.Path, busyTimeout.Milliseconds())
	conn, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	writer, err := sql.Open(driverName, dsn+"&_txlock=immediate")
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	// One transaction writes at a time
	writer.SetMaxOpenConns(1)
	writer.SetConnMaxLifetime(time.Hour)

	// Configure connection pool (single writer, multiple readers)
	conn.SetMaxOpenConns(5)
	conn.SetMaxIdleConns(2)
	conn.SetConnMaxLifetime(time.Hour)

	db := &DB{conn: &pool{DB: conn, writer: writer}, path: cfg.Path}

	if err := db.initSchema(); err != nil {
		db.conn.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Set file permissions to 0600 (user read/write only)
	if err := os.Chmod(cfg.Path, 0600); err != nil {
		db.conn.Close()
		return nil, fmt.Errorf("failed to set database permissions: %w", err)
	}

	return db, nil
}

// initSchema creates tables and indexes if they don't exist
func (db *DB) initSchema() error {
	for _, pragma := range []string{"PRAGMA journal_mode=WAL;", "PRAGMA wal_autocheckpoint=1000;", "PRAGMA foreign_keys=ON;"} {
		if _, err := db.conn.Exec(pragma); err != nil {
			return fmt.Errorf("failed to configure database: %w", err)
		}
	}

	// Not a transaction that takes the write lock: nearly always the schema
	// is there, and a command that only reads must not wait for a writer
	tx, err := db.conn.DB.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	for _, stmt := range schema {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("failed to execute schema: %w", err)
		}
	}

	var version string
	err = tx.QueryRow("SELECT value FROM meta WHERE key = ?", MetaKeySchemaVersion).Scan(&version)
	switch {
	case err == sql.ErrNoRows:
		now := time.Now().UTC().Format(time.RFC3339)
		for key, value := range map[string]string{MetaKeySchemaVersion: SchemaVersion, MetaKeyCreatedAt: now} {
			if _, err := tx.Exec("INSERT INTO meta (key, value) VALUES (?, ?)", key, value); err != nil {
				return fmt.Errorf("failed to insert meta %s: %w", key, err)
			}
		}
	case err != nil:
		return fmt.Errorf("failed to read schema version: %w", err)
	case version == "3.0.0", version == "3.1.0":
		// 3.1.0 added what is kept per kind of index; what 3.0.0 indexed was
		// the graph, which 3.2.0 builds anew
		if err := upgradeProjects(tx); err != nil {
			return err
		}
	case version != SchemaVersion:
		return fmt.Errorf("database %s has schema %s, this version needs %s; remove it and index again", db.path, version, SchemaVersion)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit schema transaction: %w", err)
	}
	return nil
}

// upgradeProjects brings a database to 3.2.0, in which a reference stays
// within its project. Before, the files that are not in a repository were
// one project together, and an import could lead into any project, so the
// graph holds edges that are no longer made, most of them from that one
// project. Taking those out one file at a time costs far more than building
// the graph anew, so it is emptied and every file is indexed again. What the
// other kinds of index hold stays.
func upgradeProjects(tx *sql.Tx) error {
	for _, table := range []string{"graph_edges", "symbol_refs", "symbol_defs", "symbol_dirty", "entities", "chunks"} {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			return fmt.Errorf("failed to upgrade schema: %w", err)
		}
	}
	for _, stmt := range schema {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("failed to upgrade schema: %w", err)
		}
	}
	if _, err := tx.Exec("DELETE FROM file_modules WHERE module = 'graph'"); err != nil {
		return fmt.Errorf("failed to upgrade schema: %w", err)
	}
	if _, err := tx.Exec("UPDATE meta SET value = ? WHERE key = ?", SchemaVersion, MetaKeySchemaVersion); err != nil {
		return fmt.Errorf("failed to upgrade schema: %w", err)
	}
	return nil
}

// Close closes the database connection and flushes WAL
func (db *DB) Close() error {
	if db.conn == nil {
		return nil
	}

	// Checkpoint WAL before closing
	_, err := db.conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	closeErr := db.conn.Close()

	if err != nil {
		// Log but don't fail on checkpoint error
		slog.Warn("Failed to checkpoint WAL", "error", err)
	}

	// Mark conn as nil to prevent double-close
	db.conn = nil

	return closeErr
}

// Path returns the database file path
func (db *DB) Path() string {
	return db.path
}

// GetMeta retrieves a metadata value by key
func (db *DB) GetMeta(key string) (string, error) {
	var value string
	err := db.conn.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get meta: %w", err)
	}
	return value, nil
}

// SetMeta stores a metadata value
func (db *DB) SetMeta(key, value string) error {
	_, err := db.conn.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)", key, value)
	if err != nil {
		return fmt.Errorf("failed to set meta: %w", err)
	}
	return nil
}

// RawQuery executes a raw SQL query with args and returns the result rows
func (db *DB) RawQuery(query string, args ...interface{}) (*sql.Rows, error) {
	return db.conn.Query(query, args...)
}

// lineCache holds the lines of the files queries have read code from
var lineCache = struct {
	sync.Mutex
	files map[string][]string
}{files: make(map[string][]string)}

// fileLines returns lines start to end (1-indexed, inclusive) of a file as it
// is on disk now, or an empty string when it cannot be read
func fileLines(path string, start, end int64) string {
	lineCache.Lock()
	defer lineCache.Unlock()

	lines, ok := lineCache.files[path]
	if !ok {
		if content, err := os.ReadFile(path); err == nil {
			lines = strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
		}
		lineCache.files[path] = lines
	}
	if start < 1 || start > int64(len(lines)) {
		return ""
	}
	end = min(end, int64(len(lines)))
	return strings.Join(lines[start-1:end], "\n")
}
