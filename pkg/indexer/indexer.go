// Package indexer keeps the index of source files. What is kept is up to its
// modules, one for each kind of index: the code graph, embeddings. A module
// stores what it derives from a file and knows nothing of the others, so each
// can be switched on or off, for everything or for one project, and built or
// rebuilt alone.
package indexer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
	"github.com/wouteroostervld/motorsaw/pkg/config"
	"github.com/wouteroostervld/motorsaw/pkg/db"
)

// Module is one kind of index
type Module interface {
	// Name identifies the module in the configuration and in the index
	Name() string
	// Ready reports why the module could not index a file now, if it could not
	Ready() error
	// Index stores what the module derives from a file, in place of what it
	// stored for that file before
	Index(ctx context.Context, src *Source) error
	// Forget removes what the module stored for a file
	Forget(fileID int64) error
}

// ModuleError is the failure of one module on a file
type ModuleError struct {
	Module string
	Err    error
}

func (e *ModuleError) Error() string { return e.Module + " index: " + e.Err.Error() }
func (e *ModuleError) Unwrap() error { return e.Err }

// Source is a file being indexed, as the modules see it
type Source struct {
	FileID  int64
	Path    string
	Content []byte

	parsed map[int]*chunker.File
}

// Parse returns the file parsed into chunks of at most maxChunkSize bytes (0
// for no limit), or false when it is not in a language that can be parsed.
// Modules that ask for the same size share the parse.
func (s *Source) Parse(maxChunkSize int) (*chunker.File, bool) {
	if parsed, done := s.parsed[maxChunkSize]; done {
		return parsed, parsed != nil
	}
	parsed, ok := chunker.Parse(s.Path, s.Content, maxChunkSize)
	if !ok {
		parsed = nil
	}
	if s.parsed == nil {
		s.parsed = make(map[int]*chunker.File)
	}
	s.parsed[maxChunkSize] = parsed
	return parsed, ok
}

// Selector tells which modules are switched on for a file, by name
type Selector func(path string) (map[string]bool, error)

// Indexer runs the modules over files
type Indexer struct {
	db      db.Database
	enabled Selector
	modules []Module        // All of them, in the order they run
	scope   map[string]bool // The modules this indexer works on; nil for all
}

// New creates an indexer. Modules run in the order given. A nil selector
// switches every module on for every file.
func New(database db.Database, enabled Selector, modules ...Module) *Indexer {
	return &Indexer{db: database, enabled: enabled, modules: modules}
}

// Only returns an indexer that works on the named modules and leaves what the
// others hold as it is. A command that needs one kind of index uses it to
// bring just that one up to date. Forgetting a file still goes for all.
func (idx *Indexer) Only(names ...string) *Indexer {
	scoped := *idx
	scoped.scope = make(map[string]bool, len(names))
	for _, name := range names {
		scoped.scope[name] = true
	}
	return &scoped
}

// Plan is what is to be done for a file
type Plan struct {
	Update []string // Modules that have to index it
	Forget []string // Modules that hold it but are switched off for it
}

// Empty reports whether there is nothing to do
func (p Plan) Empty() bool {
	return len(p.Update) == 0 && len(p.Forget) == 0
}

// Plan tells what is to be done for a file with the given modification time,
// when held is what FileModules says about it. With force, modules index it
// even when they hold it as it is.
func (idx *Indexer) Plan(path string, modTime int64, held map[string]int64, force bool) (Plan, error) {
	var plan Plan
	enabled, err := idx.enabledFor(path)
	if err != nil {
		return plan, err
	}
	for _, m := range idx.modules {
		name := m.Name()
		if idx.scope != nil && !idx.scope[name] {
			continue
		}
		at, holds := held[name]
		switch {
		case enabled == nil || enabled[name]:
			if force || !holds || at != modTime {
				plan.Update = append(plan.Update, name)
			}
		case holds:
			plan.Forget = append(plan.Forget, name)
		}
	}
	return plan, nil
}

func (idx *Indexer) enabledFor(path string) (map[string]bool, error) {
	if idx.enabled == nil {
		return nil, nil
	}
	enabled, err := idx.enabled(path)
	if err != nil {
		return nil, err
	}
	if enabled == nil {
		enabled = map[string]bool{}
	}
	return enabled, nil
}

// Ready reports why one of the named modules could not index a file now.
// Asking before a run saves finding out once for every file.
func (idx *Indexer) Ready(names []string) error {
	for _, m := range idx.modules {
		for _, name := range names {
			if m.Name() != name {
				continue
			}
			if err := m.Ready(); err != nil {
				return &ModuleError{Module: name, Err: err}
			}
		}
	}
	return nil
}

// IndexFile brings what the modules hold of a file up to date: a module that
// is switched on for it indexes it unless it holds it as it is now, and one
// that is switched off forgets it. A module that fails does not stop the
// others, and is tried again the next time.
func (idx *Indexer) IndexFile(ctx context.Context, filePath string, force bool) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	// Nanoseconds, so an edit within the second it was indexed is still seen
	modTime := info.ModTime().UnixNano()
	fileID, err := idx.db.UpsertFile(filePath, modTime, hash)
	if err != nil {
		return fmt.Errorf("failed to upsert file: %w", err)
	}

	held, err := idx.db.FileModules(fileID)
	if err != nil {
		return err
	}
	plan, err := idx.Plan(filePath, modTime, held, force)
	if err != nil {
		return err
	}

	src := &Source{FileID: fileID, Path: filePath, Content: content}
	var failed []error
	for _, m := range idx.modules {
		name := m.Name()
		switch {
		case contains(plan.Update, name):
			if err := m.Index(ctx, src); err != nil {
				failed = append(failed, &ModuleError{Module: name, Err: err})
				continue
			}
			if err := idx.db.SetFileModule(fileID, name, modTime); err != nil {
				failed = append(failed, err)
			}
		case contains(plan.Forget, name):
			if err := m.Forget(fileID); err != nil {
				failed = append(failed, &ModuleError{Module: name, Err: err})
				continue
			}
			if err := idx.db.ClearFileModule(fileID, name); err != nil {
				failed = append(failed, err)
			}
		}
	}
	return errors.Join(failed...)
}

// ForgetFile drops everything the index has of a file, in every module. The
// file on disk is not touched: this is for a file that is already gone from it.
func (idx *Indexer) ForgetFile(fileID int64, filePath string) error {
	held, err := idx.db.FileModules(fileID)
	if err != nil {
		return err
	}
	for _, m := range idx.modules {
		// The vectors are in a table where finding a file's rows means going
		// through all of them: a fifth of a second a file in a large index.
		// Whatever has vectors is recorded as held, so a file that is not
		// has none to forget.
		if _, ok := held[m.Name()]; !ok && m.Name() == config.IndexEmbeddings {
			continue
		}
		if err := m.Forget(fileID); err != nil {
			return &ModuleError{Module: m.Name(), Err: err}
		}
	}
	return idx.db.ForgetFile(filePath)
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// projectRoot returns the directory of the repository a file belongs to, or
// an empty string when it is not in one
func projectRoot(filePath string) string {
	dir := filepath.Dir(filePath)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
