package worker

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/wouteroostervld/chainsaw/pkg/db"
	"github.com/wouteroostervld/chainsaw/pkg/indexer"
)

// Config holds worker configuration
type Config struct {
	DB           db.Database
	Indexer      *indexer.Indexer
	PollInterval time.Duration
	BatchSize    int
	MaxRetries   int
}

// IndexWorker processes files from the work queue
type IndexWorker struct {
	db           db.Database
	indexer      atomic.Pointer[indexer.Indexer]
	pollInterval time.Duration
	batchSize    int
	maxRetries   int
}

// NewIndexWorker creates a new index worker
func NewIndexWorker(cfg *Config) *IndexWorker {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 10
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}

	w := &IndexWorker{
		db:           cfg.DB,
		pollInterval: cfg.PollInterval,
		batchSize:    cfg.BatchSize,
		maxRetries:   cfg.MaxRetries,
	}
	w.indexer.Store(cfg.Indexer)
	return w
}

// SetIndexer makes the worker index with another indexer from the next file
// on, as when the configuration was read again
func (w *IndexWorker) SetIndexer(idx *indexer.Indexer) {
	w.indexer.Store(idx)
}

// Start begins processing files from the queue
func (w *IndexWorker) Start(ctx context.Context) error {
	slog.Debug("Index worker started", "poll_interval", w.pollInterval, "batch_size", w.batchSize)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	// Process immediately on startup
	w.processBatch(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Debug("Index worker stopped")
			return ctx.Err()
		case <-ticker.C:
			w.processBatch(ctx)
		}
	}
}

// processBatch fetches and processes a batch of pending files
func (w *IndexWorker) processBatch(ctx context.Context) {
	files, err := w.db.GetPendingFiles(w.batchSize)
	if err != nil {
		slog.Error("Failed to get pending files", "error", err)
		return
	}

	if len(files) == 0 {
		return // No work to do
	}

	slog.Debug("Processing batch", "count", len(files))

	for _, file := range files {
		select {
		case <-ctx.Done():
			return
		default:
			w.processFile(ctx, file)
		}
	}
}

// processFile processes a single file from the queue
func (w *IndexWorker) processFile(ctx context.Context, file *db.File) {
	// Mark as processing to prevent duplicate work
	if err := w.db.MarkFileProcessing(file.ID); err != nil {
		slog.Error("Failed to mark file processing", "file", file.Path, "error", err)
		return
	}

	slog.Debug("Processing file", "file", file.Path, "retry", file.RetryCount)
	idx := w.indexer.Load()

	// A file that is gone from disk has nothing left to index; the index
	// forgets it
	if _, err := os.Stat(file.Path); os.IsNotExist(err) {
		if err := idx.ForgetFile(file.ID, file.Path); err != nil {
			slog.Error("Failed to forget removed file", "file", file.Path, "error", err)
			return
		}
		slog.Debug("Removed file detected", "file", file.Path)
		return
	}

	// Do the actual indexing: every kind of index that is switched on for the file
	if err := idx.IndexFile(ctx, file.Path, false); err != nil {
		w.handleFailure(file, err)
		return
	}

	// Success - mark as indexed
	if err := w.db.MarkFileIndexed(file.ID); err != nil {
		slog.Error("Failed to mark file indexed", "file", file.Path, "error", err)
		return
	}

	slog.Debug("Indexed file", "file", file.Path)
}

// handleFailure handles indexing failures with retry logic
func (w *IndexWorker) handleFailure(file *db.File, indexErr error) {
	retryCount := file.RetryCount + 1

	if retryCount >= w.maxRetries {
		// Permanent failure
		if err := w.db.MarkFileFailed(file.ID, indexErr.Error(), retryCount); err != nil {
			slog.Error("Failed to mark file failed", "file", file.Path, "error", err)
			return
		}
		slog.Error("File indexing failed permanently", "file", file.Path, "retries", retryCount, "error", indexErr)
	} else {
		// Re-queue for retry
		if err := w.db.MarkFilePending(file.Path, file.LastModTime, file.ContentHash); err != nil {
			slog.Error("Failed to requeue file", "file", file.Path, "error", err)
			return
		}
		slog.Warn("File indexing failed, will retry", "file", file.Path, "retry", retryCount, "max_retries", w.maxRetries, "error", indexErr)
	}
}
