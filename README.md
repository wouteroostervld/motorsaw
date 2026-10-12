# 🪚 Chainsaw

**A code graph and context-aware search for your codebase, built from the syntax tree**

Chainsaw parses your source files and answers three kinds of question
without reading whole files: *where is this text, and what is it inside of*,
*what does this file look like*, and *what calls what*. That needs no
embeddings, no model and no GPU: indexing is parsing, and takes seconds for
a project.

A fourth question, *where is the code that does this*, does need a model.
Search by meaning is there for who wants it, as a separate kind of index that
is off until you switch it on.

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**Status:** under active development. Commands, configuration and the format
of the index still change from one version to the next; an index made by an
older version is built again, or has to be removed.

## Commands

```bash
# Search with a regular expression (PCRE); each match shows the definition it is in
chainsaw grep 'IS_EXT2_SB' fs/ext4

# A file folded to its signatures, each with the lines of its body
chainsaw outline pkg/indexer/indexer.go

# The code graph: who calls this, what implements that
chainsaw query 'MATCH (f)-[:calls]->(t {name: "Open"}) RETURN f.name, f.file, f.lines'

# Search by meaning (off by default, see below)
chainsaw search "where are retries handled"
```

`grep` and `outline` work on the files as they are on disk and need no index.
`query` keeps its own: before it runs, whatever is new or changed under the
current directory is indexed, so there is nothing to set up and results are
never stale. For a very large tree, index it once by hand:

```bash
chainsaw index .          # parse the source files under a path
```

### What a result looks like

```
$ chainsaw grep -s 'IS_EXT[23]_SB' fs/ext4
fs/ext4/super.c
  (outside any definition): 3
  FUNCTION ext4_check_opt_consistency  (lines 2773-2853): 2
  FUNCTION _ext4_show_options  (lines 2960-3089): 1
  FUNCTION ext4_set_def_opts  (lines 4388-4443): 1
  FUNCTION ext4_check_feature_compatibility  (lines 4713-4815): 2
```

`grep`, `query` and `search` can show the code around a result with `--expand`:

| | |
|---|---|
| `--expand body` | the whole definition around each result |
| `--expand file` | the file folded to signatures, with those definitions in full |
| `--expand calls` | as `file`, and also what those definitions call |

## Kinds of index

What is indexed is made of parts that work alone and together. Each can be
switched on or off, for everything or for one project:

| Kind | Holds | Used by | Default |
|---|---|---|---|
| `graph` | Definitions, calls, imports, from the syntax tree | `query`, `--expand calls` | on |
| `embeddings` | A vector per chunk of code, from a model | `search` | **off** |

Embeddings cost a request to a model for every file, so they stay off until
you say so and name a model in `~/.chainsaw/config.yaml`:

```yaml
profiles:
  default:
    index:
      embeddings: true          # everywhere, or leave this out and...
    embeddings:
      model: nomic-embed-text   # Ollama on localhost, or any OpenAI-compatible server
```

...switch them on for just the projects that want them, with a `.config.yaml`
in the project:

```yaml
index:
  embeddings: true
```

A command only builds the kind of index it uses: `query` never waits for a
model. See [Kinds of index](MANUAL.md#kinds-of-index) in the manual.

## Projects

A call is matched with the definitions of its own project: the repository
the file is in (where `.git` is), a Python virtual environment, or else the
directory right below one of your included directories. Without that
boundary a call of `get` would be linked to everything of that name in every
tree you ever indexed.

Where references may lead outside a project is yours to say, with `follow:`
in `~/.chainsaw/config.yaml` or in the `.config.yaml` of a project:

```yaml
follow:
  - venv           # the project's Python virtual environment, or the active one
  - go             # the Go standard library and the module cache
  - headers        # the header files of the system
  - ~/src/shared   # any directory
```

Only what is named through an import, or placed by the Go type checker, is
followed, and only into what is in the index: `chainsaw index .venv` puts a
library there. See [The graph](MANUAL.md#the-graph) in the manual.

## Languages

Go, Python, JavaScript, TypeScript/TSX, Rust, Java, C, C++, Ruby, BCPL and
Motorola 68000 assembly are parsed for definitions, calls and imports. Go
calls are resolved exactly with the Go type checker, including calls through
interfaces. Markdown, HTML and XML fold into an outline but define no symbols.

In 68000 assembly (`.s`, `.asm`, `.i`, in the syntax of the Amiga assemblers)
a definition is a global label and the lines up to the next one: a `FUNCTION`
when code follows the label, a `STRUCT` when data does. Macros are functions
too. `jsr` and `bsr` are calls, and so is a jump or branch to another global
label; `jsr _LVOAllocMem(a6)` is a call of `_LVOAllocMem`. Assembly for
another processor in a file with one of these extensions is left alone.

In BCPL (`.b`, `.bcpl`) a definition is a function or a routine, whether it is
introduced by `LET` or by `AND`, and a `GET` is an import. Reserved words are
upper case. Headers named `.h` are read as C++.

## Installation

```bash
git clone https://github.com/wouteroostervld/chainsaw.git
cd chainsaw
make install
```

Needs Go 1.24.7+, a C compiler (for SQLite and the tree-sitter grammars) and
the PCRE2 development files (`pcre2-devel` on Fedora, `libpcre2-dev` on
Debian and Ubuntu).
The `go` command must be on the PATH for exact Go call resolution into
dependencies.

## Documentation

- **[Manual](MANUAL.md)**: commands, the query language, the graph, configuration
- **[Agent skill](share/skills/chainsaw/SKILL.md)**: how an AI coding agent should use it

## How it works

```
source files ── tree-sitter ─┬► definitions, calls, imports ──► graph ─────────────► chainsaw query
      │       (+ go/types    │
      │         for Go)      └► chunks ── model ──► embeddings (optional) ──► chainsaw search
      │
      └─────────────────────────────────────────────────────────────────────► chainsaw grep / outline
```

The index lives in `~/.chainsaw/graph.db`. It holds names, locations, edges
and, when switched on, vectors. It holds no source text, and can be deleted
at any time.

A daemon is optional: `chainsaw daemon start` watches the directories you
configured and indexes what changes, so that a query finds the index up to
date already. It reads the configuration again on SIGHUP
(`systemctl --user reload chainsawd`). See [The index](MANUAL.md#the-index) in the manual.

## License

MIT, see [LICENSE](LICENSE).

## Credits

Built with [tree-sitter](https://tree-sitter.github.io/), SQLite,
[sqlite-vec](https://github.com/asg017/sqlite-vec),
[PCRE2](https://www.pcre.org/) and
[fsnotify](https://github.com/fsnotify/fsnotify).
