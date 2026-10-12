# Chainsaw Developer Documentation

**Version:** 2.1  
**Last Updated:** 2024

---

## Table of Contents

1. [Quick Start](#quick-start)
2. [Architecture](#architecture)
3. [Design](#design)
4. [Development Workflow](#development-workflow)
5. [Testing](#testing)
6. [Contributing](#contributing)

---

## Quick Start

### Prerequisites

- Go 1.24.7 or later
- [Ollama](https://ollama.ai/) running locally
- SQLite with vec extension support (bundled)
- Make (optional but recommended)

### One-Time Setup

```bash
# Clone and initialize
git clone https://github.com/wouteroostervld/chainsaw.git
cd chainsaw
go mod download

# Build and install
make install

# Or install daemon directly
make daemon-install
```

### Daily Development Loop

```bash
# Make your code changes...

# Quick rebuild + restart daemon
make dev

# Or manual steps
make install
systemctl --user restart chainsawd

# Run tests
make test

# View daemon logs
journalctl --user -u chainsawd -f
```

### Development Commands

| Command | Description |
|---------|-------------|
| `make install` | Build and install to `~/.local/bin` |
| `make update` | Alias for install (update existing) |
| `make build` | Build locally (creates `./chainsaw`) |
| `make daemon-install` | Install daemon systemd service |
| `make daemon-restart` | Restart daemon with new binary |
| `make dev` | Build + restart daemon (quick dev loop) |
| `make test` | Run all tests |
| `make clean` | Remove build artifacts |
| `make version` | Show current version |

### Manual Build

If you prefer not to use Make:

```bash
# Build
go build -o chainsaw ./cmd/chainsaw

# Install
cp chainsaw ~/.local/bin/

# Restart daemon
systemctl --user restart chainsawd
```

---

## Architecture

### System Overview

Chainsaw is a **GraphRAG** (Graph-Retrieval Augmented Generation) system that combines:
- **Vector search** for semantic code discovery
- **Knowledge graph** for relational code understanding
- **Background indexing** via filesystem watcher
- **Cypher queries** for graph traversal

```
┌─────────────────────────────────────────────────────────┐
│                    Chainsaw Binary                      │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │  CLI Mode    │  │ Daemon Mode  │  │  Init Mode   │  │
│  │  (Read-Only) │  │ (Indexer)    │  │  (Setup)     │  │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘  │
└─────────┼──────────────────┼──────────────────┼─────────┘
          │                  │                  │
          ▼                  ▼                  ▼
    ┌─────────────────────────────────────────────┐
    │         SQLite Database (WAL Mode)          │
    │  ┌──────────┐  ┌──────────┐  ┌──────────┐  │
    │  │  Files   │  │  Chunks  │  │  Graph   │  │
    │  │ Registry │  │ + Vectors│  │  Edges   │  │
    │  └──────────┘  └──────────┘  └──────────┘  │
    └──────────────────┬──────────────────────────┘
                       │
                       ▼
              ┌────────────────┐
              │ Ollama Backend │
              │  (Embeddings)  │
              └────────────────┘
```

### Binary Modes

**Single codebase** provides three modes:

1. **CLI Mode** (`chainsaw search`, `chainsaw graph query`, etc.)
   - Read-only database access
   - Query execution and result formatting
   - YAML output for LLM consumption
   - Context-aware (CWD-based filtering)

2. **Daemon Mode** (`chainsaw daemon start`)
   - Background filesystem watcher
   - Persistent work queue
   - Two-pass indexing (embeddings + graph)
   - Concurrent processing with configurable workers
   - At startup and on SIGHUP (`daemon.reload` in `cmd/chainsaw/daemon.go`,
     the warm restart): reads the configuration (SIGHUP only; at startup it
     has just been read), sets the watches, prunes the files the
     configuration ignores from the index, scans. What else is to happen on
     SIGHUP goes there.

3. **Init Mode** (`chainsaw init`)
   - Database schema creation
   - Initial configuration setup

### Two-Pass Indexing Pipeline

The daemon processes files in **two distinct passes**:

#### Pass 1: Embedding Generation
```
File Event → Debounce → Hash Check → Chunking → Ollama (Embedding) → vec_chunks
```

- **Debouncing**: 500ms sliding window groups rapid saves
- **Hash Check**: SHA256 comparison to skip unchanged files
- **Chunking**: Token-based splitting (configurable size + overlap)
- **Embedding**: Batch requests to Ollama (e.g., `nomic-embed-text`)
- **Storage**: Insert into `vec_chunks` virtual table (sqlite-vec)

#### Pass 2: Graph Extraction
```
Chunk → Ollama (Generation) → Parse Relations → Symbol Index → Cross-Chunk Edges → graph_edges
```

- **LLM Analysis**: Send chunk to generation model (e.g., `llama3`, `phi3`)
- **Parsing**: JSON or Regex strategy (configurable per model)
- **Symbol Indexing**: Build `symbol → [chunkID]` mapping
- **Edge Creation**: Link chunks that share symbols with relations
- **Model Tracking**: Store which model created each edge (for easy switching)

### Component Architecture

```
chainsaw/
├── cmd/
│   └── chainsaw/              # Main binary entry point
│       ├── main.go            # Command routing
│       ├── daemon.go          # Daemon mode implementation
│       ├── search.go          # Search command
│       └── graph.go           # Graph query command
│
├── pkg/
│   ├── config/                # Configuration system
│   │   ├── types.go           # Data structures
│   │   ├── loader.go          # Global + local config loading
│   │   ├── paths.go           # Path resolution & validation
│   │   ├── cache.go           # Thread-safe config caching
│   │   └── watcher.go         # fsnotify-based config watching
│   │
│   ├── db/                    # Database layer
│   │   ├── schema.go          # SQLite DDL with WAL mode
│   │   ├── db.go              # Connection pooling, health checks
│   │   ├── files.go           # File registry operations
│   │   ├── chunks.go          # Vector chunk operations
│   │   ├── graph.go           # Knowledge graph operations
│   │   └── interface.go       # Database interface for DI
│   │
│   ├── indexer/               # Indexing engine
│   │   ├── indexer.go         # Main indexing logic
│   │   ├── chunker.go         # Token-based text splitting
│   │   ├── extractor.go       # Graph relation extraction
│   │   └── queue.go           # Work queue management
│   │
│   ├── llm/                   # LLM integrations
│   │   ├── ollama/            # Ollama client
│   │   │   ├── client.go      # HTTP API wrapper
│   │   │   ├── embedding.go   # Embedding generation
│   │   │   └── generate.go    # Text generation
│   │   └── openai/            # OpenAI client (future)
│   │
│   ├── cypher/                # Cypher query transpiler
│   │   ├── parser.go          # Cypher AST parser
│   │   ├── transpiler.go      # Cypher → SQL conversion
│   │   └── executor.go        # Query execution
│   │
│   ├── filter/                # File filtering
│   │   └── filter.go          # Include/exclude/whitelist/blacklist
│   │
│   └── watcher/               # Filesystem watcher
│       ├── watcher.go         # fsnotify wrapper
│       └── debounce.go        # Event debouncing
│
├── examples/                  # Configuration examples
│   ├── global-config.yaml     # Annotated global config
│   ├── local-config.yaml      # Annotated local config
│   ├── SKILLS.md              # Coding guidelines template
│   └── chainsawd.service      # Systemd unit file
│
└── Makefile                   # Build automation
```

---

## Design

### Core Philosophy

1. **Unix-Compliant**: Single binary, composable commands, pipe-friendly YAML output
2. **Security-First**: Path validation, SQL injection prevention, permission checks
3. **Zero-Latency Search**: Local-first with WAL mode for concurrent reads
4. **Model-Agnostic**: Configurable LLM backends with pluggable parsers

### Design Principles

#### 1. Dependency Injection
```go
// No global state - inject dependencies explicitly
type Indexer struct {
    db     db.Database
    llm    llm.Client
    config *config.MergedConfig
}

func NewIndexer(db db.Database, llm llm.Client, cfg *config.MergedConfig) *Indexer {
    return &Indexer{db: db, llm: llm, config: cfg}
}
```

#### 2. Interfaces for Testability
```go
// All IO operations abstracted behind interfaces
type Database interface {
    UpsertFile(ctx context.Context, file *File) error
    SearchSimilar(ctx context.Context, query string, limit int) ([]*Chunk, error)
    // ... 20+ methods
}

type FileSystem interface {
    ReadFile(path string) ([]byte, error)
    Stat(path string) (os.FileInfo, error)
    // ... file operations
}
```

#### 3. Context Propagation
```go
// Always pass context for cancellation/timeouts
func (idx *Indexer) IndexFile(ctx context.Context, path string) error {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
    defer cancel()
    
    // All downstream calls receive context
    chunks, err := idx.chunkFile(ctx, path)
    // ...
}
```

#### 4. Error Wrapping
```go
import "fmt"

func (db *SQLiteDB) GetFile(path string) (*File, error) {
    // Wrap errors with context for debugging
    row := db.conn.QueryRow("SELECT * FROM files WHERE path = ?", path)
    if err := row.Scan(&file); err != nil {
        return nil, fmt.Errorf("failed to get file %s: %w", path, err)
    }
    return file, nil
}
```

### Database Schema

#### Core Tables

```sql
-- Configuration and metadata
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT
);

-- File registry (tracks indexing state)
CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY,
    path TEXT UNIQUE NOT NULL,
    last_mod_time INTEGER,
    content_hash TEXT,           -- SHA256 for change detection
    indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    file_size INTEGER,
    language TEXT
);
CREATE INDEX idx_files_path ON files(path);
CREATE INDEX idx_files_hash ON files(content_hash);

-- Code chunks with metadata
CREATE TABLE IF NOT EXISTS chunks (
    id INTEGER PRIMARY KEY,
    file_id INTEGER NOT NULL,
    content TEXT NOT NULL,
    start_line INTEGER,
    end_line INTEGER,
    chunk_index INTEGER,         -- Position in file
    FOREIGN KEY(file_id) REFERENCES files(id) ON DELETE CASCADE
);
CREATE INDEX idx_chunks_file ON chunks(file_id);

-- Vector embeddings (sqlite-vec virtual table)
CREATE VIRTUAL TABLE IF NOT EXISTS vec_chunks USING vec0(
    chunk_id INTEGER PRIMARY KEY,
    embedding FLOAT[384]         -- Dimension configurable per model
);

-- Extracted code entities
CREATE TABLE IF NOT EXISTS entities (
    id INTEGER PRIMARY KEY,
    chunk_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    entity_type TEXT NOT NULL,   -- FUNCTION, METHOD, TYPE, etc.
    FOREIGN KEY(chunk_id) REFERENCES chunks(id) ON DELETE CASCADE
);
CREATE INDEX idx_entities_chunk ON entities(chunk_id);
CREATE INDEX idx_entities_name ON entities(name);
CREATE INDEX idx_entities_type ON entities(entity_type);

-- Knowledge graph edges
CREATE TABLE IF NOT EXISTS graph_edges (
    source_chunk_id INTEGER,
    target_chunk_id INTEGER,
    weight REAL,                 -- Similarity score or confidence
    relation_type TEXT,          -- calls, imports, implements, etc.
    created_by_model TEXT,       -- Track which model extracted this
    PRIMARY KEY (source_chunk_id, target_chunk_id, relation_type),
    FOREIGN KEY(source_chunk_id) REFERENCES chunks(id) ON DELETE CASCADE,
    FOREIGN KEY(target_chunk_id) REFERENCES chunks(id) ON DELETE CASCADE
);
CREATE INDEX idx_graph_source ON graph_edges(source_chunk_id);
CREATE INDEX idx_graph_target ON graph_edges(target_chunk_id);
CREATE INDEX idx_graph_model ON graph_edges(created_by_model);

-- Persistent work queue
CREATE TABLE IF NOT EXISTS work_queue (
    id INTEGER PRIMARY KEY,
    path TEXT NOT NULL,
    priority INTEGER DEFAULT 0,
    queued_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    status TEXT DEFAULT 'pending' -- pending, processing, done, failed
);
CREATE INDEX idx_queue_status ON work_queue(status, priority DESC);
```

#### WAL Mode Configuration

```go
// Enable WAL mode for concurrent daemon writes + CLI reads
func Open(dbPath string) (*sql.DB, error) {
    db, err := sql.Open("sqlite3", dbPath)
    if err != nil {
        return nil, err
    }
    
    // Set WAL mode
    _, err = db.Exec(`
        PRAGMA journal_mode=WAL;
        PRAGMA busy_timeout=5000;
        PRAGMA synchronous=NORMAL;
    `)
    
    // Set file permissions (user R/W only)
    os.Chmod(dbPath, 0600)
    
    return db, nil
}
```

### LLM Integration Patterns

#### Strategy Pattern for Model Outputs

Different models output different formats. The system uses a **Strategy Pattern** to handle parsing:

```yaml
# config.yaml
profiles:
  high_accuracy:
    graph_driver:
      model: "llama3"
      output_format: "json"      # Unmarshal JSON response
      custom_system_prompt: |
        Return JSON array of edges: [{"source":"A","target":"B","relation":"calls","weight":0.8}]
  
  high_speed:
    graph_driver:
      model: "phi3:mini"
      output_format: "regex"     # Apply regex to each line
      parsing_regex: '(?P<source>.+?)\s->\s(?P<relation>.+?)\s->\s(?P<target>.+)'
```

#### Parsing Implementations

```go
type GraphParser interface {
    Parse(output string) ([]Edge, error)
}

// JSON parser
type JSONParser struct{}
func (p *JSONParser) Parse(output string) ([]Edge, error) {
    var edges []Edge
    if err := json.Unmarshal([]byte(output), &edges); err != nil {
        return nil, err
    }
    return edges, nil
}

// Regex parser
type RegexParser struct {
    pattern *regexp.Regexp
}
func (p *RegexParser) Parse(output string) ([]Edge, error) {
    lines := strings.Split(output, "\n")
    edges := make([]Edge, 0, len(lines))
    for _, line := range lines {
        match := p.pattern.FindStringSubmatch(line)
        if match != nil {
            edges = append(edges, Edge{
                Source:   match[1],
                Target:   match[3],
                Relation: match[2],
            })
        }
    }
    return edges, nil
}
```

### Graph Extraction Strategy

#### Two-Phase Edge Extraction

**Problem**: LLM extracts relations like `funcA calls funcB`, but funcA and funcB might be in different chunks.

**Solution**: Two-phase symbol indexing + cross-chunk linking

```go
// Phase 1: Symbol Indexing
func (idx *Indexer) buildSymbolIndex(fileID int64) (map[string][]int64, error) {
    symbolIndex := make(map[string][]int64)
    
    chunks, _ := idx.db.GetChunksForFile(fileID)
    for _, chunk := range chunks {
        // Extract entities from chunk
        entities, _ := idx.extractEntities(chunk)
        for _, entity := range entities {
            symbolIndex[entity.Name] = append(symbolIndex[entity.Name], chunk.ID)
        }
    }
    
    return symbolIndex, nil
}

// Phase 2: Cross-Chunk Edge Creation
func (idx *Indexer) extractGraphEdges(chunk *Chunk, symbolIndex map[string][]int64) error {
    // Send chunk to LLM
    rawEdges, _ := idx.llm.ExtractRelations(chunk.Content)
    
    // Parse according to strategy
    parser := idx.getParser()
    edges, _ := parser.Parse(rawEdges)
    
    // Create cross-chunk edges
    for _, edge := range edges {
        sourceChunks := symbolIndex[edge.Source]
        targetChunks := symbolIndex[edge.Target]
        
        for _, srcID := range sourceChunks {
            for _, tgtID := range targetChunks {
                if srcID != tgtID {  // Skip self-edges
                    idx.db.UpsertEdge(srcID, tgtID, edge.Weight, edge.Relation, idx.config.GraphModel)
                }
            }
        }
    }
    
    return nil
}
```

### Security Considerations

#### 1. Path Traversal Prevention

```go
func validatePath(path string, allowedRoots []string) error {
    // Resolve to absolute path
    absPath, err := filepath.Abs(path)
    if err != nil {
        return fmt.Errorf("invalid path: %w", err)
    }
    
    // Check if within allowed roots
    for _, root := range allowedRoots {
        absRoot, _ := filepath.Abs(root)
        if strings.HasPrefix(absPath, absRoot) {
            return nil
        }
    }
    
    return fmt.Errorf("path outside allowed roots: %s", absPath)
}
```

#### 2. SQL Injection Prevention

```go
// ALWAYS use placeholders - NEVER fmt.Sprintf
// ❌ BAD
query := fmt.Sprintf("SELECT * FROM files WHERE path = '%s'", userInput)

// ✅ GOOD
query := "SELECT * FROM files WHERE path = ?"
row := db.QueryRow(query, userInput)
```

#### 3. Local Configuration Security

Local `.config.yaml` files can customize behavior but with strict constraints:

- **Additive Only**: Can only make filtering MORE restrictive
- **Include Scope**: CLI can only add paths within global `include`
- **Whitelist Immutable**: Global whitelist cannot be overridden
- **Path Validation**: All paths validated against traversal attacks

```go
// Merge local config (additive only)
func mergeLocal(global, local *Config) (*Config, error) {
    merged := *global
    
    // Add excludes (more restrictive)
    merged.Exclude = append(merged.Exclude, local.Exclude...)
    
    // Add blacklist patterns (more restrictive)
    merged.Blacklist = append(merged.Blacklist, local.Blacklist...)
    
    // NEVER merge whitelist (global immutable)
    // NEVER remove global restrictions
    
    return &merged, nil
}
```

#### 4. Database Permissions

```bash
# Database must be user-only (no group/other access)
chmod 600 ~/.chainsaw/chainsaw.db
chmod 600 ~/.chainsaw/chainsaw.db-wal
chmod 600 ~/.chainsaw/chainsaw.db-shm
```

#### 5. Ollama Isolation

```go
// Communicate only over localhost
const ollamaEndpoint = "http://127.0.0.1:11434"

// Health check on startup
func (c *OllamaClient) HealthCheck() error {
    resp, err := http.Get(ollamaEndpoint + "/api/version")
    if err != nil {
        return fmt.Errorf("ollama not reachable: %w", err)
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != 200 {
        return fmt.Errorf("ollama unhealthy: status %d", resp.StatusCode)
    }
    
    return nil
}
```

### Configuration System

#### Global Configuration

**Location**: `~/.chainsaw/config.yaml`

```yaml
version: "2.1"
active_profile: "default"

profiles:
  default:
    # Directories to recursively index
    include:
      - ~/go/src/myproject
      - ~/Documents/code
    
    # Directory names to skip
    exclude:
      - node_modules
      - .git
      - vendor
      - build
    
    # File patterns to reject (applied first)
    blacklist:
      - ".*\\.log$"
      - ".*\\.tmp$"
      - ".*\\.secret$"
    
    # Exceptions to blacklist (allows through)
    whitelist:
      - ".*\\.go$"
      - ".*\\.py$"
      - ".*\\.js$"
    
    # Embedding model
    embedding_model: "nomic-embed-text"
    chunk_size: 512
    overlap: 64
    
    # Graph extraction
    graph_driver:
      model: "llama3"
      temperature: 0.1
      concurrency: 2
      output_format: "json"
      custom_system_prompt: |
        Analyze code. Return JSON: [{"source":"A","target":"B","relation":"calls","weight":0.8}]
```

#### Local Configuration Override

**Location**: `.config.yaml` in project directories

```yaml
# Project-specific overrides (additive only)
exclude:
  - tmp/
  - "*.cache"

blacklist:
  - ".*_test\\.go$"  # Skip test files for this project
```

#### Filter Evaluation Order

Two orthogonal systems work together:

1. **Directory Filtering**:
   ```
   is_in_include(path) AND NOT is_in_exclude(path)
   ```

2. **File Filtering**:
   ```
   NOT matches_blacklist(file) OR matches_whitelist(file)
   ```

The whitelist acts as an **exception** to the blacklist, allowing blacklisted files through.

---

## Development Workflow

### Project Structure Navigation

```
chainsaw/
├── cmd/chainsaw/          # Start here for command routing
├── pkg/db/                # Database operations
├── pkg/indexer/           # File processing logic
├── pkg/llm/               # LLM client implementations
├── pkg/cypher/            # Query parsing and execution
└── pkg/config/            # Configuration loading
```

### Common Tasks

#### Adding a New Command

1. Add command case in `cmd/chainsaw/main.go`:
```go
case "mycommand":
    if len(os.Args) < 3 {
        fmt.Println("Usage: chainsaw mycommand <arg>")
        os.Exit(1)
    }
    handleMyCommand(os.Args[2])
```

2. Implement handler:
```go
func handleMyCommand(arg string) {
    db, err := openDB()
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()
    
    // Your logic here
}
```

3. Add to usage message in `main()`

4. Test manually:
```bash
make install
chainsaw mycommand test
```

#### Modifying Database Schema

1. Update DDL in `pkg/db/schema.go`:
```go
const schemaVersion = 3  // Increment version

const createTablesSQL = `
CREATE TABLE IF NOT EXISTS my_new_table (
    id INTEGER PRIMARY KEY,
    data TEXT
);
`
```

2. Add migration logic in `db.go`:
```go
func migrate(db *sql.DB, fromVersion, toVersion int) error {
    if fromVersion < 3 {
        _, err := db.Exec(`CREATE TABLE my_new_table (...)`)
        if err != nil {
            return err
        }
    }
    return nil
}
```

3. Update interface in `pkg/db/interface.go`:
```go
type Database interface {
    // ... existing methods
    InsertMyData(data string) error
}
```

4. Implement methods in `db.go`:
```go
func (db *SQLiteDB) InsertMyData(data string) error {
    _, err := db.conn.Exec("INSERT INTO my_new_table (data) VALUES (?)", data)
    return err
}
```

5. Test with fresh database:
```bash
rm ~/.chainsaw/chainsaw.db
make install
chainsaw init
```

#### Adding an LLM Provider

1. Create package `pkg/llm/myprovider/`:
```go
// client.go
type Client struct {
    apiKey string
    endpoint string
}

func NewClient(apiKey string) *Client {
    return &Client{apiKey: apiKey, endpoint: "https://api.provider.com"}
}
```

2. Implement interfaces:
```go
// embedding.go
func (c *Client) GenerateEmbedding(text string) ([]float32, error) {
    // API call to provider
}

// generate.go
func (c *Client) ExtractRelations(text string) (string, error) {
    // API call to provider
}
```

3. Add provider detection in `cmd/chainsaw/main.go`:
```go
func createLLMClient(cfg *config.ProfileConfig) llm.Client {
    switch cfg.Provider {
    case "ollama":
        return ollama.NewClient()
    case "myprovider":
        return myprovider.NewClient(cfg.APIKey)
    default:
        log.Fatal("unknown provider")
    }
}
```

4. Document in `examples/global-config.yaml`

#### Adding a Cypher Feature

1. Update parser in `pkg/cypher/parser.go`:
```go
type Query struct {
    // ... existing fields
    MyNewClause string
}

func Parse(cypher string) (*Query, error) {
    // Add parsing logic for new syntax
}
```

2. Update transpiler in `pkg/cypher/transpiler.go`:
```go
func (t *Transpiler) Transpile(q *Query) (string, error) {
    // Generate SQL for new clause
}
```

3. Add tests in `pkg/cypher/transpiler_test.go`:
```go
func TestMyNewClause(t *testing.T) {
    q := Parse("MATCH (n) WHERE n.prop = 'value' RETURN n")
    sql, err := Transpile(q)
    assert.NoError(t, err)
    assert.Contains(t, sql, "WHERE")
}
```

### Debugging

#### Database Inspection

```bash
# Open database
sqlite3 ~/.chainsaw/chainsaw.db

# Show tables
.tables

# Check row counts
SELECT COUNT(*) FROM files;
SELECT COUNT(*) FROM chunks;
SELECT COUNT(*) FROM graph_edges;

# Inspect a specific file
SELECT * FROM files WHERE path LIKE '%main.go%';

# Check graph edges
SELECT e.relation_type, COUNT(*) 
FROM graph_edges e 
GROUP BY e.relation_type;
```

#### Daemon Debugging

```bash
# Stop daemon
systemctl --user stop chainsawd

# Run in foreground with debug output
./chainsaw daemon start -debug

# Or check logs
journalctl --user -u chainsawd -f --since "5 minutes ago"

# Check what files are being watched
journalctl --user -u chainsawd | grep "watching"

# Check indexing activity
journalctl --user -u chainsawd | grep "indexed"
```

#### Profiling

```go
// Add pprof to daemon
import _ "net/http/pprof"

func main() {
    go func() {
        log.Println(http.ListenAndServe("localhost:6060", nil))
    }()
    
    // ... normal daemon code
}
```

```bash
# Profile CPU usage
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30

# Profile memory
go tool pprof http://localhost:6060/debug/pprof/heap

# Visualize
go tool pprof -http=:8080 http://localhost:6060/debug/pprof/profile
```

---

## Testing

### Test Organization

```
pkg/
├── config/
│   ├── loader_test.go         # Table-driven tests
│   ├── cache_test.go          # Concurrency tests
│   └── mock_fs.go             # Test doubles
├── db/
│   ├── db_test.go             # Integration tests
│   ├── chunks_test.go         # Unit tests
│   └── graph_test.go          # Graph algorithm tests
└── indexer/
    ├── chunker_test.go        # Chunking strategy tests
    └── extractor_test.go      # LLM mock tests
```

### Running Tests

```bash
# All tests
go test ./...

# Specific package
go test ./pkg/db

# Verbose output
go test -v ./pkg/indexer

# With coverage
go test -cover ./...

# Generate coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Run specific test
go test ./pkg/config -run TestMergeLocal

# With race detector
go test -race ./...

# Benchmark tests
go test -bench=. ./pkg/indexer
```

### Test Patterns

#### Table-Driven Tests

```go
func TestChunking(t *testing.T) {
    tests := []struct {
        name      string
        input     string
        chunkSize int
        want      int
    }{
        {"empty", "", 512, 0},
        {"single_chunk", "hello world", 512, 1},
        {"multiple_chunks", strings.Repeat("a", 1024), 512, 2},
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            chunks := ChunkText(tt.input, tt.chunkSize)
            if len(chunks) != tt.want {
                t.Errorf("got %d chunks, want %d", len(chunks), tt.want)
            }
        })
    }
}
```

#### Mocking for Isolation

```go
// Mock filesystem
type MockFS struct {
    files map[string][]byte
}

func (m *MockFS) ReadFile(path string) ([]byte, error) {
    content, ok := m.files[path]
    if !ok {
        return nil, os.ErrNotExist
    }
    return content, nil
}

// Use in tests
func TestIndexFile(t *testing.T) {
    mockFS := &MockFS{
        files: map[string][]byte{
            "/test/file.go": []byte("package main\n"),
        },
    }
    
    indexer := NewIndexer(db, llm, mockFS)
    err := indexer.IndexFile("/test/file.go")
    assert.NoError(t, err)
}
```

#### Database Tests (Integration)

```go
func TestGraphTraversal(t *testing.T) {
    // Setup: Create temporary database
    tmpDB := t.TempDir() + "/test.db"
    db, err := Open(tmpDB)
    require.NoError(t, err)
    defer db.Close()
    
    // Insert test data
    db.UpsertEdge(1, 2, 0.9, "calls", "test")
    db.UpsertEdge(2, 3, 0.8, "calls", "test")
    
    // Test: Multi-hop traversal
    neighbors, err := db.GetNeighbors(GetNeighborsOptions{
        ChunkID:  1,
        MaxDepth: 2,
    })
    
    // Assert
    assert.NoError(t, err)
    assert.Len(t, neighbors, 2)
    assert.Contains(t, neighborIDs(neighbors), int64(2))
    assert.Contains(t, neighborIDs(neighbors), int64(3))
}
```

#### Concurrency Tests

```go
func TestCacheConcurrency(t *testing.T) {
    cache := NewMemoryCache()
    
    var wg sync.WaitGroup
    for i := 0; i < 100; i++ {
        wg.Add(1)
        go func(n int) {
            defer wg.Done()
            key := fmt.Sprintf("key%d", n%10)
            cache.Set(key, &MergedConfig{})
            cache.Get(key)
        }(i)
    }
    
    wg.Wait()
    // Should not panic or race
}
```

#### LLM Mock Tests

```go
type MockLLM struct {
    responses map[string]string
}

func (m *MockLLM) ExtractRelations(text string) (string, error) {
    if resp, ok := m.responses[text]; ok {
        return resp, nil
    }
    return `[{"source":"A","target":"B","relation":"calls","weight":0.8}]`, nil
}

func TestGraphExtraction(t *testing.T) {
    mockLLM := &MockLLM{
        responses: map[string]string{
            "func A() { B() }": `[{"source":"A","target":"B","relation":"calls"}]`,
        },
    }
    
    extractor := NewExtractor(mockLLM)
    edges, err := extractor.Extract("func A() { B() }")
    
    assert.NoError(t, err)
    assert.Len(t, edges, 1)
    assert.Equal(t, "A", edges[0].Source)
    assert.Equal(t, "B", edges[0].Target)
}
```

### Test Coverage Goals

- **pkg/config**: 85%+ (core logic, many edge cases)
- **pkg/db**: 60%+ (integration tests, harder to mock)
- **pkg/indexer**: 70%+ (business logic)
- **pkg/cypher**: 90%+ (parser/transpiler must be precise)
- **pkg/llm**: 50%+ (mostly thin wrappers)

### CI/CD

```yaml
# .github/workflows/test.yml
name: Tests
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.24'
      
      - name: Run tests
        run: go test -race -coverprofile=coverage.out ./...
      
      - name: Upload coverage
        uses: codecov/codecov-action@v3
        with:
          file: ./coverage.out
```

---

## Contributing

### Getting Started

1. **Fork and clone**:
```bash
git clone https://github.com/yourusername/chainsaw.git
cd chainsaw
git remote add upstream https://github.com/wouteroostervld/chainsaw.git
```

2. **Create a branch**:
```bash
git checkout -b feature/my-feature
```

3. **Make changes and test**:
```bash
# Write code...
go test ./...
go build -o chainsaw ./cmd/chainsaw
```

4. **Commit with descriptive messages**:
```bash
git commit -m "Add WHERE clause support to Cypher transpiler

- Implement WHERE parsing in cypher/parser.go
- Add SQL generation for WHERE conditions
- Add tests for simple and complex predicates
- Update documentation

Fixes #123"
```

5. **Push and create PR**:
```bash
git push origin feature/my-feature
# Open PR on GitHub
```

### Code Style

#### Go Conventions

- Follow [Effective Go](https://golang.org/doc/effective_go)
- Use `gofmt` (automatic with `go fmt ./...`)
- Run `go vet ./...` before committing
- Use meaningful variable names (no single-letter except loop counters)

```go
// ✅ GOOD
func (idx *Indexer) processChunk(chunk *Chunk) error {
    embedding, err := idx.llm.GenerateEmbedding(chunk.Content)
    if err != nil {
        return fmt.Errorf("failed to generate embedding: %w", err)
    }
    return idx.db.InsertEmbedding(chunk.ID, embedding)
}

// ❌ BAD
func (i *Indexer) pc(c *Chunk) error {
    e, err := i.l.GE(c.C)
    if err != nil {
        return err  // No context
    }
    i.d.IE(c.I, e)
    return nil
}
```

#### Comments

- **Package comments**: Describe package purpose
- **Exported functions**: Document what, not how
- **Complex logic**: Explain why, not what

```go
// Package indexer implements the two-pass file indexing pipeline.
// It extracts code chunks, generates embeddings, and builds the knowledge graph.
package indexer

// IndexFile processes a file through the two-pass indexing pipeline.
// It first generates embeddings for all chunks, then extracts graph relations
// between chunks. Returns an error if either pass fails.
func (idx *Indexer) IndexFile(path string) error {
    // Hash check to skip unchanged files
    // (Expensive LLM calls make change detection critical)
    if !idx.hasChanged(path) {
        return nil
    }
    
    // ... implementation
}
```

#### Error Handling

```go
// Wrap errors with context
if err != nil {
    return fmt.Errorf("failed to open database %s: %w", path, err)
}

// Don't wrap io.EOF or other sentinel errors
if err == io.EOF {
    return err  // Don't wrap
}

// Check for specific errors
if errors.Is(err, os.ErrNotExist) {
    // Handle not found
}
```

### Pull Request Guidelines

#### PR Title Format
```
<type>: <short description>

Types: feat, fix, docs, refactor, test, perf
```

Examples:
- `feat: Add WHERE clause support to Cypher queries`
- `fix: Prevent path traversal in local config loading`
- `docs: Update installation instructions for macOS`
- `refactor: Extract graph parser into strategy pattern`
- `test: Add concurrency tests for config cache`
- `perf: Batch embedding requests to reduce latency`

#### PR Description Template

```markdown
## Description
Brief description of what this PR does.

## Motivation
Why is this change needed? What problem does it solve?

## Changes
- Bullet list of specific changes
- Include file paths if relevant

## Testing
How was this tested?
- [ ] Unit tests added/updated
- [ ] Integration tests pass
- [ ] Manual testing performed

## Checklist
- [ ] Code follows project style guidelines
- [ ] Tests pass locally (`go test ./...`)
- [ ] Documentation updated (if needed)
- [ ] No breaking changes (or clearly documented)
```

### Review Process

1. **Automated checks must pass**:
   - All tests pass
   - No race conditions (`go test -race`)
   - Code formatting (`go fmt`)
   - Linting (`go vet`)

2. **Manual review**:
   - Code quality and clarity
   - Test coverage for new code
   - Security considerations
   - Performance implications

3. **Approval required**:
   - At least one maintainer approval
   - All review comments addressed

4. **Merge**:
   - Squash commits for cleaner history
   - Maintainer will merge

### Areas for Contribution

#### High Priority

- [ ] **WHERE clause** for Cypher queries (filter nodes/edges)
- [ ] **Aggregation functions** (SUM, AVG, MAX, MIN)
- [ ] **Language-specific extractors** (Go AST, Python AST, etc.)
- [ ] **Performance optimization** (batch processing, caching)

#### Medium Priority

- [ ] **Web UI** for visualization
- [ ] **Remote Ollama** support (network endpoints)
- [ ] **Multi-repository** indexing and search
- [ ] **Export/import** for sharing indices

#### Low Priority (Nice to Have)

- [ ] **LSP integration** (jump to definition via graph)
- [ ] **Diff-based re-indexing** (incremental updates)
- [ ] **Graph algorithms** (PageRank, community detection)
- [ ] **Alternative vector backends** (Qdrant, Milvus)

### Code Review Checklist

When reviewing PRs, check:

- [ ] **Security**: Path validation, SQL safety, input sanitization
- [ ] **Testing**: Adequate test coverage, edge cases covered
- [ ] **Performance**: No obvious inefficiencies, appropriate data structures
- [ ] **Error Handling**: Errors wrapped with context, no silent failures
- [ ] **Concurrency**: Proper locking, no race conditions
- [ ] **Documentation**: Comments for exported functions, updated README if needed
- [ ] **Backwards Compatibility**: Schema migrations if DB changes, config versioning

### Development Resources

#### Documentation

- [DESIGN.md](DESIGN.md) - Original design specification
- [IMPLEMENTATION_SUMMARY.md](IMPLEMENTATION_SUMMARY.md) - Implementation details
- [GRAPH_FUNCTIONALITY.md](GRAPH_FUNCTIONALITY.md) - Graph system documentation
- [examples/](examples/) - Configuration examples and installation scripts

#### External Resources

- [SQLite WAL Mode](https://www.sqlite.org/wal.html)
- [sqlite-vec Documentation](https://github.com/asg017/sqlite-vec)
- [Ollama API Reference](https://github.com/ollama/ollama/blob/main/docs/api.md)
- [Cypher Query Language](https://neo4j.com/docs/cypher-manual/current/)

#### Communication

- **Issues**: Use GitHub Issues for bug reports and feature requests
- **Discussions**: Use GitHub Discussions for questions and ideas
- **Pull Requests**: Reference related issues in PR descriptions

---

## License

MIT License - See [LICENSE](LICENSE) file for details.

---

## Acknowledgments

**Core Dependencies**:
- [sqlite-vec](https://github.com/asg017/sqlite-vec) - Vector search extension for SQLite
- [Ollama](https://ollama.ai/) - Local LLM and embedding provider
- [fsnotify](https://github.com/fsnotify/fsnotify) - Cross-platform filesystem notifications

**Contributors**:
- [Wouter Oosterveld](https://github.com/wouteroostervld) - Original author

**Special Thanks**:
- All contributors who have opened issues and submitted PRs
- The GraphRAG and vector search community for inspiration

---

**Built with ❤️ for better code understanding**

*Last updated: 2024 | Version 2.1*
