---
name: chainsaw
description: Structural code search for a local codebase with the chainsaw CLI. Greps with the enclosing function or type shown for every match, folds a file to its signatures, and queries the call graph (who calls X, what does X call, what implements an interface). Use when locating code, finding usages or callers, getting to know a large file, or planning a rename, removal or refactor that must cover every use. Prefer it over plain grep and over reading whole files.
---

# Chainsaw

`chainsaw` looks at code through its syntax tree. Three commands, all
read-only, plus a fourth that only works where it is switched on:

| Command | Answers | Needs the index |
|---|---|---|
| `chainsaw grep <pattern> [path...]` | Where is this text, and which definition is it inside? | No |
| `chainsaw outline <file>` | What is in this file, without reading it? | No |
| `chainsaw query '<cypher>'` | Who calls this? What does it call? What implements it? | Yes, kept current automatically |
| `chainsaw search <words>` | Where is the code that does this, when I have no name for it? | Yes, and off by default |

Run them from inside the project. Parsed languages: Go, Python, JavaScript,
TypeScript, Rust, Java, C, C++, Ruby, BCPL, Motorola 68000 assembly (a global
label and the lines under it are one definition). A file is told by its
extension; BCPL also by its content, in a file without an extension or a
`.h`. When `outline` says a file is not in a language it can parse and you
know what it is, pass `--lang` (`--lang bcpl`); `grep` takes it too.

If `chainsaw` is not on the PATH, say so and work with plain grep; do not
install it yourself. It is built from
<https://github.com/wouteroostervld/chainsaw> with `make install`.

## grep: search, with the definition around each match

```bash
chainsaw grep 'IS_EXT2_SB' fs/ext4        # matches grouped by enclosing definition
chainsaw grep -s 'ctx\.Done\(\)'          # only the list of definitions, with counts
chainsaw grep -i -w 'timeout' pkg         # -i ignore case, -w whole words, -F literal
chainsaw grep -s --limit 40 'err' pkg     # the first 40, and how many there are in all
```

```
fs/ext4/super.c
  FUNCTION ext4_check_opt_consistency  (lines 2773-2853)
    2781: if ((ctx->opt_flags & MOPT_NO_EXT2) && IS_EXT2_SB(sb)) {
  FUNCTION _ext4_show_options  (lines 2960-3089)
    3012: if (IS_EXT2_SB(sb))
```

Patterns are PCRE: lookaround, backreferences and `\b` work. The search is
exhaustive and always current, so **use `grep -s` to enumerate every place
before a change that must be complete** (renaming, removing a feature). The
line ranges tell you what to read.

Exhaustive means the source of the project: hidden directories, excluded
ones and what git ignores (build output, a virtualenv) are passed by, as
they are by the index. To search one of those, name it
(`chainsaw grep X build`) or pass `--no-ignore`.

When there may be much, pass `--limit N` rather than piping into `head`: the
last line still gives the total, so you know what you have not seen.

## outline: a file folded to its signatures

```bash
chainsaw outline pkg/indexer/indexer.go
chainsaw outline --expand Indexer.IndexFile pkg/indexer/indexer.go   # one definition in full
chainsaw outline --body Indexer.IndexFile pkg/indexer/indexer.go     # that definition and nothing else
chainsaw outline --symbols pkg/indexer/indexer.go                    # type, name, lines, signature
chainsaw outline pkg/db/files.go pkg/db/chunks.go                    # several files, each under its name
```

```go
func New(cfg *Config, database db.Database) *Indexer // ... chunk: lines 44-54
func (idx *Indexer) IndexFile(ctx context.Context, filePath string) error // ... chunk: lines 56-109
```

A folded line gives the signature and the lines of the body. Read those lines
of the file when you need the body. **Outline a large file before reading
it.** `--expand` takes a qualified name (`Type.method`), a bare name or a
line number, and may be given more than once. To read one definition, use
`--body NAME` and not `--expand NAME | sed`: it prints the definition alone,
with its file and lines.

## query: the call graph

```bash
# Who calls a function
chainsaw query 'MATCH (f)-[:calls]->(t) WHERE t.name = "ext4_orphan_add" RETURN f.name, f.file, f.lines'

# What a function calls, up to three levels deep
chainsaw query 'MATCH (a {name: "handleIndex"})-[:calls*1..3]->(b) RETURN b.name, b.file, b.lines'

# Everything that can reach a method, including through an interface
chainsaw query 'MATCH (f)-[:calls|may_call]->(t) WHERE t.name ENDS WITH ".StoreFileSymbols" RETURN f.name, t.name'

# What implements an interface
chainsaw query 'MATCH (s)-[:implements]->(i {name: "Database"}) RETURN s.name, s.file'

# Definitions by name or place
chainsaw query 'MATCH (n:FUNCTION|METHOD) WHERE n.name =~ "^handle" RETURN n.signature, n.file, n.lines'
```

- **Nodes** `(v:LABEL {name: "x"})`. Labels: `FUNCTION METHOD CLASS STRUCT
  INTERFACE TYPE CONSTANT VARIABLE PACKAGE EXTERNAL`. Methods are named
  `Type.method`. `EXTERNAL` is something called but not in the index
  (`fmt.Println`). Names from BCPL that brackets with `$(` are in lower case
  in the graph, whatever the source has: `cgerror` for `CGERROR`.
- **Edges** `-[:calls]->`, `<-[:calls]-`, `-[:calls|may_call]->`,
  `-[:calls*1..3]->`. Types: `calls`, `may_call`, `implements`, `imports`,
  `defines`.
- **Properties** `name type file line end_line lines signature container
  short_name snippet`.
- **WHERE** `= <> < > <= >=`, `CONTAINS`, `STARTS WITH`, `ENDS WITH`, `=~`,
  `IN [...]`, `IS NULL`, `IS NOT NULL`, `AND OR NOT`.
- **RETURN** `v`, `v.prop`, `count(*)`, `count(v)`, `AS`, `DISTINCT`; then
  `ORDER BY ... DESC`, `SKIP`, `LIMIT`.

Results are limited to the current directory; add `--all` to lift that.
`chainsaw query --help` has the whole grammar.

`calls` is certain. `may_call` means "may end up there": in Go, a call
through an interface, to each implementing type; in other languages, a method
call on a value of unknown type, to every definition with that name. In Go a
call through an interface points at the interface's method
(`Database.UpsertFile`), so to find the callers of an implementation ask for
`[:calls|may_call]`.

The index needs no care: before a query runs, files under the current
directory that are new or changed are indexed. Only if a note says there were
too many files to index in passing, run `chainsaw index .` once.

**A line `warning: ...` after the results means they may be stale or
incomplete**: the index could not be brought up to date, most often because
another chainsaw was writing to it. Run the query again before relying on
what it says, above all on an empty result.

## --expand: show the code

Add to `grep`, `query` or `search` to see code instead of only locations:

```bash
chainsaw grep --expand body 'SkipDir' cmd
chainsaw query --expand body 'MATCH (f)-[:calls]->(t {name: "nearest"}) RETURN f'
```

| | |
|---|---|
| `--expand body` | The whole definition around each result. Smallest; start here. |
| `--expand file` | The file folded to signatures, with those definitions in full. |
| `--expand calls` | As `file`, plus what those definitions call. For following code. |

## Shaping the output

These replace the pipes you would otherwise write; options may stand before
or after the pattern, query or file.

| Instead of | Use |
|---|---|
| `chainsaw grep ... \| head -40` | `--limit 40`: the total is still given |
| `... \| cut -c1-160` | `--width 160`, on `grep`, `outline`, `query` and `search` |
| `outline --expand X f \| sed -n '/X/,/^}/p'` | `outline --body X f` |
| `query ... \| grep name \| sed ...` | `query --oneline`: one row a line, values separated by tabs |
| `search ... \| grep File` | `search --oneline`: `path:lines`, distance, name |
| `for f in ...; do chainsaw outline $f; done` | `chainsaw outline a.go b.go` |

`grep --oneline` writes `path:line: [definition] text`, and with `-s`
`path:start-end: [definition] count`: one match a line, for `sort`, `wc -l`
or a further filter.

## Which to use

- **"Where is X used?"** `chainsaw grep -s 'X'`: every use, grouped by definition.
- **"Change X everywhere."** `chainsaw grep 'X'` for the complete list. Never rely on the graph alone for completeness: it does not see strings, comments, macros or reflection.
- **"What does this file do?"** `chainsaw outline <file>`.
- **"Who calls X?" / "What does X reach?"** `chainsaw query`, then `--expand body` to read the callers.
- **"What implements this interface?"** `chainsaw query` with `[:implements]`.
- **You only have a description and no name.** Try `chainsaw search --expand body "<description>"`. It works only where the user switched on the embeddings index (`chainsaw status` says `embeddings: on here`). If it reports that no model is configured, or returns nothing with a note that embeddings are off, do not switch them on yourself: it costs the user model requests. Guess identifiers with `grep -i`, or outline the likely files.
