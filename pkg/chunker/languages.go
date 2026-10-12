package chunker

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unsafe"

	tree_sitter_xml "github.com/tree-sitter-grammars/tree-sitter-xml/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tree_sitter_html "github.com/tree-sitter/tree-sitter-html/bindings/go"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	tree_sitter_bcpl "github.com/wouteroostervld/chainsaw/pkg/chunker/bcpl"
	tree_sitter_m68k "github.com/wouteroostervld/chainsaw/pkg/chunker/m68k"
)

type set map[string]bool

func setOf(items ...string) set {
	s := make(set, len(items))
	for _, item := range items {
		s[item] = true
	}
	return s
}

// defKind says what a definition node kind stands for
type defKind struct {
	etype     string // Entity type; empty for nodes that only group (impl blocks, namespaces)
	container bool   // Functions directly inside are methods of it
}

var (
	function  = defKind{etype: TypeFunction}
	class     = defKind{etype: TypeClass, container: true}
	structure = defKind{etype: TypeStruct, container: true}
	iface     = defKind{etype: TypeInterface, container: true}
	typeDef   = defKind{etype: TypeType}
	enum      = defKind{etype: TypeType, container: true}
	group     = defKind{}
)

// langSpec describes how to read one language's syntax tree
type langSpec struct {
	name         string
	grammar      func() unsafe.Pointer
	comment      string             // Comment prefix
	commentEnd   string             // Comment suffix, for languages without line comments
	markup       bool               // A document format: chunks only, no symbols
	defs         map[string]defKind // Definition node kinds
	wrappers     set                // Parent kinds that belong to the definition they wrap
	groups       set                // Parent kinds that hold several definitions; what leads the group leads the first
	leading      set                // Sibling kinds above a definition that belong to it
	calls        map[string]string  // Call node kind -> field holding the callee ("" = the node itself)
	selfNames    set                // Receivers that refer to the enclosing container
	implicitSelf bool               // Bare calls may be calls on the enclosing container
	builtins     set                // Built-in functions and type conversions, called without a receiver
	importKinds  set
	imports      func(b *builder, n *sitter.Node)
	// describe fills in what the generic code cannot derive; false rejects the node
	describe func(b *builder, n *sitter.Node, d *def) bool

	// Assembly has no definition nodes: a definition is a label and the lines
	// up to the next one
	label      func(b *builder, n *sitter.Node) bool // The node is a label that starts a definition
	remark     func(b *builder, n *sitter.Node) bool // The node is a comment, though its kind says otherwise
	labelKinds set                                   // Kinds of label nodes, whether they start a definition or not
	dataKinds  set                                   // Kinds that make what a label names data and not code
	// target names the label an instruction calls or jumps to; "" when it does neither
	target func(b *builder, n *sitter.Node, sc scope) string

	// parse parses the source when it needs more than the grammar alone, and
	// reports whether names are the same whatever their case
	parse func(p *sitter.Parser, src []byte) (*sitter.Tree, bool)

	once sync.Once
	lang *sitter.Language
}

func (s *langSpec) language() *sitter.Language {
	s.once.Do(func() { s.lang = sitter.NewLanguage(s.grammar()) })
	return s.lang
}

// sourceExtensions are file extensions that may appear in import paths
var sourceExtensions = setOf(".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx", ".h", ".hh", ".hpp", ".hxx", ".py", ".rb", ".s", ".asm", ".i", ".b")

var importSeparators = regexp.MustCompile(`::|[/.\\]`)

// importBase returns the last element of an import path: the name of the
// file or directory that most likely holds the imported code.
func (s *langSpec) importBase(path string) string {
	if s.name == "go" {
		return goImportBase(path)
	}
	if ext := filepath.Ext(path); sourceExtensions[ext] {
		path = strings.TrimSuffix(path, ext)
	}
	parts := importSeparators.Split(path, -1)
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			return parts[i]
		}
	}
	return ""
}

var (
	goMajorVersion  = regexp.MustCompile(`^v[0-9]+$`)
	goVersionSuffix = regexp.MustCompile(`\.v[0-9]+$`)
)

func goImportBase(path string) string {
	parts := strings.Split(path, "/")
	base := parts[len(parts)-1]
	if goMajorVersion.MatchString(base) && len(parts) > 1 {
		base = parts[len(parts)-2]
	}
	return goVersionSuffix.ReplaceAllString(base, "")
}

var goSpec = &langSpec{
	name:    "go",
	grammar: tree_sitter_go.Language,
	comment: "//",
	defs: map[string]defKind{
		"function_declaration": function,
		"method_declaration":   function,
		"type_declaration":     typeDef,
		"method_elem":          function, // Method of an interface
	},
	leading: setOf("comment"),
	calls:   map[string]string{"call_expression": "function"},
	builtins: setOf(
		"append", "cap", "clear", "close", "complex", "copy", "delete", "imag", "len", "make", "max", "min",
		"new", "panic", "print", "println", "real", "recover",
		"any", "bool", "byte", "complex64", "complex128", "error", "float32", "float64", "int", "int8", "int16",
		"int32", "int64", "rune", "string", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
	),
	importKinds: setOf("import_spec", "package_clause"),
	imports: func(b *builder, n *sitter.Node) {
		if n.Kind() == "package_clause" {
			if n.NamedChildCount() > 0 {
				b.file.Module = b.text(n.NamedChild(0))
			}
			return
		}
		path := strings.Trim(b.text(n.ChildByFieldName("path")), "\"`")
		local := b.text(n.ChildByFieldName("name"))
		if local == "" {
			local = goImportBase(path)
		}
		b.addImport(local, path, "")
	},
	describe: func(b *builder, n *sitter.Node, d *def) bool {
		switch n.Kind() {
		case "type_declaration":
			if n.NamedChildCount() == 0 {
				return false
			}
			spec := n.NamedChild(0)
			d.name = b.text(spec.ChildByFieldName("name"))
			if t := spec.ChildByFieldName("type"); t != nil {
				switch t.Kind() {
				case "struct_type":
					d.etype = TypeStruct
				case "interface_type":
					d.etype = TypeInterface
					d.holds = true
				}
			}
		case "method_declaration":
			recv := n.ChildByFieldName("receiver")
			if recv == nil || recv.NamedChildCount() == 0 {
				return true
			}
			param := recv.NamedChild(0)
			d.selfName = b.text(param.ChildByFieldName("name"))
			d.container = b.text(findKind(param.ChildByFieldName("type"), "type_identifier"))
		}
		return true
	},
}

var pythonSpec = &langSpec{
	name:    "python",
	grammar: tree_sitter_python.Language,
	comment: "#",
	defs: map[string]defKind{
		"function_definition": function,
		"class_definition":    class,
	},
	wrappers:  setOf("decorated_definition"),
	leading:   setOf("comment"),
	calls:     map[string]string{"call": "function"},
	selfNames: setOf("self", "cls"),
	builtins: setOf(
		"abs", "all", "any", "bool", "bytes", "callable", "dict", "dir", "enumerate", "filter", "float",
		"format", "frozenset", "getattr", "hasattr", "hash", "id", "int", "isinstance", "issubclass", "iter",
		"len", "list", "map", "max", "min", "next", "object", "open", "ord", "print", "range", "repr",
		"reversed", "round", "set", "setattr", "sorted", "str", "sum", "super", "tuple", "type", "zip",
	),
	importKinds: setOf("import_statement", "import_from_statement"),
	imports: func(b *builder, n *sitter.Node) {
		module := n.ChildByFieldName("module_name")
		for i := uint(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(i)
			if module != nil && c.StartByte() == module.StartByte() {
				continue
			}
			name, local := b.text(c), b.text(c)
			if c.Kind() == "aliased_import" {
				name = b.text(c.ChildByFieldName("name"))
				local = b.text(c.ChildByFieldName("alias"))
			} else if c.Kind() != "dotted_name" {
				continue
			}
			if module != nil {
				b.addImport(local, b.text(module), name)
			} else {
				b.addImport(local, name, "")
			}
		}
	},
}

// jsImports handles ES module imports for JavaScript and TypeScript
func jsImports(b *builder, n *sitter.Node) {
	source := b.text(n.ChildByFieldName("source"))
	if source == "" {
		return
	}
	bound := false
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		switch n.Kind() {
		case "identifier":
			// Default and namespace imports bind the module itself
			b.addImport(b.text(n), source, "")
			bound = true
		case "import_specifier":
			name := b.text(n.ChildByFieldName("name"))
			local := name
			if alias := n.ChildByFieldName("alias"); alias != nil {
				local = b.text(alias)
			}
			b.addImport(local, source, name)
			bound = true
		default:
			for i := uint(0); i < n.NamedChildCount(); i++ {
				visit(n.NamedChild(i))
			}
		}
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c.Kind() == "import_clause" {
			visit(c)
		}
	}
	if !bound {
		b.addImport("", source, "")
	}
}

// jsDescribe recognises functions and classes bound with const/let/var
func jsDescribe(b *builder, n *sitter.Node, d *def) bool {
	switch n.Kind() {
	case "lexical_declaration", "variable_declaration":
		if n.NamedChildCount() == 0 {
			return false
		}
		decl := n.NamedChild(0)
		name, value := decl.ChildByFieldName("name"), decl.ChildByFieldName("value")
		if name == nil || value == nil || name.Kind() != "identifier" {
			return false
		}
		switch value.Kind() {
		case "arrow_function", "function_expression", "generator_function":
			d.etype = TypeFunction
		case "class":
			d.etype = TypeClass
		default:
			return false
		}
		d.name = b.text(name)
	}
	return true
}

var jsDefs = map[string]defKind{
	"function_declaration":           function,
	"generator_function_declaration": function,
	"method_definition":              function,
	"class_declaration":              class,
	"lexical_declaration":            function,
	"variable_declaration":           function,
}

var tsDefs = map[string]defKind{
	"function_declaration":           function,
	"generator_function_declaration": function,
	"method_definition":              function,
	"class_declaration":              class,
	"abstract_class_declaration":     class,
	"interface_declaration":          iface,
	"type_alias_declaration":         typeDef,
	"enum_declaration":               typeDef,
	"lexical_declaration":            function,
	"variable_declaration":           function,
	"internal_module":                group,
}

var jsBuiltins = setOf(
	"require", "Array", "BigInt", "Boolean", "Date", "Error", "Map", "Number", "Object", "Promise", "RegExp",
	"Set", "String", "Symbol", "parseFloat", "parseInt",
)

var jsCalls = map[string]string{"call_expression": "function", "new_expression": "constructor"}

func jsLike(name string, grammar func() unsafe.Pointer, defs map[string]defKind) *langSpec {
	return &langSpec{
		name:        name,
		grammar:     grammar,
		comment:     "//",
		defs:        defs,
		wrappers:    setOf("export_statement"),
		leading:     setOf("comment", "decorator"),
		calls:       jsCalls,
		selfNames:   setOf("this", "super"),
		builtins:    jsBuiltins,
		importKinds: setOf("import_statement"),
		imports:     jsImports,
		describe:    jsDescribe,
	}
}

var rustSpec = &langSpec{
	name:    "rust",
	grammar: tree_sitter_rust.Language,
	comment: "//",
	defs: map[string]defKind{
		"function_item": function,
		"struct_item":   {etype: TypeStruct},
		"enum_item":     typeDef,
		"union_item":    {etype: TypeStruct},
		"trait_item":    iface,
		"impl_item":     {container: true},
		"mod_item":      group,
	},
	leading:     setOf("line_comment", "block_comment", "attribute_item"),
	calls:       map[string]string{"call_expression": "function"},
	selfNames:   setOf("self", "Self"),
	builtins:    setOf("Some", "Ok", "Err", "Box", "Vec", "String"),
	importKinds: setOf("use_declaration"),
	imports: func(b *builder, n *sitter.Node) {
		rustUse(b, n.ChildByFieldName("argument"), "")
	},
	describe: func(b *builder, n *sitter.Node, d *def) bool {
		if n.Kind() == "impl_item" {
			d.name = b.text(findKind(n.ChildByFieldName("type"), "type_identifier"))
		}
		return true
	},
}

func rustUse(b *builder, n *sitter.Node, prefix string) {
	if n == nil {
		return
	}
	join := func(path string) string {
		if prefix == "" {
			return path
		}
		return prefix + "::" + path
	}
	switch n.Kind() {
	case "identifier":
		if prefix != "" {
			b.addImport(b.text(n), prefix, b.text(n))
		}
	case "scoped_identifier":
		name := b.text(n.ChildByFieldName("name"))
		b.addImport(name, join(b.text(n.ChildByFieldName("path"))), name)
	case "use_as_clause":
		path := n.ChildByFieldName("path")
		alias := b.text(n.ChildByFieldName("alias"))
		if path != nil && path.Kind() == "scoped_identifier" {
			b.addImport(alias, join(b.text(path.ChildByFieldName("path"))), b.text(path.ChildByFieldName("name")))
		} else if prefix != "" {
			b.addImport(alias, prefix, b.text(path))
		}
	case "scoped_use_list":
		rustUse(b, n.ChildByFieldName("list"), join(b.text(n.ChildByFieldName("path"))))
	case "use_list":
		for i := uint(0); i < n.NamedChildCount(); i++ {
			rustUse(b, n.NamedChild(i), prefix)
		}
	}
}

var javaSpec = &langSpec{
	name:    "java",
	grammar: tree_sitter_java.Language,
	comment: "//",
	defs: map[string]defKind{
		"class_declaration":       class,
		"record_declaration":      class,
		"interface_declaration":   iface,
		"enum_declaration":        enum,
		"method_declaration":      function,
		"constructor_declaration": function,
	},
	leading:      setOf("line_comment", "block_comment"),
	calls:        map[string]string{"method_invocation": "", "object_creation_expression": "type"},
	selfNames:    setOf("this", "super"),
	implicitSelf: true,
	importKinds:  setOf("import_declaration"),
	imports: func(b *builder, n *sitter.Node) {
		if n.NamedChildCount() == 0 {
			return
		}
		path := n.NamedChild(0)
		if strings.HasSuffix(strings.TrimSuffix(strings.TrimSpace(b.text(n)), ";"), "*") {
			b.addImport("", b.text(path), "")
			return
		}
		// The imported class is the module: Foo.bar() resolves into Foo.java
		b.addImport(b.text(path.ChildByFieldName("name")), b.text(path), "")
	},
}

// cDescribe finds the name of C and C++ definitions, which sits inside the declarator
func cDescribe(b *builder, n *sitter.Node, d *def) bool {
	switch n.Kind() {
	case "function_definition", "type_definition":
		decl := n.ChildByFieldName("declarator")
		for decl != nil && decl.ChildByFieldName("declarator") != nil {
			decl = decl.ChildByFieldName("declarator")
		}
		if decl == nil {
			return false
		}
		if decl.Kind() == "qualified_identifier" {
			d.container = b.text(decl.ChildByFieldName("scope"))
			decl = decl.ChildByFieldName("name")
		}
		d.name = b.text(decl)
		return isIdentifier(d.name)
	case "struct_specifier", "class_specifier", "union_specifier", "enum_specifier":
		// Only definitions, not uses of the type
		return n.ChildByFieldName("body") != nil
	}
	return true
}

func cImports(b *builder, n *sitter.Node) {
	b.addImport("", b.text(n.ChildByFieldName("path")), "")
}

var cSpec = &langSpec{
	name:    "c",
	grammar: tree_sitter_c.Language,
	comment: "//",
	defs: map[string]defKind{
		"function_definition": function,
		"struct_specifier":    {etype: TypeStruct},
		"union_specifier":     {etype: TypeStruct},
		"enum_specifier":      typeDef,
		"type_definition":     typeDef,
	},
	leading:     setOf("comment"),
	calls:       map[string]string{"call_expression": "function"},
	importKinds: setOf("preproc_include"),
	imports:     cImports,
	describe:    cDescribe,
}

var cppSpec = &langSpec{
	name:    "cpp",
	grammar: tree_sitter_cpp.Language,
	comment: "//",
	defs: map[string]defKind{
		"function_definition":  function,
		"class_specifier":      class,
		"struct_specifier":     structure,
		"union_specifier":      {etype: TypeStruct},
		"enum_specifier":       typeDef,
		"type_definition":      typeDef,
		"namespace_definition": group,
	},
	wrappers:     setOf("template_declaration"),
	leading:      setOf("comment"),
	calls:        map[string]string{"call_expression": "function", "new_expression": "type"},
	selfNames:    setOf("this"),
	implicitSelf: true,
	importKinds:  setOf("preproc_include"),
	imports:      cImports,
	describe:     cDescribe,
}

var rubySpec = &langSpec{
	name:    "ruby",
	grammar: tree_sitter_ruby.Language,
	comment: "#",
	defs: map[string]defKind{
		"method":           function,
		"singleton_method": function,
		"class":            class,
		"module":           class,
	},
	leading:      setOf("comment"),
	calls:        map[string]string{"call": ""},
	selfNames:    setOf("self"),
	implicitSelf: true,
	builtins: setOf(
		"attr_accessor", "attr_reader", "attr_writer", "extend", "include", "lambda", "loop", "p", "print",
		"private", "proc", "protected", "public", "puts", "raise", "require", "require_relative",
	),
}

// BCPL has functions (LET f(x) = expression) and routines (LET f(x) BE
// command), often chained with AND into one declaration. Headers are included
// textually with GET.
var bcplSpec = &langSpec{
	name:    "bcpl",
	grammar: tree_sitter_bcpl.Language,
	comment: "//",
	defs: map[string]defKind{
		"function_definition": function,
		"routine_definition":  function,
		"constant_definition": {etype: TypeConstant},
	},
	parse:       bcplParse,
	describe:    bcplDescribe,
	groups:      setOf("let_declaration"),
	leading:     setOf("comment", "block_comment"),
	calls:       map[string]string{"call_expression": "function"},
	importKinds: setOf("get_directive"),
	imports: func(b *builder, n *sitter.Node) {
		b.addImport("", b.text(n.ChildByFieldName("path")), "")
	},
}

// bcplDescribe tells the entries of MANIFEST, which are constants, from those
// of GLOBAL and STATIC, which are cells that hold a value or a routine
func bcplDescribe(b *builder, n *sitter.Node, d *def) bool {
	if n.Kind() != "constant_definition" {
		return true
	}
	if p := n.Parent(); p != nil && p.Kind() != "manifest_declaration" {
		d.etype = TypeVariable
	}
	return true
}

// m68kLocal reports whether a label is local to the global label above it:
// .loop, 1$, or \@ inside a macro
func m68kLocal(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, "$") || strings.Contains(name, `\`)
}

// m68kBranches are the instructions that transfer control to their last operand
var m68kBranches = setOf(
	"jsr", "jmp", "bsr", "bra",
	"bhi", "bls", "bcc", "bhs", "bcs", "blo", "bne", "beq", "bvc", "bvs", "bpl", "bmi", "bge", "blt", "bgt", "ble",
	"dbt", "dbf", "dbra", "dbhi", "dbls", "dbcc", "dbhs", "dbcs", "dblo", "dbne", "dbeq", "dbvc", "dbvs", "dbpl",
	"dbmi", "dbge", "dblt", "dbgt", "dble",
)

// m68kLabel returns the name of a label, or "" when the node is not one
func m68kLabel(b *builder, n *sitter.Node) string {
	if kind := n.Kind(); kind != "label" && kind != "external_label" {
		return ""
	}
	return b.text(n.ChildByFieldName("name"))
}

// m68kTarget returns the label an instruction calls or jumps to. A jump
// counts as a call when it leaves the definition it is in: that is how one
// routine continues in another. A call through a register with a named
// offset, jsr _LVOAllocMem(a6), is a call of that name.
func m68kTarget(b *builder, n *sitter.Node, sc scope) string {
	if n.Kind() != "instruction" {
		return ""
	}
	mnemonic := strings.ToLower(b.text(n.ChildByFieldName("mnemonic")))
	operands := n.ChildByFieldName("operands")
	if !m68kBranches[mnemonic] || operands == nil || operands.NamedChildCount() == 0 {
		return ""
	}

	target := operands.NamedChild(operands.NamedChildCount() - 1)
	switch target.Kind() {
	case "offset_address":
		target = target.ChildByFieldName("offset")
	case "absolute_value":
		target = target.ChildByFieldName("value")
	}
	if target == nil || target.Kind() != "symbol" {
		return ""
	}
	// The grammar takes the size of an absolute address, Start.l, as part of the name
	name := b.text(target)
	if n := len(name); n > 2 && name[n-2] == '.' && strings.ContainsRune("lwLW", rune(name[n-1])) {
		name = name[:n-2]
	}
	if m68kLocal(name) || !isIdentifier(name) {
		return ""
	}
	if mnemonic != "jsr" && mnemonic != "bsr" && sc.sym >= 0 && b.file.Symbols[sc.sym].Name == name {
		return "" // A loop
	}
	return name
}

// Motorola 68000 assembly, in the syntax of the Amiga assemblers (vasm,
// Devpac, AsmOne). Global labels are the definitions; local labels belong to
// the one above them.
var m68kSpec = &langSpec{
	name:        "m68k",
	grammar:     tree_sitter_m68k.Language,
	comment:     ";",
	defs:        map[string]defKind{"macro_definition": function},
	leading:     setOf("comment"),
	calls:       map[string]string{"macro_call": "name"},
	importKinds: setOf("include"),
	imports: func(b *builder, n *sitter.Node) {
		b.addImport("", b.text(n.ChildByFieldName("path")), "")
	},
	label: func(b *builder, n *sitter.Node) bool {
		name := m68kLabel(b, n)
		return name != "" && name[0] != '*' && !m68kLocal(name)
	},
	// A line that holds only a * is a comment, which the grammar reads as a label
	remark: func(b *builder, n *sitter.Node) bool {
		return strings.HasPrefix(m68kLabel(b, n), "*")
	},
	labelKinds: setOf("label", "external_label"),
	dataKinds:  setOf("data_constant", "data_space", "include_bin"),
	target:     m68kTarget,
}

// Markdown is split along its headings by markdownSections; it has no grammar
var markdownSpec = &langSpec{
	name:       "markdown",
	comment:    "<!--",
	commentEnd: " -->",
	markup:     true,
}

// HTML and XML have no definitions to fold. A chunk that is too large gets
// its largest elements folded instead.
var htmlSpec = &langSpec{
	name:       "html",
	grammar:    tree_sitter_html.Language,
	comment:    "<!--",
	commentEnd: " -->",
	markup:     true,
	leading:    setOf("comment"),
}

var xmlSpec = &langSpec{
	name:       "xml",
	grammar:    tree_sitter_xml.LanguageXML,
	comment:    "<!--",
	commentEnd: " -->",
	markup:     true,
	leading:    setOf("Comment"),
}

var extensions = map[string]*langSpec{
	".md":       markdownSpec,
	".markdown": markdownSpec,
	".html":     htmlSpec,
	".htm":      htmlSpec,
	".xml":      xmlSpec,
	".xsd":      xmlSpec,
	".xsl":      xmlSpec,
	".xslt":     xmlSpec,
	".svg":      xmlSpec,
	".go":       goSpec,
	".py":       pythonSpec,
	".pyi":      pythonSpec,
	".rs":       rustSpec,
	".java":     javaSpec,
	".c":        cSpec,
	".h":        cppSpec,
	".cc":       cppSpec,
	".cpp":      cppSpec,
	".cxx":      cppSpec,
	".hh":       cppSpec,
	".hpp":      cppSpec,
	".hxx":      cppSpec,
	".rb":       rubySpec,
	".b":        bcplSpec,
	".bcpl":     bcplSpec,
	".s":        m68kSpec,
	".asm":      m68kSpec,
	".i":        m68kSpec,
}

func init() {
	js := jsLike("javascript", tree_sitter_javascript.Language, jsDefs)
	ts := jsLike("typescript", tree_sitter_typescript.LanguageTypescript, tsDefs)
	tsx := jsLike("tsx", tree_sitter_typescript.LanguageTSX, tsDefs)
	for _, ext := range []string{".js", ".mjs", ".cjs", ".jsx"} {
		extensions[ext] = js
	}
	for _, ext := range []string{".ts", ".mts", ".cts"} {
		extensions[ext] = ts
	}
	extensions[".tsx"] = tsx
}

// languages are the specs by the name of their language
var languages = map[string]*langSpec{}

func init() {
	for _, spec := range extensions {
		languages[spec.name] = spec
	}
}

// ambiguous are the extensions that more than one language uses. The spec
// in extensions is what such a file is, unless its content says otherwise.
var ambiguous = setOf(".h")

// specFor tells the language of a file: by its extension, and by its content
// when it has none or one that says too little. Files of BCPL from before
// there were extensions have none, and its headers share .h with C.
func specFor(path string, src []byte) *langSpec {
	ext := strings.ToLower(filepath.Ext(path))
	spec := extensions[ext]
	if (ext == "" || ambiguous[ext]) && looksLikeBCPL(src) {
		return bcplSpec
	}
	return spec
}

// Recognised reports whether a file on disk is one that can be parsed. For
// most the name says so; one without an extension is read to see, a little
// of it.
func Recognised(path string) bool {
	if Supported(path) {
		return true
	}
	if filepath.Ext(path) != "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, sniffSize)
	n, _ := io.ReadFull(f, head)
	return specFor(path, head[:n]) != nil
}

// findKind returns the first node of the given kind at or below n
func findKind(n *sitter.Node, kind string) *sitter.Node {
	if n == nil || n.Kind() == kind {
		return n
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if found := findKind(n.NamedChild(i), kind); found != nil {
			return found
		}
	}
	return nil
}
