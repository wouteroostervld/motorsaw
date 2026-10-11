package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// FileWatcher watches directories for file changes
type FileWatcher struct {
	watcher  *fsnotify.Watcher
	onChange func(path string)
	mu       sync.Mutex
	watched  map[string]bool
	debounce time.Duration
	pending  map[string]*time.Timer
}

// Config holds watcher configuration
type Config struct {
	DebounceDelay time.Duration // Delay before triggering onChange (default: 1s)
	OnChange      func(path string)
}

// New creates a new file watcher
func New(cfg *Config) (*FileWatcher, error) {
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.DebounceDelay == 0 {
		cfg.DebounceDelay = time.Second
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create watcher: %w", err)
	}

	return &FileWatcher{
		watcher:  watcher,
		onChange: cfg.OnChange,
		watched:  make(map[string]bool),
		debounce: cfg.DebounceDelay,
		pending:  make(map[string]*time.Timer),
	}, nil
}

// Watch adds a directory to the watch list
func (w *FileWatcher) Watch(path string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("failed to resolve path: %w", err)
	}

	if w.watched[abs] {
		return nil // Already watching
	}

	if err := w.watcher.Add(abs); err != nil {
		return fmt.Errorf("failed to watch %s: %w", abs, err)
	}

	w.watched[abs] = true
	return nil
}

// Unwatch removes a directory from the watch list
func (w *FileWatcher) Unwatch(path string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("failed to resolve path: %w", err)
	}

	if !w.watched[abs] {
		return nil // Not watching
	}

	// A directory that was removed is not watched any more already
	delete(w.watched, abs)
	if err := w.watcher.Remove(abs); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
		return fmt.Errorf("failed to unwatch %s: %w", abs, err)
	}
	return nil
}

// Start begins watching for file changes
func (w *FileWatcher) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-w.watcher.Events:
			if !ok {
				return fmt.Errorf("watcher events channel closed")
			}

			// Only care about write and create events
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}

			w.handleEvent(event.Name)

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return fmt.Errorf("watcher errors channel closed")
			}
			// Log error but continue watching
			slog.Error("Watcher error", "error", err)
		}
	}
}

// handleEvent debounces file change events
func (w *FileWatcher) handleEvent(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Cancel existing timer if any
	if timer, exists := w.pending[path]; exists {
		timer.Stop()
	}

	// Set new debounce timer
	w.pending[path] = time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.pending, path)
		w.mu.Unlock()

		if w.onChange != nil {
			w.onChange(path)
		}
	})
}

// Close stops the watcher and releases resources
func (w *FileWatcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Cancel all pending timers
	for _, timer := range w.pending {
		timer.Stop()
	}
	w.pending = make(map[string]*time.Timer)

	return w.watcher.Close()
}

// Watched returns the list of watched directories
func (w *FileWatcher) Watched() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	paths := make([]string, 0, len(w.watched))
	for path := range w.watched {
		paths = append(paths, path)
	}
	return paths
}
