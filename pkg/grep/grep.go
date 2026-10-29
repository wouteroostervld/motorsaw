// Package grep searches files for a regular expression and reports each
// matching line together with the definition it is in: the qualified name of
// the function, method, struct or interface around it.
//
// Patterns are Perl-compatible regular expressions (PCRE2).
package grep

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
	"github.com/wouteroostervld/motorsaw/pkg/filter"
	"github.com/wouteroostervld/motorsaw/pkg/pcre"
)

// maxFileSize is the largest file that is searched
const maxFileSize = 5 << 20

// Options control a search
type Options struct {
	IgnoreCase bool
	Fixed      bool     // The pattern is a literal string, not a regular expression
	Word       bool     // The pattern must match whole words
	Exclude    []string // Directories to skip, besides hidden ones (see filter.Excluded)
	Signature  bool     // Only matches in the signature of a definition count: in it, before its body
}

// Line is a line that matched
type Line struct {
	Number int // 1-indexed
	Text   string
}

// Group holds the matching lines that share an enclosing definition
type Group struct {
	Symbol    string // Qualified name; empty for lines outside any definition
	Type      string // Entity type of the definition
	StartLine int    // Extent of the definition
	EndLine   int
	Lines     []Line
}

// File holds the matches in one file, grouped by definition in file order
type File struct {
	Path   string
	Groups []Group
}

// Search looks for the pattern in the given files and directories
func Search(pattern string, paths []string, opts Options) ([]File, error) {
	if opts.Fixed {
		pattern = regexp.QuoteMeta(pattern)
	}
	if opts.Word {
		pattern = `\b(?:` + pattern + `)\b`
	}
	// ^ and $ match at line ends, as grep users expect
	flags := pcre.Multiline
	if opts.IgnoreCase {
		flags |= pcre.Caseless
	}
	re, err := pcre.Compile(pattern, flags)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	defer re.Close()

	// Matching is quick and done in order, with the one compiled pattern.
	// Finding the definitions around the matches means parsing each file
	// that has any, which is the slow part and is done in parallel.
	var found []*candidate
	err = walk(paths, opts.Exclude, func(path string) {
		if c := matchFile(re, path); c != nil {
			found = append(found, c)
		}
	})
	if err != nil {
		return nil, err
	}
	return each(found, func(c *candidate) File { return c.group(opts.Signature) }), nil
}

// Match looks in the given files and directories for what a tree-sitter
// query captures: patterns over the syntax tree, where Search has a pattern
// over the text. Only files in a language the query fits are searched.
func Match(query *chunker.Query, paths []string, opts Options) ([]File, error) {
	var fits []string
	err := walk(paths, opts.Exclude, func(path string) {
		if query.Fits(path) {
			fits = append(fits, path)
		}
	})
	if err != nil {
		return nil, err
	}
	return each(fits, func(path string) File {
		content := readText(path)
		if content == nil {
			return File{}
		}
		captures := query.Find(path, content)
		if len(captures) == 0 {
			return File{}
		}
		c := &candidate{path: path, content: content}
		for _, capture := range captures {
			c.offsets = append(c.offsets, capture.Start)
		}
		return c.group(opts.Signature)
	}), nil
}

// walk calls search for the files named, and for the regular files under
// the directories named. Hidden and excluded directories are skipped.
func walk(paths, excluded []string, search func(path string)) error {
	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			return err
		}
		// Excluded directories are told by their absolute path
		from, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			// A file named on the command line is searched whatever it is
			search(root)
			continue
		}
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				// A directory named on the command line is searched even
				// when it is hidden or excluded, or inside one that is
				if path != root && (strings.HasPrefix(name, ".") || filter.Excluded(from, filepath.Join(from, path[len(root):]), excluded)) {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() {
				search(path)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// each turns every item into the matches of a file, in parallel, and returns
// those that have any, in the order of the items
func each[T any](items []T, group func(T) File) []File {
	files := make([]File, len(items))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				files[i] = group(items[i])
			}
		}()
	}
	for i := range items {
		next <- i
	}
	close(next)
	wg.Wait()

	matched := files[:0]
	for _, f := range files {
		if len(f.Groups) > 0 {
			matched = append(matched, f)
		}
	}
	return matched
}

// candidate is a file with matches, before its definitions are known
type candidate struct {
	path    string
	content []byte
	offsets []int // Where the matches start, in order
}

// readText returns the content of a file; nil when it is empty, too large or
// not text
func readText(path string) []byte {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxFileSize || info.Size() == 0 {
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(content[:min(len(content), 8000)], 0) >= 0 {
		return nil
	}
	return content
}

// matchFile returns the matches in a file; nil when there are none or the
// file is not text
func matchFile(re *pcre.Regexp, path string) *candidate {
	content := readText(path)
	if content == nil {
		return nil
	}

	// A pattern that gives up on a file, by running into a match limit, is
	// treated as not matching it
	matches, err := re.FindAllIndex(content)
	if err != nil || len(matches) == 0 {
		return nil
	}

	c := &candidate{path: path, content: content}
	for _, m := range matches {
		c.offsets = append(c.offsets, m[0])
	}
	return c
}

// group parses the file and groups the lines its matches start on by the
// definition around them. With signature set, only the matches in the
// signature of a definition are kept.
func (c *candidate) group(signature bool) File {
	lines := bytes.Split(c.content, []byte{'\n'})
	parsed, _ := chunker.Parse(c.path, c.content, 0)

	var numbers []int
	line, last := 1, 0
	for _, offset := range c.offsets {
		if signature && (parsed == nil || !parsed.InSignature(offset)) {
			continue
		}
		line += bytes.Count(c.content[last:offset], []byte{'\n'})
		last = offset
		if len(numbers) == 0 || numbers[len(numbers)-1] != line {
			numbers = append(numbers, line)
		}
	}

	file := File{Path: c.path}
	groups := make(map[int]int) // Symbol index -> index into file.Groups
	for _, n := range numbers {
		symbol := -1
		if parsed != nil {
			symbol = parsed.SymbolAt(n)
		}
		g, ok := groups[symbol]
		if !ok {
			g = len(file.Groups)
			groups[symbol] = g
			group := Group{}
			if symbol >= 0 {
				s := parsed.Symbols[symbol]
				group = Group{Symbol: s.QualifiedName(), Type: s.Type, StartLine: s.StartLine, EndLine: s.EndLine}
			}
			file.Groups = append(file.Groups, group)
		}
		text := strings.TrimRight(string(lines[n-1]), "\r")
		file.Groups[g].Lines = append(file.Groups[g].Lines, Line{Number: n, Text: text})
	}

	sort.SliceStable(file.Groups, func(i, j int) bool {
		return file.Groups[i].Lines[0].Number < file.Groups[j].Lines[0].Number
	})
	return file
}
