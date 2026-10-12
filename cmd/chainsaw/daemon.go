package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wouteroostervld/chainsaw/pkg/chunker"
	"github.com/wouteroostervld/chainsaw/pkg/config"
	"github.com/wouteroostervld/chainsaw/pkg/db"
	"github.com/wouteroostervld/chainsaw/pkg/indexer"
	"github.com/wouteroostervld/chainsaw/pkg/watcher"
	"github.com/wouteroostervld/chainsaw/pkg/worker"
)

func handleDaemon() {
	if len(os.Args) < 3 {
		printDaemonUsage()
		os.Exit(1)
	}

	subcommand := os.Args[2]

	switch subcommand {
	case "start":
		handleDaemonStart()
	case "status":
		handleDaemonStatus()
	case "help", "-h":
		printDaemonUsage()
	default:
		fmt.Printf("Unknown daemon subcommand: %s\n\n", subcommand)
		printDaemonUsage()
		os.Exit(1)
	}
}

func printDaemonUsage() {
	fmt.Println("chainsaw daemon - Background indexing daemon")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  chainsaw daemon start [-debug] [-loglevel level] [-scan=false] [-scan-kinds kinds]")
	fmt.Println("                        Start the daemon")
	fmt.Println("  chainsaw daemon status")
	fmt.Println("                        Show daemon status")
	fmt.Println()
	fmt.Println("Flags for 'start' command:")
	fmt.Println("  -debug          Enable debug logging")
	fmt.Println("  -loglevel LEVEL Set log level: debug, info, warn, error (default: info)")
	fmt.Println("  -scan=false     Do not bring the index up to date with the watched")
	fmt.Println("                  directories at startup; only index what changes from now on")
	fmt.Println("  -scan-kinds K   The kinds of index the startup scan fills, comma-separated")
	fmt.Println("                  (graph, embeddings; default: graph)")
	fmt.Println()
	fmt.Println("The scan flags override daemon.scan and daemon.scan_kinds of the configuration.")
	fmt.Println()
	fmt.Println("On SIGHUP the daemon reads the configuration again, watches the directories")
	fmt.Println("it names now, makes the index forget the files it ignores and scans again.")
}

func handleDaemonStart() {
	startFlags := flag.NewFlagSet("daemon-start", flag.ExitOnError)
	debug := startFlags.Bool("debug", false, "Enable debug logging")
	logLevelStr := startFlags.String("loglevel", "info", "Log level (debug, info, warn, error)")

	scan := startFlags.Bool("scan", true, "Bring the index up to date at startup")
	scanKinds := startFlags.String("scan-kinds", "", "Kinds of index the startup scan fills")

	startFlags.Parse(os.Args[3:])

	level := slog.LevelInfo
	switch {
	case *debug:
		level = slog.LevelDebug
	case *logLevelStr == "debug":
		level = slog.LevelDebug
	case *logLevelStr == "warn" || *logLevelStr == "warning":
		level = slog.LevelWarn
	case *logLevelStr == "error":
		level = slog.LevelError
	case *logLevelStr != "info":
		fmt.Fprintf(os.Stderr, "Unknown log level: %s (known: debug, info, warn, error)\n", *logLevelStr)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Before anything that takes long: a SIGHUP that is not asked for ends
	// the process
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	hupChan := make(chan os.Signal, 1)
	signal.Notify(hupChan, syscall.SIGHUP)

	dbPath := db.DefaultPath()
	database, err := db.Open(db.Config{Path: dbPath})
	if err != nil {
		slog.Error("Failed to open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	// What to scan for: the configuration, unless a flag says otherwise
	var flags scanFlags
	startFlags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "scan":
			flags.scan = scan
		case "scan-kinds":
			if flags.kinds, err = config.CheckIndexKinds(strings.Split(*scanKinds, ",")); err != nil {
				slog.Error("Flag cannot be used", "flag", "-scan-kinds", "error", err)
				os.Exit(1)
			}
		}
	})

	slog.Info("Daemon starting", "database", dbPath, "config", configPath())
	d, err := newDaemon(database, flags)
	if err != nil {
		slog.Error("Configuration cannot be used", "error", err)
		os.Exit(1)
	}
	defer d.watcher.Close()

	if err := database.ResetStuckProcessing(); err != nil {
		slog.Warn("Failed to reset stuck processing state", "error", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := d.worker.Start(ctx); err != nil && err != context.Canceled {
			slog.Error("Index worker error", "error", err)
		}
	}()
	go func() {
		if err := d.watcher.Start(ctx); err != nil && err != context.Canceled {
			slog.Error("Watcher error", "error", err)
		}
	}()

	// Watching, pruning and scanning go on beside this, and again on SIGHUP
	served := make(chan struct{})
	go func() {
		defer close(served)
		d.serve(ctx, hupChan)
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	slog.Info("Daemon running")

	var lastStatus string // To say it again only when it is news

	for {
		select {
		case <-sigChan:
			slog.Info("Daemon stopping")
			cancel()
			<-served
			slog.Info("Daemon stopped")
			return

		case <-ticker.C:
			counts, err := database.Count()
			if err != nil {
				slog.Error("Failed to count what is indexed", "error", err)
				continue
			}
			var edgeCount int64
			for _, n := range counts.Edges {
				edgeCount += n
			}
			byStatus, _ := database.CountFilesByStatus()
			status := []any{
				"files", counts.Files, "indexed", byStatus["indexed"], "pending", byStatus["pending"],
				"processing", byStatus["processing"], "failed", byStatus["failed"],
				"definitions", counts.Definitions, "edges", edgeCount,
			}
			// An idle daemon has nothing new to say
			level := slog.LevelDebug
			if now := fmt.Sprint(status...); now != lastStatus {
				level, lastStatus = slog.LevelInfo, now
			}
			slog.Log(context.Background(), level, "Index status", status...)
		}
	}
}

// scanFlags are the flags that win over daemon.scan and daemon.scan_kinds of
// the configuration, however often it is read
type scanFlags struct {
	scan  *bool    // nil when not given
	kinds []string // nil when not given
}

// daemonConfig is the configuration as the daemon uses it. It is not changed
// once made: reading the configuration again makes another.
type daemonConfig struct {
	st      *settings
	idx     *indexer.Indexer // Keeps every kind of index that is on for a file
	include []string         // The included directories, as absolute paths
	scanFor []string         // The kinds of index the scan fills; none for no scan
}

// loadDaemonConfig reads the user's configuration. Without one the daemon
// starts on defaults and the current directory, but it does not fall back to
// them later, when the file may only be away for a moment.
func loadDaemonConfig(database *db.DB, flags scanFlags, first bool) (*daemonConfig, error) {
	_, err := os.Stat(configPath())
	missing := errors.Is(err, fs.ErrNotExist)
	if missing && !first {
		return nil, err
	}

	st, err := loadSettings()
	if err != nil {
		return nil, err
	}
	if missing {
		slog.Warn("No configuration, using defaults and the current directory", "config", configPath())
		st.profile.Include = []string{"."}
	}
	cfg := &daemonConfig{st: st}
	if cfg.idx, err = st.indexer(database); err != nil {
		return nil, err
	}
	if err := cfg.idx.Ready([]string{config.IndexEmbeddings}); err != nil {
		slog.Info("Not making embeddings, no model is configured")
	}

	seen := make(map[string]bool)
	for _, dir := range st.profile.Include {
		abs, err := config.NormalizePath(dir)
		if err != nil {
			slog.Warn("Skipping invalid include path", "dir", dir, "error", err)
			continue
		}
		if !seen[abs] {
			seen[abs] = true
			cfg.include = append(cfg.include, abs)
		}
	}

	if cfg.scanFor, err = st.profile.DaemonScan(); err != nil {
		return nil, fmt.Errorf("daemon.scan_kinds: %w", err)
	}
	if flags.kinds != nil {
		cfg.scanFor = flags.kinds
	}
	if flags.scan != nil {
		if !*flags.scan {
			cfg.scanFor = nil
		} else if cfg.scanFor == nil {
			cfg.scanFor = []string{config.IndexGraph}
		}
	}
	return cfg, nil
}

// ignores reports whether a file is one the daemon does not keep in the
// index, and whether it is for the daemon to say: it is only when the file
// is under an included directory. There it is ignored when the scan would
// not come to it, for being in a hidden or excluded directory, or would pass
// it over for the blacklist.
func (c *daemonConfig) ignores(path string) (ignored, included bool) {
	dir := filepath.Dir(path)
	walked := false
	for _, root := range c.include {
		if dir != root && !strings.HasPrefix(dir, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator)) {
			continue
		}
		included = true
		// Included directories may lie inside each other, and a file is
		// kept when the scan comes to it from any of them
		if !c.st.walk.SkipDir(root, dir) && !c.st.walk.SkipFile(root, path) {
			walked = true
			break
		}
	}
	if !included {
		return false, false
	}
	return !walked, true
}

// daemon is what the running daemon is made of
type daemon struct {
	database *db.DB
	worker   *worker.IndexWorker
	watcher  *watcher.FileWatcher
	flags    scanFlags
	current  atomic.Pointer[daemonConfig] // Replaced when the configuration is read again
}

// newDaemon reads the configuration and makes a daemon of it. Nothing runs
// yet: the worker and the watcher are to be started, and serve does the rest.
func newDaemon(database *db.DB, flags scanFlags) (*daemon, error) {
	cfg, err := loadDaemonConfig(database, flags, true)
	if err != nil {
		return nil, err
	}
	d := &daemon{database: database, flags: flags}
	d.worker = worker.NewIndexWorker(&worker.Config{
		DB:           database,
		Indexer:      cfg.idx,
		PollInterval: 5 * time.Second,
		BatchSize:    5,
		MaxRetries:   3,
	})
	d.watcher, err = watcher.New(&watcher.Config{DebounceDelay: time.Second, OnChange: d.changed})
	if err != nil {
		return nil, fmt.Errorf("failed to create watcher: %w", err)
	}
	d.use(cfg)
	return d, nil
}

// use makes a configuration the one the daemon works by
func (d *daemon) use(cfg *daemonConfig) {
	d.current.Store(cfg)
	d.worker.SetIndexer(cfg.idx)
}

// changed queues a file that was written to, for the worker to index
func (d *daemon) changed(path string) {
	cfg := d.current.Load()

	// A project may have switched a kind of index on or off
	switch filepath.Base(path) {
	case ".config.yaml":
		cfg.st.kinds.Reset()
		cfg.st.walk.Reset()
		return
	case ".gitignore":
		// What it leaves out from now on; what it left in the index goes
		// when the daemon prunes
		cfg.st.walk.Reset()
		return
	}

	// Only what can be parsed has anything to index
	if ignored, included := cfg.ignores(path); ignored || !included {
		slog.Debug("Filtered out file", "file", path)
		return
	}
	if !chunker.Recognised(path) {
		return
	}

	content, err := os.ReadFile(path)
	if err != nil {
		slog.Debug("Failed to read file for queueing", "file", path, "error", err)
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		slog.Debug("Failed to stat file for queueing", "file", path, "error", err)
		return
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	modTime := info.ModTime().UnixNano()

	if err := d.database.MarkFilePending(path, modTime, hash); err != nil {
		slog.Error("Failed to queue file", "file", path, "error", err)
		return
	}
	slog.Debug("Queued file for indexing", "file", path)
}

// serve does what the daemon does when it starts, and does it again each
// time it is asked to, until the daemon stops. One reload runs at a time:
// when another is asked for, what the running one still had to do is left,
// and is done by the next, by the configuration as it is then.
func (d *daemon) serve(ctx context.Context, again <-chan os.Signal) {
	for first := true; ; first = false {
		work, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			d.reload(work, first)
		}()

		select {
		case <-ctx.Done():
		case <-again:
		}
		stop()
		<-done
		if ctx.Err() != nil {
			return
		}
	}
}

// reload is the warm restart of the daemon: everything it does to start,
// done again without the process ending. The first time is the start
// itself, for which the configuration has just been read. What is to happen
// on SIGHUP is added here.
func (d *daemon) reload(ctx context.Context, first bool) {
	if !first {
		slog.Info("Reloading", "config", configPath())
		// A configuration that cannot be used is no reason to stop: the
		// daemon carries on with the one it has
		if cfg, err := loadDaemonConfig(d.database, d.flags, false); err != nil {
			slog.Error("Configuration not reloaded, keeping the one in use", "error", err)
		} else {
			d.use(cfg)
		}
	}
	cfg := d.current.Load()

	// Watching only shows what changes from now on. What changed while the
	// daemon was away, or was not looking, is found by scanning, once the
	// watches are set so that nothing falls between the two. Pruning comes
	// before the scan: it is quick, and the scan is not.
	roots := d.watch(ctx, cfg)
	if ctx.Err() != nil {
		return
	}
	d.prune(ctx, cfg)
	if ctx.Err() != nil {
		return
	}
	if len(cfg.scanFor) > 0 && len(roots) > 0 {
		startupScan(ctx, d.database, cfg.st, roots, cfg.scanFor)
	}
}

// watch sets the watches to the directories the configuration includes, as
// they are on disk now: directories that are new or newly included are
// watched, and ones that are gone or excluded are not any more. It returns
// the included directories that are there.
func (d *daemon) watch(ctx context.Context, cfg *daemonConfig) (roots []string) {
	want := make(map[string]bool)
	passed := 0
	for _, root := range cfg.include {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			slog.Warn("Included directory is not there", "dir", root)
			continue
		}
		roots = append(roots, root)
		filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			switch {
			case ctx.Err() != nil:
				return filepath.SkipAll
			case err != nil:
				slog.Warn("Failed to read directory", "dir", path, "error", err)
				return nil
			case !entry.IsDir():
				return nil
			case path != root && cfg.st.walk.SkipDir(root, path):
				passed++
				return filepath.SkipDir
			}
			want[path] = true
			return nil
		})
	}
	if ctx.Err() != nil {
		return roots
	}

	had := make(map[string]bool)
	added, removed, failed := 0, 0, 0
	for _, dir := range d.watcher.Watched() {
		had[dir] = true
		if want[dir] {
			continue
		}
		if err := d.watcher.Unwatch(dir); err != nil {
			slog.Warn("Failed to stop watching directory", "dir", dir, "error", err)
		}
		removed++
	}
	for dir := range want {
		if had[dir] {
			continue
		}
		if err := d.watcher.Watch(dir); err != nil {
			failed++
			slog.Warn("Failed to watch directory", "dir", dir, "error", err)
			continue
		}
		added++
	}

	if len(want) == 0 {
		slog.Warn("No directories are watched, name them under include in the configuration", "config", configPath())
	} else {
		slog.Info("Watching directories", "include", roots, "watched", len(want)-failed, "added", added, "removed", removed, "excluded", passed, "failed", failed)
	}
	return roots
}

// prune makes the index forget the files the configuration ignores, and
// says what it did
func (d *daemon) prune(ctx context.Context, cfg *daemonConfig) {
	start := time.Now()
	stats, err := pruneIgnored(ctx, d.database, cfg)
	result := []any{"took", time.Since(start).Round(time.Millisecond), "checked", stats.checked, "pruned", stats.pruned}
	switch {
	case errors.Is(err, context.Canceled):
		slog.Info("Prune stopped", result...)
	case err != nil:
		slog.Error("Prune failed", append(result, "error", err)...)
	default:
		slog.Info("Prune done", result...)
	}
}

type pruneStats struct {
	checked int // Files of the index that were looked at
	pruned  int // Files it was made to forget
}

// pruneIgnored makes the index forget the files under the included
// directories that the configuration ignores: files in a directory that is
// excluded or hidden, and files the blacklist rejects. The scan only forgets
// what is gone from disk, so without this a directory that is excluded after
// it was indexed stays in the index for ever. Nothing on disk is changed.
//
// What the index holds outside the included directories is left alone: it
// was indexed by hand, and is not the daemon's to judge. A file under them
// that was indexed by name from a directory that is excluded is forgotten,
// as there is no telling it from one that was indexed before the directory
// was excluded.
func pruneIgnored(ctx context.Context, database *db.DB, cfg *daemonConfig) (stats pruneStats, err error) {
	seen := make(map[string]bool) // Included directories may lie inside each other
	for _, root := range cfg.include {
		indexed, err := database.IndexedFiles(root)
		if err != nil {
			return stats, err
		}
		for path, f := range indexed {
			if seen[path] {
				continue
			}
			seen[path] = true
			if ctx.Err() != nil {
				return stats, ctx.Err()
			}
			stats.checked++
			if ignored, _ := cfg.ignores(path); !ignored {
				continue
			}
			if err := cfg.idx.ForgetFile(f.ID, path); err != nil {
				return stats, err
			}
			stats.pruned++
			slog.Debug("Ignored file pruned", "file", path)
		}
	}
	return stats, nil
}

// startupScan brings the index up to date with the included directories, as
// "chainsaw index" would: new and modified files are indexed and the index
// forgets files that are gone. It works beside the watcher and stops between
// two files when the daemon does.
func startupScan(ctx context.Context, database *db.DB, st *settings, roots []string, kinds []string) {
	slog.Info("Scan started", "kinds", kinds, "dirs", roots)
	start := time.Now()
	stats, err := syncIndex(database, st, roots, syncOptions{ctx: ctx, only: kinds, log: slog.Default()})
	result := []any{
		"took", time.Since(start).Round(time.Millisecond), "indexed", stats.indexed, "unchanged", stats.unchanged,
		"removed", stats.removed, "failed", stats.failed,
	}
	switch {
	case errors.Is(err, context.Canceled):
		slog.Info("Scan stopped", append(result, "to_go", stats.pending)...)
	case errors.Is(err, indexer.ErrNoEmbedder):
		slog.Warn("Scan made no embeddings, no model is configured")
	case err != nil:
		slog.Error("Scan failed", append(result, "error", err)...)
	default:
		slog.Info("Scan done", result...)
	}
}

func handleDaemonStatus() {
	handleStatus()
}
