package filter

import (
	"os"
	"path/filepath"
	"testing"
)

// tree writes files below a new directory and returns it. A name that ends
// in a slash is a directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if name[len(name)-1] == '/' {
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGitIgnore(t *testing.T) {
	dir := tree(t, map[string]string{
		"proj/.git/":             "",
		"proj/.git/info/exclude": "scratch\n",
		"proj/.gitignore": `# What is built or fetched
/build/
tools/
*.o
!keep.o
.venv
doc/**/*.pdf
/top.txt
logs/*.log
a\ b
`,
		"proj/src/.gitignore":    "generated/\n!vendor.o\n/local.txt\n",
		"proj/tools/inner/.git/": "",
		"plain/tools/x.c":        "",
	})
	proj := filepath.Join(dir, "proj")
	g := NewGitIgnore()

	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"build", true, true},
		{"build/cg68/mcg5.b", false, true},
		{"src/build", true, false}, // Tied to the top by its slash
		{"build", false, false},    // A directory is named, and this is a file
		{"tools", true, true},
		{"src/tools/x.c", false, true}, // Without a slash in front: at any depth
		{"main.o", false, true},
		{"src/deep/main.o", false, true},
		{"keep.o", false, false},
		{"src/vendor.o", false, false},
		{"vendor.o", false, true}, // The exception is that of src only
		{".venv/lib/x.py", false, true},
		{"doc/a.pdf", false, true},
		{"doc/x/y/a.pdf", false, true},
		{"other/a.pdf", false, false},
		{"top.txt", false, true},
		{"src/top.txt", false, false},
		{"src/local.txt", false, true},
		{"local.txt", false, false},
		{"logs/a.log", false, true},
		{"logs/sub/a.log", false, false}, // * stops at a slash
		{"src/generated/x.go", false, true},
		{"generated/x.go", false, false},
		{"scratch/notes.c", false, true},
		{"a b", false, true},
		{"src/main.c", false, false},
		// A repository inside an ignored directory
		{"tools/inner/x.java", false, true},
	}
	for _, tt := range tests {
		if got := g.Ignored(proj, filepath.Join(proj, tt.path), tt.isDir); got != tt.want {
			t.Errorf("Ignored(%s, dir %v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
		}
	}

	// From above the repository, as the daemon walks
	if !g.Ignored(dir, filepath.Join(proj, "tools/inner/x.java"), false) || !g.Ignored(dir, filepath.Join(proj, "build"), true) {
		t.Error("what a repository ignores is not ignored in a walk that comes from above it")
	}
	// What is named by hand is not left out for where it is
	if g.Ignored(filepath.Join(proj, "build"), filepath.Join(proj, "build/cg68/mcg5.b"), false) {
		t.Error("a file is ignored in a walk that starts inside the ignored directory")
	}
	if !g.Ignored(filepath.Join(proj, "build"), filepath.Join(proj, "build/cg68/mcg5.o"), false) {
		t.Error("*.o is not ignored in a walk that starts inside an ignored directory")
	}
	// Outside a repository nothing is ignored
	if g.Ignored(dir, filepath.Join(dir, "plain/tools/x.c"), false) {
		t.Error("a file outside any repository is ignored")
	}

	// The files are read once, until Reset
	if err := os.WriteFile(filepath.Join(proj, ".gitignore"), []byte("src/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !g.Ignored(proj, filepath.Join(proj, "build"), true) {
		t.Error("the ignore file was read again without Reset")
	}
	g.Reset()
	if g.Ignored(proj, filepath.Join(proj, "build"), true) || !g.Ignored(proj, filepath.Join(proj, "src/main.c"), false) {
		t.Error("the ignore file was not read again after Reset")
	}
}

func TestGitIgnore_OuterRepository(t *testing.T) {
	// A home directory kept in git ignores everything it did not add. That
	// is not for the repositories in it.
	dir := tree(t, map[string]string{
		"home/.git/":                "",
		"home/.gitignore":           "*\n",
		"home/projects/app/.git/":   "",
		"home/projects/app/main.go": "",
		"home/projects/loose.go":    "",
	})
	home := filepath.Join(dir, "home")
	g := NewGitIgnore()
	if g.Ignored(filepath.Join(home, "projects"), filepath.Join(home, "projects/app/main.go"), false) {
		t.Error("a repository is ignored by one above the root of the walk")
	}
	if !g.Ignored(filepath.Join(home, "projects"), filepath.Join(home, "projects/loose.go"), false) {
		t.Error("a file of the outer repository itself is not ignored by it")
	}
	// The home directory itself is not taken for a repository
	t.Setenv("HOME", home)
	g.Reset()
	if g.Ignored(filepath.Join(home, "projects"), filepath.Join(home, "projects/loose.go"), false) {
		t.Error("the repository in the home directory was followed")
	}
}

func TestWalk(t *testing.T) {
	dir := tree(t, map[string]string{
		"proj/.git/":                  "",
		"proj/.gitignore":             "build/\n*.log\n",
		"proj/.config.yaml":           "exclude: [fixtures, \"third/party\"]\nblacklist: ['_gen\\.go$']\n",
		"proj/build/x.go":             "",
		"proj/src/fixtures/x.go":      "",
		"proj/src/third/party/x.go":   "",
		"proj/src/api_gen.go":         "",
		"proj/src/main.go":            "",
		"proj/src/run.log":            "",
		"proj/vendor/x.go":            "",
		"proj/.hidden/x.go":           "",
		"proj/sub/.config.yaml":       "gitignore: false\n",
		"proj/sub/build/x.go":         "",
		"proj/sub/deeper/build/x.log": "",
	})
	proj := filepath.Join(dir, "proj")
	w := NewWalk([]string{"vendor"}, []string{`\.min\.js$`}, nil, true)

	dirs := map[string]bool{
		"build":           true, // .gitignore
		"vendor":          true, // The configuration
		".hidden":         true,
		"src":             false,
		"src/fixtures":    true, // .config.yaml of the project
		"src/third":       false,
		"src/third/party": true,
		"sub/build":       false, // .config.yaml of sub switches .gitignore off
	}
	for name, want := range dirs {
		if got := w.SkipDir(proj, filepath.Join(proj, name)); got != want {
			t.Errorf("SkipDir(%s) = %v, want %v", name, got, want)
		}
	}
	files := map[string]bool{
		"src/main.go":                 false,
		"src/api_gen.go":              true, // Blacklist of the project
		"src/run.log":                 true, // .gitignore
		"src/app.min.js":              true, // Blacklist of the configuration
		"sub/deeper/build/x.log":      false,
		"sub/deeper/build/app.min.js": true,
	}
	for name, want := range files {
		if got := w.SkipFile(proj, filepath.Join(proj, name)); got != want {
			t.Errorf("SkipFile(%s) = %v, want %v", name, got, want)
		}
	}

	// From above the project, as the daemon walks, and from inside it
	if !w.SkipDir(dir, filepath.Join(proj, "src/fixtures")) || !w.SkipDir(filepath.Join(proj, "src"), filepath.Join(proj, "src/fixtures")) {
		t.Error("what a project excludes is not excluded in a walk from above or inside it")
	}
	// What is named by hand is walked
	if w.SkipDir(filepath.Join(proj, "src/fixtures"), filepath.Join(proj, "src/fixtures/more")) {
		t.Error("a directory below an excluded one that was named is skipped")
	}

	// Without .gitignore
	plain := NewWalk([]string{"vendor"}, nil, nil, false)
	if plain.SkipDir(proj, filepath.Join(proj, "build")) || !plain.SkipDir(proj, filepath.Join(proj, "vendor")) {
		t.Error("a walk that does not follow .gitignore should skip vendor and not build")
	}
}
