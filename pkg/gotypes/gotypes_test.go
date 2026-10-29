package gotypes

import (
	"go/build"
	"os"
	"path/filepath"
	"testing"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
)

const storeSource = `package store

import "fmt"

// Store looks things up.
type Store interface {
	Get(key string) string
}

type Mem struct {
	data map[string]string
}

func (m *Mem) Get(key string) string {
	return fmt.Sprint(m.data[key])
}

func Open() *Mem {
	return &Mem{}
}
`

const appSource = `package app

import (
	"strings"

	"example.com/app/store"
	"example.com/missing/dep"
)

type Service struct {
	s    store.Store
	name strings.Builder
	hook func(string)
}

func (svc *Service) Lookup(key string) string {
	v := svc.s.Get(key)
	svc.name.WriteString(v)
	svc.hook(v)
	dep.Report(len(v))
	return clean(string(v))
}

func clean(s string) string {
	return strings.TrimSpace(s)
}

func New() *Service {
	m := store.Open()
	m.Get("x")
	return &Service{s: m}
}
`

func writeModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":         "module example.com/app\n\ngo 1.21\n",
		"store/store.go": storeSource,
		"app/app.go":     appSource,
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func apply(t *testing.T, r *Resolver, path string) *chunker.File {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, ok := chunker.Parse(path, src, 0)
	if !ok {
		t.Fatal("Parse not supported")
	}
	if err := r.Apply(parsed, path); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	return parsed
}

// refsFrom returns the references of one relation leaving a symbol
func refsFrom(f *chunker.File, source, relation string) []chunker.Ref {
	var refs []chunker.Ref
	for _, ref := range f.Refs {
		if ref.Relation != relation {
			continue
		}
		name := ""
		if ref.Source >= 0 {
			name = f.Symbols[ref.Source].QualifiedName()
		}
		if name == source {
			ref.Source = 0
			refs = append(refs, ref)
		}
	}
	return refs
}

func hasRef(refs []chunker.Ref, want chunker.Ref) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}

func TestApply(t *testing.T) {
	root := writeModule(t)
	storePath := filepath.Join(root, "store", "store.go")
	appPath := filepath.Join(root, "app", "app.go")

	r := NewResolver()
	app := apply(t, r, appPath)

	lookup := refsFrom(app, "Service.Lookup", chunker.RelationCalls)
	wantLookup := []chunker.Ref{
		// A call through an interface is a call of the interface's method
		{Relation: "calls", Name: "Get", Container: "Store", TargetPath: storePath, External: "store.Store.Get", Member: true},
		// The type checker knows the type of the field, and so the method
		{Relation: "calls", Name: "WriteString", Container: "Builder", TargetPath: filepath.Join(build.Default.GOROOT, "src", "strings", "builder.go"), External: "strings.Builder.WriteString", Member: true},
		{Relation: "calls", Name: "clean", TargetPath: appPath, External: "app.clean"},
		// The dependency is not installed: the syntax tree's reference stays
		{Relation: "calls", Name: "Report", ImportBase: "dep", External: "dep.Report", Member: true},
	}
	for _, want := range wantLookup {
		if !hasRef(lookup, want) {
			t.Errorf("Lookup: missing %+v in %+v", want, lookup)
		}
	}
	// Not calls of a declared function: a conversion, a built-in, a function value
	if len(lookup) != len(wantLookup) {
		t.Errorf("Lookup has %d calls, want %d: %+v", len(lookup), len(wantLookup), lookup)
	}

	// ... which may end up in any type that implements the interface
	dispatch := chunker.Ref{Relation: RelationMayCall, Name: "Get", Container: "Mem", TargetPath: storePath, Member: true}
	if got := refsFrom(app, "Service.Lookup", RelationMayCall); len(got) != 1 || got[0] != dispatch {
		t.Errorf("Lookup may_call = %+v, want %+v", got, dispatch)
	}

	// On a value of a concrete type the method is known exactly
	newCalls := refsFrom(app, "New", chunker.RelationCalls)
	wantNew := []chunker.Ref{
		{Relation: "calls", Name: "Open", TargetPath: storePath, External: "store.Open"},
		{Relation: "calls", Name: "Get", Container: "Mem", TargetPath: storePath, External: "store.Mem.Get", Member: true},
	}
	for _, want := range wantNew {
		if !hasRef(newCalls, want) {
			t.Errorf("New: missing %+v in %+v", want, newCalls)
		}
	}
	if got := refsFrom(app, "New", RelationMayCall); len(got) != 0 {
		t.Errorf("New may_call = %+v, want none", got)
	}

	// Imports are left as the chunker found them
	if got := refsFrom(app, "", chunker.RelationImports); len(got) != 3 {
		t.Errorf("got %d imports, want 3: %+v", len(got), got)
	}

	store := apply(t, r, storePath)
	implements := chunker.Ref{Relation: RelationImplements, Name: "Store", TargetPath: storePath, External: "store.Store"}
	if got := refsFrom(store, "Mem", RelationImplements); len(got) != 1 || got[0] != implements {
		t.Errorf("Mem implements = %+v, want %+v", got, implements)
	}
	// The interface's method is a symbol, so calls of it have a target
	found := false
	for _, sym := range store.Symbols {
		found = found || (sym.QualifiedName() == "Store.Get" && sym.Type == chunker.TypeMethod)
	}
	if !found {
		t.Errorf("Store.Get is not a symbol: %+v", store.Symbols)
	}
}

// Generated code maps its lines back to its source with //line and /*line*/
// directives. A call's target is still the Go file that declares it, since
// that is the file the index has.
func TestApply_LineDirectives(t *testing.T) {
	root := writeModule(t)
	appPath := filepath.Join(root, "app", "app.go")

	src := "package app\n\ntype T struct{}\n\n//line src/T.php:10\nfunc (t *T) M() {}\n\nfunc f(t *T) {\n\t/*line src/f.php:3*/ t.M()\n\tg()\n}\n\n//line src/g.php:1\nfunc g() {}\n"
	if err := os.WriteFile(appPath, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	app := apply(t, NewResolver(), appPath)
	for _, want := range []chunker.Ref{
		{Relation: "calls", Name: "M", Container: "T", TargetPath: appPath, External: "app.T.M", Member: true},
		{Relation: "calls", Name: "g", TargetPath: appPath, External: "app.g"},
	} {
		if got := refsFrom(app, "f", chunker.RelationCalls); !hasRef(got, want) {
			t.Errorf("f: missing %+v in %+v", want, got)
		}
	}
}

func TestApply_SeesEdits(t *testing.T) {
	root := writeModule(t)
	appPath := filepath.Join(root, "app", "app.go")

	r := NewResolver()
	apply(t, r, appPath)

	edited := "package app\n\nfunc clean(s string) string {\n\treturn trim(s)\n}\n\nfunc trim(s string) string {\n\treturn s\n}\n"
	if err := os.WriteFile(appPath, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	app := apply(t, r, appPath)
	want := chunker.Ref{Relation: "calls", Name: "trim", TargetPath: appPath, External: "app.trim"}
	if got := refsFrom(app, "clean", chunker.RelationCalls); len(got) != 1 || got[0] != want {
		t.Errorf("clean calls = %+v, want %+v", got, want)
	}
}

func TestApply_NotAPackageFile(t *testing.T) {
	root := writeModule(t)
	path := filepath.Join(root, "app", "other_windows.go")
	if err := os.WriteFile(path, []byte("//go:build ignore\n\npackage app\n"), 0600); err != nil {
		t.Fatal(err)
	}
	parsed, _ := chunker.Parse(path, []byte("package app\n"), 0)
	if err := NewResolver().Apply(parsed, path); err == nil {
		t.Error("Apply accepted a file that is excluded from its package")
	}
}
