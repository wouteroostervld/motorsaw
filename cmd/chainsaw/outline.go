package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wouteroostervld/chainsaw/pkg/chunker"
	"github.com/wouteroostervld/chainsaw/pkg/view"
)

const outlineUsage = `Usage: chainsaw outline [options] <file>...

Shows a source file folded to its skeleton: every definition is reduced to
its signature and the lines of its body. Works on the file as it is on disk;
the index is not used. Several files are shown one after the other, each
under its name.

Options:
  --expand NAME    Show this definition in full; a qualified name (Type.method),
                   a bare name, or a line number. May be given more than once.
  --body NAME      Show this definition in full and nothing else. May be given
                   more than once.
  --symbols        List the definitions instead: type, name, lines, signature
  --lang LANGUAGE  Read the files as this language, whatever their names say
                   (bcpl, m68k, c, ...). Without it the name of a file tells,
                   and for a file without an extension what is in it.
  --width N        Cut lines to N characters

Options may stand before or after the files.

Examples:
  chainsaw outline pkg/indexer/indexer.go
  chainsaw outline --expand Indexer.IndexFile pkg/indexer/indexer.go
  chainsaw outline --body Indexer.IndexFile pkg/indexer/indexer.go
  chainsaw outline --symbols pkg/db/*.go`

// names collects a flag that may be repeated
type names []string

func (n *names) String() string     { return strings.Join(*n, ",") }
func (n *names) Set(v string) error { *n = append(*n, v); return nil }

func handleOutline(args []string) {
	flags := flag.NewFlagSet("outline", flag.ExitOnError)
	flags.Usage = func() { fmt.Println(outlineUsage) }
	var expand, body names
	flags.Var(&expand, "expand", "")
	flags.Var(&body, "body", "")
	symbols := flags.Bool("symbols", false, "")
	lang := flags.String("lang", "", "")
	width := flags.Int("width", 0, "")
	given := parse(flags, args)

	if len(given) < 1 {
		fmt.Println(outlineUsage)
		os.Exit(1)
	}
	checkLanguage(*lang)
	out, done := output(*width)
	defer done()

	cwd := workingDir()
	var hits []view.Hit            // What --body asks for
	found := make(map[string]bool) // The names that are in one of the files
	for i, name := range given {
		path, err := filepath.Abs(name)
		if err != nil {
			fail("%v", err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			fail("%v", err)
		}
		f, ok := parseFile(*lang, path, content)
		if !ok {
			done()
			fail("%s is not in a language chainsaw can parse.\nIf you know what it is, say so with --lang: one of %s",
				name, strings.Join(chunker.Languages(), ", "))
		}

		for _, n := range body {
			for _, chunk := range chunksNamed(f, n) {
				found[n] = true
				hits = append(hits, view.Hit{Path: path, Line: f.Chunks[chunk].StartLine})
			}
		}
		if len(body) > 0 {
			continue
		}

		if len(given) > 1 {
			if i > 0 {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "## %s\n\n", displayPath(path, cwd))
		}
		if *symbols {
			for _, s := range f.Symbols {
				fmt.Fprintf(out, "%-9s %-40s %d-%d  %s\n", s.Type, s.QualifiedName(), s.StartLine, s.EndLine, s.Signature)
			}
			continue
		}

		expanded := make(map[int]bool)
		for _, n := range expand {
			for _, chunk := range chunksNamed(f, n) {
				found[n] = true
				expanded[chunk] = true
			}
		}
		rendered := f.Render(expanded)
		fmt.Fprint(out, rendered)
		if len(given) > 1 && !strings.HasSuffix(rendered, "\n") {
			fmt.Fprintln(out)
		}
	}

	for _, n := range append(body, expand...) {
		if !found[n] && !*symbols {
			done()
			fail("no definition named %s in %s", n, strings.Join(given, ", "))
		}
	}
	if len(hits) > 0 {
		renderHits(out, hits, view.ModeBody, cwd, true)
	}
}

// chunksNamed finds the chunks of a file that a name stands for: a qualified
// name (Type.method), a bare name, the title of a section, or a line number
func chunksNamed(f *chunker.File, name string) []int {
	if line, err := strconv.Atoi(name); err == nil {
		return []int{f.ChunkAt(line)}
	}
	named := make(map[int]bool)
	for _, s := range f.Symbols {
		if s.QualifiedName() == name || s.Name == name || (f.FoldCase && strings.EqualFold(s.Name, name)) {
			named[f.ChunkAt(s.StartLine)] = true
		}
	}
	// Sections of a document have a title but are not symbols
	for i, c := range f.Chunks {
		if c.Title == name || strings.HasSuffix(c.Title, " > "+name) {
			named[i] = true
		}
	}
	var chunks []int
	for i := range f.Chunks {
		if named[i] {
			chunks = append(chunks, i)
		}
	}
	return chunks
}

// parseFile parses a file in the language it is said to be in, or in the one
// its name and content show when none is said
func parseFile(lang, path string, content []byte) (*chunker.File, bool) {
	if lang != "" {
		return chunker.ParseAs(lang, path, content, 0)
	}
	return chunker.Parse(path, content, 0)
}

// checkLanguage ends the command when --lang names a language that cannot be parsed
func checkLanguage(lang string) {
	if lang == "" {
		return
	}
	for _, known := range chunker.Languages() {
		if strings.EqualFold(lang, known) {
			return
		}
	}
	fail("--lang %s: not a language chainsaw can parse; it parses %s", lang, strings.Join(chunker.Languages(), ", "))
}
