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

	"github.com/wouteroostervld/chainsaw/pkg/chunker"
	"github.com/wouteroostervld/chainsaw/pkg/filter"
	"github.com/wouteroostervld/chainsaw/pkg/pcre"
)

// maxFileSize is the largest file that is searched
const maxFileSize = 5 << 20

// Options control a search
type Options struct {
	IgnoreCase bool
	Fixed      bool     // The pattern is a literal string, not a regular expression
	Word       bool     // The pattern must match whole words
	Exclude    []string // Directories to skip, besides hidden ones (see filter.Excluded)
	// Walk says what is left out under a directory, in place of Exclude:
	// also what git ignores, and what a project excludes
	Walk *filter.Walk
	// Language is the language to read files as: those named among the
	// paths, and under a directory those whose name does not say what they
	// are. Empty to go by name and content.
	Language string
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
	search := func(path string, named bool) {
		if c := matchFile(re, path); c != nil {
			if opts.Language != "" && (named || !chunker.Supported(path)) {
				c.language = opts.Language
			}
			found = append(found, c)
		}
	}

	walk := opts.Walk
	if walk == nil {
		walk = filter.NewWalk(opts.Exclude, nil, nil, false)
	}

	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		// Excluded directories are told by their absolute path
		from, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			// A file named on the command line is searched whatever it is
			search(root, true)
			continue
		}
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			abs := filepath.Join(from, path[len(root):])
			if d.IsDir() {
				// A directory named on the command line is searched even
				// when it is hidden, excluded or ignored, or inside one that is
				if path != root && walk.SkipDir(from, abs) {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() && !walk.GitIgnored(from, abs) {
				search(path, false)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	files := make([]File, len(found))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				files[i] = found[i].group()
			}
		}()
	}
	for i := range found {
		next <- i
	}
	close(next)
	wg.Wait()
	return files, nil
}

// candidate is a file with matches, before its definitions are known
type candidate struct {
	path     string
	language string // What to read it as; empty to go by name and content
	content  []byte
	numbers  []int // The lines with a match, in order
}

// matchFile returns the lines of a file that match; nil when there are none
// or the file is not text
func matchFile(re *pcre.Regexp, path string) *candidate {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxFileSize || info.Size() == 0 {
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(content[:min(len(content), 8000)], 0) >= 0 {
		return nil
	}

	// A pattern that gives up on a file, by running into a match limit, is
	// treated as not matching it
	matches, err := re.FindAllIndex(content)
	if err != nil || len(matches) == 0 {
		return nil
	}

	// The lines the matches start on
	var numbers []int
	line, offset := 1, 0
	for _, m := range matches {
		line += bytes.Count(content[offset:m[0]], []byte{'\n'})
		offset = m[0]
		if len(numbers) == 0 || numbers[len(numbers)-1] != line {
			numbers = append(numbers, line)
		}
	}
	return &candidate{path: path, content: content, numbers: numbers}
}

// group parses the file and groups its matching lines by the definition
// around them
func (c *candidate) group() File {
	lines := bytes.Split(c.content, []byte{'\n'})
	var parsed *chunker.File
	if c.language != "" {
		parsed, _ = chunker.ParseAs(c.language, c.path, c.content, 0)
	} else {
		parsed, _ = chunker.Parse(c.path, c.content, 0)
	}

	file := File{Path: c.path}
	groups := make(map[int]int) // Symbol index -> index into file.Groups
	for _, n := range c.numbers {
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
