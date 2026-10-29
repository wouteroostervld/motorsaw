package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// ignoring makes the project of project(t) a repository that ignores its
// build directory, with a source file in it
func ignoring(t *testing.T, dir string) {
	t.Helper()
	for name, content := range map[string]string{
		".git/HEAD":       "ref: refs/heads/main\n",
		".gitignore":      "build/\n*.gen.py\n",
		"build/out.py":    "def out():\n    return 1\n",
		"sub/made.gen.py": "def made():\n    return 1\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSyncIndex_Gitignore(t *testing.T) {
	dir, database := project(t)
	ignoring(t, dir)

	// What git ignores is built or fetched, and is left out
	if got := sync(t, database, dir, syncOptions{}); got != (syncStats{indexed: 2}) {
		t.Fatalf("first run = %+v, want a.py and sub/b.py indexed", got)
	}
	if got, want := indexed(t, database, dir), []string{"a.py", "sub/b.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("indexed %v, want %v", got, want)
	}

	// Named by hand it is indexed, as what the configuration excludes is
	if got := sync(t, database, filepath.Join(dir, "build"), syncOptions{}); got != (syncStats{indexed: 1}) {
		t.Errorf("the ignored directory, named = %+v, want 1 indexed", got)
	}

	// And everything is when .gitignore is not followed
	st, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	st.walk = newWalk(st.profile, false)
	stats, err := syncIndex(database, st, []string{dir}, syncOptions{})
	if err != nil || stats != (syncStats{indexed: 1, unchanged: 3}) {
		t.Errorf("without .gitignore = %+v, %v; want sub/made.gen.py indexed besides", stats, err)
	}

	// The configuration can say so too, for everything or for a project
	configure(t, "    gitignore: false\n")
	if st, err := loadSettings(); err != nil || st.walk.SkipDir(dir, filepath.Join(dir, "build")) {
		t.Errorf("gitignore: false in the configuration still skips build (%v)", err)
	}
}

func TestForget(t *testing.T) {
	dir, database := project(t)
	ignoring(t, dir)
	st, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}

	// As it was before .gitignore was followed: everything is in the index
	st.walk = newWalk(st.profile, false)
	if _, err := syncIndex(database, st, []string{dir}, syncOptions{}); err != nil {
		t.Fatal(err)
	}
	all := []string{"a.py", "build/out.py", "sub/b.py", "sub/made.gen.py"}
	if got := indexed(t, database, dir); !reflect.DeepEqual(got, all) {
		t.Fatalf("indexed %v, want %v", got, all)
	}
	before := definitions(t, database)

	st, err = loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	// A dry run says what would go and changes nothing
	stats, err := forget(database, st, []string{dir}, forgetOptions{ignored: true, dryRun: true})
	if err != nil || stats.forgotten != 2 || stats.kept != 2 {
		t.Fatalf("dry run = %+v, %v; want 2 to forget and 2 kept", stats, err)
	}
	wantDirs := map[string]int{filepath.Join(dir, "build"): 1, filepath.Join(dir, "sub"): 1}
	if !reflect.DeepEqual(stats.byDir, wantDirs) {
		t.Errorf("dry run by directory = %v, want %v", stats.byDir, wantDirs)
	}
	if got := indexed(t, database, dir); !reflect.DeepEqual(got, all) {
		t.Fatalf("a dry run left %v in the index", got)
	}

	// What is ignored goes, with its definitions
	stats, err = forget(database, st, []string{dir}, forgetOptions{ignored: true})
	if err != nil || stats.forgotten != 2 || stats.kept != 2 {
		t.Fatalf("forget --ignored = %+v, %v; want 2 forgotten and 2 kept", stats, err)
	}
	if got, want := indexed(t, database, dir), []string{"a.py", "sub/b.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("indexed %v after forgetting what is ignored, want %v", got, want)
	}
	// Two modules and two functions less
	if n := definitions(t, database); n != before-4 {
		t.Errorf("%d definitions, want %d", n, before-4)
	}

	// A path that is named goes whatever it is, and the files stay on disk
	stats, err = forget(database, st, []string{filepath.Join(dir, "sub"), filepath.Join(dir, "gone")}, forgetOptions{})
	if err != nil || stats.forgotten != 1 {
		t.Fatalf("forget sub = %+v, %v; want 1 forgotten", stats, err)
	}
	if got, want := indexed(t, database, dir), []string{"a.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("indexed %v after forgetting sub, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub/b.py")); err != nil {
		t.Errorf("forgetting touched the file on disk: %v", err)
	}
}
