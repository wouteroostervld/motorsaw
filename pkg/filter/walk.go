package filter

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// LocalConfigName is the file in which a project says what is left out of it
const LocalConfigName = ".config.yaml"

// Walk says what a walk through a tree leaves out. Every command that walks
// asks it, so that what one skips another does not keep:
//
//   - hidden directories;
//   - the directories the configuration excludes (see Excluded);
//   - what the .config.yaml of a directory excludes below it;
//   - what git ignores (see GitIgnore), unless that is switched off;
//   - files the blacklist rejects and the whitelist does not let through.
//
// All of it goes by the part of a path below the root of the walk: what is
// named by hand is not left out for where it is.
type Walk struct {
	Exclude   []string // Directories to skip, of the configuration
	Blacklist []string // Regular expressions on the path of files to skip
	Whitelist []string // Exceptions to the blacklist

	git *GitIgnore // nil when .gitignore is not followed

	mu     sync.Mutex
	locals map[string]*local // By directory, what its .config.yaml says; nil for none
}

// local is what a .config.yaml says about what to leave out
type local struct {
	Exclude   []string `yaml:"exclude"`
	Blacklist []string `yaml:"blacklist"`
	Gitignore *bool    `yaml:"gitignore"`
}

// NewWalk creates the rules of a walk. With gitignore, what git ignores is
// left out; a .config.yaml can still say otherwise for the tree below it.
func NewWalk(exclude, blacklist, whitelist []string, gitignore bool) *Walk {
	w := &Walk{Exclude: exclude, Blacklist: blacklist, Whitelist: whitelist, locals: make(map[string]*local)}
	if gitignore {
		w.git = NewGitIgnore()
	}
	return w
}

// Reset forgets what was read from .gitignore and .config.yaml files, so
// that they are read again
func (w *Walk) Reset() {
	w.mu.Lock()
	w.locals = make(map[string]*local)
	w.mu.Unlock()
	if w.git != nil {
		w.git.Reset()
	}
}

// SkipDir reports whether a walk from root passes a directory by. Both paths
// must be absolute.
func (w *Walk) SkipDir(root, dir string) bool {
	root, dir = filepath.Clean(root), filepath.Clean(dir)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return false
	}
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(name, ".") {
			return name != ".." // Not below root: not for this walk to say
		}
	}
	if Excluded(root, dir, w.Exclude) {
		return true
	}
	for _, at := range w.configured(root, filepath.Dir(dir)) {
		if Excluded(at.from, dir, at.Exclude) {
			return true
		}
	}
	return w.gitIgnored(root, dir, true)
}

// SkipFile reports whether a walk from root passes by a file in a directory
// it does come to: the blacklist rejects it, or git ignores it
func (w *Walk) SkipFile(root, path string) bool {
	if ok, _ := ShouldIndexFile(path, w.Blacklist, w.Whitelist); !ok {
		return true
	}
	root, path = filepath.Clean(root), filepath.Clean(path)
	for _, at := range w.configured(root, filepath.Dir(path)) {
		if len(at.Blacklist) > 0 {
			if ok, _ := ShouldIndexFile(path, at.Blacklist, w.Whitelist); !ok {
				return true
			}
		}
	}
	return w.gitIgnored(root, path, false)
}

// GitIgnored reports whether a file is left out of a walk from root for
// being ignored by git
func (w *Walk) GitIgnored(root, path string) bool {
	return w.gitIgnored(filepath.Clean(root), filepath.Clean(path), false)
}

func (w *Walk) gitIgnored(root, path string, isDir bool) bool {
	if w.git == nil {
		return false
	}
	// The nearest .config.yaml that says anything about it decides
	for _, at := range w.configured(root, filepath.Dir(path)) {
		if at.Gitignore != nil {
			if !*at.Gitignore {
				return false
			}
			break
		}
	}
	return w.git.Ignored(root, path, isDir)
}

// placed is a .config.yaml and the directory its entries are held against
type placed struct {
	*local
	from string
}

// configured returns what the .config.yaml files in and above a directory
// say, nearest first. One above the root of the walk counts too, but only
// for what lies below the root.
func (w *Walk) configured(root, dir string) []placed {
	w.mu.Lock()
	defer w.mu.Unlock()

	var found []placed
	for {
		l, known := w.locals[dir]
		if !known {
			l = readLocal(filepath.Join(dir, LocalConfigName))
			w.locals[dir] = l
		}
		if l != nil {
			from := dir
			if below(root, dir) {
				from = root
			}
			found = append(found, placed{local: l, from: from})
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return found
		}
		dir = parent
	}
}

// readLocal reads what a .config.yaml says about what to leave out; nil when
// it is not there, cannot be read, or says nothing about it
func readLocal(path string) *local {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var l local
	if err := yaml.Unmarshal(content, &l); err != nil {
		return nil
	}
	if len(l.Exclude) == 0 && len(l.Blacklist) == 0 && l.Gitignore == nil {
		return nil
	}
	return &l
}
