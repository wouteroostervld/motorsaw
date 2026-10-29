package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/wouteroostervld/motorsaw/pkg/config"
	"github.com/wouteroostervld/motorsaw/pkg/db"
	"github.com/wouteroostervld/motorsaw/pkg/embed"
	"github.com/wouteroostervld/motorsaw/pkg/view"
)

const searchUsage = `Usage: motorsaw search [options] <words>...

Searches by meaning: finds the definitions and sections whose content is
closest to what you describe, whatever words they use. Results are limited
to the current directory unless --all is given.

This uses the embeddings index, which is off by default and needs a model;
"motorsaw status" shows whether it is on here. To find a name or a string,
"motorsaw grep" is exact and needs nothing.

Options:
  --all            Do not limit results to the current directory
  --limit N        Results to return (default 10)
  --expand body    Also show the definition around each result
  --expand file    Also show its file folded to signatures, with the definition in full
  --expand calls   As file, and also what those definitions call
  --no-index       Search the index as it is, without first embedding what changed here
  --oneline        One result on a line: path:lines (the path alone when the
                   file changed), distance and what it is in, separated by
                   tabs, and nothing else
  --width N        Cut lines to N characters

Options may stand before or after the words.

Before a search runs, source files under the current directory that are new
or modified are embedded, when embeddings are switched on for them.

A result in a file that changed after it was embedded, as can happen with
--no-index or --all, is shown without lines: the lines it was at may now
hold something else. "motorsaw outline" gives the lines as they are.

Examples:
  motorsaw search "where are retries handled"
  motorsaw search --expand body --limit 3 parse a config file`

// autoEmbedLimit is the most files a search will embed by itself. Each one is
// a request to a model, so the limit is far lower than for parsing.
const autoEmbedLimit = 100

func handleSearch(args []string) {
	flags := flag.NewFlagSet("search", flag.ExitOnError)
	flags.Usage = func() { fmt.Println(searchUsage) }
	all := flags.Bool("all", false, "")
	limit := flags.Int("limit", 10, "")
	expand := flags.String("expand", "", "")
	noIndex := flags.Bool("no-index", false, "")
	oneline := flags.Bool("oneline", false, "")
	width := flags.Int("width", 0, "")
	given := parse(flags, args)

	if len(given) < 1 {
		fmt.Println(searchUsage)
		os.Exit(1)
	}
	if *limit < 1 {
		fail("--limit must be at least 1")
	}
	query := strings.Join(given, " ")
	out, done := output(*width)
	defer done()

	var mode view.Mode
	if *expand != "" {
		var err error
		if mode, err = view.ParseMode(*expand); err != nil {
			fail("%v", err)
		}
	}

	st, err := loadSettings()
	if err != nil {
		fail("%v", err)
	}
	client, err := st.embedder()
	if err != nil {
		fail("%v", err)
	}
	if client == nil {
		fail("no embedding model is configured.\n\n%s", embeddingsHelp)
	}

	cwd := workingDir()
	database := openDB()
	defer database.Close()
	if !*noIndex {
		autoIndex(database, cwd, config.IndexEmbeddings)
	}

	have, _, err := database.EmbeddingModel()
	if err != nil {
		fail("%v", err)
	}
	if have != "" && have != client.Model() {
		fail("the index holds embeddings made with %s, and %s is configured; run \"motorsaw index .\" to make them again", have, client.Model())
	}

	root := cwd
	if *all {
		root = ""
	}
	if !*oneline {
		fmt.Fprintf(out, "search: %q\n", query)
	}
	var hits []view.Hit
	found := 0
	if have != "" {
		vectors, err := client.Embed(context.Background(), []string{embed.QueryText(client.Model(), query)})
		if err != nil {
			fail("%v", err)
		}
		results, err := database.SearchEmbeddings(vectors[0], *limit, root)
		if err != nil {
			fail("%v", err)
		}
		found = len(results)
		for i, h := range results {
			current := embeddedAsIs(database, h.Path)
			if current {
				// Only lines that still hold what was found can be shown
				hits = append(hits, view.Hit{Path: h.Path, Line: h.StartLine})
			}
			if *oneline {
				where := displayPath(h.Path, cwd)
				if current {
					where += fmt.Sprintf(":%d-%d", h.StartLine, h.EndLine)
				}
				fmt.Fprintf(out, "%s\t%.3f\t%s\n", where, h.Distance, oneLine(h.Title))
				continue
			}
			if i == 0 {
				fmt.Fprintln(out, "results:")
			}
			printValue(out, "  - ", "file", displayPath(h.Path, cwd))
			if current {
				printValue(out, "    ", "lines", fmt.Sprintf("%d-%d", h.StartLine, h.EndLine))
			} else {
				printValue(out, "    ", "changed", "the file changed after it was embedded; motorsaw outline gives its lines")
			}
			if h.Title != "" {
				printValue(out, "    ", "in", h.Title)
			}
			fmt.Fprintf(out, "    distance: %.3f\n", h.Distance)
		}
	}

	if found == 0 {
		if !*oneline {
			fmt.Fprintln(out, "results: []")
		}
		done()
		// Nothing found is most often nothing embedded
		if kinds, err := st.kinds.ForDir(cwd); err == nil && !kinds[config.IndexEmbeddings] {
			fmt.Fprintf(os.Stderr, "note: the embeddings index is not switched on for this directory.\n\n%s\n", embeddingsHelp)
		}
		return
	}
	if !*oneline {
		fmt.Fprintf(out, "total: %d\n", found)
	}

	if mode != "" && len(hits) > 0 {
		fmt.Fprintln(out)
		renderHits(out, hits, mode, cwd, *noIndex)
	}
}

// embeddedAsIs reports whether a file is as it was when its embeddings were
// made, so that the lines they give are still its lines
func embeddedAsIs(database *db.DB, path string) bool {
	f, err := database.GetFile(path)
	if err != nil || f == nil {
		return false
	}
	held, err := database.FileModules(f.ID)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.ModTime().UnixNano() == held[config.IndexEmbeddings]
}
