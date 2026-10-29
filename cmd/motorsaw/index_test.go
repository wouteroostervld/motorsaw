package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wouteroostervld/motorsaw/pkg/db"
	"github.com/wouteroostervld/motorsaw/pkg/indexer"
)

// project writes a small source tree and returns its directory and an index
func project(t *testing.T) (string, *db.DB) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // No user configuration: default excludes

	dir := t.TempDir()
	for name, content := range map[string]string{
		"a.py":                "def a():\n    return b()\n",
		"sub/b.py":            "def b():\n    return 1\n",
		"notes.txt":           "not source\n",
		".hidden/c.py":        "def c():\n    return 1\n",
		"node_modules/d/d.js": "function d() { return 1; }\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	database, err := db.Open(db.Config{Path: filepath.Join(t.TempDir(), "graph.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return dir, database
}

func sync(t *testing.T, database *db.DB, root string, opts syncOptions) syncStats {
	t.Helper()
	return syncAll(t, database, []string{root}, opts)
}

func syncAll(t *testing.T, database *db.DB, roots []string, opts syncOptions) syncStats {
	t.Helper()
	st, err := loadSettings()
	if err != nil {
		t.Fatalf("loadSettings failed: %v", err)
	}
	stats, err := syncIndex(database, st, roots, opts)
	if err != nil {
		t.Fatalf("syncIndex failed: %v", err)
	}
	return stats
}

func definitions(t *testing.T, database *db.DB) int64 {
	t.Helper()
	counts, err := database.Count()
	if err != nil {
		t.Fatal(err)
	}
	return counts.Definitions
}

func TestSyncIndex(t *testing.T) {
	dir, database := project(t)

	// Source files only, and not from hidden or excluded directories
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{indexed: 2}) {
		t.Fatalf("first run = %+v, want 2 indexed", got)
	}
	// Two modules and two functions
	if n := definitions(t, database); n != 4 {
		t.Errorf("%d definitions after the first run, want 4", n)
	}

	// Nothing changed: nothing to do
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{unchanged: 2}) {
		t.Errorf("second run = %+v, want 2 unchanged", got)
	}

	// A modified file is indexed again, and only that one
	path := filepath.Join(dir, "a.py")
	if err := os.WriteFile(path, []byte("def a():\n    return 1\n\ndef a2():\n    return 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{indexed: 1, unchanged: 1}) {
		t.Errorf("after an edit = %+v, want 1 indexed and 1 unchanged", got)
	}
	if n := definitions(t, database); n != 5 {
		t.Errorf("%d definitions after the edit, want 5", n)
	}

	// A deleted file leaves the index
	if err := os.Remove(filepath.Join(dir, "sub", "b.py")); err != nil {
		t.Fatal(err)
	}
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{unchanged: 1, removed: 1}) {
		t.Errorf("after a delete = %+v, want 1 unchanged and 1 removed", got)
	}
	if n := definitions(t, database); n != 3 {
		t.Errorf("%d definitions after the delete, want 3", n)
	}

	// Forcing indexes what has not changed
	if got := sync(t, database, dir, syncOptions{force: true}); got != (syncStats{indexed: 1}) {
		t.Errorf("forced = %+v, want 1 indexed", got)
	}
}

func TestSyncIndex_NoExtension(t *testing.T) {
	dir, database := project(t)

	// BCPL from before files had extensions is told by what is in it; other
	// files without one are passed over
	for name, content := range map[string]string{
		"bcp/mcg1": "SECTION \"MCG1\"\n\nLET start() BE\n$( stop(0)\n$)\n",
		"Makefile": "all:\n\techo $(CC)\n",
		"LICENSE":  "Do as you like.\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{indexed: 3}) {
		t.Fatalf("first run = %+v, want a.py, sub/b.py and bcp/mcg1 indexed", got)
	}
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{unchanged: 3}) {
		t.Errorf("second run = %+v, want 3 unchanged", got)
	}
	// Named by hand it is taken the same way
	if got := sync(t, database, filepath.Join(dir, "LICENSE"), syncOptions{}); got != (syncStats{skipped: 1}) {
		t.Errorf("a text file named by hand = %+v, want it passed over", got)
	}
}

func TestSyncOptions_Write(t *testing.T) {
	busy := errors.New("failed to insert entity: database is locked")

	// Another process is writing: asked again until it is done
	tries := 0
	var told bytes.Buffer
	opts := syncOptions{patience: time.Minute, progress: &told}
	err := opts.write(func() error {
		if tries++; tries < 3 {
			return busy
		}
		return nil
	})
	if err != nil || tries != 3 {
		t.Errorf("write = %v after %d tries, want it done at the third", err, tries)
	}
	if got := strings.Count(told.String(), "waiting"); got != 1 {
		t.Errorf("said %d times that it waits, want once:\n%s", got, told.String())
	}

	// Not for ever
	tries = 0
	if err := (syncOptions{}).write(func() error { tries++; return busy }); !db.IsBusy(err) || tries != 2 {
		t.Errorf("write without patience = %v after %d tries, want the error after one more try", err, tries)
	}
	// And any other error is not waited for
	tries = 0
	other := errors.New("no such file")
	if err := opts.write(func() error { tries++; return other }); err != other || tries != 1 {
		t.Errorf("write = %v after %d tries, want the error at once", err, tries)
	}
}

func TestSyncIndex_Limit(t *testing.T) {
	dir, database := project(t)

	// More to do than the limit allows: leave it all, and say how much
	if got := sync(t, database, dir, syncOptions{limit: 1}); got != (syncStats{pending: 2}) {
		t.Errorf("over the limit = %+v, want 2 pending", got)
	}
	if n := definitions(t, database); n != 0 {
		t.Errorf("%d definitions indexed despite the limit", n)
	}

	// Within the limit it goes ahead
	if got := sync(t, database, dir, syncOptions{limit: 2}); got != (syncStats{indexed: 2}) {
		t.Errorf("within the limit = %+v, want 2 indexed", got)
	}
}

func TestSyncIndex_SingleFile(t *testing.T) {
	dir, database := project(t)

	if got := sync(t, database, filepath.Join(dir, "sub", "b.py"), syncOptions{}); got != (syncStats{indexed: 1}) {
		t.Errorf("single file = %+v, want 1 indexed", got)
	}

	// Syncing one directory leaves what was indexed elsewhere alone
	if got := sync(t, database, filepath.Join(dir, "sub"), syncOptions{}); got != (syncStats{unchanged: 1}) {
		t.Errorf("its directory = %+v, want 1 unchanged", got)
	}
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{indexed: 1, unchanged: 1}) {
		t.Errorf("the whole tree = %+v, want 1 indexed and 1 unchanged", got)
	}
}

func TestSyncIndex_SeveralPaths(t *testing.T) {
	dir, database := project(t)

	// Files and directories together, as a pipeline would pass them. A file
	// given twice, or also found under a directory, is indexed once. A file
	// named directly is indexed even from a directory that is otherwise
	// skipped, and one that is not source is passed over.
	roots := []string{
		filepath.Join(dir, "a.py"),
		filepath.Join(dir, "sub"),
		filepath.Join(dir, "sub", "b.py"),
		filepath.Join(dir, ".hidden", "c.py"),
		filepath.Join(dir, "notes.txt"),
		filepath.Join(dir, "a.py"),
	}
	if got := syncAll(t, database, roots, syncOptions{}); got != (syncStats{indexed: 3, skipped: 1}) {
		t.Errorf("several paths = %+v, want 3 indexed and 1 skipped", got)
	}
	if got := syncAll(t, database, roots, syncOptions{}); got != (syncStats{unchanged: 3, skipped: 1}) {
		t.Errorf("again = %+v, want 3 unchanged and 1 skipped", got)
	}

	// A file indexed by name stays when its directory is synced later, even
	// though the walk skips it
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{unchanged: 2}) {
		t.Errorf("the tree afterwards = %+v, want 2 unchanged and nothing removed", got)
	}

	// Naming files does not remove others from the index
	if got := syncAll(t, database, roots[:1], syncOptions{}); got != (syncStats{unchanged: 1}) {
		t.Errorf("one file = %+v, want 1 unchanged", got)
	}
	if n := definitions(t, database); n != 6 {
		t.Errorf("%d definitions, want 6", n)
	}
}

// embeddingServer stands in for a model: it answers in the Ollama protocol
// and counts the texts it was asked to embed
func embeddingServer(t *testing.T, texts *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		vectors := make([][]float32, len(req.Input))
		for i, text := range req.Input {
			*texts++
			vectors[i] = []float32{float32(len(text)), 1, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	t.Cleanup(server.Close)
	return server
}

// configure writes the user's configuration; project must have been called
func configure(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".motorsaw")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	content = "version: \"3\"\nactive_profile: default\nprofiles:\n  default:\n    exclude: [node_modules]\n" + content
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func held(t *testing.T, database *db.DB) map[string]int64 {
	t.Helper()
	counts, err := database.CountFilesByModule()
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestSyncIndex_EmbeddingsOffByDefault(t *testing.T) {
	dir, database := project(t)
	asked := 0
	server := embeddingServer(t, &asked)

	// A model alone switches nothing on
	configure(t, "    embeddings:\n      base_url: "+server.URL+"\n      model: m\n")
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{indexed: 2}) {
		t.Fatalf("first run = %+v, want 2 indexed", got)
	}
	if asked != 0 || held(t, database)["embeddings"] != 0 || held(t, database)["graph"] != 2 {
		t.Errorf("model asked for %d texts, index holds %v", asked, held(t, database))
	}
}

func TestSyncIndex_EmbeddingsPerProject(t *testing.T) {
	dir, database := project(t)
	asked := 0
	server := embeddingServer(t, &asked)
	configure(t, "    embeddings:\n      base_url: "+server.URL+"\n      model: m\n")
	sync(t, database, dir, syncOptions{})

	// One directory switches embeddings on: its files are embedded, the
	// graph is not built again, and the rest is left as it is
	local := filepath.Join(dir, "sub", ".config.yaml")
	if err := os.WriteFile(local, []byte("index:\n  embeddings: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := definitions(t, database)
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{indexed: 1, unchanged: 1}) {
		t.Fatalf("after switching on = %+v, want 1 indexed and 1 unchanged", got)
	}
	if asked == 0 || held(t, database)["embeddings"] != 1 || definitions(t, database) != before {
		t.Errorf("model asked for %d texts, index holds %v, %d definitions (was %d)", asked, held(t, database), definitions(t, database), before)
	}
	hits, err := database.SearchEmbeddings([]float32{1, 1, 0}, 5, dir)
	if err != nil || len(hits) == 0 || hits[0].Path != filepath.Join(dir, "sub", "b.py") {
		t.Errorf("search = %+v, %v", hits, err)
	}

	// A run for the graph alone does not ask the model, even after an edit
	path := filepath.Join(dir, "sub", "b.py")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(content, "\n# edited\n"...), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	asked = 0
	if got := sync(t, database, dir, syncOptions{only: []string{"graph"}}); got != (syncStats{indexed: 1, unchanged: 1}) {
		t.Errorf("graph only = %+v, want 1 indexed and 1 unchanged", got)
	}
	if asked != 0 {
		t.Errorf("a run for the graph asked the model for %d texts", asked)
	}
	// What that left stale is caught up afterwards
	if got := sync(t, database, dir, syncOptions{only: []string{"embeddings"}}); got != (syncStats{indexed: 1, unchanged: 1}) || asked == 0 {
		t.Errorf("embeddings afterwards = %+v with %d texts, want 1 indexed", got, asked)
	}

	// Switching off drops what was held, and nothing else
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{indexed: 1, unchanged: 1}) {
		t.Errorf("after switching off = %+v, want 1 indexed and 1 unchanged", got)
	}
	if n, _ := database.CountEmbeddings(); n != 0 || held(t, database)["embeddings"] != 0 || definitions(t, database) != before {
		t.Errorf("%d embeddings left, index holds %v, %d definitions", n, held(t, database), definitions(t, database))
	}
}

func TestSyncIndex_EmbeddingsWithoutModel(t *testing.T) {
	dir, database := project(t)
	configure(t, "    index:\n      embeddings: true\n")

	// Said once, before anything is indexed
	st, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncIndex(database, st, []string{dir}, syncOptions{}); !errors.Is(err, indexer.ErrNoEmbedder) {
		t.Fatalf("err = %v, want ErrNoEmbedder", err)
	}
	// What needs no model still works
	if got := sync(t, database, dir, syncOptions{only: []string{"graph"}}); got != (syncStats{indexed: 2}) {
		t.Errorf("graph only = %+v, want 2 indexed", got)
	}
}

func TestSyncIndex_ModelChanged(t *testing.T) {
	dir, database := project(t)
	asked := 0
	server := embeddingServer(t, &asked)
	model := func(name string) {
		configure(t, "    index:\n      embeddings: true\n    embeddings:\n      base_url: "+server.URL+"\n      model: "+name+"\n")
	}

	model("one")
	sync(t, database, dir, syncOptions{})
	if have, _, _ := database.EmbeddingModel(); have != "one" {
		t.Fatalf("model = %q, want one", have)
	}

	// In passing nothing is thrown away; when asked for, all is made again
	model("two")
	if got := sync(t, database, dir, syncOptions{limit: 10}); got != (syncStats{unchanged: 2}) {
		t.Errorf("in passing = %+v, want 2 unchanged", got)
	}
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{indexed: 2}) {
		t.Errorf("asked for = %+v, want 2 indexed", got)
	}
	if have, _, _ := database.EmbeddingModel(); have != "two" {
		t.Errorf("model = %q, want two", have)
	}
}

func TestSyncIndex_ModelAway(t *testing.T) {
	dir, database := project(t)
	for _, name := range []string{"c.py", "d.py", "e.py", "f.py", "g.py"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("def f():\n    return 1\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	configure(t, "    index:\n      embeddings: true\n    embeddings:\n      base_url: "+server.URL+"\n      model: m\n")

	// Not a line for each of thousands of files
	st, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	stats, err := syncIndex(database, st, []string{dir}, syncOptions{})
	if err == nil || stats.failed != 3 {
		t.Fatalf("stats = %+v, err = %v, want a stop after 3 failures", stats, err)
	}
	// The graph of the files it got to is kept
	if held(t, database)["graph"] != 3 {
		t.Errorf("index holds %v, want the graph of 3 files", held(t, database))
	}
}

func TestLoadSettings_BrokenConfiguration(t *testing.T) {
	project(t)
	dir := filepath.Join(os.Getenv("HOME"), ".motorsaw")
	os.MkdirAll(dir, 0700)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("profiles: [not, a, map\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Defaults would quietly switch off what the configuration switches on
	if _, err := loadSettings(); err == nil {
		t.Error("a configuration that cannot be read was replaced by defaults")
	}

	configure(t, "    index:\n      vibes: true\n")
	if _, err := loadSettings(); err == nil {
		t.Error("an unknown kind of index was accepted")
	}
}

func TestSyncIndex_Stopped(t *testing.T) {
	dir, database := project(t)
	st, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}

	// A run that is told to stop indexes nothing more and says what is left
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats, err := syncIndex(database, st, []string{dir}, syncOptions{ctx: ctx})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if stats.indexed != 0 || stats.pending != 2 {
		t.Errorf("stopped run = %+v, want nothing indexed and 2 to go", stats)
	}

	// The next run picks up where it stopped
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{indexed: 2}) {
		t.Errorf("run after a stopped one = %+v, want 2 indexed", got)
	}
}

func TestStartupScan(t *testing.T) {
	dir, database := project(t)
	st, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}

	startupScan(context.Background(), database, st, []string{dir}, []string{"graph"})
	if n := definitions(t, database); n != 4 {
		t.Errorf("%d definitions after the scan, want 4", n)
	}
}
