package chunker

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Query is a tree-sitter query: patterns over the syntax tree, written as
// S-expressions, with captures that name the nodes to report.
//
//	(function_declaration result: (pointer_type) @result)
//
// The kinds of nodes and the names of fields belong to a grammar, so a query
// is compiled for every language apart, and searches the files of the
// languages whose grammar it fits.
type Query struct {
	compiled map[*langSpec]*sitter.Query
}

// Capture is a node that a query captured
type Capture struct {
	Name  string
	Start int // Byte offset into the file
	End   int // Exclusive
}

// QueryLanguages are the names of the languages a query can be written for
func QueryLanguages() []string {
	var names []string
	for _, spec := range grammars() {
		names = append(names, spec.name)
	}
	return names
}

// grammars are the languages that are parsed with tree-sitter, by name
func grammars() []*langSpec {
	seen := make(map[*langSpec]bool)
	var specs []*langSpec
	add := func(spec *langSpec) {
		if spec.grammar != nil && !seen[spec] {
			seen[spec] = true
			specs = append(specs, spec)
		}
	}
	for _, spec := range extensions {
		add(spec)
	}
	for _, shared := range candidates {
		for _, spec := range shared {
			add(spec)
		}
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].name < specs[j].name })
	return specs
}

// NewQuery compiles a query for the named language, or for every language
// whose grammar it fits when no language is named. Of a query with several
// patterns, a language takes those that fit it, so that one query can say the
// same thing in the words of more than one grammar. A pattern that captures
// nothing is reported as a whole.
func NewQuery(source, language string) (*Query, error) {
	q := &Query{compiled: make(map[*langSpec]*sitter.Query)}
	patterns := splitPatterns(source)
	rejected := make(map[string][]string) // Reason -> languages
	var reasons []string
	known := false
	for _, spec := range grammars() {
		if language != "" && spec.name != language {
			continue
		}
		known = true
		compiled, err := compileQuery(spec, source, patterns)
		if err != nil {
			if err.Kind == sitter.QueryErrorSyntax {
				// Wrong in every language alike
				q.Close()
				return nil, fmt.Errorf("%s", err.Error())
			}
			// Languages that reject the query for the same reason share a line
			reason := err.Error()
			if _, known := rejected[reason]; !known {
				reasons = append(reasons, reason)
			}
			rejected[reason] = append(rejected[reason], spec.name)
			continue
		}
		q.compiled[spec] = compiled
	}
	if !known {
		return nil, fmt.Errorf("unknown language %q; known are %s", language, strings.Join(QueryLanguages(), ", "))
	}
	if len(q.compiled) == 0 {
		var errs []string
		for _, reason := range reasons {
			errs = append(errs, strings.Join(rejected[reason], ", ")+": "+reason)
		}
		return nil, fmt.Errorf("the query fits no language:\n  %s", strings.Join(errs, "\n  "))
	}
	return q, nil
}

// compileQuery compiles for one language the patterns of a query that fit
// it. The error, when none does, is that of the query as it was written.
func compileQuery(spec *langSpec, source string, patterns []string) (*sitter.Query, *sitter.QueryError) {
	whole, wholeErr := sitter.NewQuery(spec.language(), source)
	if wholeErr == nil {
		whole.Close()
	} else if wholeErr.Kind == sitter.QueryErrorSyntax {
		return nil, wholeErr
	}

	var fitting []string
	for _, pattern := range patterns {
		compiled, err := sitter.NewQuery(spec.language(), pattern)
		if err != nil {
			continue
		}
		if len(compiled.CaptureNames()) == 0 {
			pattern += " @match"
		}
		compiled.Close()
		fitting = append(fitting, pattern)
	}
	if len(fitting) == 0 {
		return nil, wholeErr
	}
	return sitter.NewQuery(spec.language(), strings.Join(fitting, "\n"))
}

// splitPatterns returns the patterns of a query, each with what follows it:
// its quantifier and its captures. Comments are left out.
func splitPatterns(source string) []string {
	var patterns []string
	var current strings.Builder
	depth := 0
	flush := func() {
		if pattern := strings.TrimSpace(current.String()); pattern != "" {
			patterns = append(patterns, pattern)
		}
		current.Reset()
	}
	for i := 0; i < len(source); i++ {
		c := source[i]
		switch {
		case c == ';':
			for i < len(source) && source[i] != '\n' {
				i++
			}
			current.WriteByte('\n')
			continue
		case c == '"':
			if depth == 0 {
				flush()
			}
			start := i
			for i++; i < len(source) && source[i] != '"'; i++ {
				if source[i] == '\\' {
					i++
				}
			}
			current.WriteString(source[start:min(i+1, len(source))])
			continue
		case c == '(' || c == '[':
			if depth == 0 {
				flush()
			}
			depth++
		case c == ')' || c == ']':
			depth--
		}
		current.WriteByte(c)
	}
	flush()
	return patterns
}

// Close releases the compiled query
func (q *Query) Close() {
	for _, compiled := range q.compiled {
		compiled.Close()
	}
	q.compiled = nil
}

// Languages are the names of the languages the query fits
func (q *Query) Languages() []string {
	var names []string
	for spec := range q.compiled {
		names = append(names, spec.name)
	}
	sort.Strings(names)
	return names
}

// Fits reports whether the query can match in a file, going by its language
func (q *Query) Fits(path string) bool {
	for _, spec := range specsFor(path) {
		if q.compiled[spec] != nil {
			return true
		}
	}
	return false
}

// Find returns the nodes the query captures in a file, in file order. A
// capture whose name begins with an underscore only serves the query itself,
// as the subject of a predicate, and is left out. Find may be called from
// several goroutines at once.
func (q *Query) Find(path string, src []byte) []Capture {
	if !q.Fits(path) {
		return nil
	}
	spec, tree := parseFile(path, src)
	if tree == nil {
		return nil
	}
	defer tree.Close()
	compiled := q.compiled[spec]
	if compiled == nil {
		return nil
	}

	cursor := sitter.NewQueryCursor()
	defer cursor.Close()
	names := compiled.CaptureNames()
	seen := make(map[Capture]bool)
	var found []Capture
	matches := cursor.Matches(compiled, tree.RootNode(), src)
	for m := matches.Next(); m != nil; m = matches.Next() {
		for _, c := range m.Captures {
			name := names[c.Index]
			if strings.HasPrefix(name, "_") {
				continue
			}
			capture := Capture{Name: name, Start: int(c.Node.StartByte()), End: int(c.Node.EndByte())}
			if !seen[capture] {
				seen[capture] = true
				found = append(found, capture)
			}
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Start < found[j].Start })
	return found
}

// maxLeafText is how much of the text of a leaf is shown in a syntax tree
const maxLeafText = 40

// SyntaxTree writes out the syntax tree of a file as a query sees it: a named
// node on a line, with the field it is in, its kind and its lines, and its
// text when nothing is below it; a keyword or sign on a line, between quotes,
// as a query writes it. Only nodes that touch the lines from..to
// (1-indexed, inclusive) are shown; 0 leaves that side open. It returns false
// when the language is not parsed with tree-sitter.
func SyntaxTree(path string, src []byte, from, to int) (string, bool) {
	if spec := specFor(path); spec == nil || spec.grammar == nil {
		return "", false
	}
	_, tree := parseFile(path, src)
	if tree == nil {
		return "", false
	}
	defer tree.Close()

	cursor := tree.Walk()
	defer cursor.Close()
	var out strings.Builder
	var visit func(depth int)
	visit = func(depth int) {
		n := cursor.Node()
		first, last := int(n.StartPosition().Row)+1, int(n.EndPosition().Row)+1
		if n.EndPosition().Column == 0 && last > first {
			last--
		}
		if (to > 0 && first > to) || last < from {
			return
		}
		if n.IsNamed() {
			out.WriteString(strings.Repeat("  ", depth))
			if field := cursor.FieldName(); field != "" {
				out.WriteString(field + ": ")
			}
			out.WriteString("(" + n.Kind() + ")")
			if n.NamedChildCount() == 0 {
				text := strings.Join(strings.Fields(string(src[n.StartByte():n.EndByte()])), " ")
				if runes := []rune(text); len(runes) > maxLeafText {
					text = string(runes[:maxLeafText-1]) + "…"
				}
				out.WriteString(" " + text)
			}
			if first == last {
				fmt.Fprintf(&out, "  [%d]\n", first)
			} else {
				fmt.Fprintf(&out, "  [%d-%d]\n", first, last)
			}
			depth++
		} else if !n.IsMissing() {
			// A keyword or sign has no kind of its own: a query names it by
			// its text, between quotes, and so it is shown
			out.WriteString(strings.Repeat("  ", depth))
			if field := cursor.FieldName(); field != "" {
				out.WriteString(field + ": ")
			}
			fmt.Fprintf(&out, "%s  [%d]\n", strconv.Quote(n.Kind()), first)
		}
		if cursor.GotoFirstChild() {
			for {
				visit(depth)
				if !cursor.GotoNextSibling() {
					break
				}
			}
			cursor.GotoParent()
		}
	}
	visit(0)
	return out.String(), true
}
