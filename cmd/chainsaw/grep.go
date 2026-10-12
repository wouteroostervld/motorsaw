package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wouteroostervld/chainsaw/pkg/config"
	"github.com/wouteroostervld/chainsaw/pkg/grep"
	"github.com/wouteroostervld/chainsaw/pkg/view"
)

const grepUsage = `Usage: chainsaw grep [options] <pattern> [path...]

Searches files with a Perl-compatible regular expression and shows, for each
matching line, the definition it is in: the qualified name of the function,
method, struct or interface. Searches the current directory when no path is
given. Hidden directories and the excluded directories of your configuration
are skipped, and so is what git ignores (.gitignore, .git/info/exclude):
what is built or downloaded. A path that is named is searched whatever it is.

Options:
  -i               Ignore case
  -F               The pattern is a literal string
  -w               Match whole words only
  -s               Only list the definitions that contain a match, with a count
  --limit N        Show the first N matching lines only (with -s, the first N
                   definitions); the count of all of them is still given
  --oneline        One match on a line, as path:line: [definition] text
  --width N        Cut lines to N characters
  --no-ignore      Also search what git ignores
  --lang LANGUAGE  Read files as this language (bcpl, m68k, c, ...): the files
                   named, and under a directory those whose name does not say
                   what they are. Without it the name of a file tells, and
                   for a file without an extension what is in it.
  --expand body    Show the whole definition around each match
  --expand file    Show each file folded to signatures, with those definitions in full
  --expand calls   As file, and also what those definitions call. This uses the
                   index, which is first brought up to date for the current directory
  --no-index       With --expand calls, use the index as it is

Options may stand before or after the pattern and the paths. Put -- before a
pattern that begins with a dash.

Examples:
  chainsaw grep 'IS_EXT2_SB' fs/ext4
  chainsaw grep -s 'ctx\.Done\(\)'
  chainsaw grep -s --limit 40 'err' pkg
  chainsaw grep --expand body '(?<=func )handle\w+'`

func handleGrep(args []string) {
	flags := flag.NewFlagSet("grep", flag.ExitOnError)
	flags.Usage = func() { fmt.Println(grepUsage) }
	ignoreCase := flags.Bool("i", false, "")
	fixed := flags.Bool("F", false, "")
	word := flags.Bool("w", false, "")
	symbolsOnly := flags.Bool("s", false, "")
	expand := flags.String("expand", "", "")
	noIndex := flags.Bool("no-index", false, "")
	limit := flags.Int("limit", 0, "")
	oneline := flags.Bool("oneline", false, "")
	width := flags.Int("width", 0, "")
	lang := flags.String("lang", "", "")
	noIgnore := flags.Bool("no-ignore", false, "")
	given := parse(flags, args)
	checkLanguage(*lang)

	if len(given) < 1 {
		fmt.Println(grepUsage)
		os.Exit(1)
	}
	paths := given[1:]
	if len(paths) == 0 {
		paths = []string{"."}
	}
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			fail("%v", err)
		}
		paths[i] = abs
	}

	files, err := grep.Search(given[0], paths, grep.Options{
		IgnoreCase: *ignoreCase,
		Fixed:      *fixed,
		Word:       *word,
		Walk:       newWalk(activeProfile(), !*noIgnore),
		Language:   *lang,
	})
	if err != nil {
		fail("%v", err)
	}
	if len(files) == 0 {
		fmt.Println("No matches")
		os.Exit(1)
	}

	out, done := output(*width)
	defer done()

	all := count(files, *symbolsOnly)
	summary := fmt.Sprintf("%d matching lines in %d files", count(files, false), len(files))
	stopped := *limit > 0 && all > *limit
	if stopped {
		files = first(files, *limit, *symbolsOnly)
		what := ""
		if *symbolsOnly {
			what = " definitions"
		}
		summary += fmt.Sprintf(", the first %d%s shown", *limit, what)
	}

	cwd := workingDir()
	if *expand != "" {
		mode, err := view.ParseMode(*expand)
		if err != nil {
			fail("%v", err)
		}
		var hits []view.Hit
		for _, f := range files {
			for _, g := range f.Groups {
				for _, l := range g.Lines {
					hits = append(hits, view.Hit{Path: f.Path, Line: l.Number})
				}
			}
		}
		renderHits(out, hits, mode, cwd, *noIndex)
		if stopped {
			done()
			fmt.Fprintf(os.Stderr, "note: %s\n", summary)
		}
		return
	}

	if *oneline {
		printMatchLines(out, files, cwd, *symbolsOnly)
		if stopped {
			done()
			fmt.Fprintf(os.Stderr, "note: %s\n", summary)
		}
		return
	}
	printMatches(out, files, cwd, *symbolsOnly)
	fmt.Fprintf(out, "\n%s\n", summary)
}

// count is the number of matching lines in the files, or of the definitions
// they are in
func count(files []grep.File, definitions bool) int {
	n := 0
	for _, f := range files {
		if definitions {
			n += len(f.Groups)
			continue
		}
		for _, g := range f.Groups {
			n += len(g.Lines)
		}
	}
	return n
}

// first cuts the files down to their first n matching lines, or to their
// first n definitions; a definition that is kept keeps its count
func first(files []grep.File, n int, definitions bool) []grep.File {
	var kept []grep.File
	for _, f := range files {
		var groups []grep.Group
		for _, g := range f.Groups {
			if n <= 0 {
				break
			}
			if definitions {
				n--
			} else {
				if len(g.Lines) > n {
					g.Lines = g.Lines[:n]
				}
				n -= len(g.Lines)
			}
			groups = append(groups, g)
		}
		if len(groups) == 0 {
			break
		}
		kept = append(kept, grep.File{Path: f.Path, Groups: groups})
	}
	return kept
}

// printMatches writes the matches grouped by file and by definition
func printMatches(out io.Writer, files []grep.File, cwd string, symbolsOnly bool) {
	for _, f := range files {
		fmt.Fprintln(out, displayPath(f.Path, cwd))
		for _, g := range f.Groups {
			header := "(outside any definition)"
			if g.Symbol != "" {
				header = fmt.Sprintf("%s %s  (lines %d-%d)", g.Type, g.Symbol, g.StartLine, g.EndLine)
			}
			if symbolsOnly {
				fmt.Fprintf(out, "  %s: %d\n", header, len(g.Lines))
				continue
			}
			fmt.Fprintf(out, "  %s\n", header)
			for _, l := range g.Lines {
				fmt.Fprintf(out, "    %d: %s\n", l.Number, strings.TrimSpace(l.Text))
			}
		}
	}
}

// printMatchLines writes one match on a line, in the form other tools read:
// path:line: text, with the definition the line is in between brackets. When
// only definitions are asked for, a line is path:start-end: [definition] count.
func printMatchLines(out io.Writer, files []grep.File, cwd string, symbolsOnly bool) {
	for _, f := range files {
		path := displayPath(f.Path, cwd)
		for _, g := range f.Groups {
			in := ""
			if g.Symbol != "" {
				in = "[" + g.Symbol + "] "
			}
			if symbolsOnly {
				if g.Symbol == "" {
					fmt.Fprintf(out, "%s: %d\n", path, len(g.Lines))
				} else {
					fmt.Fprintf(out, "%s:%d-%d: %s%d\n", path, g.StartLine, g.EndLine, in, len(g.Lines))
				}
				continue
			}
			for _, l := range g.Lines {
				fmt.Fprintf(out, "%s:%d: %s%s\n", path, l.Number, in, strings.TrimSpace(l.Text))
			}
		}
	}
}

// renderHits shows hits in their context. The index is only opened when the
// view needs to know what the hits call, and is then brought up to date
// unless asked not to.
func renderHits(out io.Writer, hits []view.Hit, mode view.Mode, cwd string, noIndex bool) {
	r := &view.Renderer{Out: out, Mode: mode, Base: cwd}
	if mode == view.ModeCalls {
		database := openDB()
		defer database.Close()
		if !noIndex {
			autoIndex(database, cwd, config.IndexGraph)
		}
		r.Finder = database
	}
	if err := r.Render(hits); err != nil {
		fail("%v", err)
	}
}

// displayPath shows a path relative to the current directory when it is under it
func displayPath(path, cwd string) string {
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return path
}
