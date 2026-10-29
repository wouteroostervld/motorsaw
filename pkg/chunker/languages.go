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
	tree_sitter_php "github.com/tree-sitter/tree-sitter-php/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	tree_sitter_bcpl "github.com/wouteroostervld/motorsaw/pkg/chunker/bcpl"
	tree_sitter_cobol "github.com/wouteroostervld/motorsaw/pkg/chunker/cobol"
	tree_sitter_gas "github.com/wouteroostervld/motorsaw/pkg/chunker/gas"
	tree_sitter_m68k "github.com/wouteroostervld/motorsaw/pkg/chunker/m68k"
	tree_sitter_nasm "github.com/wouteroostervld/motorsaw/pkg/chunker/nasm"
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
	// data and passive are for grammars where a kind does not say enough
	data    func(b *builder, n *sitter.Node) bool // The node, a label or what follows one, makes what it names data
	passive func(b *builder, n *sitter.Node) bool // The node is neither code nor data, and the label above it names what follows
	// target names the label an instruction calls or jumps to; "" when it does neither
	target func(b *builder, n *sitter.Node, sc scope) string
	// shared says the extensions are used by other languages too, so that a
	// file which parses badly is taken to be in one of those
	shared bool

	// prepare parses the source when it needs more than the grammar alone,
	// and reports whether names are the same whatever their case
	prepare func(p *sitter.Parser, src []byte) (*sitter.Tree, bool)

	once sync.Once
	lang *sitter.Language
}

func (s *langSpec) language() *sitter.Language {
	s.once.Do(func() { s.lang = sitter.NewLanguage(s.grammar()) })
	return s.lang
}

// sourceExtensions are file extensions that may appear in import paths
var sourceExtensions = setOf(".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx", ".h", ".hh", ".hpp", ".hxx", ".py", ".rb", ".php", ".s", ".asm", ".i", ".inc", ".nasm", ".b")

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

// phpUse reads one `use` declaration. The clause of a grouped one,
// use Foo\{A, B as C}, is a name without a path of its own.
func phpUse(b *builder, n *sitter.Node) {
	var prefix string
	if n.NamedChildCount() > 0 && n.NamedChild(0).Kind() == "namespace_name" {
		prefix = b.text(n.NamedChild(0)) + `\`
	}
	var clauses []*sitter.Node
	for i := uint(0); i < n.NamedChildCount(); i++ {
		switch c := n.NamedChild(i); c.Kind() {
		case "namespace_use_clause":
			clauses = append(clauses, c)
		case "namespace_use_group":
			for j := uint(0); j < c.NamedChildCount(); j++ {
				clauses = append(clauses, c.NamedChild(j))
			}
		}
	}
	for _, c := range clauses {
		if c.NamedChildCount() == 0 {
			continue
		}
		path := strings.TrimPrefix(prefix+b.text(c.NamedChild(0)), `\`)
		local := b.text(c.ChildByFieldName("alias"))
		if local == "" {
			local = path[strings.LastIndex(path, `\`)+1:]
		}
		// The imported class is the module: Foo::bar() resolves into Foo.php
		b.addImport(local, path, "")
	}
}

// phpInclude reads require and include with a literal path
func phpInclude(b *builder, n *sitter.Node) {
	if n.NamedChildCount() == 1 && n.NamedChild(0).Kind() == "string" {
		b.addImport("", b.text(n.NamedChild(0)), "")
	}
}

var phpSpec = &langSpec{
	name:    "php",
	grammar: tree_sitter_php.LanguagePHP,
	comment: "//",
	defs: map[string]defKind{
		"function_definition":   function,
		"method_declaration":    function,
		"class_declaration":     class,
		"trait_declaration":     class,
		"interface_declaration": iface,
		"enum_declaration":      enum,
	},
	leading: setOf("comment"),
	calls: map[string]string{
		"function_call_expression":        "function",
		"member_call_expression":          "",
		"nullsafe_member_call_expression": "",
		"scoped_call_expression":          "",
	},
	selfNames: setOf("$this", "self", "static"),
	importKinds: setOf(
		"namespace_use_declaration", "require_expression", "require_once_expression",
		"include_expression", "include_once_expression",
	),
	imports: func(b *builder, n *sitter.Node) {
		if n.Kind() == "namespace_use_declaration" {
			phpUse(b, n)
		} else {
			phpInclude(b, n)
		}
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
	prepare:     bcplParse,
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

// m68kLoad is what an address register was last loaded with, when that is
// something that can be called
type m68kLoad struct {
	names    []string // The label, or what a comment says it is
	relation string   // calls when the operand names the label, may_call when a comment does
	age      int      // Instructions since
}

// m68kLoadSpan is how many instructions a register is taken to keep what it
// was loaded with: a call through it comes right after, with at most the
// arguments set up in between
const m68kLoadSpan = 6

var (
	m68kName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.$]*$`)
	// The number of a BCPL global, as a disassembly notes it: G82, G-21
	m68kGlobal = regexp.MustCompile(`^G-?[0-9]+$`)
)

// m68kComment returns the words of the comment after an instruction, on the
// same line
func m68kComment(b *builder, n *sitter.Node) []string {
	c := n.NextNamedSibling()
	if c == nil || c.Kind() != "comment" {
		return nil
	}
	if _, row := b.ownStart(c); row != n.EndPosition().Row {
		return nil
	}
	return strings.Fields(strings.TrimLeft(b.text(c), "; \t\r\n*"))
}

// m68kNamed returns the names a comment gives to what an instruction calls
// or loads. Generated sources and disassemblies say there what the syntax
// cannot:
//
//	jsr     -198(a6)        ; exec AllocMem           a library vector
//	movea.l 328(a2),a4      ; G82 dos_g82_UnLoadSeg   a BCPL global
//
// The first is the routine AllocMem of exec, which such sources define as
// exec_AllocMem; the second names its label outright. A comment of any
// other shape is prose, and names nothing.
func m68kNamed(words []string, vector bool) []string {
	for _, w := range words {
		if !m68kName.MatchString(w) {
			return nil
		}
	}
	switch {
	case len(words) == 1 && !m68kGlobal.MatchString(words[0]):
		return words
	case len(words) == 2 && m68kGlobal.MatchString(words[0]):
		return words[1:]
	case len(words) == 2 && vector:
		return []string{words[0] + "_" + words[1], words[1]}
	}
	return nil
}

// m68kSymbol returns the label an operand names: label, label(pc), (label).l,
// #label, or label-base(an), an offset from a base that is in the register.
// "" when it names none, as -198(a6) and (a0).
func m68kSymbol(b *builder, operand *sitter.Node) string {
	target := operand
	switch target.Kind() {
	case "offset_address":
		target = target.ChildByFieldName("offset")
	case "absolute_value", "immediate_value":
		target = target.ChildByFieldName("value")
	}
	if target != nil && target.Kind() == "binary_expression" && target.NamedChildCount() > 0 {
		target = target.NamedChild(0)
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
	return name
}

// m68kTarget returns the label an instruction calls or jumps to. A jump
// counts as a call when it leaves the definition it is in: that is how one
// routine continues in another. A call through a register with a named
// offset, jsr _LVOAllocMem(a6), is a call of that name.
//
// Most calls of compiled code go through a register, and name nothing. Two
// things still tell where they go, and are recorded here as references of
// their own:
//
//   - the register was loaded with a label just before: lea Handler(pc),a0
//     and jsr (a0). Code compiled from BCPL loads the routine into a4 and
//     calls through a5, which holds the entry sequence: lea
//     routine-base(a4),a4 and jsr (a5).
//   - a comment after the call, or after the load, names the target (see
//     m68kNamed). That is not checked by anything, so it is a may_call.
func m68kTarget(b *builder, n *sitter.Node, sc scope) string {
	switch n.Kind() {
	case "label", "external_label":
		// What a register holds does not carry over into the next routine
		if name := m68kLabel(b, n); name != "" && !m68kLocal(name) {
			b.loads = nil
		}
		return ""
	case "instruction":
	default:
		return ""
	}
	mnemonic := strings.ToLower(b.text(n.ChildByFieldName("mnemonic")))
	operands := n.ChildByFieldName("operands")
	for reg, load := range b.loads {
		if load.age++; load.age > m68kLoadSpan {
			delete(b.loads, reg)
		}
	}
	if operands == nil || operands.NamedChildCount() == 0 {
		return ""
	}
	last := operands.NamedChild(operands.NamedChildCount() - 1)

	if !m68kBranches[mnemonic] {
		// Whatever is written to an address register replaces what it held
		if operands.NamedChildCount() < 2 || last.Kind() != "address_register" {
			return ""
		}
		reg := strings.ToLower(b.text(last))
		delete(b.loads, reg)
		if mnemonic != "lea" && mnemonic != "movea" && mnemonic != "move" {
			return ""
		}
		source := operands.NamedChild(0)
		load := &m68kLoad{relation: RelationCalls}
		if name := m68kSymbol(b, source); name != "" && (mnemonic == "lea" || source.Kind() == "immediate_value") {
			load.names = []string{name}
		} else if mnemonic != "lea" {
			load.names, load.relation = m68kNamed(m68kComment(b, n), false), RelationMayCall
		}
		if len(load.names) > 0 {
			if b.loads == nil {
				b.loads = make(map[string]*m68kLoad)
			}
			b.loads[reg] = load
		}
		return ""
	}

	if name := m68kSymbol(b, last); name != "" {
		if mnemonic != "jsr" && mnemonic != "bsr" && sc.sym >= 0 && b.file.Symbols[sc.sym].Name == name {
			return "" // A loop
		}
		return name
	}
	if mnemonic != "jsr" && mnemonic != "jmp" {
		return ""
	}

	// Through a register
	names, relation := []string(nil), RelationMayCall
	if reg := m68kRegister(b, last); reg != "" {
		load := b.loads[reg]
		if load == nil && reg == "a5" {
			load, reg = b.loads["a4"], "a4"
		}
		if load != nil {
			names, relation = load.names, load.relation
			delete(b.loads, reg)
		}
	}
	if names == nil {
		names = m68kNamed(m68kComment(b, n), last.Kind() == "offset_address")
	}
	for i, name := range names {
		ref := Ref{Source: sc.sym, Relation: relation, Name: name}
		// One word may be prose, and the second reading of two is a guess:
		// they count when something has that name. What an operand names,
		// and the label a comment spells out in full, is in the graph anyway.
		if relation == RelationCalls || (i == 0 && len(names) > 1) || strings.Contains(name, "_") {
			ref.External = name
		}
		b.addRef(ref)
	}
	return ""
}

// m68kRegister returns the address register an operand goes through with
// nothing added, (a0), or ""
func m68kRegister(b *builder, operand *sitter.Node) string {
	if operand.Kind() != "indirect_address" || operand.NamedChildCount() != 1 {
		return ""
	}
	if reg := operand.NamedChild(0); reg.Kind() == "address_register" {
		return strings.ToLower(b.text(reg))
	}
	return ""
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
	shared:     true,
}

// x86Branches are the instructions that transfer control to their operand,
// in Intel mnemonics and in the AT&T forms with a suffix
var x86Branches = setOf(
	"call", "callq", "calll", "callw", "jmp", "jmpq", "jmpl", "jmpw",
	"ja", "jae", "jb", "jbe", "jc", "je", "jg", "jge", "jl", "jle",
	"jna", "jnae", "jnb", "jnbe", "jnc", "jne", "jng", "jnge", "jnl", "jnle",
	"jno", "jnp", "jns", "jnz", "jo", "jp", "jpe", "jpo", "js", "jz",
	"jcxz", "jecxz", "jrcxz", "loop", "loope", "loopne", "loopz", "loopnz",
)

// x86Target applies the rule of m68kTarget to x86: a call is a call, and so is
// a jump that leaves the definition it is in. name is the operand when it is a
// plain symbol.
func x86Target(b *builder, mnemonic, name string, sc scope) string {
	mnemonic = strings.ToLower(mnemonic)
	if !x86Branches[mnemonic] || !isIdentifier(name) {
		return ""
	}
	if !strings.HasPrefix(mnemonic, "call") && sc.sym >= 0 && b.file.Symbols[sc.sym].Name == name {
		return "" // A loop
	}
	return name
}

// nasmLine returns the label and the instruction of a source line, either of
// which may be nil
func nasmLine(n *sitter.Node) (label, instruction *sitter.Node) {
	if n.Kind() != "source_line" {
		return nil, nil
	}
	return findChild(n, "label"), findChild(n, "instruction")
}

// findChild returns the first child of a kind, or nil
func findChild(n *sitter.Node, kind string) *sitter.Node {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c.Kind() == kind {
			return c
		}
	}
	return nil
}

// nasmLabel returns the name of the label a source line starts with, or ""
func nasmLabel(b *builder, n *sitter.Node) string {
	label, _ := nasmLine(n)
	if label == nil {
		return ""
	}
	return b.text(label.ChildByFieldName("name"))
}

// nasmLocal reports whether a label is local to the global label above it
func nasmLocal(name string) bool {
	return strings.HasPrefix(name, ".")
}

func nasmTarget(b *builder, n *sitter.Node, sc scope) string {
	if n.Kind() != "actual_instruction" {
		return ""
	}
	operands := n.ChildByFieldName("operands")
	if operands == nil || operands.NamedChildCount() == 0 {
		return ""
	}
	// jmp short next, call near helper: the symbol is what the operand ends with
	operand := operands.NamedChild(operands.NamedChildCount() - 1)
	var symbol *sitter.Node
	for i := uint(0); i < operand.NamedChildCount(); i++ {
		if c := operand.NamedChild(i); c.Kind() == "word" {
			symbol = c
		} else if c.Kind() != "operand_prefix" {
			return "" // [rel puts], rax, a number
		}
	}
	if symbol == nil {
		return ""
	}
	name := b.text(symbol)
	if nasmLocal(name) {
		return ""
	}
	return x86Target(b, b.text(n.ChildByFieldName("instruction")), name, sc)
}

// x86 assembly in the Intel syntax of NASM, which Ghidra and most
// disassemblers print as well. Global labels are the definitions; local
// labels, .loop, belong to the one above them.
var nasmSpec = &langSpec{
	name:    "nasm",
	grammar: tree_sitter_nasm.Language,
	comment: ";",
	defs:    map[string]defKind{"preproc_multiline_macro": function},
	leading: setOf("comment"),
	// The label of a line is what defines, whatever else is on the line
	describe: func(b *builder, n *sitter.Node, d *def) bool {
		if n.Kind() == "source_line" {
			d.name = nasmLabel(b, n)
		}
		return true
	},
	label: func(b *builder, n *sitter.Node) bool {
		name := nasmLabel(b, n)
		return name != "" && !nasmLocal(name)
	},
	// A line with nothing but a label on it
	passive: func(b *builder, n *sitter.Node) bool {
		label, instruction := nasmLine(n)
		return label != nil && instruction == nil
	},
	data: func(b *builder, n *sitter.Node) bool {
		_, instruction := nasmLine(n)
		if instruction == nil || instruction.NamedChildCount() == 0 {
			return false
		}
		switch instruction.NamedChild(0).Kind() {
		case "pseudo_instruction_dx", "pseudo_instruction_resx", "pseudo_instruction_equ_command", "pseudo_instruction_incbin_command":
			return true
		}
		return false
	},
	target:      nasmTarget,
	importKinds: setOf("preproc_include"),
	imports: func(b *builder, n *sitter.Node) {
		b.addImport("", b.text(n.ChildByFieldName("path")), "")
	},
	shared: true,
}

// gasLocal reports whether a label is local to its file: the .L labels the
// compilers make, and the numeric labels, 1: and jmp 1b
func gasLocal(name string) bool {
	if strings.HasPrefix(name, ".L") {
		return true
	}
	return name != "" && strings.Trim(name, "0123456789") == ""
}

func gasLabel(b *builder, n *sitter.Node) string {
	if n.Kind() != "label" {
		return ""
	}
	return b.text(n.ChildByFieldName("name"))
}

// gasDirective returns the name of a directive, or ""
func gasDirective(b *builder, n *sitter.Node) string {
	if n.Kind() != "directive" {
		return ""
	}
	return strings.ToLower(b.text(n.ChildByFieldName("name")))
}

// gasDataDirectives put data where they are
var gasDataDirectives = setOf(
	".byte", ".word", ".short", ".value", ".2byte", ".long", ".int", ".4byte", ".quad", ".8byte", ".octa",
	".ascii", ".asciz", ".string", ".string8", ".string16", ".float", ".single", ".double",
	".zero", ".space", ".skip", ".fill", ".incbin", ".uleb128", ".sleb128",
)

// gasPassiveDirectives tell the assembler about a symbol or a place, and put
// neither code nor data anywhere
var gasPassiveDirectives = setOf(
	".align", ".balign", ".p2align", ".type", ".size", ".globl", ".global", ".weak", ".hidden",
	".protected", ".internal", ".local", ".file", ".loc", ".ident", ".comm", ".lcomm", ".set", ".equ",
)

func gasTarget(b *builder, n *sitter.Node, sc scope) string {
	if n.Kind() != "instruction" {
		return ""
	}
	operands := n.ChildByFieldName("operands")
	if operands == nil || operands.NamedChildCount() == 0 {
		return ""
	}
	// Only a plain symbol: *%rax and *table(,%rax,8) are calls through a register or a table
	target := operands.NamedChild(operands.NamedChildCount() - 1)
	if target.Kind() != "symbol" {
		return ""
	}
	name := b.text(target.ChildByFieldName("name"))
	if gasLocal(name) || target.ChildByFieldName("name").Kind() == "local_reference" {
		return ""
	}
	return x86Target(b, b.text(n.ChildByFieldName("mnemonic")), name, sc)
}

// x86 assembly in the AT&T syntax of the GNU assembler: what gcc -S, clang -S
// and objdump write. Global labels are the definitions; the .L labels of the
// compilers and numeric labels belong to the one above them.
var gasSpec = &langSpec{
	name:    "gas",
	grammar: tree_sitter_gas.Language,
	comment: "#",
	leading: setOf("comment"),
	label: func(b *builder, n *sitter.Node) bool {
		name := gasLabel(b, n)
		return name != "" && !gasLocal(name)
	},
	labelKinds: setOf("label"),
	data: func(b *builder, n *sitter.Node) bool {
		return gasDataDirectives[gasDirective(b, n)]
	},
	passive: func(b *builder, n *sitter.Node) bool {
		name := gasDirective(b, n)
		return gasPassiveDirectives[name] || strings.HasPrefix(name, ".cfi_")
	},
	target:      gasTarget,
	importKinds: setOf("directive"),
	imports: func(b *builder, n *sitter.Node) {
		if gasDirective(b, n) != ".include" {
			return
		}
		if args := n.ChildByFieldName("arguments"); args != nil && args.NamedChildCount() > 0 {
			b.addImport("", b.text(args.NamedChild(0)), "")
		}
	},
	shared: true,
}

// cobolName is how a name is kept: COBOL does not tell cases apart
func cobolName(s string) string { return strings.ToUpper(strings.Trim(s, "'\"")) }

// cobolIsName reports whether a node is a name, as opposed to a literal or an expression
func cobolIsName(n *sitter.Node) bool {
	return n.IsNamed() && (n.Kind() == "WORD" || n.Kind() == "numeric_name")
}

// cobolNamedChild returns the first child of a kind, or nil
func cobolNamedChild(n *sitter.Node, kind string) *sitter.Node {
	if n == nil {
		return nil
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c.Kind() == kind {
			return c
		}
	}
	return nil
}

// cobolFirstName returns the name a paragraph, section or program id starts with
func cobolFirstName(b *builder, n *sitter.Node) string {
	if n == nil {
		return ""
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); cobolIsName(c) {
			return cobolName(b.text(c))
		}
	}
	return ""
}

// cobolLevelOne reports whether a data description starts a record: level 01
// or 77. The items below a record are its fields, and are not definitions.
func cobolLevelOne(b *builder, n *sitter.Node) bool {
	level := b.text(cobolNamedChild(n, "level_number"))
	return level == "01" || level == "1" || level == "77"
}

// cobolTarget adds the calls a statement makes. A paragraph or section is
// performed or jumped to by name; a program is called by a literal name, or
// through EXEC CICS LINK and XCTL.
func cobolTarget(b *builder, n *sitter.Node, sc scope) string {
	call := func(name string) {
		if name != "" {
			b.addRef(Ref{Source: sc.sym, Relation: RelationCalls, Name: name, External: name})
		}
	}
	switch n.Kind() {
	case "perform_statement":
		// PERFORM first [THRU last]; a name after VARYING is a variable
		names := make([]string, 0, 2)
		for i := uint(0); i < n.ChildCount(); i++ {
			c := n.Child(i)
			switch {
			case cobolIsName(c) && len(names) == 0:
				names = append(names, cobolName(b.text(c)))
			case cobolIsName(c) && len(names) == 1 && i > 0 && thru(b.text(n.Child(i-1))):
				names = append(names, cobolName(b.text(c)))
			}
		}
		for _, name := range names {
			call(name)
		}
	case "go_to_statement":
		for i := uint(0); i < n.ChildCount(); i++ {
			c := n.Child(i)
			if !c.IsNamed() && strings.EqualFold(b.text(c), "DEPENDING") {
				break
			}
			if name := cobolName(b.text(c)); cobolIsName(c) && (sc.sym < 0 || b.file.Symbols[sc.sym].Name != name) {
				call(name)
			}
		}
	case "call_statement":
		if c := n.NamedChild(0); c != nil && c.Kind() == "string_literal" {
			call(cobolName(b.text(c)))
		}
	case "cics_link", "cics_xctl":
		for i := uint(0); i < n.NamedChildCount(); i++ {
			opt := n.NamedChild(i)
			if opt.Kind() == "cics_option" && opt.NamedChildCount() == 2 &&
				strings.EqualFold(b.text(opt.NamedChild(0)), "PROGRAM") && opt.NamedChild(1).Kind() == "string_literal" {
				call(cobolName(b.text(opt.NamedChild(1))))
			}
		}
	}
	return ""
}

func thru(word string) bool {
	return strings.EqualFold(word, "THRU") || strings.EqualFold(word, "THROUGH")
}

// IBM Enterprise COBOL, with EXEC CICS and EXEC SQL. The program is a
// function named by its PROGRAM-ID, which is what CALL names; the paragraphs
// and sections are functions inside it. A paragraph runs to the next one, as
// a label does in assembly, since the grammar does not nest them. A record
// is its 01 line only, as the fields below it are siblings in the tree.
var cobolSpec = &langSpec{
	name:    "cobol",
	grammar: tree_sitter_cobol.Language,
	comment: "*>",
	defs: map[string]defKind{
		"program_definition": function,
		"data_description":   {etype: TypeStruct},
	},
	leading:     setOf("comment"),
	importKinds: setOf("copy_statement"),
	imports: func(b *builder, n *sitter.Node) {
		b.addImport("", cobolFirstNameOrLiteral(b, n), "")
	},
	describe: func(b *builder, n *sitter.Node, d *def) bool {
		switch n.Kind() {
		case "program_definition":
			d.name = cobolFirstName(b, cobolNamedChild(cobolNamedChild(n, "identification_division"), "program_id_paragraph"))
		case "section", "paragraph":
			d.name = cobolFirstName(b, n)
		case "data_description":
			if !cobolLevelOne(b, n) {
				return false
			}
			d.name = cobolName(b.text(cobolNamedChild(n, "entry_name")))
			return d.name != "FILLER"
		}
		return true
	},
	label: func(b *builder, n *sitter.Node) bool {
		return n.Kind() == "section" || n.Kind() == "paragraph"
	},
	labelKinds: setOf("section", "paragraph"),
	target:     cobolTarget,
}

func cobolFirstNameOrLiteral(b *builder, n *sitter.Node) string {
	if n.NamedChildCount() == 0 {
		return ""
	}
	return cobolName(b.text(n.NamedChild(0)))
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
	".php":      phpSpec,
	".b":        bcplSpec,
	".bcpl":     bcplSpec,
	".s":        m68kSpec,
	".asm":      m68kSpec,
	".i":        m68kSpec,
	".nasm":     nasmSpec,
	".inc":      nasmSpec,
	".cbl":      cobolSpec,
	".cob":      cobolSpec,
	".cobol":    cobolSpec,
	".cpy":      cobolSpec,
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

// candidates are the languages of the extensions that more than one language
// uses: assembly for every processor is kept in .s and .asm files. A file is
// read as the one it fits best, see parseFile.
var candidates = map[string][]*langSpec{
	".s":    {m68kSpec, gasSpec, nasmSpec},
	".asm":  {m68kSpec, nasmSpec, gasSpec},
	".i":    {m68kSpec, gasSpec, nasmSpec},
	".inc":  {nasmSpec, m68kSpec, gasSpec},
	".nasm": {nasmSpec, gasSpec},
}

// languages are the specs by the name of their language
var languages = map[string]*langSpec{}

func init() {
	for _, spec := range extensions {
		languages[spec.name] = spec
	}
	for _, specs := range candidates {
		for _, spec := range specs {
			languages[spec.name] = spec
		}
	}
}

// ambiguous are the extensions that more than one language uses and that
// only the content of a file tells apart: BCPL headers share .h with C.
var ambiguous = setOf(".h")

// specFor returns the language of a file: of those it may be in, the first
func specFor(path string, src []byte) *langSpec {
	if specs := specsFor(path, src); len(specs) > 0 {
		return specs[0]
	}
	return nil
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

// specsFor returns the languages a file may be in
func specsFor(path string, src []byte) []*langSpec {
	ext := strings.ToLower(filepath.Ext(path))
	// Files of BCPL from before there were extensions have none, and its
	// headers share .h with C: what is in them tells
	if (ext == "" || ambiguous[ext]) && looksLikeBCPL(src) {
		return []*langSpec{bcplSpec}
	}
	if shared, ok := candidates[ext]; ok {
		return shared
	}
	if spec := extensions[ext]; spec != nil {
		return []*langSpec{spec}
	}
	return nil
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
