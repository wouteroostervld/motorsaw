package filter

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// GitIgnore tells what git ignores: what the .gitignore files of a
// repository and its .git/info/exclude name. What is ignored there is built,
// downloaded or generated, and is as a rule not what one wants searched or
// indexed. The files are read once and kept; Reset reads them again.
//
// The ignore file of the user (core.excludesFile) is not read, and neither
// is the index of git: a file that is tracked although a pattern names it
// counts as ignored here. A repository that is the home directory itself is
// passed over: it holds settings, and ignores what is not one.
type GitIgnore struct {
	mu      sync.Mutex
	files   map[string][]ignorePattern // By the path of the ignore file; nil when there is none
	repos   map[string]string          // By directory, the repository it is in; "" for none
	ignored map[string]bool            // By directory, whether it is ignored
}

// NewGitIgnore creates an empty GitIgnore
func NewGitIgnore() *GitIgnore {
	g := &GitIgnore{}
	g.Reset()
	return g
}

// Reset forgets what was read, so that the ignore files are read again
func (g *GitIgnore) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files = make(map[string][]ignorePattern)
	g.repos = make(map[string]string)
	g.ignored = make(map[string]bool)
}

// ignorePattern is one line of an ignore file
type ignorePattern struct {
	re      *regexp.Regexp // Against the path below the directory of the file
	negate  bool           // The line starts with !: what it matches is not ignored after all
	dirOnly bool           // The line ends in /: it matches directories only
}

// Ignored reports whether git ignores a file or a directory, going by the
// part of its path below root only: a walk that is started inside an ignored
// directory, because someone named it, is not stopped by where it is. Both
// paths must be absolute.
func (g *GitIgnore) Ignored(root, path string, isDir bool) bool {
	root, path = filepath.Clean(root), filepath.Clean(path)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.check(root, path, isDir, false)
}

// check is Ignored for a path, or with outer for the repository a path is
// in: one repository inside another is ignored when the outer one ignores
// it. The outer one is only asked when the walk comes through it. One that
// lies above the root of the walk has no say over a repository of its own:
// a home directory that is kept in git, with everything ignored that is not
// added, would leave out every project in it.
func (g *GitIgnore) check(root, path string, isDir, outer bool) bool {
	if path == root || !below(path, root) {
		return false
	}
	repo := g.repository(filepath.Dir(path))
	if repo == "" || (outer && repo != root && !below(repo, root)) {
		return false
	}
	// From the top down: what is inside an ignored directory is ignored,
	// whatever a pattern further down says
	top := root
	if below(repo, root) {
		top = repo
	}
	rel, err := filepath.Rel(top, path)
	if err != nil {
		return false
	}
	names := strings.Split(rel, string(filepath.Separator))
	at := top
	for i, name := range names {
		at = filepath.Join(at, name)
		if i == len(names)-1 && !isDir {
			if g.matches(repo, at, false) {
				return true
			}
			break
		}
		ignored, known := g.ignored[at]
		if !known {
			ignored = g.matches(repo, at, true)
			g.ignored[at] = ignored
		}
		if ignored {
			return true
		}
	}
	// A repository inside another is ignored when the outer one ignores it
	return g.check(root, repo, true, true)
}

// repository returns the top directory of the repository a directory is in,
// or "" when it is in none
func (g *GitIgnore) repository(dir string) string {
	if repo, known := g.repos[dir]; known {
		return repo
	}
	repo := ""
	// A directory for a repository, a file for a worktree or a submodule
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && dir != os.Getenv("HOME") {
		repo = dir
	} else if parent := filepath.Dir(dir); parent != dir {
		repo = g.repository(parent)
	}
	g.repos[dir] = repo
	return repo
}

// matches reports whether the ignore files of a repository name a path: the
// last line that matches says, where the file nearest to the path comes
// last and .git/info/exclude first
func (g *GitIgnore) matches(repo, path string, isDir bool) bool {
	ignored := false
	apply := func(file, base string) {
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return
		}
		rel = filepath.ToSlash(rel)
		for _, p := range g.patterns(file) {
			if (!p.dirOnly || isDir) && p.re.MatchString(rel) {
				ignored = !p.negate
			}
		}
	}

	apply(filepath.Join(repo, ".git", "info", "exclude"), repo)
	dirs := []string{repo}
	if rel, err := filepath.Rel(repo, filepath.Dir(path)); err == nil && rel != "." {
		at := repo
		for _, name := range strings.Split(rel, string(filepath.Separator)) {
			at = filepath.Join(at, name)
			dirs = append(dirs, at)
		}
	}
	for _, dir := range dirs {
		apply(filepath.Join(dir, ".gitignore"), dir)
	}
	return ignored
}

// patterns returns the lines of an ignore file
func (g *GitIgnore) patterns(file string) []ignorePattern {
	if patterns, known := g.files[file]; known {
		return patterns
	}
	var patterns []ignorePattern
	if content, err := os.ReadFile(file); err == nil {
		for _, line := range strings.Split(string(content), "\n") {
			if p, ok := parseIgnoreLine(line); ok {
				patterns = append(patterns, p)
			}
		}
	}
	g.files[file] = patterns
	return patterns
}

// parseIgnoreLine reads one line of an ignore file, as gitignore(5) has it
func parseIgnoreLine(line string) (ignorePattern, bool) {
	line = strings.TrimSuffix(line, "\r")
	// Spaces at the end do not count, unless a backslash keeps them
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = strings.TrimSuffix(line, " ")
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return ignorePattern{}, false
	}

	var p ignorePattern
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if line == "" {
		return ignorePattern{}, false
	}
	// A slash at the start or in the middle ties the pattern to the
	// directory of the file; without one it names something at any depth
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")

	var re strings.Builder
	re.WriteString("^")
	if !anchored {
		re.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch c {
		case '*':
			if i+1 < len(line) && line[i+1] == '*' {
				start := i == 0 || line[i-1] == '/'
				i++
				switch {
				case start && i+1 < len(line) && line[i+1] == '/':
					// "**/": any number of directories, also none
					re.WriteString("(?:.*/)?")
					i++
				case start && i+1 == len(line):
					// "/**": everything inside
					re.WriteString(".*")
				default:
					re.WriteString("[^/]*")
				}
			} else {
				re.WriteString("[^/]*")
			}
		case '?':
			re.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(line[i+1:], ']')
			if end <= 0 {
				re.WriteString(`\[`)
				break
			}
			class := line[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			re.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		case '\\':
			if i+1 < len(line) {
				i++
				re.WriteString(regexp.QuoteMeta(string(line[i])))
			}
		default:
			re.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	re.WriteString("$")

	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return ignorePattern{}, false
	}
	p.re = compiled
	return p, true
}
