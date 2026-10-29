package db

// SchemaVersion identifies the layout of the database. A database written by
// another version is not migrated, unless initSchema says how: the index is
// cheap to rebuild.
const SchemaVersion = "3.3.0"

// schema holds the DDL statements, in dependency order
var schema = []string{
	// Configuration and version info
	`CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`,

	// Indexed files, and the queue of files waiting to be indexed
	`CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT UNIQUE NOT NULL,
    last_mod_time INTEGER NOT NULL,
    content_hash TEXT NOT NULL,
    indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    status TEXT DEFAULT 'indexed',
    error_message TEXT,
    retry_count INTEGER DEFAULT 0,
    queued_at DATETIME
);`,
	`CREATE INDEX IF NOT EXISTS idx_files_path ON files(path);`,
	`CREATE INDEX IF NOT EXISTS idx_files_hash ON files(content_hash);`,
	`CREATE INDEX IF NOT EXISTS idx_files_status ON files(status, queued_at);`,

	// Which kinds of index hold a file, and the modification time the file
	// had when each was built. The kinds are built independently: one can be
	// switched on later, or fail, without the others being redone.
	`CREATE TABLE IF NOT EXISTS file_modules (
    file_id INTEGER NOT NULL,
    module TEXT NOT NULL,
    mod_time INTEGER NOT NULL,
    PRIMARY KEY (file_id, module)
);`,
	`CREATE INDEX IF NOT EXISTS idx_file_modules_module ON file_modules(module);`,

	// Chunks are the pieces a parsed file is folded into: the file skeleton
	// and one per definition large enough to fold. Only where they are is
	// kept; the text stays in the file.
	`CREATE TABLE IF NOT EXISTS chunks (
    chunk_id INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL,
    start_line INTEGER NOT NULL,
    end_line INTEGER NOT NULL
);`,
	`CREATE INDEX IF NOT EXISTS idx_chunks_file ON chunks(file_id);`,

	// Entities are the nodes of the graph: definitions found by parsing, and
	// placeholders (chunk_id 0) for what is referenced but not in the index
	`CREATE TABLE IF NOT EXISTS entities (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    chunk_id INTEGER NOT NULL,
    UNIQUE(name, entity_type, chunk_id)
);`,
	`CREATE INDEX IF NOT EXISTS idx_entities_name ON entities(name);`,
	`CREATE INDEX IF NOT EXISTS idx_entities_type ON entities(entity_type);`,
	`CREATE INDEX IF NOT EXISTS idx_entities_chunk ON entities(chunk_id);`,

	// Edges of the graph
	`CREATE TABLE IF NOT EXISTS graph_edges (
    source_entity_id INTEGER NOT NULL,
    target_entity_id INTEGER NOT NULL,
    relation_type TEXT NOT NULL,
    chunk_id INTEGER NOT NULL,
    weight REAL DEFAULT 1.0,
    PRIMARY KEY (source_entity_id, target_entity_id, relation_type, chunk_id),
    FOREIGN KEY(source_entity_id) REFERENCES entities(id) ON DELETE CASCADE,
    FOREIGN KEY(target_entity_id) REFERENCES entities(id) ON DELETE CASCADE
);`,
	`CREATE INDEX IF NOT EXISTS idx_graph_source ON graph_edges(source_entity_id);`,
	`CREATE INDEX IF NOT EXISTS idx_graph_target ON graph_edges(target_entity_id);`,
	`CREATE INDEX IF NOT EXISTS idx_graph_relation ON graph_edges(relation_type);`,

	// What is known about an entity that is a definition: where it is and
	// what it looks like
	`CREATE TABLE IF NOT EXISTS symbol_defs (
    entity_id INTEGER PRIMARY KEY,
    file_id INTEGER NOT NULL,
    root TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    container TEXT NOT NULL DEFAULT '',
    signature TEXT NOT NULL DEFAULT '',
    start_line INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0
);`,
	`CREATE INDEX IF NOT EXISTS idx_symbol_defs_name ON symbol_defs(name);`,
	`CREATE INDEX IF NOT EXISTS idx_symbol_defs_file ON symbol_defs(file_id);`,

	// References (calls, imports) found by parsing. Edges are derived from
	// these by matching names to definitions, which is redone whenever
	// definitions appear or disappear.
	`CREATE TABLE IF NOT EXISTS symbol_refs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL,
    root TEXT NOT NULL DEFAULT '',
    source_entity_id INTEGER NOT NULL,
    chunk_id INTEGER NOT NULL,
    relation_type TEXT NOT NULL,
    name TEXT NOT NULL,
    container TEXT NOT NULL DEFAULT '',
    import_base TEXT NOT NULL DEFAULT '',
    target_path TEXT NOT NULL DEFAULT '',
    external_name TEXT NOT NULL,
    member BOOLEAN NOT NULL DEFAULT 0,
    resolved BOOLEAN NOT NULL DEFAULT 0
);`,
	`CREATE INDEX IF NOT EXISTS idx_symbol_refs_name ON symbol_refs(name);`,
	`CREATE INDEX IF NOT EXISTS idx_symbol_refs_file ON symbol_refs(file_id);`,
	`CREATE INDEX IF NOT EXISTS idx_symbol_refs_resolved ON symbol_refs(resolved);`,

	// Names whose definitions changed; references to them need resolving again
	`CREATE TABLE IF NOT EXISTS symbol_dirty (
    name TEXT PRIMARY KEY
);`,

	// Where the references of a project may lead outside it: the directories
	// it follows its imports into
	`CREATE TABLE IF NOT EXISTS project_follows (
    root TEXT NOT NULL,
    dir TEXT NOT NULL,
    PRIMARY KEY (root, dir)
);`,

	// What text each embedded chunk was made from, as a hash. A chunk with
	// the same text, in another copy of a file or after an edit elsewhere in
	// it, takes the vector that is there instead of asking the model again.
	// The vectors are in embedding_chunks, which is made when the first are
	// stored.
	`CREATE TABLE IF NOT EXISTS embedding_keys (
    chunk_id INTEGER PRIMARY KEY,
    text_key TEXT NOT NULL
);`,
	`CREATE INDEX IF NOT EXISTS idx_embedding_keys_key ON embedding_keys(text_key);`,
}

// MetaKeys are standard keys stored in the meta table
const (
	MetaKeySchemaVersion = "schema_version"
	MetaKeyCreatedAt     = "created_at"

	// The model the embeddings were made with, and the length of its vectors
	MetaKeyEmbeddingModel = "embedding_model"
	MetaKeyEmbeddingDim   = "embedding_dimension"
)
