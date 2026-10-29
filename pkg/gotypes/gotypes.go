// Package gotypes resolves the calls in Go source files with the Go type
// checker.
//
// The syntax tree only gives the name of what is called. The type checker
// knows which function or method that name refers to, in which file it is
// declared, and which types implement an interface. This turns the calls the
// chunker could only match by name into exact ones.
package gotypes

import (
	"bufio"
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
)

// Relation types added on top of those the chunker reports
const (
	// RelationMayCall is a call that is dispatched at run time: a call of an
	// interface method may call the method of any type implementing it
	RelationMayCall = "may_call"
	// RelationImplements links a type to an interface it implements
	RelationImplements = "implements"
)

// maxCached is the number of type-checked packages kept for the files that
// are indexed next; the files of a package are usually indexed in a row
const maxCached = 4

// Resolver type-checks Go packages and rewrites the references of their files
type Resolver struct {
	mu sync.Mutex

	// Packages outside the module being checked (standard library and
	// dependencies) do not change while indexing; they are type-checked once,
	// by directory, and shared by every check
	extFset    *token.FileSet
	external   map[string]*types.Package
	extLoading map[string]bool
	goroot     string
	deps       map[string]*moduleDeps // By module root

	cached []*checked
}

// moduleDeps maps the import paths a module depends on to their directories
type moduleDeps struct {
	signature string
	dirs      map[string]string
}

// NewResolver creates a resolver
func NewResolver() *Resolver {
	return &Resolver{
		extFset:    token.NewFileSet(),
		external:   make(map[string]*types.Package),
		extLoading: make(map[string]bool),
		deps:       make(map[string]*moduleDeps),
	}
}

// gorootDir returns the root of the Go installation, or an empty string
func (r *Resolver) gorootDir() string {
	if r.goroot == "" {
		r.goroot = build.Default.GOROOT
		if r.goroot == "" {
			if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
				r.goroot = strings.TrimSpace(string(out))
			}
		}
	}
	return r.goroot
}

// moduleDirs returns where the dependencies of a module are, as the go
// command resolves them. Without a go command, dependencies stay unresolved
// and calls into them keep their syntactic reference.
func (r *Resolver) moduleDirs(modRoot string) map[string]string {
	if modRoot == "" {
		return nil
	}
	var sig strings.Builder
	for _, name := range []string{"go.mod", "go.sum"} {
		if info, err := os.Stat(filepath.Join(modRoot, name)); err == nil {
			fmt.Fprintf(&sig, "%d:%d|", info.Size(), info.ModTime().UnixNano())
		}
	}
	if deps := r.deps[modRoot]; deps != nil && deps.signature == sig.String() {
		return deps.dirs
	}

	deps := &moduleDeps{signature: sig.String(), dirs: make(map[string]string)}
	r.deps[modRoot] = deps

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-test", "-e", "-f", "{{.ImportPath}}\t{{.Dir}}", "./...")
	cmd.Dir = modRoot
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		slog.Warn("Could not list Go dependencies; calls into them are matched by name", "module", modRoot, "error", err)
		return deps.dirs
	}
	for _, line := range strings.Split(string(out), "\n") {
		path, dir, ok := strings.Cut(line, "\t")
		// Test variants are listed as "path [path.test]"
		if ok && dir != "" && !strings.Contains(path, " ") {
			deps.dirs[path] = dir
		}
	}
	return deps.dirs
}

// externalDir returns the directory of a package outside the module
func (r *Resolver) externalDir(path, srcDir, modRoot string) string {
	if goroot := r.gorootDir(); goroot != "" {
		src := filepath.Join(goroot, "src")
		// The standard library vendors some of its dependencies
		if strings.HasPrefix(srcDir, src+string(filepath.Separator)) {
			if dir := filepath.Join(src, "vendor", filepath.FromSlash(path)); isDir(dir) {
				return dir
			}
		}
		if dir := filepath.Join(src, filepath.FromSlash(path)); isDir(dir) {
			return dir
		}
	}
	return r.moduleDirs(modRoot)[path]
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// externalImporter imports packages on behalf of a package outside the module
type externalImporter struct {
	resolver *Resolver
	modRoot  string
}

func (e externalImporter) Import(path string) (*types.Package, error) {
	return e.ImportFrom(path, "", 0)
}

func (e externalImporter) ImportFrom(path, srcDir string, mode types.ImportMode) (*types.Package, error) {
	return e.resolver.importExternal(path, srcDir, e.modRoot)
}

// importExternal type-checks a package outside the module from its source.
// Only its declarations matter, so function bodies are skipped and C code is
// never compiled.
func (r *Resolver) importExternal(path, srcDir, modRoot string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	dir := r.externalDir(path, srcDir, modRoot)
	if dir == "" {
		return nil, fmt.Errorf("package %s not found", path)
	}
	if pkg := r.external[dir]; pkg != nil {
		return pkg, nil
	}
	if r.extLoading[dir] {
		return nil, fmt.Errorf("import cycle through %s", path)
	}
	r.extLoading[dir] = true
	defer delete(r.extLoading, dir)

	pkg, err := checkDeclarations(r.extFset, path, dir, externalImporter{resolver: r, modRoot: modRoot})
	if err != nil {
		return nil, err
	}
	r.external[dir] = pkg
	return pkg, nil
}

// checkDeclarations type-checks the declarations of the package in a directory
func checkDeclarations(fset *token.FileSet, path, dir string, imp types.Importer) (*types.Package, error) {
	bp, err := build.Default.ImportDir(dir, 0)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for _, name := range append(append([]string{}, bp.GoFiles...), bp.CgoFiles...) {
		f, _ := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if f != nil {
			files = append(files, f)
		}
	}
	conf := types.Config{
		Importer:         imp,
		IgnoreFuncBodies: true,
		FakeImportC:      true,
		Error:            func(error) {},
	}
	pkg, _ := conf.Check(path, fset, files, nil)
	if pkg == nil {
		return nil, fmt.Errorf("type check of %s failed", path)
	}
	return pkg, nil
}

// checked is a type-checked package
type checked struct {
	signature string
	fset      *token.FileSet
	files     map[string]*ast.File // By absolute path
	pkg       *types.Package
	info      *types.Info
	load      *loader
}

// Apply replaces the calls the chunker found in a Go file by the ones the
// type checker resolves, and adds the interfaces its types implement. Calls
// the type checker cannot resolve, for instance into a dependency that is not
// installed, keep their syntactic reference.
func (r *Resolver) Apply(parsed *chunker.File, path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	c, err := r.check(path)
	if err != nil {
		return err
	}
	file := c.files[path]
	if file == nil {
		return fmt.Errorf("%s is not part of its package (excluded by build constraints?)", path)
	}

	x := &extractor{
		resolver:   r,
		checked:    c,
		symbols:    make(map[string]int),
		seen:       make(map[chunker.Ref]bool),
		unresolved: make(map[string]bool),
	}
	for i, sym := range parsed.Symbols {
		key := symbolKey(sym.Name, sym.Container)
		if _, ok := x.symbols[key]; !ok {
			x.symbols[key] = i
		}
	}
	x.file(file)

	// Keep what only the syntax tree can tell
	var refs []chunker.Ref
	for _, ref := range parsed.Refs {
		if ref.Relation != chunker.RelationCalls || x.unresolved[unresolvedKey(ref.Source, ref.Name)] {
			refs = append(refs, ref)
		}
	}
	parsed.Refs = append(refs, x.refs...)
	return nil
}

func symbolKey(name, container string) string {
	return container + "." + name
}

func unresolvedKey(source int, name string) string {
	return fmt.Sprintf("%d.%s", source, name)
}

// check type-checks the package a file belongs to
func (r *Resolver) check(path string) (*checked, error) {
	dir := filepath.Dir(path)
	bp, err := build.Default.ImportDir(dir, 0)
	if err != nil {
		if _, noGo := err.(*build.NoGoError); !noGo && bp == nil {
			return nil, err
		}
	}

	// A test file is checked with the package it tests, or, for an external
	// test package, with the other files of that package
	base := filepath.Base(path)
	names := append(append([]string{}, bp.GoFiles...), bp.CgoFiles...)
	suffix := ""
	switch {
	case contains(bp.TestGoFiles, base):
		names = append(names, bp.TestGoFiles...)
	case contains(bp.XTestGoFiles, base):
		names = append([]string{}, bp.XTestGoFiles...)
		suffix = "_test"
	}
	if !contains(names, base) {
		return nil, fmt.Errorf("%s is not part of its package (excluded by build constraints?)", path)
	}
	sort.Strings(names)

	var sig strings.Builder
	sig.WriteString(dir + suffix)
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&sig, "|%s:%d:%d", name, info.Size(), info.ModTime().UnixNano())
	}
	signature := sig.String()
	for _, c := range r.cached {
		if c.signature == signature {
			return c, nil
		}
	}

	load := newLoader(r, dir)
	c := &checked{
		signature: signature,
		fset:      load.fset,
		files:     make(map[string]*ast.File),
		load:      load,
		info: &types.Info{
			Uses:       make(map[*ast.Ident]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
		},
	}
	var files []*ast.File
	for _, name := range names {
		filePath := filepath.Join(dir, name)
		f, err := parser.ParseFile(load.fset, filePath, nil, parser.SkipObjectResolution)
		if f == nil {
			return nil, err
		}
		c.files[filePath] = f
		files = append(files, f)
	}

	// Type errors are expected, for instance when a dependency is missing;
	// whatever does type-check is still used
	conf := types.Config{
		Importer:    load,
		FakeImportC: true,
		Error:       func(error) {},
	}
	c.pkg, _ = conf.Check(load.importPath(dir)+suffix, load.fset, files, c.info)
	if c.pkg == nil {
		return nil, fmt.Errorf("type check of %s failed", dir)
	}
	load.local[c.pkg] = true

	r.cached = append(r.cached, c)
	if len(r.cached) > maxCached {
		r.cached = r.cached[1:]
	}
	return c, nil
}

func contains(list []string, item string) bool {
	for _, s := range list {
		if s == item {
			return true
		}
	}
	return false
}

// loader imports packages for one check. Packages of the module being checked
// are type-checked from their current source, without function bodies;
// everything else is shared between checks.
type loader struct {
	resolver *Resolver
	fset     *token.FileSet
	modRoot  string // Directory holding go.mod, or empty
	modPath  string // Module path declared in go.mod
	pkgs     map[string]*types.Package
	local    map[*types.Package]bool // Packages of the module
	loading  map[string]bool
}

func newLoader(r *Resolver, dir string) *loader {
	l := &loader{
		resolver: r,
		fset:     token.NewFileSet(),
		pkgs:     make(map[string]*types.Package),
		local:    make(map[*types.Package]bool),
		loading:  make(map[string]bool),
	}
	l.modRoot, l.modPath = findModule(dir)
	return l
}

// findModule returns the directory and path of the module a directory is in
func findModule(dir string) (root, path string) {
	for {
		if f, err := os.Open(filepath.Join(dir, "go.mod")); err == nil {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				fields := strings.Fields(scanner.Text())
				if len(fields) >= 2 && fields[0] == "module" {
					return dir, strings.Trim(fields[1], `"`)
				}
			}
			return "", ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

// importPath returns the import path of a directory in the module
func (l *loader) importPath(dir string) string {
	if l.modRoot != "" {
		if rel, err := filepath.Rel(l.modRoot, dir); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return l.modPath
			}
			return l.modPath + "/" + filepath.ToSlash(rel)
		}
	}
	return filepath.Base(dir)
}

// moduleDir returns the directory of an import path inside the module
func (l *loader) moduleDir(path string) (string, bool) {
	if l.modRoot == "" {
		return "", false
	}
	if path == l.modPath {
		return l.modRoot, true
	}
	if rel, ok := strings.CutPrefix(path, l.modPath+"/"); ok {
		return filepath.Join(l.modRoot, filepath.FromSlash(rel)), true
	}
	return "", false
}

func (l *loader) Import(path string) (*types.Package, error) {
	return l.ImportFrom(path, "", 0)
}

func (l *loader) ImportFrom(path, srcDir string, mode types.ImportMode) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	dir, ok := l.moduleDir(path)
	if !ok {
		return l.resolver.importExternal(path, srcDir, l.modRoot)
	}
	if pkg := l.pkgs[path]; pkg != nil {
		return pkg, nil
	}
	if l.loading[path] {
		return nil, fmt.Errorf("import cycle through %s", path)
	}
	l.loading[path] = true
	defer delete(l.loading, path)

	pkg, err := checkDeclarations(l.fset, path, dir, l)
	if err != nil {
		return nil, err
	}
	l.pkgs[path] = pkg
	l.local[pkg] = true
	return pkg, nil
}

// extractor collects the references of one file
type extractor struct {
	resolver   *Resolver
	checked    *checked
	symbols    map[string]int // Chunker symbol index by name and container
	refs       []chunker.Ref
	seen       map[chunker.Ref]bool
	unresolved map[string]bool
	implCache  map[*types.Named][]*types.Named
}

func (x *extractor) add(ref chunker.Ref) {
	if !x.seen[ref] {
		x.seen[ref] = true
		x.refs = append(x.refs, ref)
	}
}

// source returns the chunker symbol for a declaration, -1 for the file itself
func (x *extractor) source(name, container string) int {
	if i, ok := x.symbols[symbolKey(name, container)]; ok {
		return i
	}
	return -1
}

func (x *extractor) file(file *ast.File) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			source := x.source(d.Name.Name, receiverName(d))
			if d.Body != nil {
				x.calls(d.Body, source)
			}
		case *ast.GenDecl:
			// Calls in package-level initializers belong to the file
			x.calls(d, -1)
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					x.implements(ts)
				}
			}
		}
	}
}

// receiverName returns the name of the receiver type of a method declaration
func receiverName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	expr := d.Recv.List[0].Type
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
		case *ast.ParenExpr:
			expr = t.X
		case *ast.IndexExpr:
			expr = t.X
		case *ast.IndexListExpr:
			expr = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

func (x *extractor) calls(node ast.Node, source int) {
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			x.call(call, source)
		}
		return true
	})
}

func (x *extractor) call(call *ast.CallExpr, source int) {
	// Look through parentheses and explicit type arguments
	fun := call.Fun
	for {
		switch f := fun.(type) {
		case *ast.ParenExpr:
			fun = f.X
			continue
		case *ast.IndexExpr:
			fun = f.X
			continue
		case *ast.IndexListExpr:
			fun = f.X
			continue
		}
		break
	}

	var obj types.Object
	var name string
	switch f := fun.(type) {
	case *ast.Ident:
		name, obj = f.Name, x.checked.info.Uses[f]
	case *ast.SelectorExpr:
		name = f.Sel.Name
		if sel := x.checked.info.Selections[f]; sel != nil {
			obj = sel.Obj()
		} else {
			obj = x.checked.info.Uses[f.Sel]
		}
	default:
		// A call of a function literal or of the result of another call
		return
	}

	fn, ok := obj.(*types.Func)
	if !ok {
		// Built-ins, conversions and calls of function values have no
		// declaration to point at. What did not type-check keeps the
		// reference from the syntax tree.
		if obj == nil {
			x.unresolved[unresolvedKey(source, name)] = true
		}
		return
	}
	fn = fn.Origin()

	container, iface, ok := receiver(fn)
	if !ok {
		// A method of an anonymous interface
		return
	}
	x.add(chunker.Ref{
		Source:     source,
		Relation:   chunker.RelationCalls,
		Name:       fn.Name(),
		Container:  container,
		TargetPath: x.position(fn),
		External:   externalName(fn.Pkg(), container, fn.Name()),
		Member:     container != "",
	})

	// The call is dispatched to whatever implements the interface
	if iface != nil {
		for _, impl := range x.implementers(iface) {
			m, _, _ := types.LookupFieldOrMethod(types.NewPointer(impl), true, fn.Pkg(), fn.Name())
			method, ok := m.(*types.Func)
			if !ok {
				continue
			}
			method = method.Origin()
			implContainer, promoted, _ := receiver(method)
			if promoted != nil {
				// The type embeds the interface; there is no method of its own
				continue
			}
			x.add(chunker.Ref{
				Source:     source,
				Relation:   RelationMayCall,
				Name:       method.Name(),
				Container:  implContainer,
				TargetPath: x.position(method),
				Member:     true,
			})
		}
	}
}

// receiver returns the name of the type a method is declared on, and that
// type when it is an interface. It returns false for a method of a type
// without a name, which has no declaration to point at.
func receiver(fn *types.Func) (container string, iface *types.Named, ok bool) {
	sig, isSig := fn.Type().(*types.Signature)
	if !isSig || sig.Recv() == nil {
		return "", nil, true
	}
	t := types.Unalias(sig.Recv().Type())
	if ptr, isPtr := t.(*types.Pointer); isPtr {
		t = types.Unalias(ptr.Elem())
	}
	named, isNamed := t.(*types.Named)
	if !isNamed {
		return "", nil, false
	}
	if types.IsInterface(named) {
		return named.Obj().Name(), named, true
	}
	return named.Obj().Name(), nil, true
}

func externalName(pkg *types.Package, container, name string) string {
	parts := []string{}
	if pkg != nil {
		parts = append(parts, pkg.Name())
	}
	if container != "" {
		parts = append(parts, container)
	}
	return strings.Join(append(parts, name), ".")
}

// builtinPath stands in for the file of what is declared by the language
// itself, such as the Error method of error
const builtinPath = "builtin"

// position returns the file an object is declared in: the Go file, not the
// file a //line or /*line*/ directive in generated code points back to
func (x *extractor) position(obj types.Object) string {
	if !obj.Pos().IsValid() {
		return builtinPath
	}
	fset := x.resolver.extFset
	if x.checked.load.local[obj.Pkg()] {
		fset = x.checked.fset
	}
	return fset.PositionFor(obj.Pos(), false).Filename
}

// moduleTypes returns the named types declared in the packages of the module
// that are in view: the package being checked and what it imports
func (x *extractor) moduleTypes() []*types.Named {
	var named []*types.Named
	var pkgs []*types.Package
	for pkg := range x.checked.load.local {
		pkgs = append(pkgs, pkg)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Path() < pkgs[j].Path() })
	for _, pkg := range pkgs {
		scope := pkg.Scope()
		for _, name := range scope.Names() {
			if tn, ok := scope.Lookup(name).(*types.TypeName); ok && !tn.IsAlias() {
				if n, ok := tn.Type().(*types.Named); ok && n.TypeParams().Len() == 0 {
					named = append(named, n)
				}
			}
		}
	}
	return named
}

// implementers returns the types in view that implement an interface
func (x *extractor) implementers(iface *types.Named) []*types.Named {
	if impls, ok := x.implCache[iface]; ok {
		return impls
	}
	if x.implCache == nil {
		x.implCache = make(map[*types.Named][]*types.Named)
	}
	var impls []*types.Named
	underlying, ok := iface.Underlying().(*types.Interface)
	if ok && underlying.NumMethods() > 0 {
		for _, t := range x.moduleTypes() {
			if !types.IsInterface(t) && implementsEither(t, underlying) {
				impls = append(impls, t)
			}
		}
	}
	x.implCache[iface] = impls
	return impls
}

func implementsEither(t *types.Named, iface *types.Interface) bool {
	return types.Implements(t, iface) || types.Implements(types.NewPointer(t), iface)
}

// implements adds the interfaces of the module that a type declared in the
// file implements
func (x *extractor) implements(ts *ast.TypeSpec) {
	obj, ok := x.checked.pkg.Scope().Lookup(ts.Name.Name).(*types.TypeName)
	if !ok || obj.IsAlias() {
		return
	}
	t, ok := obj.Type().(*types.Named)
	if !ok || types.IsInterface(t) || t.TypeParams().Len() > 0 {
		return
	}
	source := x.source(ts.Name.Name, "")
	if source < 0 {
		return
	}
	for _, candidate := range x.moduleTypes() {
		iface, ok := candidate.Underlying().(*types.Interface)
		if !ok || iface.NumMethods() == 0 || !implementsEither(t, iface) {
			continue
		}
		x.add(chunker.Ref{
			Source:     source,
			Relation:   RelationImplements,
			Name:       candidate.Obj().Name(),
			TargetPath: x.position(candidate.Obj()),
			External:   externalName(candidate.Obj().Pkg(), "", candidate.Obj().Name()),
		})
	}
}
