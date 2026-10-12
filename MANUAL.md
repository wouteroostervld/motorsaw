# Chainsaw Manual

Chainsaw looks at code through its syntax tree. It has three ways of looking,
`grep`, `outline` and `query`, and one way of showing what it found in more
or less context, `--expand`. A fourth way of looking, `search`, finds code by
what it means; it needs a model and is off until you switch it on.

## Contents

- [Installation](#installation)
- [grep](#grep): search, with the definition around each match
- [outline](#outline): a file folded to its signatures
- [query](#query): the code graph
- [search](#search): by meaning, when embeddings are switched on
- [Showing code: --expand](#showing-code---expand)
- [The index](#the-index)
- [Kinds of index](#kinds-of-index): what is on, and where
- [The graph](#the-graph)
- [Configuration](#configuration)
- [Troubleshooting](#troubleshooting)

## Installation

```bash
git clone https://github.com/wouteroostervld/chainsaw.git
cd chainsaw
make install          # builds to ~/.local/bin/chainsaw
```

Needs Go 1.24.7+, a C compiler and the PCRE2 development files
(`pcre2-devel` on Fedora, `libpcre2-dev` on Debian and Ubuntu). Nothing else:
there is no model to pull and no service to run, unless you switch on
`search`. `grep` and `outline` work straight away; `query` indexes what it
needs by itself.

## grep

```bash
chainsaw grep [options] <pattern> [path...]
```

Searches files with a Perl-compatible regular expression (PCRE2: lookaround,
backreferences, `\b`, `(?i)`, and so on) and groups the matching lines by the
definition they are in.

```
$ chainsaw grep 'relation_type\s+(IN|=)' pkg/db
pkg/db/chunks.go
  METHOD DB.Callees  (lines 95-123)
    102: JOIN graph_edges g ON g.source_entity_id = sd.entity_id AND g.relation_type = ?
pkg/db/symbols.go
  FUNCTION resolveSymbols  (lines 197-285)
    233: WHERE source_entity_id = ? AND chunk_id = ? AND relation_type IN (?, ?)
```

Each group names the innermost definition around the lines: its type
(`FUNCTION`, `METHOD`, `STRUCT`, `INTERFACE`, `CLASS`, `TYPE`), its qualified
name (`Type.method`), and where it starts and ends. Lines outside any
definition, and matches in files that cannot be parsed, are listed without
one.

| Option | |
|---|---|
| `-i` | Ignore case |
| `-F` | The pattern is a literal string |
| `-w` | Match whole words only |
| `-s` | Only list the definitions that contain a match, with a count |
| `--limit N` | Show the first N matching lines only; with `-s`, the first N definitions |
| `--oneline` | One match on a line: `path:line: [definition] text` |
| `--width N` | Cut lines to N characters |
| `--no-ignore` | Also search what git ignores |
| `--lang LANGUAGE` | Read files as this language, see [outline](#outline) |
| `--expand body\|file\|calls` | Show the code, see [below](#showing-code---expand) |

Without a path the current directory is searched. Hidden directories, the
`exclude` directories of your configuration and what git ignores are
skipped, as they are by `chainsaw index` and the daemon (see [What is left
out](#what-is-left-out)); a file or directory named explicitly is always
searched. `^` and `$` match at line ends. No index is
needed, so results are always current.

`-s` is the quick way to answer "which functions touch this": it is the list
to work through when something has to change everywhere.

`--limit` is for a pattern that may match a great deal. Unlike cutting the
output with `head`, it still says how much there is:

```
$ chainsaw grep -s --limit 3 'err' pkg/db
pkg/db/chunks.go
  METHOD DB.InsertChunk  (lines 5-20): 7
  METHOD DB.DeleteChunksForFile  (lines 22-48): 10
  METHOD DB.Count  (lines 59-88): 8

663 matching lines in 12 files, the first 3 definitions shown
```

`--oneline` is for handing the matches to another program. Every line is one
match, in the form editors and other tools read, and nothing else is written:
the note that `--limit` stopped the list goes to standard error.

```
$ chainsaw grep --oneline 'ForgetFile' cmd
cmd/chainsaw/index.go:295: [syncIndex] if err := idx.ForgetFile(f.ID, path); err != nil {
$ chainsaw grep -s --oneline 'ForgetFile' pkg/db/files.go
pkg/db/files.go:104-117: [DB.ForgetFile] 2
```

Options may stand before or after the pattern and the paths, here and in the
other commands. Put `--` before a pattern that begins with a dash:
`chainsaw grep -F -- '--limit'`.

## outline

```bash
chainsaw outline [options] <file>...
```

Prints a file folded to its skeleton. Every definition of five lines or more
is reduced to its signature and a reference to the lines of its body:

```go
package indexer

func New(cfg *Config, database db.Database) *Indexer // ... chunk: lines 44-54

func (idx *Indexer) IndexFile(ctx context.Context, filePath string) error // ... chunk: lines 56-109
```

This repeats inside: a class lists its methods, a method its nested
functions. Markdown folds along its headings.

| Option | |
|---|---|
| `--expand NAME` | Show this definition in full: a qualified name, a bare name, or a line number. Repeatable. |
| `--body NAME` | Show this definition in full and nothing else. Repeatable. |
| `--symbols` | List the definitions instead: type, name, lines, signature |
| `--width N` | Cut lines to N characters |
| `--lang LANGUAGE` | Read the files as this language, whatever their names say |

The language of a file is told by its extension. A file without one, and a
`.h`, is read as BCPL when its content shows that it is. Where neither says,
name the language: `chainsaw outline --lang bcpl fragment`. `grep --lang`
does the same for the files it is given by name, and under a directory for
those whose name does not say what they are. The languages are `bcpl`, `c`,
`cpp`, `go`, `html`, `java`, `javascript`, `m68k`, `markdown`, `python`,
`ruby`, `rust`, `tsx`, `typescript` and `xml`.

A name that is not in the file is an error on standard error, with exit
status 1.

`--expand` shows a definition in its place in the folded file; `--body` shows
the definition alone, under its file and lines:

````
$ chainsaw outline --body cut cmd/chainsaw/output.go
## cmd/chainsaw/output.go:71-79 cut

```go
// cut shortens a line to a number of characters; the last one is then an
// ellipsis, to show that there was more
func cut(line string, width int) string {
	...
}
```
````

Several files are shown one after the other, each under a heading with its
name. A name given to `--expand` or `--body` is then looked for in all of
them.

## query

```bash
chainsaw query [options] '<cypher>'
```

Queries the graph of definitions and how they relate, in a subset of Cypher.

```cypher
// Who calls this function?
MATCH (f)-[:calls]->(t) WHERE t.name = "ext4_orphan_add" RETURN f.name, f.file, f.lines

// What does this one call, up to three levels deep?
MATCH (a {name: "handleIndex"})-[:calls*1..3]->(b) RETURN b.name, b.signature

// Which types implement an interface, and what do calls through it reach?
MATCH (s)-[:implements]->(i {name: "Database"}) RETURN s.name
MATCH (f)-[:may_call]->(t) WHERE t.name ENDS WITH ".StoreFileSymbols" RETURN f.name, t.name

// The functions of a directory, with their signatures
MATCH (n:FUNCTION|METHOD) WHERE n.file CONTAINS "/indexer/" RETURN n.signature, n.lines

// Most-called functions
MATCH (f)-[:calls]->(t) RETURN t.name, count(f) AS callers ORDER BY callers DESC LIMIT 10

// What is called but not defined in the index
MATCH (f)-[:calls]->(t:EXTERNAL) RETURN t.name, count(f) AS n ORDER BY n DESC LIMIT 20
```

### Patterns

| | |
|---|---|
| `(v)` `(v:LABEL)` `(v:A\|B)` | A node; labels are entity types, not case-sensitive |
| `(v {name: "x"})` | A node with a property equal to a value |
| `-[:type]->` `<-[:type]-` `-[:type]-` | An edge, forward, backward or either way |
| `-[:a\|b]->` | Any of several relation types |
| `-[r:type]->` | Names the edge so its properties can be used |
| `-[:calls*1..3]->` `-[:calls*2]->` `-[*]->` | A path of several edges; up to 5 when no limit is given |
| `-->` `<--` `--` | An edge of any type |

A query matches one path: `(a)-[...]->(b)-[...]->(c)`, of any length, or a
single node.

### Properties

| Node | |
|---|---|
| `name` | Qualified name: `Open`, `Store.Close` |
| `type` | Entity type |
| `file` | Path of the file it is defined in; null for `EXTERNAL` |
| `line`, `end_line`, `lines` | Where the definition starts and ends; `lines` is `"12-40"` |
| `signature` | The definition up to its body |
| `container`, `short_name` | The two halves of a method's name |
| `snippet` | The code of the definition, read from the file as it is now |

| Edge | |
|---|---|
| `type` | Relation type |
| `weight` | 1.0 for an exact edge, 0.5 for one matched by name |
| `depth` | Length of a path of several edges |

### Clauses

- `WHERE` with `=` `<>` `<` `>` `<=` `>=`, `CONTAINS`, `STARTS WITH`,
  `ENDS WITH`, `=~` (regular expression), `IN [...]`, `IS NULL`,
  `IS NOT NULL`, combined with `AND`, `OR`, `NOT` and parentheses
- `RETURN` a node (`v` gives name, type, file and lines), a property, or
  `count(*)`, `count(v)`, `count(DISTINCT v.prop)`; `AS` to name a column;
  `DISTINCT`. With a count, the other columns are what is grouped by.
- `ORDER BY ... [DESC]`, `SKIP n`, `LIMIT n`

### Options

| Option | |
|---|---|
| `--all` | Do not limit results to the current directory |
| `--limit N` | Rows to return when the query has no `LIMIT` (default 100) |
| `--expand body\|file\|calls` | Also show the code of the returned nodes |
| `--no-index` | Query the index as it is, without first indexing what changed in the current directory |
| `--sql` | Print the SQL the query is translated to |
| `--oneline` | One row on a line, its values separated by tabs |
| `--width N` | Cut lines to N characters |

`--oneline` writes the rows and nothing else: no names of columns and no
total, and a value of several lines with `\n` between them. It is the form
to hand to `sort`, `cut` or a loop in the shell.

```
$ chainsaw query --oneline 'MATCH (f)-[:calls]->(t {name: "displayPath"}) RETURN f.name, f.lines'
printMatches	185-204
handleOutline	44-124
```

By default a result must touch the current directory: at least one node of
the matched path is defined under it. `chainsaw graph query` is accepted as
an older spelling of `chainsaw query`.

## search

```bash
chainsaw search "where are retries handled"
chainsaw search --expand body --limit 3 parse a config file
```

Finds the definitions and sections whose content is closest in meaning to
what you describe, whatever words they use. It is for when you have a
description and no name; when you have a name or a string, `grep` is exact
and needs nothing.

`search` uses the `embeddings` index, which is **off by default**: see
[Kinds of index](#kinds-of-index) for switching it on. Without a model
configured it says so and stops.

```
$ chainsaw search --limit 2 "send a request to the model and read the vectors"
search: "send a request to the model and read the vectors"
results:
  - file: pkg/embed/embed.go
    lines: 60-104
    in: Client.Embed
    distance: 0.318
  - file: pkg/embed/embed_test.go
    lines: 17-43
    in: TestEmbed_Ollama
    distance: 0.326
total: 2
```

`distance` is the cosine distance between your words and the chunk: 0 is the
same, 1 is unrelated. A result without `in` is a file as a whole, folded to
its signatures.

| Option | |
|---|---|
| `--all` | Do not limit results to the current directory |
| `--limit N` | Results to return (default 10) |
| `--expand body`, `file`, `calls` | Also show the code, as for `grep` and `query` |
| `--no-index` | Search the index as it is, without first embedding what changed here |
| `--oneline` | One result on a line: `path:lines`, distance and what it is in, separated by tabs |
| `--width N` | Cut lines to N characters |

Before a search runs, files under the current directory that are new or
modified are embedded, when embeddings are on for them. More than 100 files
are left for `chainsaw index .`, since every file is a request to the model.

## Showing code: --expand

`grep` and `query` find places. `--expand` shows the code at those places,
in one of three amounts:

| | |
|---|---|
| `body` | The whole definition around each place. Nested definitions stay folded. |
| `file` | The file folded as by `outline`, with those definitions in full. All places in one file share one view. |
| `calls` | As `file`, and also what those definitions call: in place when it is in the same file, after it when it is in another. Uses the index, and follows `calls` edges only. |

```bash
chainsaw grep --expand body 'filepath\.SkipDir' cmd
chainsaw query --expand file 'MATCH (f)-[:calls]->(t {name: "nearest"}) RETURN f'
```

`body` is the smallest and usually enough. `file` shows where the code sits.
`calls` is for following the code without opening more files.

## The index

```bash
chainsaw index <path>...      # parse and record files, or the files under directories
chainsaw index --force <path> # also files that have not changed
chainsaw index --only graph . # one kind of index only (graph, embeddings)
chainsaw index --no-ignore .  # also what git ignores
chainsaw forget <path>...     # make the index forget the files under a path
chainsaw forget --ignored     # ... or only what is left out here by now
chainsaw status               # what is in the index
chainsaw daemon start         # watch the configured directories for changes
```

**You rarely need any of these.** Before `chainsaw query` runs, and before
`--expand calls` looks up what is called, the source files under the current
directory that are new or modified are indexed, and the index forgets files
that are gone from disk (reported as `removed file detected: <file>`).
Chainsaw never deletes or changes a source file.
A first query on a project that was never indexed just takes a moment longer
(this repository: about two seconds) and says so on standard error. Results
never come from stale files. `--no-index` skips the step.

A file counts as modified when its modification time differs from when it
was indexed. When more than 1,000 files need indexing the step is skipped
with a note, because it would take too long to do in passing; run
`chainsaw index .` once for a tree that size.

`chainsaw index` does the same by hand, with no limit and for paths outside
the current directory. It takes seconds: the Linux kernel's `fs/ext4` (70,000
lines) in about two. It accepts any number of files and directories, and
reads more from standard input when given `-`, so it fits in a pipeline:

```bash
chainsaw index pkg cmd/chainsaw/main.go
git diff --name-only | chainsaw index -
find . -name '*.go' -newer Makefile -print0 | xargs -0 chainsaw index
find . -name '*.py' -print0 | chainsaw index -0 -
```

A file named directly is indexed whatever the configuration excludes; one
that is not in a parseable language is passed over, so a pipeline does not
have to filter. A path that does not exist is reported, the rest is still
indexed, and the exit status is 1.

The daemon is optional as well. It watches the `include` directories of your
configuration and re-indexes files as they change. Watching only shows what
changes while it runs, so when it starts it first scans those directories as
`chainsaw index` would: new and modified files are indexed, and the index
forgets files that are gone. The scan runs beside the watching and fills the
graph only; it makes embeddings when you name them:

```yaml
profiles:
  default:
    daemon:
      scan: true                        # false: only index what changes from now on
      scan_kinds: [graph, embeddings]   # default: [graph]
```

`chainsaw daemon start -scan=false` and `-scan-kinds graph,embeddings` do the
same for one run, and win over the configuration. A kind that is named is
still only built where it is switched on. Files that change while the daemon
runs are indexed in every kind that is on for them, whatever the scan does.

Before it scans, the daemon makes the index forget the files under the
`include` directories that are [left out](#what-is-left-out): those in a
directory that is hidden or in `exclude`, those the `blacklist` rejects, and
what git ignores. So a
directory you exclude after it was indexed leaves the index, and with it a
file there that you indexed by name. What you indexed by hand outside the
`include` directories is left alone. No file on disk is touched.

After changing `~/.chainsaw/config.yaml`, send the daemon a SIGHUP, with
`systemctl --user reload chainsawd` when it runs as the service of
`examples/chainsawd.service`, or with `kill -HUP`. It is a warm restart: the
daemon reads the configuration again, sets its watches to the directories
that are included now (also the ones made since it started), prunes, and
scans as when it starts. A configuration that cannot be used is reported in
the log and the daemon carries on with the one it had. The flags it was
started with still hold.

### What is left out

A walk through a directory, by `index`, a query that indexes in passing,
`grep` or the daemon, passes by:

- hidden directories;
- the `exclude` directories of your configuration, and the files its
  `blacklist` rejects;
- what the `.config.yaml` of a directory excludes or blacklists, for the
  tree below it;
- what git ignores: the `.gitignore` files of a repository, at every depth,
  and its `.git/info/exclude`. That is what is built, downloaded or
  generated: `build/`, `.venv/`, a toolchain under `tools/`.

All of these go by the part of a path below where the walk starts, so a
file or a directory that you name is indexed or searched whatever it is in:
`chainsaw index build/generated` works though `build/` is ignored.

To keep what git ignores, say `gitignore: false`, in the profile for
everything or in the `.config.yaml` of a project for the tree below it;
`--no-ignore` on `index` and `grep` does it for one run. The ignore file of
your git configuration (`core.excludesFile`) is not read. A file that is
tracked although a pattern names it counts as ignored. A repository that is
your home directory itself is passed over.

```yaml
# ~/projects/kick_reverse/.config.yaml
gitignore: false            # build/ holds sources worth indexing
exclude: [tools, .venv]     # so say here what is not
```

`chainsaw index` does not take out of the index what came to be left out
after it was indexed: it only forgets files that are gone from disk. The
daemon does, when it starts and on SIGHUP (below). Without a daemon, or
right now:

```bash
chainsaw forget -n --ignored   # what would go, counted by directory
chainsaw forget --ignored      # forget what a walk from here leaves out now
chainsaw forget tools build    # forget everything under these, whatever it is
```

`forget` touches nothing on disk, and `chainsaw index` brings back what it
is asked for.

### When the index is busy

One process writes to the index at a time. A command that has to write
while another does, a query that indexes in passing while the daemon scans,
waits for it: ten seconds, and then for as long as it has patience, asking
again a little longer apart each time. That is five minutes for `index`,
`forget` and the scan of the daemon, and five seconds for a query. It says
once that it waits.

A query that could not bring the index up to date answers from the index as
it was, and says so twice: in a note on standard error, and in a line
`warning: ...` after the results. With `--oneline`, standard output holds
rows only and the note is on standard error alone.

The index is `~/.chainsaw/graph.db`. It holds names, locations, edges and,
when embeddings are on, vectors. It holds no source text: the code a query
or a search shows (`snippet`, `--expand`) is read from the files when it is
asked for. It can be deleted at any time.

## Kinds of index

The index is made of independent parts. Each is built from the same parse of
a file, stores its own data, and can be switched on or off without touching
the others.

| Kind | Holds | Used by | Cost | Default |
|---|---|---|---|---|
| `graph` | Definitions, calls and imports | `query`, `--expand calls` | Parsing: milliseconds a file | on |
| `embeddings` | A vector for each chunk of code | `search` | A request to a model for every file | off |

`grep` and `outline` use no index at all.

**Switching a kind on or off** is done under `index:`, in the profile of
`~/.chainsaw/config.yaml` for everything, or in a `.config.yaml` in a
directory for the files below it. The nearest `.config.yaml` above a file
decides; a kind it does not mention keeps the setting of the profile.

```yaml
# ~/projects/big-service/.config.yaml
index:
  embeddings: true     # search by meaning in this project
```

```yaml
# ~/projects/vendored-thing/.config.yaml
index:
  graph: false         # not worth a graph
```

**Embeddings need a model**, and that is only ever read from your own
`~/.chainsaw/config.yaml`: a project can ask for embeddings, it cannot say
where your source is sent.

```yaml
profiles:
  default:
    embeddings:
      model: nomic-embed-text            # Ollama on http://localhost:11434
```

```yaml
    embeddings:                          # or any server that speaks the OpenAI API
      base_url: https://api.openai.com/v1
      api_key: sk-...
      model: text-embedding-3-small
```

`provider: ollama` or `provider: openai` says which protocol to speak when it
does not follow from the rest (an `api_key` or an address ending in `/v1`
means OpenAI). Naming a model switches nothing on; `index: {embeddings:
true}` does. With embeddings on and no model, `chainsaw index` stops and
says so, and `query` carries on as before.

**The kinds are kept current separately.** A command brings up to date only
the kind it uses, so `query` never waits for a model, and `chainsaw index`
does all that are on. Switching `embeddings` on later embeds the files
without building the graph again; a model that is away leaves the graph
built, and the embeddings are tried again next time. After three files in a
row that the model fails on, a run stops rather than repeat the error for
every file.

**Switching a kind off** for a file drops what that kind held of it, the next
time `chainsaw index` or a command that uses that kind comes by. For
embeddings that is work thrown away, so switch them off where you mean it.

**Changing the model** makes the stored vectors useless: vectors of two
models cannot be compared. `chainsaw search` says so and stops; the next
`chainsaw index` drops them and embeds again with the new model.

`chainsaw status` shows which kinds are on in the current directory, how
many files each holds, and which model the embeddings were made with.

What is embedded is the same chunks the outline folds a file into: each
definition, and the file folded to its signatures, each with its file and
name in front. Only where a chunk is is stored, with its vector.

## The graph

Every definition is an entity; every call and import in the source is an
edge.

**Entity types:** `FUNCTION`, `METHOD` (named `Type.method`), `CLASS`,
`STRUCT`, `INTERFACE`, `TYPE`, `CONSTANT` and `VARIABLE` (in BCPL: the
entries of `MANIFEST`, and of `GLOBAL` and `STATIC`), `PACKAGE` (a file's
package or module, and what it imports), and `EXTERNAL` for something that
is called but not defined in the index, such as `fmt.Println`.

**Relation types:**

- `calls`: a call whose target is known
- `may_call`: a call that may end up there
- `implements`: a type implements an interface (Go)
- `imports`: a file imports a package
- `defines`: a package defines its functions and types, a class its methods

How exact `calls` is depends on the language.

**Go** files are run through the Go type checker. Every call has its exact
target, whatever variable, field or package it is reached through. A call of
an interface method is a `calls` edge to that method of the interface, plus a
`may_call` edge to the method of each type that implements it. So to find
everything that can reach an implementation, ask for `[:calls|may_call]`.
Calls of function values have no target and are left out. Implementations
are looked for in the package and what it imports; the `implements` edges
find the rest. Dependencies are located with the `go` command; without it,
calls into them fall back to the rules below.

**The other languages** have names, not types. An edge is `calls` when the
target follows from the source: a function in the same directory, a name
reached through an import, a method on `self`/`this` or on a named type.
When only the name is known, as with a method call on a variable, the edge is
`may_call` and goes to every definition in the same project with that
name.

**A reference stays within its project.** A project is the directory of a
repository (where `.git` is). A Python virtual environment is a project of
its own, also inside a repository. A file in neither belongs to the
directory right below the included directory it is under, so an unpacked
source tree in `~/Projects` is a project too. Without this a call would be
matched with whatever has that name in everything you have ever indexed.

**Following libraries** is off by default and switched on with `follow:`,
in the profile or in the `.config.yaml` in the directory of a project:

```yaml
follow:
  - venv           # the Python virtual environment of the project
  - go             # the Go standard library and the module cache
  - headers        # the header files of the system
  - ~/src/shared   # any directory
```

`venv` is the environment in `.venv`, `venv` or `env` of the project, or
else the one that is active (`$VIRTUAL_ENV`). What is named through an
import, or placed by the Go type checker, can then also lead to a definition
in those directories. A match by name alone never leaves the project: a name
like `get` is defined in every library. Following only says where references
may lead; the library has to be in the index, which it is after `chainsaw
index` on its directory (for example `chainsaw index .venv`). A change of
`follow` is picked up the next time a file of the project is indexed;
`chainsaw index --force .` does that at once.

A graph built from syntax cannot see calls made through reflection, or
strings that name a function. `chainsaw grep` can.

### BCPL

Two dialects are read, and a file says by itself which it is in. With `{`
and `}` it is that of the compiler of today: reserved words in upper case,
`Foo` and `foo` two names. With `$(` and `$)` and no braces it is that of
the 1980s, of TRIPOS and AmigaDOS:

- No cases are told apart. `let`, `Let` and `LET` are one word, and
  `CGERROR(...)` calls `LET cgerror(...)`. `outline --body` and `--expand`
  take a name in any case. In the graph the names of such a file are in
  lower case, whatever the source has, so ask for them that way:
  `WHERE t.name = "cgerror"`, or `WHERE t.name =~ "(?i)^cgerror$"` when you
  do not want to think about it. The signature keeps the spelling of the
  source.
- Names may hold dots and end in one: `err.p`, `clear.cmp`, `cgcondjump.`.
- `$(tag ... $)tag` closes every section still open inside it.
- `$$TAG := TRUE` sets a conditional compilation tag and is no code;
  `$<TAG ... $>TAG` is compiled when the tag is set and `$<TAG' ... $>TAG'`
  when it is not. Both are parsed, since nothing in a file says which tags
  are set. Where that cannot be, because the two are alternatives for one
  place (`n := $<TRIPOS 5 $>TRIPOS $<RSX 6 $>RSX`), the conditional texts
  are left out one at a time until the file parses, and what was left out
  defines and calls nothing. `grep` still finds it there.
- `/*/ ... //*/` is one comment: comments do not nest in that dialect.

A routine is as a rule declared in a header (`GLOBAL $( cgapply:120 $)`, a
`VARIABLE`) and defined in a source (`LET cgapply(op, k) BE`, a `FUNCTION`).
A call goes to the definition that is code, and to the declaration only
when the index holds no definition, as for the routines of a library.
`GET "LIBHDR"` is an import of `libhdr.h` or `libhdr`, whatever the case.

## Configuration

`~/.chainsaw/config.yaml` is optional. Chainsaw uses these settings of the
active profile:

```yaml
version: "3"
active_profile: "default"

profiles:
  default:
    # Kinds of index, see above. These are the defaults.
    index:
      graph: true
      embeddings: false
    # Where references may lead outside their project, see above. Nowhere
    # by default.
    # follow: [venv, go, headers]
    # The model for the embeddings index; needed only when it is on
    # embeddings:
    #   model: nomic-embed-text
    # Directories the daemon watches, and scans when it starts
    include:
      - ~/Projects
    # The scan at startup; these are the defaults
    daemon:
      scan: true
      scan_kinds: [graph]
    # Directory names skipped by grep, index and the daemon
    exclude:
      - node_modules
      - vendor
      - .venv
      - __pycache__
    # Files never indexed (regular expressions on the path)
    blacklist:
      - '\.min\.js$'
```

An `exclude` entry is the name of a directory (`vendor`), a glob for one
(`*.cache`), several names in a row (`tools/ghidra`) or an absolute path; a
slash at the end is allowed. A project's `.config.yaml`
can add to `exclude` and `blacklist`, for the tree below it, and set
`gitignore`, `index` and `follow`. `gitignore: false` in the profile keeps
what git ignores everywhere; see [What is left out](#what-is-left-out). Of the settings of
older versions, `embedding_model` still names the model when `embeddings:`
does not (and, as before, switches nothing on); `chunk_size`, `overlap`,
`graph_driver` and `llm_*` are ignored.

A configuration file that is there but cannot be read is an error for
`index`, `query` and `search`, not a reason to carry on with defaults:
defaults would switch off what it switches on.

## Troubleshooting

**A file "is not in a language chainsaw can parse".** Its name does not say
what it is and its content did not show it. Name the language:
`chainsaw outline --lang bcpl <file>`.

**A query takes minutes, or indexes a directory it should not.** It walks
something large that is not source: see [What is left out](#what-is-left-out).
Put the directory in `.gitignore` or in `exclude`, then `chainsaw forget
--ignored` there.

**`database is locked`, or a `warning:` that results may be stale.** Another
chainsaw kept writing to the index for longer than the command waits: see
[When the index is busy](#when-the-index-is-busy). Ask again when it is done;
`journalctl --user -u chainsawd -f` shows what the daemon is doing.

**`query` returns nothing.** Results are limited to the current directory;
try `--all`. If a note says there were too many files to index in passing,
run `chainsaw index .` once.

**`search` returns nothing.** The embeddings index is off unless you switched
it on; `chainsaw status` shows whether it is on in this directory, and the
note under the empty result says how to switch it on.

**`search` says the index holds embeddings made with another model.** Run
`chainsaw index .` to make them again with the model that is configured now.

**A call is missing from the graph.** In Go, check that the package compiles
and that `go` is on the PATH. In other languages, a method call on a variable
is a `may_call` edge, not `calls`. Ruby calls written without parentheses are
not recognised.

**The index is from another version.** Chainsaw says so and stops. Remove
`~/.chainsaw/graph.db` and index again.

**Daemon logs.** `journalctl --user -u chainsawd -f`
