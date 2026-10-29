package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
	"github.com/wouteroostervld/motorsaw/pkg/grep"
)

const matchUsage = `Usage: motorsaw match [options] <query> [path...]
       motorsaw match --tree [--lines N-M] <file>

Searches the syntax tree of files with a tree-sitter query, and shows each
node the query captures with the definition it is in, as grep does for text.
Searches the current directory when no path is given.

A query is a pattern of nodes, written as the tree is: (kind field: (kind)).
A node is captured with @name; a pattern that captures nothing is reported as
a whole. Conditions on the text of a capture come last, between brackets of
their own. --tree shows the tree of a file, to write a query from.

  (kind)                       A node of that kind
  (kind (other))               ...with a node of the other kind right below it
  (kind field: (other))        ...in that field
  (kind !field)                ...without that field
  (_)                          Any named node; _ is any node at all
  "return"                     A keyword or sign
  (a) @x                       Report this node. A capture named @_x is not reported
  (a)* (a)+ (a)?               Repeated, or optional
  [(a) (b)]                    One or the other
  (kind . (a))                 a is the first node below; (a) . (b) follow each other
  (#eq? @x "text")             The capture is this text; (#eq? @x @y) for two alike
  (#match? @x "^regexp")       The capture matches (Go syntax, not PCRE)
  (#any-of? @x "a" "b")        The capture is one of these
  (#not-eq? ...) (#not-match? ...) (#not-any-of? ...)

The kinds and fields are those of a grammar, so a query is tried on every
language and searches the files of those it fits. Of several patterns, a
language takes the ones that fit it: write a pattern for each grammar to ask
the same of more than one. An error is given only when nothing fits.

Options:
  --signature      Only captures in the signature of a definition: in the
                   definition, before its body. Works alike in every language
  --lang NAME      Only this language: ` + "%s" + `
  --writes NAME    Instead of a query: the places that assign a variable, field
                   or property of this name, or an element of one (=, +=, ++,
                   and in PHP =&). Go, PHP, Python, JavaScript, TypeScript, C
  --tree           Show the syntax tree of a file instead of searching
  --lines N-M      With --tree, only the nodes on these lines
  -s               Only list the definitions that contain a match, with a count
  --limit N        Show the first N matching lines only (with -s, the first N
                   definitions); the count of all of them is still given
  --oneline        One match on a line, as path:line: [definition] text
  --width N        Cut lines to N characters
  --expand body    Show the whole definition around each match
  --expand file    Show each file folded to signatures, with those definitions in full
  --expand calls   As file, and also what those definitions call. This uses the
                   index, which is first brought up to date for the current directory
  --no-index       With --expand calls, use the index as it is

Examples:
  # Go functions that return a pointer
  motorsaw match '(function_declaration result: (pointer_type) @r)'

  # Definitions in any language with a parameter named ctx
  motorsaw match --signature '((identifier) @p (#eq? @p "ctx"))'

  # Anything with a pointer in its signature, in Go and Rust and in C and C++
  motorsaw match --signature -s '(pointer_type) @p (pointer_declarator) @p'

  # Go methods that take a context and return only an error
  motorsaw match '(method_declaration
      parameters: (parameter_list (parameter_declaration type: (_) @_t))
      result: (type_identifier) @_r
      (#eq? @_t "context.Context") (#eq? @_r "error")) @m'

  # Every function that writes Current::$database, grouped by function
  motorsaw match -s --writes '$database' src

  # The tree of a few lines
  motorsaw match --tree --lines 40-52 pkg/grep/grep.go`

func handleMatch(args []string) {
	usage := fmt.Sprintf(matchUsage, strings.Join(chunker.QueryLanguages(), ", "))
	flags := flag.NewFlagSet("match", flag.ExitOnError)
	flags.Usage = func() { fmt.Println(usage) }
	signature := flags.Bool("signature", false, "")
	language := flags.String("lang", "", "")
	tree := flags.Bool("tree", false, "")
	lines := flags.String("lines", "", "")
	writes := flags.String("writes", "", "")
	shaped := shapeFlags(flags)
	given := parse(flags, args)

	if *writes != "" {
		// The query is made from the name; what is given are the paths
		q, err := writesQuery(*writes)
		if err != nil {
			fail("%v", err)
		}
		given = append([]string{q}, given...)
	}
	if len(given) < 1 {
		fmt.Println(usage)
		os.Exit(1)
	}
	if *tree {
		showTree(given, *lines, *shaped.width)
		return
	}

	query, err := chunker.NewQuery(given[0], *language)
	if err != nil {
		fail("%v", err)
	}
	defer query.Close()

	files, err := grep.Match(query, absolute(given[1:]), grep.Options{
		Exclude:   activeProfile().Exclude,
		Signature: *signature,
	})
	if err != nil {
		fail("%v", err)
	}
	if len(files) == 0 {
		// Nothing found may mean that the query is in the words of another grammar
		fmt.Fprintf(os.Stderr, "note: the query fits %s\n", strings.Join(query.Languages(), ", "))
	}
	shaped.report(files)
}

// showTree writes the syntax tree of files, as a query sees it
func showTree(paths []string, lines string, width int) {
	from, to := 0, 0
	if lines != "" {
		first, last, ranged := strings.Cut(lines, "-")
		var err error
		if from, err = strconv.Atoi(first); err != nil || from < 1 {
			fail("--lines takes a line or a range of lines, as in 40-52")
		}
		to = from
		if ranged {
			if to, err = strconv.Atoi(last); err != nil || to < from {
				fail("--lines takes a line or a range of lines, as in 40-52")
			}
		}
	}

	out, done := output(width)
	defer done()
	cwd := workingDir()
	for i, path := range absolute(paths) {
		src, err := os.ReadFile(path)
		if err != nil {
			fail("%v", err)
		}
		text, ok := chunker.SyntaxTree(path, src, from, to)
		if !ok {
			fail("%s is not in a language with a syntax tree", displayPath(path, cwd))
		}
		if len(paths) > 1 {
			if i > 0 {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "## %s\n\n", displayPath(path, cwd))
		}
		fmt.Fprint(out, text)
	}
}
