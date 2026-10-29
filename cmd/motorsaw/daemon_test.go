package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/wouteroostervld/motorsaw/pkg/db"
)

// daemonProject is project with more in it to ignore, and a tree beside it
// that is not included
func daemonProject(t *testing.T) (dir, outside string, database *db.DB) {
	t.Helper()
	dir, database = project(t)
	outside = t.TempDir()
	for path, content := range map[string]string{
		filepath.Join(dir, "third/t.py"):          "def t():\n    return 1\n",
		filepath.Join(dir, "third/lib/l.py"):      "def l():\n    return 1\n",
		filepath.Join(dir, "sub/x_gen.py"):        "def x():\n    return 1\n",
		filepath.Join(dir, "sub/keep_gen.py"):     "def k():\n    return 1\n",
		filepath.Join(outside, "third/o.py"):      "def o():\n    return 1\n",
		filepath.Join(outside, ".hidden/p.py"):    "def p():\n    return 1\n",
		filepath.Join(outside, "sub/also_gen.py"): "def q():\n    return 1\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, outside, database
}

// configureDaemon writes the user's configuration, with the given settings
// of the profile; project must have been called
func configureDaemon(t *testing.T, include []string, settings string) {
	t.Helper()
	content := "version: \"3\"\nactive_profile: default\nprofiles:\n  default:\n    include:\n"
	for _, dir := range include {
		content += "      - " + dir + "\n"
	}
	writeConfiguration(t, content+settings)
}

func writeConfiguration(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".motorsaw")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// indexed returns the files the index holds, as paths below the given
// directories, sorted
func indexed(t *testing.T, database *db.DB, dirs ...string) []string {
	t.Helper()
	files := []string{}
	for _, dir := range dirs {
		held, err := database.IndexedFiles(dir)
		if err != nil {
			t.Fatal(err)
		}
		for path := range held {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, rel)
		}
	}
	sort.Strings(files)
	return files
}

// everyFile returns the regular files of a tree, hidden and excluded ones too
func everyFile(t *testing.T, dirs ...string) []string {
	t.Helper()
	var files []string
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				files = append(files, path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func TestPruneIgnored(t *testing.T) {
	dir, outside, database := daemonProject(t)

	// Every file is in the index, as when it was indexed by name or before
	// the configuration ignored it
	configureDaemon(t, []string{dir}, "")
	syncAll(t, database, everyFile(t, dir, outside), syncOptions{})
	if got := len(indexed(t, database, dir)) + len(indexed(t, database, outside)); got != 11 {
		t.Fatalf("%d files indexed, want 11", got)
	}
	before := definitions(t, database)

	configureDaemon(t, []string{dir}, `    exclude: [node_modules, third/]
    blacklist: ['_gen\.py$']
    whitelist: ['keep_gen\.py$']
`)
	cfg, err := loadDaemonConfig(database, scanFlags{}, false)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := pruneIgnored(context.Background(), database, cfg)
	if err != nil {
		t.Fatalf("pruneIgnored failed: %v", err)
	}
	// Forgotten: node_modules/d/d.js and third/t.py and third/lib/l.py for
	// the excluded directory, .hidden/c.py for the hidden one, sub/x_gen.py
	// for the blacklist
	if stats != (pruneStats{checked: 8, pruned: 5}) {
		t.Errorf("prune = %+v, want 5 of 8 pruned", stats)
	}
	if got, want := indexed(t, database, dir), []string{"a.py", "sub/b.py", "sub/keep_gen.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("under the included directory the index holds %q, want %q", got, want)
	}
	// With the file goes what the index had of it: a module and a function each
	if after := definitions(t, database); after != before-10 {
		t.Errorf("%d definitions after pruning, want %d", after, before-10)
	}

	// What was indexed by hand outside the included directories stays,
	// whatever the configuration would say of it
	if got, want := indexed(t, database, outside), []string{".hidden/p.py", "sub/also_gen.py", "third/o.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("outside the included directories the index holds %q, want %q", got, want)
	}

	// The scan and the prune agree: neither undoes what the other did
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{unchanged: 3}) {
		t.Errorf("scan after pruning = %+v, want 3 unchanged", got)
	}
	if stats, err := pruneIgnored(context.Background(), database, cfg); err != nil || stats != (pruneStats{checked: 3}) {
		t.Errorf("second prune = %+v, %v, want nothing pruned of 3", stats, err)
	}

	// No file on disk was touched
	if got := len(everyFile(t, dir, outside)); got != 12 {
		t.Errorf("%d files on disk, want 12", got)
	}
}

func TestPruneIgnored_Stopped(t *testing.T) {
	dir, _, database := daemonProject(t)
	configureDaemon(t, []string{dir}, "")
	syncAll(t, database, everyFile(t, dir), syncOptions{})

	configureDaemon(t, []string{dir}, "    exclude: [third]\n")
	cfg, err := loadDaemonConfig(database, scanFlags{}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if stats, err := pruneIgnored(ctx, database, cfg); err != context.Canceled || stats.pruned != 0 {
		t.Errorf("stopped prune = %+v, %v, want nothing pruned and context.Canceled", stats, err)
	}
}

func TestDaemonConfig_Ignores(t *testing.T) {
	dir, outside, database := daemonProject(t)
	// An included directory inside an excluded one is included all the same
	configureDaemon(t, []string{dir, filepath.Join(dir, "third/lib")}, `    exclude: [third, `+filepath.Join(dir, "sub")+`/]
    blacklist: ['a\.py$']
`)
	cfg, err := loadDaemonConfig(database, scanFlags{}, false)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path              string
		ignored, included bool
	}{
		{filepath.Join(dir, "top.py"), false, true},
		{filepath.Join(dir, "a.py"), true, true}, // Blacklisted
		{filepath.Join(dir, "third/t.py"), true, true},
		{filepath.Join(dir, "third/lib/l.py"), false, true},
		{filepath.Join(dir, "third/lib/third/l.py"), true, true},
		{filepath.Join(dir, "sub/b.py"), true, true}, // Excluded by its absolute path
		{filepath.Join(dir, "subs/b.py"), false, true},
		{filepath.Join(dir, ".hidden/c.py"), true, true},
		{filepath.Join(dir, "pkg/.cache/x/c.py"), true, true},
		{filepath.Join(dir, ".hidden.py"), false, true}, // Only directories hide
		{filepath.Join(outside, "third/o.py"), false, false},
		{dir + "2/a.py", false, false},
	}
	for _, tt := range tests {
		if ignored, included := cfg.ignores(tt.path); ignored != tt.ignored || included != tt.included {
			t.Errorf("ignores(%s) = %v, %v, want %v, %v", tt.path, ignored, included, tt.ignored, tt.included)
		}
	}
}

func TestLoadDaemonConfig_Scan(t *testing.T) {
	_, database := project(t)
	on, off := true, false
	tests := []struct {
		name     string
		settings string
		flags    scanFlags
		want     []string
	}{
		{"default", "", scanFlags{}, []string{"graph"}},
		{"switched off", "    daemon: {scan: false}\n", scanFlags{}, nil},
		{"switched off, flag on", "    daemon: {scan: false}\n", scanFlags{scan: &on}, []string{"graph"}},
		{"flag off", "", scanFlags{scan: &off}, nil},
		{"kinds", "    daemon: {scan_kinds: [graph, embeddings]}\n", scanFlags{}, []string{"graph", "embeddings"}},
		{"kinds by flag", "    daemon: {scan_kinds: [graph, embeddings]}\n", scanFlags{kinds: []string{"embeddings"}}, []string{"embeddings"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configureDaemon(t, nil, tt.settings)
			cfg, err := loadDaemonConfig(database, tt.flags, false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.scanFor, tt.want) {
				t.Errorf("scanFor = %v, want %v", cfg.scanFor, tt.want)
			}
		})
	}

	configureDaemon(t, nil, "    daemon: {scan_kinds: [nonsense]}\n")
	if _, err := loadDaemonConfig(database, scanFlags{}, false); err == nil {
		t.Error("a kind of index that does not exist was accepted")
	}
}

// watched returns the watched directories as paths below dir, sorted
func watched(t *testing.T, d *daemon, dir string) []string {
	t.Helper()
	dirs := []string{}
	for _, path := range d.watcher.Watched() {
		rel, _ := filepath.Rel(dir, path)
		dirs = append(dirs, rel)
	}
	sort.Strings(dirs)
	return dirs
}

func testDaemon(t *testing.T, database *db.DB) *daemon {
	t.Helper()
	d, err := newDaemon(database, scanFlags{})
	if err != nil {
		t.Fatalf("newDaemon failed: %v", err)
	}
	t.Cleanup(func() { d.watcher.Close() })
	return d
}

func TestDaemonReload(t *testing.T) {
	dir, database := project(t)
	configureDaemon(t, []string{dir}, "    exclude: [node_modules]\n")
	d := testDaemon(t, database)
	ctx := context.Background()

	// The start: the included directories are watched and scanned
	d.reload(ctx, true)
	if got, want := watched(t, d, dir), []string{".", "sub"}; !reflect.DeepEqual(got, want) {
		t.Errorf("watching %q after the start, want %q", got, want)
	}
	if got, want := indexed(t, database, dir), []string{"a.py", "sub/b.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("index holds %q after the start, want %q", got, want)
	}

	// A directory is excluded and another is made: the watches follow, the
	// index forgets the one and takes in the other
	extra := filepath.Join(dir, "extra")
	if err := os.Mkdir(extra, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "e.py"), []byte("def e():\n    return 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configureDaemon(t, []string{dir}, "    exclude: [node_modules, sub/]\n")
	d.reload(ctx, false)
	if got, want := watched(t, d, dir), []string{".", "extra"}; !reflect.DeepEqual(got, want) {
		t.Errorf("watching %q after the reload, want %q", got, want)
	}
	if got, want := indexed(t, database, dir), []string{"a.py", "extra/e.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("index holds %q after the reload, want %q", got, want)
	}

	// A file that changes in the excluded directory is not queued any more
	d.changed(filepath.Join(dir, "sub/b.py"))
	d.changed(filepath.Join(dir, "notes.txt"))
	if got, want := indexed(t, database, dir), []string{"a.py", "extra/e.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("index holds %q after a change in an excluded directory, want %q", got, want)
	}

	// A configuration that cannot be used, or is gone, changes nothing
	inUse := d.current.Load()
	for name, content := range map[string]string{
		"not YAML":        "profiles: [\n",
		"no such profile": "version: \"3\"\nactive_profile: other\nprofiles:\n  default: {}\n",
		"unknown kind":    "version: \"3\"\nactive_profile: default\nprofiles:\n  default:\n    index: {nonsense: true}\n",
	} {
		writeConfiguration(t, content)
		d.reload(ctx, false)
		if d.current.Load() != inUse {
			t.Errorf("%s: the configuration in use was replaced", name)
		}
	}
	if err := os.Remove(configPath()); err != nil {
		t.Fatal(err)
	}
	d.reload(ctx, false)
	if d.current.Load() != inUse {
		t.Error("no configuration: the configuration in use was replaced")
	}
	if got, want := watched(t, d, dir), []string{".", "extra"}; !reflect.DeepEqual(got, want) {
		t.Errorf("watching %q after reloads that failed, want %q", got, want)
	}
	if got, want := indexed(t, database, dir), []string{"a.py", "extra/e.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("index holds %q after reloads that failed, want %q", got, want)
	}

	// An included directory that is dropped is not watched any more, and
	// what the index has of it stays
	configureDaemon(t, nil, "")
	d.reload(ctx, false)
	if got := d.watcher.Watched(); len(got) != 0 {
		t.Errorf("watching %q with nothing included", got)
	}
	if got, want := indexed(t, database, dir), []string{"a.py", "extra/e.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("index holds %q with nothing included, want %q", got, want)
	}
}

func TestDaemonReload_NoScan(t *testing.T) {
	dir, database := project(t)
	configureDaemon(t, []string{dir}, "    exclude: [node_modules]\n    daemon: {scan: false}\n")
	d := testDaemon(t, database)

	// Without the scan the watches are set and nothing is indexed
	d.reload(context.Background(), true)
	if got, want := watched(t, d, dir), []string{".", "sub"}; !reflect.DeepEqual(got, want) {
		t.Errorf("watching %q, want %q", got, want)
	}
	if got := indexed(t, database, dir); len(got) != 0 {
		t.Errorf("index holds %q, want nothing", got)
	}

	// A file that changes is queued, when it is one the daemon keeps
	for _, name := range []string{"a.py", ".hidden/c.py", "node_modules/d/d.js"} {
		d.changed(filepath.Join(dir, name))
	}
	if got, want := indexed(t, database, dir), []string{"a.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("queued %q, want %q", got, want)
	}
}

// eventually waits for something another goroutine is to bring about
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDaemonServe(t *testing.T) {
	dir, database := project(t)
	configureDaemon(t, []string{dir}, "    exclude: [node_modules]\n")
	d := testDaemon(t, database)

	ctx, cancel := context.WithCancel(context.Background())
	hup := make(chan os.Signal, 1)
	served := make(chan struct{})
	go func() {
		defer close(served)
		d.serve(ctx, hup)
	}()

	eventually(t, "the scan at the start", func() bool {
		return reflect.DeepEqual(indexed(t, database, dir), []string{"a.py", "sub/b.py"})
	})

	// Each signal is a reload, also when they come faster than it is done
	configureDaemon(t, []string{dir}, "    exclude: [node_modules, sub]\n")
	hup <- syscall.SIGHUP
	hup <- syscall.SIGHUP
	eventually(t, "the reload", func() bool {
		return reflect.DeepEqual(indexed(t, database, dir), []string{"a.py"}) &&
			reflect.DeepEqual(watched(t, d, dir), []string{"."})
	})

	configureDaemon(t, []string{dir}, "    exclude: [node_modules]\n")
	hup <- syscall.SIGHUP
	eventually(t, "the second reload", func() bool {
		return reflect.DeepEqual(indexed(t, database, dir), []string{"a.py", "sub/b.py"}) &&
			reflect.DeepEqual(watched(t, d, dir), []string{".", "sub"})
	})

	cancel()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}
