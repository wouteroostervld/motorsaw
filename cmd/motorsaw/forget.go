package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wouteroostervld/motorsaw/pkg/db"
)

const forgetUsage = `Usage: motorsaw forget [options] <path>...

Makes the index forget files: each file named, and every file below each
directory named. Their definitions, calls and embeddings leave the index.
Nothing on disk is touched, and "motorsaw index" brings back what it is
asked for.

This is for what should not have been indexed: a directory of tools or
build output that was indexed before it was excluded or ignored. A path
does not have to exist any more.

Options:
  --ignored        Of the files below the paths, only those that a walk would
                   leave out now: in a hidden or excluded directory, ignored
                   by git, or rejected by the blacklist. With no path, the
                   current directory.
  --no-ignore      With --ignored, do not count what git ignores
  -n, --dry-run    Say what would be forgotten, and forget nothing

Examples:
  motorsaw forget tools build .venv
  motorsaw forget --ignored            # what .gitignore and the excludes leave out here
  motorsaw forget -n --ignored ~/Projects`

func handleForget(args []string) {
	flags := flag.NewFlagSet("forget", flag.ExitOnError)
	flags.Usage = func() { fmt.Println(forgetUsage) }
	ignored := flags.Bool("ignored", false, "")
	noIgnore := flags.Bool("no-ignore", false, "")
	dryRun := flags.Bool("dry-run", false, "")
	flags.BoolVar(dryRun, "n", false, "")
	given := parse(flags, args)

	if len(given) == 0 && *ignored {
		given = []string{"."}
	}
	if len(given) == 0 {
		fmt.Println(forgetUsage)
		os.Exit(1)
	}
	var roots []string
	for _, path := range given {
		abs, err := filepath.Abs(path)
		if err != nil {
			fail("%v", err)
		}
		roots = append(roots, abs)
	}

	st, err := loadSettings()
	if err != nil {
		fail("%v", err)
	}
	if *noIgnore {
		st.walk = newWalk(st.profile, false)
	}
	database := openDB()
	defer database.Close()

	start := time.Now()
	stats, err := forget(database, st, roots, forgetOptions{ignored: *ignored, dryRun: *dryRun, progress: os.Stdout})
	if err != nil {
		fail("%v", err)
	}
	verb := "Forgot"
	if *dryRun {
		verb = "Would forget"
	}
	fmt.Printf("%s %d files in %v", verb, stats.forgotten, time.Since(start).Round(time.Millisecond))
	if *ignored {
		fmt.Printf(", %d kept", stats.kept)
	}
	fmt.Println()
	if *dryRun {
		for _, dir := range stats.dirs() {
			fmt.Printf("  %6d  %s\n", stats.byDir[dir], displayPath(dir, workingDir()))
		}
	}
}

type forgetOptions struct {
	ignored  bool      // Only what a walk from the root would leave out
	dryRun   bool      // Forget nothing
	progress io.Writer // Where to report progress on a long run; nil for silence
}

type forgetStats struct {
	forgotten int
	kept      int            // Files below the paths that stay in the index
	byDir     map[string]int // Of a dry run: how many files would go, by the directory below the root they are in
}

// dirs returns the directories of a dry run, the one with most files first
func (s forgetStats) dirs() []string {
	dirs := make([]string, 0, len(s.byDir))
	for dir := range s.byDir {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if s.byDir[dirs[i]] != s.byDir[dirs[j]] {
			return s.byDir[dirs[i]] > s.byDir[dirs[j]]
		}
		return dirs[i] < dirs[j]
	})
	return dirs
}

// forget makes the index forget the files at and below the given paths, or
// of those only the ones a walk from that path would leave out. Nothing on
// disk is changed.
func forget(database *db.DB, st *settings, roots []string, opts forgetOptions) (stats forgetStats, err error) {
	// The references to what goes are resolved again once, not once a file
	resolve := database.DeferResolving(resolveEvery)
	defer func() {
		resolveErr := resolve()
		if db.IsBusy(resolveErr) {
			resolveErr = syncOptions{progress: opts.progress, patience: indexPatience}.write(database.ResolveSymbols)
		}
		if err == nil {
			err = resolveErr
		}
	}()

	idx, err := st.indexer(database)
	if err != nil {
		return stats, err
	}
	stats.byDir = make(map[string]int)
	seen := make(map[string]bool)
	reported := time.Now()
	for _, root := range roots {
		indexed, err := database.IndexedFiles(root)
		if err != nil {
			return stats, err
		}
		paths := make([]string, 0, len(indexed))
		for path := range indexed {
			paths = append(paths, path)
		}
		sort.Strings(paths)

		for _, path := range paths {
			if seen[path] {
				continue
			}
			seen[path] = true
			if opts.ignored && !(st.walk.SkipDir(root, filepath.Dir(path)) || st.walk.SkipFile(root, path)) {
				stats.kept++
				continue
			}
			stats.forgotten++
			if opts.dryRun {
				stats.byDir[topDir(root, path)]++
				continue
			}
			// Another motorsaw may be writing: the daemon in its scan
			wait := syncOptions{progress: opts.progress, patience: indexPatience}
			if err := wait.write(func() error { return idx.ForgetFile(indexed[path].ID, path) }); err != nil {
				return stats, err
			}
			if opts.progress != nil && time.Since(reported) > 5*time.Second {
				fmt.Fprintf(opts.progress, "  forgot %d files...\n", stats.forgotten)
				reported = time.Now()
			}
		}
	}
	return stats, nil
}

// topDir returns the directory directly below root that a file is in, or
// root itself for a file that is directly in it
func topDir(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return root
	}
	first, _, below := strings.Cut(rel, string(filepath.Separator))
	if !below {
		return root
	}
	return filepath.Join(root, first)
}
