package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wouteroostervld/motorsaw/pkg/config"
	"github.com/wouteroostervld/motorsaw/pkg/cypher"
	"github.com/wouteroostervld/motorsaw/pkg/view"
)

const queryUsage = `Usage: motorsaw query [options] <cypher>

Queries the code graph: definitions and how they relate. Results are limited
to paths that touch the current directory unless --all is given.

  MATCH (f)-[:calls]->(t) WHERE t.name = "Open" RETURN f.name, f.file, f.lines
  MATCH (n:FUNCTION|METHOD) WHERE n.file CONTAINS "/indexer/" RETURN n.signature
  MATCH (a {name: "main"})-[:calls*1..3]->(b) RETURN DISTINCT b.name, b.file
  MATCH (s)-[:implements]->(i {name: "Database"}) RETURN s.name
  MATCH (f)-[:calls]->(t) RETURN t.name, count(f) AS callers ORDER BY callers DESC LIMIT 10

Patterns:
  (v:LABEL|LABEL {name: "x"})   A node; labels are entity types
  -[:type|type]->  <-[:type]-   An edge, in either direction; -[r:type]- names it
  -[:calls*1..3]->  -[*]->      A path of several edges (up to 5 when no limit is given)

Entity types:   FUNCTION METHOD CLASS STRUCT INTERFACE TYPE CONSTANT VARIABLE
                PACKAGE, EXTERNAL for what is called but not in the index
Relation types: calls, may_call (matched by name only, or dispatched through
                an interface), implements, imports, defines

Node properties: name, type, file, line, end_line, lines, signature,
                 container, short_name, snippet
Edge properties: type, weight; depth for a path of several edges

WHERE:  = <> < > <= >=, CONTAINS, STARTS WITH, ENDS WITH, =~ (regular
        expression), IN [...], IS NULL, IS NOT NULL, AND, OR, NOT, ( )
RETURN: v, v.prop, count(*), count(v), count(DISTINCT v.prop), AS, DISTINCT
Then:   ORDER BY ... [DESC], SKIP n, LIMIT n

Options:
  --all            Do not limit results to the current directory
  --limit N        Rows to return when the query has no LIMIT (default 100)
  --expand body    Also show the definition of each returned node
  --expand file    Also show its file folded to signatures, with the definition in full
  --expand calls   As file, and also what those definitions call
  --no-index       Query the index as it is, without first indexing what changed here
  --sql            Print the SQL the query is translated to
  --oneline        One row on a line, its values separated by tabs, and nothing
                   else: no names of columns, no total
  --width N        Cut lines to N characters

Options may stand before or after the query.

Before a query runs, source files under the current directory that are new
or modified are indexed, and the index forgets files that are gone from disk.`

func handleQuery(args []string) {
	flags := flag.NewFlagSet("query", flag.ExitOnError)
	flags.Usage = func() { fmt.Println(queryUsage) }
	all := flags.Bool("all", false, "")
	limit := flags.Int("limit", 100, "")
	expand := flags.String("expand", "", "")
	showSQL := flags.Bool("sql", false, "")
	noIndex := flags.Bool("no-index", false, "")
	oneline := flags.Bool("oneline", false, "")
	width := flags.Int("width", 0, "")
	given := parse(flags, args)

	if len(given) < 1 {
		fmt.Println(queryUsage)
		os.Exit(1)
	}
	query := strings.Join(given, " ")
	out, done := output(*width)
	defer done()

	opts := cypher.Options{DefaultLimit: *limit}
	cwd := workingDir()
	if !*all {
		opts.CWD = cwd
	}
	var mode view.Mode
	if *expand != "" {
		var err error
		if mode, err = view.ParseMode(*expand); err != nil {
			fail("%v", err)
		}
		opts.Locations = true
	}

	result, err := cypher.Transpile(query, opts)
	if err != nil {
		fail("%v", err)
	}
	if *showSQL {
		fmt.Fprintf(out, "%s\n-- args: %v\n\n", result.SQL, result.Args)
	}

	database := openDB()
	defer database.Close()
	stale := ""
	if !*noIndex {
		stale = autoIndex(database, cwd, config.IndexGraph)
	}

	rows, err := database.RawQuery(result.SQL, result.Args...)
	if err != nil {
		fail("running the query: %v\n%s", err, result.SQL)
	}
	defer rows.Close()

	// Each returned node is followed by where it is: path, start and end line
	values := make([]any, len(result.Columns)+3*len(result.Locations))
	ptrs := make([]any, len(values))
	for i := range values {
		ptrs[i] = &values[i]
	}

	var hits []view.Hit
	count := 0
	if !*oneline {
		fmt.Fprintf(out, "query: %q\n", query)
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			fail("reading a row: %v", err)
		}
		if count == 0 && !*oneline {
			fmt.Fprintln(out, "results:")
		}
		count++

		for i, column := range result.Columns {
			if *oneline {
				if i > 0 {
					fmt.Fprint(out, "\t")
				}
				fmt.Fprint(out, cell(values[i]))
				continue
			}
			prefix := "    "
			if i == 0 {
				prefix = "  - "
			}
			printValue(out, prefix, column, values[i])
		}
		if *oneline {
			fmt.Fprintln(out)
		}

		for i := range result.Locations {
			at := values[len(result.Columns)+3*i:]
			path, isPath := text(at[0])
			line, isLine := at[1].(int64)
			if isPath && isLine {
				hits = append(hits, view.Hit{Path: path, Line: int(line)})
			}
		}
	}
	if err := rows.Err(); err != nil {
		fail("reading rows: %v", err)
	}

	stopped := ""
	if result.Limited && count == *limit {
		stopped = fmt.Sprintf("note: stopped at %d rows; add LIMIT to the query or pass --limit for more", *limit)
	}
	switch {
	case *oneline && stopped != "":
		// Only rows on standard output, so that every line of it is one
		done()
		fmt.Fprintln(os.Stderr, stopped)
	case *oneline:
	case count == 0:
		fmt.Fprintln(out, "results: []")
	default:
		fmt.Fprintf(out, "total: %d\n", count)
		if stopped != "" {
			fmt.Fprintln(out, stopped)
		}
	}
	// Said on standard error already, and here again where the results are
	// read: an answer from a stale index looks like any other. Not among
	// rows that are one a line, which are for another program.
	if stale != "" && !*oneline {
		fmt.Fprintf(out, "warning: %s\n", stale)
	}
	if count == 0 {
		return
	}

	if mode != "" && len(hits) > 0 {
		fmt.Fprintln(out)
		// The index was brought up to date above
		renderHits(out, hits, mode, cwd, true)
	}
}

func text(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	}
	return "", false
}

// cell is a value as it stands in a row that is one line
func cell(v any) string {
	if v == nil {
		return ""
	}
	if s, isText := text(v); isText {
		return oneLine(s)
	}
	return fmt.Sprint(v)
}

// printValue writes one field of a result row as YAML
func printValue(out io.Writer, prefix, name string, v any) {
	s, isText := text(v)
	switch {
	case v == nil:
		fmt.Fprintf(out, "%s%s: null\n", prefix, name)
	case !isText:
		fmt.Fprintf(out, "%s%s: %v\n", prefix, name, v)
	case strings.Contains(s, "\n"):
		fmt.Fprintf(out, "%s%s: |\n", prefix, name)
		for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			fmt.Fprintf(out, "      %s\n", line)
		}
	case s == "" || strings.ContainsAny(s, ":#\"'{}[]*&!|>%@`") || strings.TrimSpace(s) != s:
		fmt.Fprintf(out, "%s%s: %q\n", prefix, name, s)
	default:
		fmt.Fprintf(out, "%s%s: %s\n", prefix, name, s)
	}
}
