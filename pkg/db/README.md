# pkg/db

The index: one SQLite file, `~/.motorsaw/graph.db`. It holds names,
locations, edges and, when the embeddings index is on, vectors. It holds no
source text; code is read from the files when a query asks for it. The index
can be deleted at any time and is built again from the source.

## Files

| File | Holds |
|---|---|
| `schema.go` | The tables, and the schema version |
| `db.go` | Opening the database, upgrading an older one, the queue for writing |
| `files.go` | The files the index knows, and which kinds of index hold each |
| `chunks.go` | Where the chunks of a file are; counts |
| `symbols.go` | Definitions, references, and linking the one to the other |
| `embeddings.go` | Vectors, in a table that is made when the first are stored |
| `interface.go` | What the indexer needs of the database |

## Tables

| Table | Holds |
|---|---|
| `meta` | The schema version, and the model the embeddings were made with |
| `files` | Every indexed file, with its modification time |
| `file_modules` | Which kinds of index hold a file, and as of when |
| `chunks` | The line ranges a file folds into |
| `entities` | The nodes of the graph: definitions, and placeholders for what is called but not indexed |
| `graph_edges` | The edges: `calls`, `may_call`, `imports`, `defines`, `implements` |
| `symbol_defs` | The definitions of each file, with the project they are in |
| `symbol_refs` | The calls and imports of each file, as written, before they are linked |
| `symbol_dirty` | Names whose definitions changed; references to them are linked again |
| `project_follows` | The directories a project follows its imports into |
| `embedding_chunks` | A vector per chunk (sqlite-vec); only there when embeddings are on |
| `embedding_keys` | A hash of the text each vector was made from, so the same text is not embedded twice |

## How references become edges

A file's definitions and references are stored as the parser found them.
Linking is a separate step, `resolveSymbols`: it takes the references that
were never linked and those to names whose definitions changed, picks their
targets (`selectTargets`) and writes the difference with the edges that are
there. A reference stays within its project unless the project follows the
directory the target is in.

Linking after every file is what a single change needs. Indexing many files
puts it off with `DeferResolving` and links once for the lot.

## Writing

SQLite lets one connection write at a time. The writers of one process wait
for each other in a queue (`pool`); between processes it is SQLite's busy
timeout of ten seconds that decides. Reading never waits: the database is in
WAL mode.

## Schema changes

`SchemaVersion` is checked when the database is opened. An older database is
upgraded where `initSchema` says how, which for the graph means emptying it
so that every file is indexed again; any other version is refused with the
advice to remove the file.
