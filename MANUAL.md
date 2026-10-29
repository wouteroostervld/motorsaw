# Motorsaw Manual

Motorsaw looks at code through its syntax tree. It has four ways of looking,
`grep`, `match`, `outline` and `query`, and one way of showing what it found
in more or less context, `--expand`. A fifth way of looking, `search`, finds
code by what it means; it needs a model and is off until you switch it on.

## Contents

- [Installation](#installation)
- [grep](#grep): search, with the definition around each match
- [match](#match): search the syntax tree, in signatures or anywhere
- [outline](#outline): a file folded to its signatures
- [query](#query): the code graph
- [search](#search): by meaning, when embeddings are switched on
- [Showing code: --expand](#showing-code---expand)
- [For Claude Code: hook read](#for-claude-code-hook-read): the outline instead of a large file read whole
- [The index](#the-index)
- [Kinds of index](#kinds-of-index): what is on, and where
- [The graph](#the-graph)
- [Configuration](#configuration)
- [Troubleshooting](#troubleshooting)

## Installation

```bash
git clone https://github.com/wouteroostervld/motorsaw.git
cd motorsaw
make install          # builds to ~/.local/bin/motorsaw
```

Needs Go 1.24.7+, a C compiler and the PCRE2 development files
(`pcre2-devel` on Fedora, `libpcre2-dev` on Debian and Ubuntu). Nothing else:
there is no model to pull and no service to run, unless you switch on
`search`. `grep` and `outline` work straight away; `query` indexes what it
needs by itself.

## grep

```bash
motorsaw grep [options] <pattern> [path...]
```

Searches files with a Perl-compatible regular expression (PCRE2: lookaround,
backreferences, `\b`, `(?i)`, and so on) and groups the matching lines by the
definition they are in.

```
$ motorsaw grep 'relation_type\s+(IN|=)' pkg/db
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
| `--signature` | Only matches in the signature of a definition |
| `--limit N` | Show the first N matching lines only; with `-s`, the first N definitions |
| `--oneline` | One match on a line: `path:line: [definition] text` |
| `--width N` | Cut lines to N characters |
| `--expand body\|file\|calls` | Show the code, see [below](#showing-code---expand) |

Without a path the current directory is searched. Hidden directories and the
`exclude` directories of your configuration are skipped; a file named
explicitly is always searched. `^` and `$` match at line ends. No index is
needed, so results are always current.

`-s` is the quick way to answer "which functions touch this": it is the list
to work through when something has to change everywhere.

`--limit` is for a pattern that may match a great deal. Unlike cutting the
output with `head`, it still says how much there is:

```
$ motorsaw grep -s --limit 3 'err' pkg/db
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
$ motorsaw grep --oneline 'ForgetFile' cmd
cmd/motorsaw/index.go:295: [syncIndex] if err := idx.ForgetFile(f.ID, path); err != nil {
$ motorsaw grep -s --oneline 'ForgetFile' pkg/db/files.go
pkg/db/files.go:104-117: [DB.ForgetFile] 2
```

Options may stand before or after the pattern and the paths, here and in the
other commands. Put `--` before a pattern that begins with a dash:
`motorsaw grep -F -- '--limit'`.

`--signature` keeps only the matches that begin in the signature of a
definition: in the definition, before its body. That is the same in every
language, and tells what a function takes from what it does with it:

```
$ motorsaw grep --signature 'context\.Context' pkg/indexer
pkg/indexer/embeddings.go
  METHOD Embedder.Embed  (lines 26-26)
    26: Embed(ctx context.Context, texts []string) ([][]float32, error)
  METHOD Embeddings.Index  (lines 65-109)
    67: func (e *Embeddings) Index(ctx context.Context, src *Source) error {
```

A signature here is text. To ask for its parts (the type of a result, the
number of parameters), use `match`.

## match

```bash
motorsaw match [options] <query> [path...]
motorsaw match --tree [--lines N-M] <file>
```

Searches the syntax tree with a [tree-sitter query](https://tree-sitter.github.io/tree-sitter/using-parsers/queries/),
where `grep` searches the text with a regular expression. What it finds is
shown as `grep` shows it, with the definition around each match, and takes the
same options to shape it: `-s`, `--limit`, `--oneline`, `--width`, `--expand`.
No index is needed.

A query is a pattern of nodes, written as the tree is written:

```
$ motorsaw match -s '(function_declaration result: (pointer_type) @r)' pkg/grep
pkg/grep/grep.go
  FUNCTION matchFile  (lines 211-231): 1

1 matching lines in 1 files
```

| | |
|---|---|
| `(kind)` | A node of that kind |
| `(kind (other))` | ...with a node of the other kind right below it |
| `(kind field: (other))` | ...in that field |
| `(kind !field)` | ...without that field |
| `(_)`, `_` | Any named node; any node at all |
| `"return"` | A keyword or sign |
| `(a) @x` | Report this node. A capture named `@_x` is not reported, and only serves a condition |
| `(a)*`, `(a)+`, `(a)?` | Repeated, or optional |
| `[(a) (b)]` | One or the other |
| `(kind . (a))`, `(a) . (b)` | `a` is the first node below; `b` follows `a` directly |
| `(#eq? @x "text")`, `(#eq? @x @y)` | The capture is this text; two captures are alike |
| `(#match? @x "^regexp")` | The capture matches (Go syntax, not PCRE) |
| `(#any-of? @x "a" "b")` | The capture is one of these |
| `#not-eq?`, `#not-match?`, `#not-any-of?` | The opposites |

A pattern that captures nothing is reported as a whole, at the line it
starts on. Conditions stand last in a pattern, which then needs brackets of
its own: `((identifier) @p (#eq? @p "ctx"))`.

The kinds of nodes and the names of fields are those of a grammar, and differ
from one language to the next. `--tree` shows the tree of a file as a query
sees it, to write the query from:

```
$ motorsaw match --tree --lines 213 pkg/grep/grep.go
(source_file)  [1-279]
  (function_declaration)  [213-231]
    name: (identifier) matchFile  [213]
    parameters: (parameter_list)  [213]
      (parameter_declaration)  [213]
        name: (identifier) re  [213]
        type: (pointer_type)  [213]
          (qualified_type)  [213]
            package: (package_identifier) pcre  [213]
            name: (type_identifier) Regexp  [213]
      ...
```

A query is tried on every language and searches the files of the languages
it fits, so `(identifier)` looks in nearly all of them and
`(function_declaration)` in Go, JavaScript and TypeScript. Of a query with
several patterns, a language takes the ones that fit it, so one query can say
the same thing in the words of more than one grammar:

```bash
# The names of functions, in Go and JavaScript and in Python and C
motorsaw match '(function_declaration name: (_) @n) (function_definition name: (_) @n)'
```

It is an error only when no pattern fits any language; `--lang` holds a query
to one. When nothing is found, a note names the languages that were searched.

| Option | |
|---|---|
| `--signature` | Only captures in the signature of a definition |
| `--lang NAME` | Only this language |
| `--writes NAME` | Instead of a query: what assigns this name (below) |
| `--tree` | Show the syntax tree of a file instead of searching |
| `--lines N-M` | With `--tree`, only the nodes on these lines |

### Who writes a name: --writes

`--writes NAME` stands for a query: the places that assign a variable, field
or property of that name, or an element of one. That covers `=`, `+=` and the
like, `++` and `--`, Go's `:=`, assignments of several targets at once, and in
PHP `=&` on either side (a reference can change what it points at). A read of
the name, as an argument, an index or on the right of `=`, is not a write.

```bash
# Every function that writes the field, grouped by function
motorsaw match -s --writes Current_sp_database

# PHP: that class's static property only, not local variables called $database
motorsaw match -s --writes 'Current::$database' src
```

A bare name is matched as written, in Go, PHP (with or without its `$`),
Python, JavaScript, TypeScript and C, and wherever else their patterns fit.
`Class::$name` holds PHP to the static property of that class, written with
or without its namespace; `self::` and `static::` inside the class are not
followed. A write through a pointer, an alias or a by-reference argument
cannot be seen in the syntax tree.

### Matching on signatures

Tree-sitter itself has no notion of a signature: a grammar names the parts of
a definition as it likes (`result` in Go, `return_type` in Rust and Python,
`type` in C and Java), and a query can only ask for them in those words. So
there are two ways to match on a signature.

`--signature` is the one that is the same everywhere. It keeps the captures
that begin in the signature of a definition as motorsaw knows it, the
definition up to its body, whatever the grammar calls its parts:

```bash
# Definitions with a parameter named ctx, in any language
motorsaw match --signature '((identifier) @p (#eq? @p "ctx"))'

# Anything with a pointer in its signature: a pattern for Go and Rust, one for C and C++
motorsaw match --signature -s '(pointer_type) @p (pointer_declarator) @p'
```

The other is to ask the grammar for the shape, which is exact but belongs to
one language:

```bash
# Go methods that take a context and return only an error
motorsaw match '(method_declaration
    parameters: (parameter_list (parameter_declaration type: (_) @_t))
    result: (type_identifier) @_r
    (#eq? @_t "context.Context") (#eq? @_r "error")) @m'

# BCPL functions of exactly four parameters
motorsaw match '(function_definition parameters: (parameter_list
    . (parameter) . (parameter) . (parameter) . (parameter) .)) @f'

# 68000: a jsr right after a movem
motorsaw match '((instruction mnemonic: (_) @_a) . (instruction mnemonic: (_) @_b) @call
    (#eq? @_a "movem") (#eq? @_b "jsr"))'
```

## outline

```bash
motorsaw outline [options] <file>...
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

`--expand` shows a definition in its place in the folded file; `--body` shows
the definition alone, under its file and lines:

````
$ motorsaw outline --body cut cmd/motorsaw/output.go
## cmd/motorsaw/output.go:71-79 cut

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
motorsaw query [options] '<cypher>'
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
$ motorsaw query --oneline 'MATCH (f)-[:calls]->(t {name: "displayPath"}) RETURN f.name, f.lines'
printMatches	185-204
handleOutline	44-124
```

By default a result must touch the current directory: at least one node of
the matched path is defined under it. `motorsaw graph query` is accepted as
an older spelling of `motorsaw query`.

## search

```bash
motorsaw search "where are retries handled"
motorsaw search --expand body --limit 3 parse a config file
```

Finds the definitions and sections whose content is closest in meaning to
what you describe, whatever words they use. It is for when you have a
description and no name; when you have a name or a string, `grep` is exact
and needs nothing.

`search` uses the `embeddings` index, which is **off by default**: see
[Kinds of index](#kinds-of-index) for switching it on. Without a model
configured it says so and stops.

```
$ motorsaw search --limit 2 "send a request to the model and read the vectors"
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
are left for `motorsaw index .`, since every file is a request to the model.

## Showing code: --expand

`grep` and `query` find places. `--expand` shows the code at those places,
in one of three amounts:

| | |
|---|---|
| `body` | The whole definition around each place. Nested definitions stay folded. |
| `file` | The file folded as by `outline`, with those definitions in full. All places in one file share one view. |
| `calls` | As `file`, and also what those definitions call: in place when it is in the same file, after it when it is in another. Uses the index, and follows `calls` edges only. |

```bash
motorsaw grep --expand body 'filepath\.SkipDir' cmd
motorsaw query --expand file 'MATCH (f)-[:calls]->(t {name: "nearest"}) RETURN f'
```

`body` is the smallest and usually enough. `file` shows where the code sits.
`calls` is for following the code without opening more files.

## For Claude Code: hook read

`motorsaw hook read` is a Claude Code `PreToolUse` hook for the `Read` tool.
When an agent reads a large source file whole, it is handed the file's
outline, so that it can read the lines it needs instead. A read with an offset
or a limit, a small file, a file motorsaw cannot parse, and a second whole
read of the same file in a session are left alone: an agent that does need
all of it gets it.

It is off until it is switched on, so installing it changes nothing:

```json
"hooks": {"PreToolUse": [{"matcher": "Read",
  "hooks": [{"type": "command", "command": "motorsaw hook read"}]}]},
"env": {"MOTORSAW_READ_HOOK": "deny"}
```

| Variable | |
|---|---|
| `MOTORSAW_READ_HOOK` | `off` (the default) does nothing. `deny` refuses the first whole read and gives the outline in its place. `context` lets the read go ahead with the outline added, which helps the agent find its way but saves no tokens. |
| `MOTORSAW_READ_HOOK_LINES` | Files with fewer lines are left alone (default 300). |
| `MOTORSAW_READ_HOOK_MAX` | The longest outline handed over, in bytes (default 16000). |

The switch makes a comparison easy: the same task with the hook on and off.
Every token a session reads early is read again, from the cache, on each
later call, so `deny` is the setting that saves: on a 105 KB generated file
the agent gets a 12.5 KB outline instead.

## The index

```bash
motorsaw index <path>...      # parse and record files, or the files under directories
motorsaw index --force <path> # also files that have not changed
motorsaw index --only graph . # one kind of index only (graph, embeddings)
motorsaw status               # what is in the index
motorsaw daemon start         # watch the configured directories for changes
```

**You rarely need any of these.** Before `motorsaw query` runs, and before
`--expand calls` looks up what is called, the source files under the current
directory that are new or modified are indexed, and the index forgets files
that are gone from disk (reported as `removed file detected: <file>`).
Motorsaw never deletes or changes a source file.
A first query on a project that was never indexed just takes a moment longer
(this repository: about two seconds) and says so on standard error. Results
never come from stale files. `--no-index` skips the step.

A file counts as modified when its modification time differs from when it
was indexed. When more than 1,000 files need indexing the step is skipped
with a note, because it would take too long to do in passing; run
`motorsaw index .` once for a tree that size.

`motorsaw index` does the same by hand, with no limit and for paths outside
the current directory. It takes seconds: the Linux kernel's `fs/ext4` (70,000
lines) in about two. It accepts any number of files and directories, and
reads more from standard input when given `-`, so it fits in a pipeline:

```bash
motorsaw index pkg cmd/motorsaw/main.go
git diff --name-only | motorsaw index -
find . -name '*.go' -newer Makefile -print0 | xargs -0 motorsaw index
find . -name '*.py' -print0 | motorsaw index -0 -
```

A file named directly is indexed whatever the configuration excludes; one
that is not in a parseable language is passed over, so a pipeline does not
have to filter. A path that does not exist is reported, the rest is still
indexed, and the exit status is 1.

The daemon is optional as well. It watches the `include` directories of your
configuration and re-indexes files as they change. Watching only shows what
changes while it runs, so when it starts it first scans those directories as
`motorsaw index` would: new and modified files are indexed, and the index
forgets files that are gone. The scan runs beside the watching and fills the
graph only; it makes embeddings when you name them:

```yaml
profiles:
  default:
    daemon:
      scan: true                        # false: only index what changes from now on
      scan_kinds: [graph, embeddings]   # default: [graph]
```

`motorsaw daemon start -scan=false` and `-scan-kinds graph,embeddings` do the
same for one run, and win over the configuration. A kind that is named is
still only built where it is switched on. Files that change while the daemon
runs are indexed in every kind that is on for them, whatever the scan does.

The index is `~/.motorsaw/graph.db`. It holds names, locations, edges and,
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

`grep`, `match` and `outline` use no index at all.

**Switching a kind on or off** is done under `index:`, in the profile of
`~/.motorsaw/config.yaml` for everything, or in a `.config.yaml` in a
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
`~/.motorsaw/config.yaml`: a project can ask for embeddings, it cannot say
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
true}` does. With embeddings on and no model, `motorsaw index` stops and
says so, and `query` carries on as before.

**The kinds are kept current separately.** A command brings up to date only
the kind it uses, so `query` never waits for a model, and `motorsaw index`
does all that are on. Switching `embeddings` on later embeds the files
without building the graph again; a model that is away leaves the graph
built, and the embeddings are tried again next time. After three files in a
row that the model fails on, a run stops rather than repeat the error for
every file.

**Switching a kind off** for a file drops what that kind held of it, the next
time `motorsaw index` or a command that uses that kind comes by. For
embeddings that is work thrown away, so switch them off where you mean it.

**Changing the model** makes the stored vectors useless: vectors of two
models cannot be compared. `motorsaw search` says so and stops; the next
`motorsaw index` drops them and embeds again with the new model.

`motorsaw status` shows which kinds are on in the current directory, how
many files each holds, and which model the embeddings were made with.

What is embedded is the same chunks the outline folds a file into: each
definition, and the file folded to its signatures, each with its file and
name in front. Only where a chunk is is stored, with its vector.

## The graph

Every definition is an entity; every call and import in the source is an
edge.

**Entity types:** `FUNCTION`, `METHOD` (named `Type.method`), `CLASS`,
`STRUCT`, `INTERFACE`, `TYPE`, `PACKAGE` (a file's package or module, and
what it imports), and `EXTERNAL` for something that is called but not defined
in the index, such as `fmt.Println`.

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
may lead; the library has to be in the index, which it is after `motorsaw
index` on its directory (for example `motorsaw index .venv`). A change of
`follow` is picked up the next time a file of the project is indexed;
`motorsaw index --force .` does that at once.

A graph built from syntax cannot see calls made through reflection, or
strings that name a function. `motorsaw grep` can.

## Configuration

`~/.motorsaw/config.yaml` is optional. Motorsaw uses these settings of the
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

Write directory names without a trailing slash. A project's `.config.yaml`
can add to `exclude` and `blacklist` and set `index` and `follow`. Of the settings of
older versions, `embedding_model` still names the model when `embeddings:`
does not (and, as before, switches nothing on); `chunk_size`, `overlap`,
`graph_driver` and `llm_*` are ignored.

A configuration file that is there but cannot be read is an error for
`index`, `query` and `search`, not a reason to carry on with defaults:
defaults would switch off what it switches on.

## Troubleshooting

**`query` returns nothing.** Results are limited to the current directory;
try `--all`. If a note says there were too many files to index in passing,
run `motorsaw index .` once.

**`search` returns nothing.** The embeddings index is off unless you switched
it on; `motorsaw status` shows whether it is on in this directory, and the
note under the empty result says how to switch it on.

**`search` says the index holds embeddings made with another model.** Run
`motorsaw index .` to make them again with the model that is configured now.

**A call is missing from the graph.** In Go, check that the package compiles
and that `go` is on the PATH. In other languages, a method call on a variable
is a `may_call` edge, not `calls`. Ruby calls written without parentheses are
not recognised.

**The index is from another version.** Motorsaw says so and stops. Remove
`~/.motorsaw/graph.db` and index again.

**Daemon logs.** `journalctl --user -u motorsawd -f`
