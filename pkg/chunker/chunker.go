// Package chunker splits source files into semantic chunks using tree-sitter.
//
// A file becomes a tree of chunks. The root chunk is the file skeleton: every
// definition (function, method, class, ...) is folded down to its signature
// plus a comment that points at the lines of the chunk holding the body. Each
// folded definition is a chunk of its own, with its nested definitions folded
// the same way.
//
// When a size limit is given, a chunk that is still larger gets its biggest
// statements (loops, branches, literals) folded the same way, until it fits.
//
// The same parse also yields the symbols a file defines and the calls and
// imports it contains, from which the code graph is built.
//
// Markup is chunked too, without symbols: Markdown along its sections, HTML
// and XML by folding the largest elements of a chunk that is too big.
package chunker

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Entity types of the symbols the chunker reports
const (
	TypeFunction  = "FUNCTION"
	TypeMethod    = "METHOD"
	TypeClass     = "CLASS"
	TypeStruct    = "STRUCT"
	TypeInterface = "INTERFACE"
	TypeType      = "TYPE"
	TypePackage   = "PACKAGE"
	TypeConstant  = "CONSTANT" // A named value: an entry of a BCPL MANIFEST
	TypeVariable  = "VARIABLE" // A named cell: an entry of a BCPL GLOBAL or STATIC
)

// Relation types of the references the chunker reports
const (
	RelationCalls   = "calls"
	RelationImports = "imports"
	// RelationMayCall is a call whose target is named by something that is
	// not checked, such as the comment after a call through a register
	RelationMayCall = "may_call"
)

// minFoldLines is the smallest definition that gets a chunk of its own.
// Anything shorter stays inline in the enclosing chunk: a chunk of a few
// lines says too little to be found by what it does, and gets in the way of
// the chunks that do.
const minFoldLines = 5

// minBlockLines is the smallest statement that is folded out of a chunk that
// is too large
const minBlockLines = 8

// Chunk is one semantic chunk of a file
type Chunk struct {
	Content     string
	Stub        string // Text that stands in for this chunk in its parent's content
	Title       string // The definition or section this chunk is, or is part of; empty for the file itself
	StartLine   int    // 1-indexed
	EndLine     int    // 1-indexed, inclusive
	StartOffset int    // Byte offset into the file
	EndOffset   int    // Byte offset into the file (exclusive)
	Parent      int    // Index of the enclosing chunk, -1 for the file skeleton
	Verbatim    bool   // Content is the unmodified source range (nothing folded)
}

// Symbol is a definition found in the file
type Symbol struct {
	Name      string // Bare name, e.g. "IndexFile"
	Container string // Class or receiver type for methods, e.g. "Indexer"
	Type      string // One of the Type* constants
	Parent    int    // Index of the enclosing symbol, -1 at file level
	Chunk     int    // Index of the chunk holding the definition
	Signature string // The definition up to its body, e.g. "func (idx *Indexer) IndexFile(path string) error"
	SigStart  int    // Byte range of the signature in the file
	SigEnd    int    // Exclusive
	StartLine int    // 1-indexed, including the doc comment above the definition
	EndLine   int    // 1-indexed, inclusive
}

// QualifiedName returns the name prefixed with its container, if any
func (s Symbol) QualifiedName() string {
	if s.Container == "" {
		return s.Name
	}
	return s.Container + "." + s.Name
}

// Ref is a reference from a symbol to something identified by name
type Ref struct {
	Source     int    // Index of the referencing symbol, -1 at file level
	Relation   string // One of the Relation* constants
	Name       string // Bare name of the target
	Container  string // Container the target must be in, when known (self.x(), this.x())
	ImportBase string // Last element of the import path the target comes from, when known
	TargetPath string // File the target is declared in, when a type checker resolved it
	External   string // Display name to use when the target is not in the index; empty for language built-ins
	Member     bool   // Reached through a receiver expression (x.name())
}

// File is the result of parsing a source file
type File struct {
	Language string
	Markup   bool    // A document rather than code: it has chunks, but no symbols
	FoldCase bool    // Names are the same whatever their case (BCPL of the 1980s)
	Module   string  // Package name, or the file name without extension
	Chunks   []Chunk // Chunks[0] is the file skeleton
	Symbols  []Symbol
	Refs     []Ref
}

// Supported reports whether the file's language can be parsed, going by its
// name alone. A file whose name does not say what it is may still be parsed
// when its content shows it; see Recognised.
func Supported(path string) bool {
	return extensions[strings.ToLower(filepath.Ext(path))] != nil
}

// Language returns the name of the language a file is in, or "" when it is
// not one that can be parsed. The name of the file says, and where that does
// not, or not for certain, what is in it.
func Language(path string, src []byte) string {
	if spec := specFor(path, src); spec != nil {
		return spec.name
	}
	return ""
}

// Languages returns the names of the languages that can be parsed, which
// are the names ParseAs takes
func Languages() []string {
	names := make([]string, 0, len(languages))
	for name := range languages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Parse splits a source file into semantic chunks and extracts its symbols
// and references. Chunks larger than maxChunkSize bytes are folded further
// where the syntax allows; 0 means no limit. It returns false when the
// language is not supported.
func Parse(path string, src []byte, maxChunkSize int) (*File, bool) {
	return parseWith(specFor(path, src), path, src, maxChunkSize, false)
}

// ParseAs is Parse for a file that is said to be in a language, whatever
// its name. It returns false when the language is not one of Languages.
func ParseAs(language, path string, src []byte, maxChunkSize int) (*File, bool) {
	return parseWith(languages[strings.ToLower(language)], path, src, maxChunkSize, true)
}

// parseWith parses a file in the language of a spec. When the language was
// said and not guessed, the file is taken as it comes, however badly it parses.
func parseWith(spec *langSpec, path string, src []byte, maxChunkSize int, said bool) (*File, bool) {
	if spec == nil {
		return nil, false
	}

	base := filepath.Base(path)
	b := &builder{
		spec:    spec,
		src:     src,
		maxSize: maxChunkSize,
		imports: make(map[string]importInfo),
		seen:    make(map[Ref]bool),
		file: &File{
			Language: spec.name,
			Markup:   spec.markup,
			Module:   strings.TrimSuffix(base, filepath.Ext(base)),
		},
	}

	if spec.grammar == nil {
		b.render(markdownSections(b), -1)
		return b.file, true
	}

	var tree *sitter.Tree
	var foldCase bool
	if said {
		tree, foldCase = spec.parse(src, true)
	} else {
		spec, tree, foldCase = parseFile(path, src)
	}
	if tree == nil {
		return nil, false
	}
	defer tree.Close()
	b.spec = spec
	b.file.Language = spec.name
	b.file.FoldCase = foldCase

	root := tree.RootNode()
	b.collectImports(root)

	rootFold := &fold{node: root, start: 0, end: uint(len(src)), startRow: 0, endRow: root.EndPosition().Row}
	b.walk(root, scope{fold: rootFold, sym: -1})

	b.render(rootFold, -1)
	for i, f := range b.symFolds {
		b.file.Symbols[i].Chunk = f.chunk
	}

	return b.file, true
}

// parseFile returns the language of a file and its syntax tree, which the
// caller closes; nil when the file is in none of the languages its extension
// stands for. Where the extension is shared, as .asm is by the assemblers of
// every processor, the file is read as the language it has the fewest errors
// in; the first of them wins a tie.
func parseFile(path string, src []byte) (*langSpec, *sitter.Tree, bool) {
	var best *langSpec
	var bestTree *sitter.Tree
	var bestFold bool
	bestShare := 2.0
	for _, spec := range specsFor(path, src) {
		tree, foldCase := spec.parse(src, false)
		if tree == nil {
			continue
		}
		if share := errorShare(tree.RootNode()); share < bestShare {
			if bestTree != nil {
				bestTree.Close()
			}
			best, bestTree, bestFold, bestShare = spec, tree, foldCase, share
		} else {
			tree.Close()
		}
	}
	return best, bestTree, bestFold
}

// parse returns the syntax tree of a file, which the caller closes, and whether
// its names are the same whatever their case; nil when
// the file is not in the language
func (s *langSpec) parse(src []byte, said bool) (*sitter.Tree, bool) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(s.language()); err != nil {
		return nil, false
	}
	var tree *sitter.Tree
	var foldCase bool
	if s.prepare != nil {
		tree, foldCase = s.prepare(parser, src)
	} else {
		tree = parser.Parse(src, nil)
	}
	if tree == nil {
		return nil, false
	}
	// A file said to be in the language is taken however badly it parses
	if s.shared && !said && !parsesWell(tree.RootNode()) {
		tree.Close()
		return nil, false
	}
	return tree, foldCase
}

// errorShare is the part of the top-level nodes of a tree that have errors in them
func errorShare(root *sitter.Node) float64 {
	total := root.NamedChildCount()
	if total == 0 {
		return 0
	}
	errors := uint(0)
	for i := uint(0); i < total; i++ {
		if c := root.NamedChild(i); c.IsError() || c.HasError() {
			errors++
		}
	}
	return float64(errors) / float64(total)
}

// parsesWell reports whether a file is in the language its extension stands
// for. Assembly for every processor is kept in .s and .asm files, and the
// labels and comments of another assembler parse well enough to yield
// definitions that are not there. Source in the right syntax has next to no
// errors; anything else has them in more than one line out of ten.
func parsesWell(root *sitter.Node) bool {
	total := root.NamedChildCount()
	errors := uint(0)
	for i := uint(0); i < total; i++ {
		if c := root.NamedChild(i); c.IsError() || c.HasError() {
			errors++
		}
	}
	return errors*10 <= total
}

// fold is a source range that is replaced by a stub in its parent chunk
type fold struct {
	node             *sitter.Node
	title            string
	start, end       uint // Byte range, including leading comments
	startRow, endRow uint
	indent           string
	header           string // Signature shown in the parent chunk
	sigStart         uint   // Byte offset the signature starts at
	children         []*fold
	chunk            int
}

// scope is the context a node is visited in
type scope struct {
	fold      *fold  // Nearest enclosing fold
	sym       int    // Nearest enclosing symbol, -1 at file level
	classCtx  string // Container whose body we are directly in
	selfCtx   string // Container that self/this refers to
	selfName  string // Name of the receiver variable (Go)
	container bool   // The enclosing symbol is a container
}

// def describes a definition node; language hooks may adjust it
type def struct {
	name      string
	etype     string
	container string // Explicit container (Go receiver, C++ qualified name)
	selfName  string
	holds     bool // Functions directly inside are its methods, whatever the node kind says
}

type importInfo struct {
	path   string // Module path as written
	base   string // Last element of the module path
	symbol string // Name imported from the module; empty when the module itself is bound
}

type builder struct {
	spec     *langSpec
	src      []byte
	maxSize  int
	file     *File
	imports  map[string]importInfo
	seen     map[Ref]bool
	symFolds []*fold // Fold holding each symbol, parallel to file.Symbols

	// Assembly: what the address registers were last loaded with, by
	// register, while it can still be what a call goes through
	loads map[string]*m68kLoad
}

func (b *builder) text(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	return string(b.src[n.StartByte():n.EndByte()])
}

func (b *builder) collectImports(n *sitter.Node) {
	if b.spec.imports == nil {
		return
	}
	if b.spec.importKinds[n.Kind()] {
		b.spec.imports(b, n)
		return
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		b.collectImports(n.NamedChild(i))
	}
}

// addImport records an import and the name it is bound to in the file
func (b *builder) addImport(local, path, symbol string) {
	path = strings.Trim(path, "\"'`<>")
	if path == "" {
		return
	}
	info := importInfo{path: path, base: b.spec.importBase(path), symbol: symbol}
	if local != "" && local != "_" && local != "." {
		b.imports[local] = info
	}
	if info.base != "" {
		b.addRef(Ref{Source: -1, Relation: RelationImports, Name: info.base, ImportBase: info.base, External: path})
	}
}

func (b *builder) addRef(r Ref) {
	if r.Name == "" || b.seen[r] {
		return
	}
	b.seen[r] = true
	b.file.Refs = append(b.file.Refs, r)
}

func (b *builder) walk(n *sitter.Node, sc scope) {
	kind := n.Kind()
	if dk, ok := b.spec.defs[kind]; ok {
		sc = b.define(n, n, dk, sc)
	} else if field, ok := b.spec.calls[kind]; ok {
		b.call(n, field, sc)
	} else if b.spec.target != nil {
		if name := b.spec.target(b, n, sc); name != "" {
			b.addRef(Ref{Source: sc.sym, Relation: RelationCalls, Name: name, External: name})
		}
	}
	if b.spec.label != nil {
		b.walkLabels(n, sc)
		return
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		b.walk(n.NamedChild(i), sc)
	}
}

// walkLabels visits the children of a node in a language where a definition
// is a label and the lines after it, up to the next label: assembly. Labels
// that only mark a place inside a definition do not end it.
func (b *builder) walkLabels(n *sitter.Node, sc scope) {
	children := make([]*sitter.Node, n.NamedChildCount())
	for i := range children {
		children[i] = n.NamedChild(uint(i))
	}

	inner := sc
	for i, c := range children {
		if b.spec.label(b, c) {
			end := i + 1
			for end < len(children) && !b.spec.label(b, children[end]) {
				end++
			}
			// Comments above the next label are about that label
			for end > i+1 && b.leads(children[end-1]) {
				if start, _ := b.ownStart(children[end-1]); !b.startsLineAt(start) {
					break
				}
				end--
			}

			dk := function
			if b.isData(c) {
				dk = defKind{etype: TypeStruct}
			} else {
				for _, next := range children[i+1 : end] {
					if !b.leads(next) && !b.isPassive(next) {
						if b.isData(next) {
							dk = defKind{etype: TypeStruct}
						}
						break
					}
				}
			}
			inner = b.define(c, children[end-1], dk, sc)
		}
		b.walk(c, inner)
	}
}

// isPassive reports whether a node in assembly neither is code nor data: a
// label that is not a definition, or a directive that only describes
func (b *builder) isPassive(n *sitter.Node) bool {
	return b.spec.labelKinds[n.Kind()] || (b.spec.passive != nil && b.spec.passive(b, n))
}

// isData reports whether a node in assembly makes what the label above it
// names data and not code
func (b *builder) isData(n *sitter.Node) bool {
	return b.spec.dataKinds[n.Kind()] || (b.spec.data != nil && b.spec.data(b, n))
}

// define handles a definition node and returns the scope for its children.
// last is the node the definition ends with: n itself, or a later sibling
// when the definition is a label and what follows it.
func (b *builder) define(n, last *sitter.Node, dk defKind, sc scope) scope {
	d := def{etype: dk.etype, name: b.text(n.ChildByFieldName("name"))}
	if b.spec.describe != nil && !b.spec.describe(b, n, &d) {
		return sc
	}
	if d.name == "" && dk.etype != "" {
		return sc
	}

	f := b.newFold(n)
	endRow := n.EndPosition().Row
	if last != n {
		f.end, f.endRow = b.ownEnd(last)
		endRow = f.endRow
	}

	inner := sc
	if d.etype != "" {
		if d.etype == TypeFunction && d.container == "" && sc.classCtx != "" {
			d.container = sc.classCtx
		}
		if d.etype == TypeFunction && d.container != "" {
			d.etype = TypeMethod
		}
		b.file.Symbols = append(b.file.Symbols, Symbol{
			Name:      d.name,
			Container: d.container,
			Type:      d.etype,
			Parent:    sc.sym,
			Signature: strings.Join(strings.Fields(f.header), " "),
			SigStart:  int(f.sigStart),
			SigEnd:    int(f.sigStart) + len(f.header),
			StartLine: int(f.startRow) + 1,
			EndLine:   int(f.endRow) + 1,
		})
		b.symFolds = append(b.symFolds, sc.fold)
		inner.sym = len(b.file.Symbols) - 1
	}

	// Members of a container are methods; anything nested in a function is not
	inner.classCtx = ""
	if dk.container || d.holds {
		inner.classCtx = d.name
		inner.selfCtx = d.name
	} else if d.container != "" {
		inner.selfCtx = d.container
		inner.selfName = d.selfName
	}

	if endRow-n.StartPosition().Row+1 >= minFoldLines {
		f.title = d.name
		if d.container != "" {
			f.title = d.container + "." + d.name
		}
		sc.fold.children = append(sc.fold.children, f)
		inner.fold = f
		if d.etype != "" {
			b.symFolds[inner.sym] = f
		}
	}
	return inner
}

// newFold builds the fold for a definition: its range (with wrappers such as
// decorators and the doc comment above it) and the signature that stands in
// for it in the parent chunk.
func (b *builder) newFold(n *sitter.Node) *fold {
	outer := n
	for p := outer.Parent(); p != nil && b.spec.wrappers[p.Kind()]; p = outer.Parent() {
		outer = p
	}

	first, start, startRow := outer, outer.StartByte(), outer.StartPosition().Row
	for prev := b.above(first); prev != nil && b.leads(prev); prev = b.above(first) {
		// Some grammars include the line break in a comment node
		end := prev.EndPosition()
		if end.Column > 0 {
			end.Row++
		}
		prevStart, prevRow := b.ownStart(prev)
		if end.Row != startRow || !b.startsLineAt(prevStart) {
			break
		}
		first, start, startRow = prev, prevStart, prevRow
	}

	if b.startsLineAt(start) {
		start = b.lineStart(start)
	}
	end, endRow := b.ownEnd(outer)

	sigStart := outer.StartByte()
	var header string
	if body := findBody(n, 2); body != nil && body.StartByte() > sigStart {
		header = string(b.src[sigStart:body.StartByte()])
	} else {
		header, _, _ = strings.Cut(string(b.src[sigStart:outer.EndByte()]), "\n")
	}

	// Comments between the signature and the body belong to the body
	header = strings.TrimRight(header, " \t\r\n")
	for {
		i := strings.LastIndexByte(header, '\n')
		if i < 0 || !strings.HasPrefix(strings.TrimSpace(header[i+1:]), b.spec.comment) {
			break
		}
		header = strings.TrimRight(header[:i], " \t\r\n")
	}

	return &fold{
		node:     n,
		start:    start,
		end:      end,
		startRow: startRow,
		endRow:   endRow,
		indent:   string(b.src[b.lineStart(sigStart):sigStart]),
		header:   header,
		sigStart: sigStart,
	}
}

// leads reports whether a node above a definition belongs to it, when
// nothing but more of the same is in between
func (b *builder) leads(n *sitter.Node) bool {
	return b.spec.leading[n.Kind()] || (b.spec.remark != nil && b.spec.remark(b, n))
}

// above returns the node before n. For the first definition of a group that
// is the node before the group.
func (b *builder) above(n *sitter.Node) *sitter.Node {
	if p := n.Parent(); p != nil && b.spec.groups[p.Kind()] && n.PrevSibling() == nil {
		n = p
	}
	return n.PrevNamedSibling()
}

func (b *builder) lineStart(offset uint) uint {
	for offset > 0 && b.src[offset-1] != '\n' {
		offset--
	}
	return offset
}

// startsLine reports whether only whitespace precedes the node on its line
func (b *builder) startsLine(n *sitter.Node) bool {
	return b.startsLineAt(n.StartByte())
}

func (b *builder) startsLineAt(offset uint) bool {
	return strings.TrimSpace(string(b.src[b.lineStart(offset):offset])) == ""
}

// ownStart returns where the text of a node starts. The m68k grammar lets a
// comment on a line of its own begin at the end of the line before it, with
// the line breaks in between as part of the comment.
func (b *builder) ownStart(n *sitter.Node) (offset, row uint) {
	offset, row = n.StartByte(), n.StartPosition().Row
	if offset >= n.EndByte() || (b.src[offset] != '\n' && b.src[offset] != '\r') {
		return offset, row
	}
	for ; offset < n.EndByte(); offset++ {
		switch b.src[offset] {
		case '\n':
			row++
		case '\r', ' ', '\t':
		default:
			return offset, row
		}
	}
	return offset, row
}

// ownEnd returns where a node ends, leaving out a comment on a later line
// that the grammar made its last child
func (b *builder) ownEnd(n *sitter.Node) (offset, row uint) {
	if count := n.NamedChildCount(); count > 0 {
		if c := n.NamedChild(count - 1); b.leads(c) && c.EndByte() == n.EndByte() {
			if start, _ := b.ownStart(c); start != c.StartByte() {
				return c.StartByte(), c.StartPosition().Row
			}
		}
	}
	return n.EndByte(), n.EndPosition().Row
}

// findBody returns the body of a definition, looking through wrapping nodes
// such as `const f = () => {...}`
func findBody(n *sitter.Node, depth int) *sitter.Node {
	if body := n.ChildByFieldName("body"); body != nil {
		return body
	}
	if depth == 0 {
		return nil
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if body := findBody(n.NamedChild(i), depth-1); body != nil {
			return body
		}
	}
	return nil
}

// Fields that hold the name and the receiver of a member access, across grammars
var (
	memberNameFields = []string{"field", "attribute", "property", "name", "method"}
	receiverFields   = []string{"operand", "object", "value", "argument", "path", "scope", "receiver"}
)

func firstField(n *sitter.Node, fields []string) *sitter.Node {
	for _, f := range fields {
		if c := n.ChildByFieldName(f); c != nil {
			return c
		}
	}
	return nil
}

// call records a call expression. field names the child holding the callee;
// when empty the call node itself carries the name and receiver.
func (b *builder) call(n *sitter.Node, field string, sc scope) {
	callee := n
	if field != "" {
		if callee = n.ChildByFieldName(field); callee == nil {
			return
		}
		// foo::<T>() and similar wrap the callee once more
		if inner := callee.ChildByFieldName("function"); inner != nil {
			callee = inner
		}
	}

	ref := Ref{Source: sc.sym, Relation: RelationCalls}
	var receiver string
	if field != "" && callee.NamedChildCount() == 0 {
		ref.Name = b.text(callee)
	} else {
		nameNode := firstField(callee, memberNameFields)
		if nameNode == nil || nameNode.NamedChildCount() > 0 {
			return
		}
		ref.Name = b.text(nameNode)
		if recv := firstField(callee, receiverFields); recv != nil {
			ref.Member = true
			if recv.NamedChildCount() == 0 || recv.Kind() == "variable_name" { // $this
				receiver = b.text(recv)
			}
		}
	}
	if !isIdentifier(ref.Name) || (!ref.Member && b.spec.selfNames[ref.Name]) {
		return
	}
	ref.External = ref.Name

	switch {
	case ref.Member && receiver != "" && (b.spec.selfNames[receiver] || receiver == sc.selfName):
		ref.Container = sc.selfCtx
	case ref.Member:
		if imp, ok := b.imports[receiver]; ok {
			ref.ImportBase = imp.base
			if imp.symbol != "" {
				ref.ImportBase = imp.symbol
			}
			ref.External = receiver + "." + ref.Name
		} else {
			// Type::new(), Class.method(); harmless when it is a variable
			ref.Container = receiver
		}
	default:
		if imp, ok := b.imports[ref.Name]; ok && imp.symbol != "" {
			ref.Name = imp.symbol
			ref.ImportBase = imp.base
			ref.External = imp.path + "." + imp.symbol
		} else if b.spec.builtins[ref.Name] {
			// Only of interest when the project defines something by this name
			ref.External = ""
		} else if b.spec.implicitSelf {
			ref.Container = sc.selfCtx
		}
	}
	b.addRef(ref)
}

func isIdentifier(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n(){}[]\"'`,;")
}

// render turns a fold into a chunk: its source with every child fold replaced
// by a stub, followed by the chunks of the children.
func (b *builder) render(f *fold, parent int) {
	f.chunk = len(b.file.Chunks)
	b.file.Chunks = append(b.file.Chunks, Chunk{})

	content, folded := b.content(f)
	if b.maxSize > 0 && len(content) > b.maxSize && f.node != nil && b.foldBlocks(f, len(content)) {
		content, folded = b.content(f)
	}

	b.file.Chunks[f.chunk] = Chunk{
		Content:     content,
		Stub:        b.stub(f),
		Title:       f.title,
		StartLine:   int(f.startRow) + 1,
		EndLine:     int(f.endRow) + 1,
		StartOffset: int(f.start),
		EndOffset:   int(f.end),
		Parent:      parent,
		Verbatim:    len(folded) == 0,
	}

	for _, c := range folded {
		b.render(c, f.chunk)
	}
}

// stub is the text that stands in for a fold in its parent
func (b *builder) stub(f *fold) string {
	return fmt.Sprintf("%s%s %s ... chunk: lines %d-%d%s", f.indent, f.header, b.spec.comment, f.startRow+1, f.endRow+1, b.spec.commentEnd)
}

// content returns the source of a fold with its children replaced by stubs,
// and the children that were replaced
func (b *builder) content(f *fold) (string, []*fold) {
	var sb strings.Builder
	var folded []*fold
	cursor := f.start
	for _, c := range f.children {
		if c.start < cursor || c.end > f.end {
			// Overlaps a sibling; leave it inline
			b.inline(c, f.chunk)
			continue
		}
		sb.Write(b.src[cursor:c.start])
		sb.WriteString(b.stub(c))
		cursor = c.end
		folded = append(folded, c)
	}
	sb.Write(b.src[cursor:f.end])

	content := sb.String()
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content, folded
}

// foldBlocks folds the largest statements inside an oversized fold, until its
// content fits the size limit or nothing is left to fold. It reports whether
// anything was folded.
func (b *builder) foldBlocks(f *fold, size int) bool {
	var blocks []*sitter.Node
	b.collectBlocks(f.node, f, &blocks)
	sort.SliceStable(blocks, func(i, j int) bool {
		return blocks[i].EndByte()-blocks[i].StartByte() > blocks[j].EndByte()-blocks[j].StartByte()
	})

	changed := false
	for _, n := range blocks {
		if size <= b.maxSize {
			break
		}
		header, _, _ := strings.Cut(b.text(n), "\n")
		block := &fold{
			node:     n,
			title:    f.title,
			start:    b.lineStart(n.StartByte()),
			end:      n.EndByte(),
			startRow: n.StartPosition().Row,
			endRow:   n.EndPosition().Row,
			indent:   string(b.src[b.lineStart(n.StartByte()):n.StartByte()]),
			header:   strings.TrimRight(header, " \t\r"),
		}

		// Definitions inside the block move along with it
		var rest []*fold
		for _, c := range f.children {
			if c.start >= block.start && c.end <= block.end {
				block.children = append(block.children, c)
				size += int(c.end-c.start) - len(b.stub(c))
			} else {
				rest = append(rest, c)
			}
		}
		f.children = append(rest, block)
		size -= int(block.end-block.start) - len(b.stub(block))
		changed = true
	}

	sort.Slice(f.children, func(i, j int) bool { return f.children[i].start < f.children[j].start })
	return changed
}

// collectBlocks finds the outermost nodes below n that can be folded: they
// span several lines, start on a line of their own, and do not cut through an
// existing fold.
func (b *builder) collectBlocks(n *sitter.Node, f *fold, blocks *[]*sitter.Node) {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		startRow, endRow := c.StartPosition().Row, c.EndPosition().Row
		if endRow-startRow+1 < minBlockLines {
			continue
		}
		// A node that fills its parent, like the statement list of a body,
		// would take everything with it
		fills := startRow <= n.StartPosition().Row+1 && endRow+1 >= n.EndPosition().Row
		if !fills && startRow > f.node.StartPosition().Row &&
			b.startsLine(c) && !b.spec.leading[c.Kind()] && !b.cutsFold(c, f) {
			*blocks = append(*blocks, c)
			continue
		}
		b.collectBlocks(c, f, blocks)
	}
}

// cutsFold reports whether a node is inside, or partly overlaps, a child fold
func (b *builder) cutsFold(n *sitter.Node, f *fold) bool {
	start, end := b.lineStart(n.StartByte()), n.EndByte()
	for _, c := range f.children {
		if start < c.end && end > c.start && !(start <= c.start && end >= c.end) {
			return true
		}
		if start == c.start && end == c.end {
			return true
		}
	}
	return false
}

// inline assigns a fold and everything below it to an existing chunk
func (b *builder) inline(f *fold, chunk int) {
	f.chunk = chunk
	for _, c := range f.children {
		b.inline(c, chunk)
	}
}

// Expand returns the content of a chunk with a child chunk shown in full in
// place of its stub
func Expand(content, stub, child string) string {
	return strings.Replace(content, stub, strings.TrimSuffix(child, "\n"), 1)
}

// Render returns the file folded to its skeleton, with the given chunks (and
// so the path of chunks leading to them) shown in full in place of their stubs
func (f *File) Render(expanded map[int]bool) string {
	open := make(map[int]bool)
	for i := range expanded {
		for ; i > 0 && !open[i]; i = f.Chunks[i].Parent {
			open[i] = true
		}
	}
	return f.render(0, open)
}

func (f *File) render(chunk int, open map[int]bool) string {
	content := f.Chunks[chunk].Content
	for i := chunk + 1; i < len(f.Chunks); i++ {
		if f.Chunks[i].Parent == chunk && open[i] {
			content = Expand(content, f.Chunks[i].Stub, f.render(i, open))
		}
	}
	return content
}

// ChunkAt returns the innermost chunk that holds a line (1-indexed)
func (f *File) ChunkAt(line int) int {
	best := 0
	for i, c := range f.Chunks {
		if line >= c.StartLine && line <= c.EndLine && c.EndLine-c.StartLine <= f.Chunks[best].EndLine-f.Chunks[best].StartLine {
			best = i
		}
	}
	return best
}

// InSignature reports whether a byte offset lies in the signature of a
// definition: in the definition, before its body
func (f *File) InSignature(offset int) bool {
	for _, s := range f.Symbols {
		if offset >= s.SigStart && offset < s.SigEnd {
			return true
		}
	}
	return false
}

// SymbolAt returns the innermost symbol that holds a line (1-indexed), or -1
func (f *File) SymbolAt(line int) int {
	best := -1
	for i, s := range f.Symbols {
		if line < s.StartLine || line > s.EndLine {
			continue
		}
		if best < 0 || s.EndLine-s.StartLine <= f.Symbols[best].EndLine-f.Symbols[best].StartLine {
			best = i
		}
	}
	return best
}
