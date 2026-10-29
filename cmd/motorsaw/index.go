package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
	"github.com/wouteroostervld/motorsaw/pkg/config"
	"github.com/wouteroostervld/motorsaw/pkg/db"
	"github.com/wouteroostervld/motorsaw/pkg/indexer"
)

const indexUsage = `Usage: motorsaw index [options] <path>...

Indexes source files, in every kind of index that is switched on for them:

  graph        Definitions, calls and imports, read from the syntax tree.
               On by default; takes seconds.
  embeddings   A vector for each chunk of code, for "motorsaw search". Off by
               default: it needs a model, and a request to it for every file.

Which are on is set in ~/.motorsaw/config.yaml, and for a project in a
.config.yaml in its directory; "motorsaw status" shows what is on here. A
kind that is switched off for a file drops what it held of it. Each path is a file or a directory; directories are searched for
source files, skipping the excluded directories and file blacklist of your
configuration. Files that have not been modified since they were indexed are
skipped, and a kind of index that already holds a file as it is does not
redo it when another is switched on. When a file under a given directory is gone from disk, the index
forgets it. Motorsaw never deletes or changes source files. A file named directly is indexed whatever the configuration
excludes; one in a language that cannot be parsed is passed over.

"motorsaw query" and "motorsaw search" do this by themselves for the current
directory, each for the kind of index it uses, so running it by hand is only
needed for large trees or to index something elsewhere.

Options:
  --force          Index files again even if they have not been modified
  --only KINDS     Only these kinds of index, comma-separated (graph, embeddings)
  --no-ignore      Also index what git ignores (.gitignore, .git/info/exclude)
  -                Read more paths from standard input, one per line
  -0               With -, paths on standard input end in a NUL byte

Examples:
  motorsaw index .
  motorsaw index pkg cmd/motorsaw/main.go
  git diff --name-only | motorsaw index -
  find . -name '*.go' -newer Makefile -print0 | xargs -0 motorsaw index
  find . -name '*.py' -print0 | motorsaw index -0 -`

// autoIndexLimit is the most files a query will index by itself. Parsing
// more than this takes long enough that it should be asked for.
const autoIndexLimit = 1000

// How long a run keeps trying to write to an index that another process is
// writing to: one that was asked for, and one that a query does in passing
const (
	indexPatience = 5 * time.Minute
	autoPatience  = 5 * time.Second
)

func handleIndex(args []string) {
	flags := flag.NewFlagSet("index", flag.ExitOnError)
	flags.Usage = func() { fmt.Println(indexUsage) }
	force := flags.Bool("force", false, "")
	only := flags.String("only", "", "")
	nul := flags.Bool("0", false, "")
	noIgnore := flags.Bool("no-ignore", false, "")
	flags.Parse(args)

	if flags.NArg() == 0 {
		fmt.Println(indexUsage)
		os.Exit(1)
	}

	// A path that does not exist is reported and the rest is still indexed,
	// so one bad name does not spoil a pipeline
	var roots []string
	missing := 0
	add := func(path string) {
		abs, err := filepath.Abs(path)
		if err == nil {
			_, err = os.Stat(abs)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %v\n", err)
			missing++
			return
		}
		roots = append(roots, abs)
	}
	for _, arg := range flags.Args() {
		if arg != "-" {
			add(arg)
			continue
		}
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			fail("reading standard input: %v", err)
		}
		separator := "\n"
		if *nul {
			separator = "\x00"
		}
		for _, path := range strings.Split(string(input), separator) {
			if path = strings.TrimSuffix(path, "\r"); path != "" {
				add(path)
			}
		}
	}

	st, err := loadSettings()
	if err != nil {
		fail("%v", err)
	}
	if *noIgnore {
		st.walk = newWalk(st.profile, false)
	}
	opts := syncOptions{force: *force, progress: os.Stdout, errors: os.Stderr, patience: indexPatience}
	if *only != "" {
		if opts.only, err = config.CheckIndexKinds(strings.Split(*only, ",")); err != nil {
			fail("--only: %v", err)
		}
	}

	database := openDB()
	defer database.Close()

	start := time.Now()
	stats, err := syncIndex(database, st, roots, opts)
	if errors.Is(err, indexer.ErrNoEmbedder) {
		fail("%v.\n\n%s", err, embeddingsHelp)
	}
	if db.IsBusy(err) {
		fail("another motorsaw (the daemon, or an index run) kept writing to the index for more than %v; "+
			"%d files were indexed and %d are left. Run it again when the other is done.\n(%v)",
			indexPatience, stats.indexed, stats.pending, err)
	}
	if err != nil {
		fail("%v", err)
	}

	fmt.Printf("Indexed %d files in %v", stats.indexed, time.Since(start).Round(time.Millisecond))
	if stats.unchanged > 0 {
		fmt.Printf(", %d unchanged", stats.unchanged)
	}
	if stats.removed > 0 {
		fmt.Printf(", %s", removedSummary(stats.removed))
	}
	if stats.skipped > 0 {
		fmt.Printf(", %d not source files", stats.skipped)
	}
	if stats.failed > 0 {
		fmt.Printf(", %d failed", stats.failed)
	}
	if missing > 0 {
		fmt.Printf(", %d not found", missing)
	}
	fmt.Println()
	if stats.failed > 0 || missing > 0 {
		os.Exit(1)
	}
}

type syncOptions struct {
	ctx      context.Context // Stops the run between two files when done; nil to run to the end
	only     []string        // The kinds of index to bring up to date; all of them when empty
	force    bool            // Index files even when they have not been modified
	limit    int             // Do nothing when more files than this need indexing (0 = no limit)
	progress io.Writer       // Where to report progress on a long run; nil for silence
	errors   io.Writer       // Where to report files that could not be indexed; nil for silence
	log      *slog.Logger    // Reports to the log instead, as the daemon does; nil for the writers
	// How long to keep trying when another process is writing to the index,
	// beyond the ten seconds SQLite waits by itself; see write
	patience time.Duration
}

// write does something that writes to the index, and does it again when it
// fails because another process is writing: the daemon in its scan, or an
// index run in another terminal. It gives up when that has gone on for
// longer than the run has patience for, with the error of the last try,
// which db.IsBusy tells.
func (o syncOptions) write(do func() error) error {
	var deadline time.Time
	wait := 200 * time.Millisecond
	for {
		err := do()
		if !db.IsBusy(err) || (o.ctx != nil && o.ctx.Err() != nil) {
			return err
		}
		switch {
		case deadline.IsZero():
			deadline = time.Now().Add(o.patience)
			o.waiting()
		case time.Now().After(deadline):
			return err
		}
		time.Sleep(wait)
		if wait *= 2; wait > 5*time.Second {
			wait = 5 * time.Second
		}
	}
}

// waiting reports that the run waits for another process
func (o syncOptions) waiting() {
	switch {
	case o.log != nil:
		o.log.Info("Another process is writing to the index, waiting for it")
	case o.progress != nil:
		fmt.Fprintln(o.progress, "  another motorsaw is writing to the index, waiting for it...")
	}
}

// removed reports a file the index knew that is gone from disk
func (o syncOptions) removed(path string) {
	switch {
	case o.log != nil:
		o.log.Debug("Removed file detected", "file", path)
	case o.progress != nil:
		fmt.Fprintf(o.progress, "removed file detected: %s\n", displayPath(path, workingDir()))
	}
}

// failed reports a file that could not be indexed
func (o syncOptions) failed(path string, err error) {
	switch {
	case o.log != nil:
		o.log.Warn("File indexing failed", "file", path, "error", err)
	case o.errors != nil:
		fmt.Fprintf(o.errors, "  %s: %v\n", displayPath(path, workingDir()), err)
	}
}

// progressed reports how far a long run is
func (o syncOptions) progressed(done, total int) {
	switch {
	case o.log != nil:
		o.log.Info("Scan in progress", "indexed", done, "total", total)
	case o.progress != nil:
		fmt.Fprintf(o.progress, "  indexed %d of %d files...\n", done, total)
	}
}

type syncStats struct {
	indexed   int
	unchanged int
	removed   int // Files the index knew that are gone from disk
	failed    int
	skipped   int // Files named directly that are not in a language that can be parsed
	pending   int // Files that need indexing but were left because of the limit
}

// resolveEvery is how many files are stored between two times the references
// are resolved, so that a long run shows in the graph as it goes
const resolveEvery = 500

// syncIndex brings the index up to date with the given files and with the
// files under the given directories: new and modified files are indexed, and
// the index forgets files that are gone from disk. Nothing on disk is changed.
// A file is indexed in the kinds of index that are switched on for it and do
// not hold it as it is now.
func syncIndex(database *db.DB, st *settings, roots []string, opts syncOptions) (stats syncStats, err error) {
	// References are linked to their targets once for many files, which is
	// what makes indexing a tree take minutes and not hours
	resolve := database.DeferResolving(resolveEvery)
	defer func() {
		// Once the count of who puts it off is down, it can be tried again
		resolveErr := resolve()
		if db.IsBusy(resolveErr) {
			resolveErr = opts.write(database.ResolveSymbols)
		}
		if err == nil {
			err = resolveErr
		}
	}()

	idx, err := st.indexer(database, opts.only...)
	if err != nil {
		return stats, err
	}
	if err := resetEmbeddings(database, st, opts); err != nil {
		return stats, err
	}

	// What is on disk, and which of it the index does not have as it is now
	var todo []string
	var planErr error
	known := make(map[string]db.IndexedFile)
	seen := make(map[string]bool)
	needed := make(map[string]bool) // The kinds of index that have files to index

	check := func(path string, info os.FileInfo) {
		if seen[path] {
			return
		}
		seen[path] = true
		plan, err := idx.Plan(path, info.ModTime().UnixNano(), known[path].Modules, opts.force)
		if err != nil {
			planErr = err
			return
		}
		if plan.Empty() {
			stats.unchanged++
			return
		}
		for _, kind := range plan.Update {
			needed[kind] = true
		}
		todo = append(todo, path)
	}

	for _, root := range roots {
		indexed, err := database.IndexedFiles(root)
		if err != nil {
			return stats, err
		}
		for path, f := range indexed {
			known[path] = f
		}

		info, err := os.Stat(root)
		if err != nil {
			return stats, err
		}
		if !info.IsDir() {
			// A file named directly is indexed whatever the configuration
			// excludes, as long as it can be parsed
			if chunker.Recognised(root) {
				check(root, info)
			} else if !seen[root] {
				seen[root] = true
				stats.skipped++
			}
			continue
		}

		err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				// A directory named by hand is not limited to the included
				// directories, but excluded ones are skipped as the daemon does
				if path != root && st.walk.SkipDir(root, path) {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			// What the index holds was recognised before; a file without
			// an extension has to be read to tell, which is done once
			if _, held := known[path]; !held && !chunker.Recognised(path) {
				return nil
			}
			if st.walk.SkipFile(root, path) {
				return nil
			}
			if info, err := d.Info(); err == nil {
				check(path, info)
			}
			return nil
		})
		if err != nil {
			return stats, err
		}
	}

	// Files the index has that are gone from disk. One that is still there
	// but was not walked, such as a file in an excluded directory that was
	// indexed by name, stays: only the daemon prunes those, and only under
	// the included directories (see pruneIgnored).
	for path, f := range known {
		if seen[path] {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := opts.write(func() error { return idx.ForgetFile(f.ID, path) }); err != nil {
			return stats, err
		}
		stats.removed++
		opts.removed(path)
	}

	if planErr != nil {
		return stats, planErr
	}

	if opts.limit > 0 && len(todo) > opts.limit {
		stats.pending = len(todo)
		return stats, nil
	}

	// Said once here, not once for every file
	kinds := make([]string, 0, len(needed))
	for kind := range needed {
		kinds = append(kinds, kind)
	}
	if err := idx.Ready(kinds); err != nil {
		return stats, err
	}

	ctx := opts.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	reported := time.Now()
	inARow := 0 // Files on end that a model would not embed
	for done, path := range todo {
		if ctx.Err() != nil {
			stats.pending = len(todo) - done
			return stats, ctx.Err()
		}
		if err := opts.write(func() error { return idx.IndexFile(ctx, path, opts.force) }); err != nil {
			// Every file after this one would wait as long and fail the same
			if db.IsBusy(err) {
				stats.pending = len(todo) - done
				return stats, err
			}
			opts.failed(path, err)
			stats.failed++
			// A model that is away fails every file the same way
			var failure *indexer.ModuleError
			if errors.As(err, &failure) && failure.Module == config.IndexEmbeddings {
				if inARow++; inARow == 3 && len(todo)-done > 1 {
					return stats, fmt.Errorf("stopped with %d files to go: the embeddings index failed for three files in a row", len(todo)-done-1)
				}
			}
			continue
		}
		inARow = 0
		stats.indexed++
		// Parsing reports every few hundred files; a slow model every few
		// seconds. A log is told twice a minute.
		due := stats.indexed%200 == 0 || time.Since(reported) > 5*time.Second
		if opts.log != nil {
			due = time.Since(reported) > 30*time.Second
		}
		if due {
			opts.progressed(stats.indexed, len(todo))
			reported = time.Now()
		}
	}
	return stats, nil
}

// resetEmbeddings drops the embeddings when they were made with another model
// than the one that is configured now: vectors of two models cannot be
// compared. It does so only for a run that is going to make them again.
func resetEmbeddings(database *db.DB, st *settings, opts syncOptions) error {
	if len(opts.only) > 0 {
		inScope := false
		for _, kind := range opts.only {
			inScope = inScope || kind == config.IndexEmbeddings
		}
		if !inScope {
			return nil
		}
	}
	model := st.profile.EmbeddingSettings().Model
	have, _, err := database.EmbeddingModel()
	if err != nil || have == "" || model == "" || have == model {
		return err
	}
	if opts.limit > 0 {
		// In passing, before a search: leave it to be asked for
		return nil
	}
	switch {
	case opts.log != nil:
		opts.log.Info("Embedding model changed, making embeddings again", "had", have, "configured", model)
	case opts.progress != nil:
		fmt.Fprintf(opts.progress, "embeddings were made with %s and %s is configured: making them again\n", have, model)
	}
	return database.ResetEmbeddings(config.IndexEmbeddings)
}

// removedSummary words the count of files that were found to be gone
func removedSummary(n int) string {
	if n == 1 {
		return "1 removed file detected"
	}
	return fmt.Sprintf("%d removed files detected", n)
}

// autoIndex brings one kind of index up to date with the current directory
// before it is used, so results never come from stale or missing files. It
// says what it did on standard error, and leaves a tree that is too large to
// index in passing for "motorsaw index". When it could not do it, it returns
// why, for the command to say with its results: they are then from the index
// as it was.
func autoIndex(database *db.DB, cwd string, kind string) (stale string) {
	if cwd == "" {
		return ""
	}
	st, err := loadSettings()
	if err != nil {
		fail("%v", err)
	}
	limit := autoIndexLimit
	if kind == config.IndexEmbeddings {
		limit = autoEmbedLimit
	}
	start := time.Now()
	stats, err := syncIndex(database, st, []string{cwd}, syncOptions{
		only: []string{kind}, limit: limit, progress: os.Stderr, errors: os.Stderr, patience: autoPatience,
	})
	switch {
	case db.IsBusy(err):
		stale = fmt.Sprintf("the %s index could not be brought up to date: another motorsaw (the daemon, or an index run) "+
			"has been writing to it for more than %v. Results are from the index as it was and may be stale or incomplete; "+
			"ask again in a moment", kind, time.Since(start).Round(time.Second))
	case err != nil:
		stale = fmt.Sprintf("could not bring the %s index up to date: %v (results may be stale or incomplete)", kind, err)
	case stats.pending > 0:
		stale = fmt.Sprintf("%d files here are not in the %s index or have changed, too many to index in passing; "+
			"run \"motorsaw index .\" (results may be incomplete)", stats.pending, kind)
	case stats.indexed > 0 || stats.removed > 0 || stats.failed > 0:
		fmt.Fprintf(os.Stderr, "note: %s index updated in %v: %d files indexed", kind, time.Since(start).Round(time.Millisecond), stats.indexed)
		if stats.removed > 0 {
			fmt.Fprintf(os.Stderr, ", %s", removedSummary(stats.removed))
		}
		if stats.failed > 0 {
			fmt.Fprintf(os.Stderr, ", %d failed", stats.failed)
		}
		fmt.Fprintln(os.Stderr)
	}
	if stale != "" {
		fmt.Fprintf(os.Stderr, "note: %s\n", stale)
	}
	return stale
}
