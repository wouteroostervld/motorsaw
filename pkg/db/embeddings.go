package db

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

// Embeddings are kept in a table of their own, which is only there once
// something has been embedded: the length of a vector is fixed when the table
// is made, and is not known before a model has answered. As everywhere in the
// index, only where a chunk is is kept, not its text.

func init() {
	// Makes the vec0 virtual table known to every connection opened from here on
	sqlite_vec.Auto()
}

const createEmbeddingsTable = `
CREATE VIRTUAL TABLE embedding_chunks USING vec0(
    chunk_id INTEGER PRIMARY KEY,
    file_id INTEGER,
    +start_line INTEGER,
    +end_line INTEGER,
    +title TEXT,
    embedding FLOAT[%d] distance_metric=cosine
);`

// EmbeddingChunk is a piece of a file and the vector that stands for it
type EmbeddingChunk struct {
	StartLine int    // 1-indexed
	EndLine   int    // 1-indexed, inclusive
	Title     string // The definition or section it is, or is part of; empty for the file itself
	Key       string // A hash of the text the vector was made from; empty when there is none to reuse it by
	Vector    []float32
}

// EmbeddingHit is a chunk found by a search
type EmbeddingHit struct {
	Path      string
	StartLine int
	EndLine   int
	Title     string
	Distance  float64 // Cosine distance: 0 is the same direction, 1 is unrelated
}

// EmbeddingModel returns the model the embeddings in the index were made
// with and the length of its vectors. The model is empty when there are none.
func (db *DB) EmbeddingModel() (string, int, error) {
	model, err := db.GetMeta(MetaKeyEmbeddingModel)
	if err != nil || model == "" {
		return "", 0, err
	}
	value, err := db.GetMeta(MetaKeyEmbeddingDim)
	if err != nil {
		return "", 0, err
	}
	dim, _ := strconv.Atoi(value)
	return model, dim, nil
}

// InsertEmbeddings stores the embedded chunks of a file. The first vectors
// that are stored decide the model and the length of every later one.
func (db *DB) InsertEmbeddings(fileID int64, model string, chunks []EmbeddingChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	dim := len(chunks[0].Vector)
	if dim == 0 {
		return fmt.Errorf("embedding is empty")
	}

	haveModel, haveDim, err := db.EmbeddingModel()
	if err != nil {
		return err
	}
	switch {
	case haveModel == "":
		if _, err := db.conn.Exec(fmt.Sprintf(createEmbeddingsTable, dim)); err != nil {
			return fmt.Errorf("failed to create embeddings table: %w", err)
		}
		if err := db.SetMeta(MetaKeyEmbeddingDim, strconv.Itoa(dim)); err != nil {
			return err
		}
		if err := db.SetMeta(MetaKeyEmbeddingModel, model); err != nil {
			return err
		}
	case haveModel != model:
		return fmt.Errorf("the index holds embeddings made with %s, not %s", haveModel, model)
	case haveDim != dim:
		return fmt.Errorf("%s now gives vectors of length %d, the index holds vectors of length %d", model, dim, haveDim)
	}

	// A transaction writes; see pool
	db.conn.writing.Lock()
	defer db.conn.writing.Unlock()

	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO embedding_chunks (file_id, start_line, end_line, title, embedding) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("failed to prepare embedding insert: %w", err)
	}
	defer stmt.Close()
	// A key left behind by a version that did not remove keys may name the
	// chunk_id that is given out again; this one replaces it
	keyStmt, err := tx.Prepare("INSERT OR REPLACE INTO embedding_keys (chunk_id, text_key) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("failed to prepare embedding key insert: %w", err)
	}
	defer keyStmt.Close()

	for _, c := range chunks {
		if len(c.Vector) != dim {
			return fmt.Errorf("embeddings of different lengths: %d and %d", dim, len(c.Vector))
		}
		vector, err := sqlite_vec.SerializeFloat32(c.Vector)
		if err != nil {
			return fmt.Errorf("failed to serialize embedding: %w", err)
		}
		result, err := stmt.Exec(fileID, c.StartLine, c.EndLine, c.Title, vector)
		if err != nil {
			return fmt.Errorf("failed to insert embedding: %w", err)
		}
		if c.Key == "" {
			continue
		}
		chunkID, err := result.LastInsertId()
		if err != nil {
			return fmt.Errorf("failed to insert embedding: %w", err)
		}
		if _, err := keyStmt.Exec(chunkID, c.Key); err != nil {
			return fmt.Errorf("failed to insert embedding key: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit embeddings: %w", err)
	}
	return nil
}

// DeleteEmbeddings removes the embeddings of a file
func (db *DB) DeleteEmbeddings(fileID int64) error {
	if model, _, err := db.EmbeddingModel(); err != nil || model == "" {
		return err
	}

	// A transaction writes; see pool
	db.conn.writing.Lock()
	defer db.conn.writing.Unlock()

	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM embedding_keys WHERE chunk_id IN (SELECT chunk_id FROM embedding_chunks WHERE file_id = ?)", fileID); err != nil {
		return fmt.Errorf("failed to delete embedding keys: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM embedding_chunks WHERE file_id = ?", fileID); err != nil {
		return fmt.Errorf("failed to delete embeddings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit embeddings delete: %w", err)
	}
	return nil
}

// CachedEmbeddings returns the vectors already made for texts with the given
// keys, by key. A key nothing was made for is not in the map.
func (db *DB) CachedEmbeddings(keys []string) (map[string][]float32, error) {
	if model, _, err := db.EmbeddingModel(); err != nil || model == "" {
		return nil, err
	}

	stmt, err := db.conn.Prepare(`
		SELECT e.embedding
		FROM embedding_keys k JOIN embedding_chunks e ON e.chunk_id = k.chunk_id
		WHERE k.text_key = ?
		LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare embedding lookup: %w", err)
	}
	defer stmt.Close()

	vectors := make(map[string][]float32)
	for _, key := range keys {
		if _, done := vectors[key]; done {
			continue
		}
		var blob []byte
		err := stmt.QueryRow(key).Scan(&blob)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to look up embedding: %w", err)
		}
		vector, err := deserializeFloat32(blob)
		if err != nil {
			return nil, err
		}
		vectors[key] = vector
	}
	return vectors, nil
}

// deserializeFloat32 reads a vector as sqlite-vec stores it: little-endian
// 32-bit floats
func deserializeFloat32(blob []byte) ([]float32, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("stored embedding is %d bytes, not a whole number of floats", len(blob))
	}
	vector := make([]float32, len(blob)/4)
	for i := range vector {
		vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vector, nil
}

// ResetEmbeddings removes every embedding, and with it the choice of model.
// module is the kind of index that is recorded as holding them.
func (db *DB) ResetEmbeddings(module string) error {
	// A transaction writes; see pool
	db.conn.writing.Lock()
	defer db.conn.writing.Unlock()

	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DROP TABLE IF EXISTS embedding_chunks"); err != nil {
		return fmt.Errorf("failed to drop embeddings table: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM embedding_keys"); err != nil {
		return fmt.Errorf("failed to reset embeddings: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM file_modules WHERE module = ?", module); err != nil {
		return fmt.Errorf("failed to reset embeddings: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM meta WHERE key IN (?, ?)", MetaKeyEmbeddingModel, MetaKeyEmbeddingDim); err != nil {
		return fmt.Errorf("failed to reset embeddings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit embeddings reset: %w", err)
	}
	return nil
}

// CountEmbeddings returns the number of embedded chunks
func (db *DB) CountEmbeddings() (int64, error) {
	if model, _, err := db.EmbeddingModel(); err != nil || model == "" {
		return 0, err
	}
	var count int64
	if err := db.conn.QueryRow("SELECT COUNT(*) FROM embedding_chunks").Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count embeddings: %w", err)
	}
	return count, nil
}

// SearchEmbeddings returns the chunks nearest to a vector, nearest first.
// With a root, only chunks of the files at or under that path are searched.
func (db *DB) SearchEmbeddings(query []float32, limit int, root string) ([]EmbeddingHit, error) {
	_, dim, err := db.EmbeddingModel()
	if err != nil {
		return nil, err
	}
	if dim == 0 {
		return nil, nil
	}
	if len(query) != dim {
		return nil, fmt.Errorf("the query vector has length %d, the index holds vectors of length %d", len(query), dim)
	}
	vector, err := sqlite_vec.SerializeFloat32(query)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize query embedding: %w", err)
	}

	// The scope narrows the search itself. Filtering the nearest chunks
	// afterwards would leave few or none when other projects in the index
	// are closer to the query.
	sqlText := `
		SELECT file_id, start_line, end_line, title, distance
		FROM embedding_chunks
		WHERE embedding MATCH ? AND k = ?`
	args := []any{vector, limit}
	if root != "" {
		sqlText += " AND file_id IN (SELECT id FROM files WHERE path = ? OR substr(path, 1, length(?)) = ?)"
		args = append(args, root, root+"/", root+"/")
	}
	sqlText += " ORDER BY distance"

	rows, err := db.conn.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to search embeddings: %w", err)
	}
	defer rows.Close()

	var hits []EmbeddingHit
	var fileIDs []int64
	for rows.Next() {
		var h EmbeddingHit
		var fileID int64
		var title sql.NullString
		if err := rows.Scan(&fileID, &h.StartLine, &h.EndLine, &title, &h.Distance); err != nil {
			return nil, fmt.Errorf("failed to scan embedding: %w", err)
		}
		h.Title = title.String
		hits = append(hits, h)
		fileIDs = append(fileIDs, fileID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to search embeddings: %w", err)
	}

	paths := make(map[int64]string)
	for i, id := range fileIDs {
		path, ok := paths[id]
		if !ok {
			if err := db.conn.QueryRow("SELECT path FROM files WHERE id = ?", id).Scan(&path); err != nil && err != sql.ErrNoRows {
				return nil, fmt.Errorf("failed to look up file: %w", err)
			}
			paths[id] = path
		}
		hits[i].Path = path
	}
	return hits, nil
}
