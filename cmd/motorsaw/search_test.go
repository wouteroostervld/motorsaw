package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEmbeddedAsIs(t *testing.T) {
	dir, database := project(t)
	asked := 0
	server := embeddingServer(t, &asked)
	configure(t, "    embeddings:\n      base_url: "+server.URL+"\n      model: m\n")
	if err := os.WriteFile(filepath.Join(dir, "sub", ".config.yaml"), []byte("index:\n  embeddings: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sync(t, database, dir, syncOptions{})

	path := filepath.Join(dir, "sub", "b.py")
	if !embeddedAsIs(database, path) {
		t.Error("a file just embedded is not as it was")
	}
	// Embedded or not, a file that is not in the index has no lines to give
	if embeddedAsIs(database, filepath.Join(dir, "nowhere.py")) {
		t.Error("a file the index does not know is as it was")
	}

	// An edit after the embeddings were made: their lines are not trusted
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("# moved\n"), content...), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if embeddedAsIs(database, path) {
		t.Error("an edited file is as it was")
	}

	// Embedded again, its lines are good again
	sync(t, database, dir, syncOptions{})
	if !embeddedAsIs(database, path) {
		t.Error("a file embedded again is not as it was")
	}
}
