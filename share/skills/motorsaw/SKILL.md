---
name: motorsaw
description: Structural code search for a local codebase with the motorsaw CLI. Greps with the enclosing function or type shown for every match, folds a file to its signatures, and queries the call graph (who calls X, what does X call, what implements an interface). Use when locating code, finding usages or callers, getting to know a large file, or planning a rename, removal or refactor that must cover every use. Prefer it over plain grep and over reading whole files.
---

# Motorsaw

`motorsaw` looks at code through its syntax tree. Four read-only commands,
plus a fifth that only works where it is switched on:

| Command | Answers | Needs the index |
|---|---|---|
| `motorsaw grep <pattern> [path...]` | Where is this text, and which definition is it inside? | No |
| `motorsaw match <query> [path...]` | Which definitions have this shape (a parameter, a result type, a run of instructions)? | No |
| `motorsaw outline <file>` | What is in this file, without reading it? | No |
| `motorsaw query '<cypher>'` | Who calls this? What does it call? What implements it? | Yes, kept current automatically |
| `motorsaw search <words>` | Where is the code that does this, when I have no name for it? | Yes, and off by default |

Run them from inside the project. Parsed languages: Go, Python, JavaScript,
TypeScript/TSX, Rust, Java, C, C++, Ruby, PHP, BCPL, Motorola 68000 assembly,
x86 assembly (Intel/NASM and AT&T/gas syntax) and IBM Enterprise COBOL. In
assembly a global label and the lines under it are one definition, and a
`call` or a jump to another global label is a call; in COBOL a paragraph or
section is a definition, and names are upper case. The `.s`, `.asm`, `.i` and
`.inc` files of every processor share extensions, so each is read as the
language it has the fewest errors in (`match --lang` takes `m68k`, `nasm` or
`gas`).

If `motorsaw` is not on the PATH, say so and work with plain grep; do not
install it yourself. It is built from
<https://github.com/wouteroostervld/motorsaw> with `make install`.

## grep: search, with the definition around each match

```bash
motorsaw grep 'IS_EXT2_SB' fs/ext4        # matches grouped by enclosing definition
motorsaw grep -s 'ctx\.Done\(\)'          # only the list of definitions, with counts
motorsaw grep -i -w 'timeout' pkg         # -i ignore case, -w whole words, -F literal
motorsaw grep -s --limit 40 'err' pkg     # the first 40, and how many there are in all
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

When there may be much, pass `--limit N` rather than piping into `head`: the
last line still gives the total, so you know what you have not seen.

To search only what definitions declare, not what they do, add
`--signature`: a match then counts only in the signature of a definition, in
any language.

```bash
motorsaw grep --signature -s 'context\.Context'   # what takes or returns a context
```

## match: search the syntax tree

When the question is about structure and a regular expression would guess,
give a tree-sitter query. Results look like those of `grep` and take the same
options (`-s`, `--limit`, `--oneline`, `--expand`).

```bash
motorsaw match --tree --lines 40-52 pkg/grep/grep.go    # the tree, to write a query from
motorsaw match -s '(function_declaration result: (pointer_type) @r)'
motorsaw match --signature '((identifier) @p (#eq? @p "ctx"))'   # in signatures, any language
motorsaw match '(function_definition parameters: (parameter_list . (parameter) . (parameter) .)) @f'
```

- `(kind field: (kind))` is a node; `@name` reports it, `@_name` does not;
  `(_)` is any node; `.` between nodes means "directly follows".
- Conditions come last in a pattern that has brackets of its own:
  `(#eq? @x "text")`, `(#match? @x "^re")`, `(#any-of? @x "a" "b")`.
- Kinds and fields differ between languages. **Look at `--tree` of a real
  file first** instead of guessing them. A query searches the languages
  whose grammar it fits, and of several patterns each language takes the
  ones that fit it; `--lang go` holds it to one.
- `--signature` keeps the captures before the body of a definition, which is
  how to match on signatures without knowing each grammar.

## outline: a file folded to its signatures

```bash
motorsaw outline pkg/indexer/indexer.go
motorsaw outline --expand Indexer.IndexFile pkg/indexer/indexer.go   # one definition in full
motorsaw outline --body Indexer.IndexFile pkg/indexer/indexer.go     # that definition and nothing else
motorsaw outline --symbols pkg/indexer/indexer.go                    # type, name, lines, signature
motorsaw outline pkg/db/files.go pkg/db/chunks.go                    # several files, each under its name
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
motorsaw query 'MATCH (f)-[:calls]->(t) WHERE t.name = "ext4_orphan_add" RETURN f.name, f.file, f.lines'

# What a function calls, up to three levels deep
motorsaw query 'MATCH (a {name: "handleIndex"})-[:calls*1..3]->(b) RETURN b.name, b.file, b.lines'

# Everything that can reach a method, including through an interface
motorsaw query 'MATCH (f)-[:calls|may_call]->(t) WHERE t.name ENDS WITH ".StoreFileSymbols" RETURN f.name, t.name'

# What implements an interface
motorsaw query 'MATCH (s)-[:implements]->(i {name: "Database"}) RETURN s.name, s.file'

# Definitions by name or place
motorsaw query 'MATCH (n:FUNCTION|METHOD) WHERE n.name =~ "^handle" RETURN n.signature, n.file, n.lines'
```

- **Nodes** `(v:LABEL {name: "x"})`. Labels: `FUNCTION METHOD CLASS STRUCT
  INTERFACE TYPE PACKAGE EXTERNAL`. Methods are named `Type.method`.
  `EXTERNAL` is something called but not in the index (`fmt.Println`).
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
`motorsaw query --help` has the whole grammar.

`calls` is certain. `may_call` means "may end up there": in Go, a call
through an interface, to each implementing type; in other languages, a method
call on a value of unknown type, to every definition with that name. In Go a
call through an interface points at the interface's method
(`Database.UpsertFile`), so to find the callers of an implementation ask for
`[:calls|may_call]`.

The index needs no care: before a query runs, files under the current
directory that are new or changed are indexed. Only if a note says there were
too many files to index in passing, run `motorsaw index .` once.

## --expand: show the code

Add to `grep`, `query` or `search` to see code instead of only locations:

```bash
motorsaw grep --expand body 'SkipDir' cmd
motorsaw query --expand body 'MATCH (f)-[:calls]->(t {name: "nearest"}) RETURN f'
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
| `motorsaw grep ... \| head -40` | `--limit 40`: the total is still given |
| `... \| cut -c1-160` | `--width 160`, on `grep`, `outline`, `query` and `search` |
| `outline --expand X f \| sed -n '/X/,/^}/p'` | `outline --body X f` |
| `query ... \| grep name \| sed ...` | `query --oneline`: one row a line, values separated by tabs |
| `search ... \| grep File` | `search --oneline`: `path:lines`, distance, name |
| `for f in ...; do motorsaw outline $f; done` | `motorsaw outline a.go b.go` |

`grep --oneline` writes `path:line: [definition] text`, and with `-s`
`path:start-end: [definition] count`: one match a line, for `sort`, `wc -l`
or a further filter.

## Which to use

- **"Where is X used?"** `motorsaw grep -s 'X'`: every use, grouped by definition.
- **"Change X everywhere."** `motorsaw grep 'X'` for the complete list. Never rely on the graph alone for completeness: it does not see strings, comments, macros or reflection.
- **"Which functions take or return X?"** `motorsaw grep --signature -s 'X'`; for an exact shape, `motorsaw match`.
- **"What does this file do?"** `motorsaw outline <file>`.
- **"Who calls X?" / "What does X reach?"** `motorsaw query`, then `--expand body` to read the callers.
- **"What implements this interface?"** `motorsaw query` with `[:implements]`.
- **You only have a description and no name.** Try `motorsaw search --expand body "<description>"`. It works only where the user switched on the embeddings index (`motorsaw status` says `embeddings: on here`). If it reports that no model is configured, or returns nothing with a note that embeddings are off, do not switch them on yourself: it costs the user model requests. Guess identifiers with `grep -i`, or outline the likely files.
