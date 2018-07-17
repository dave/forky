package migraty

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dave/services/progutils"
	"gopkg.in/src-d/go-billy.v4"
)

type Libify struct {
	Packages []string // package path -> package name -> true
}

func (m Libify) Apply(s *Session) Applier {
	return Applier{
		Func: func() {

			// load the program and scan types
			s.load()

			l := NewLibifier(m, s)
			if err := l.Run(); err != nil {
				panic(err)
			}

		},
	}

}

func buildContext(gorootfs, gopathfs billy.Filesystem, gopathrel string, tags ...string) *build.Context {
	b := &build.Context{
		GOARCH:      build.Default.GOARCH, // Target architecture
		GOOS:        build.Default.GOOS,   // Target operating system
		GOROOT:      "goroot",             // Go root
		GOPATH:      "gopath",             // Go path
		Compiler:    "gc",                 // Compiler to assume when computing target paths
		BuildTags:   tags,                 // Build tags
		CgoEnabled:  true,                 // Builder only: detect `import "C"` to throw proper error
		ReleaseTags: build.Default.ReleaseTags,

		// IsDir reports whether the path names a directory.
		// If IsDir is nil, Import calls os.Stat and uses the result's IsDir method.
		IsDir: func(path string) bool {
			fs := filesystem(path, gorootfs, gopathfs, gopathrel)
			fi, err := fs.Stat(path)
			return err == nil && fi.IsDir()
		},

		// HasSubdir reports whether dir is lexically a subdirectory of
		// root, perhaps multiple levels below. It does not try to check
		// whether dir exists.
		// If so, HasSubdir sets rel to a slash-separated path that
		// can be joined to root to produce a path equivalent to dir.
		// If HasSubdir is nil, Import uses an implementation built on
		// filepath.EvalSymlinks.
		HasSubdir: func(root, dir string) (rel string, ok bool) {
			// copied from default implementation to prevent use of filepath.EvalSymlinks
			const sep = string(filepath.Separator)
			root = filepath.Clean(root)
			if !strings.HasSuffix(root, sep) {
				root += sep
			}
			dir = filepath.Clean(dir)
			if !strings.HasPrefix(dir, root) {
				return "", false
			}
			return filepath.ToSlash(dir[len(root):]), true
		},

		// ReadDir returns a slice of os.FileInfo, sorted by Name,
		// describing the content of the named directory.
		// If ReadDir is nil, Import uses ioutil.ReadDir.
		ReadDir: func(dir string) ([]os.FileInfo, error) {
			fs := filesystem(dir, gorootfs, gopathfs, gopathrel)
			return fs.ReadDir(dir)
		},

		// OpenFile opens a file (not a directory) for reading.
		// If OpenFile is nil, Import uses os.Open.
		OpenFile: func(path string) (io.ReadCloser, error) {
			dir, _ := filepath.Split(path)
			fs := filesystem(dir, gorootfs, gopathfs, gopathrel)
			return fs.Open(path)
		},
	}
	return b
}

// Gets either sourcefs, rootfs or pathfs depending on the path, and if the package is part of source
func filesystem(dir string, gorootfs, gopathfs billy.Filesystem, gopathrel string) billy.Filesystem {

	dir = strings.Trim(filepath.Clean(dir), string(filepath.Separator))
	parts := strings.Split(dir, string(filepath.Separator))

	switch parts[0] {
	case "gopath":
		return gopathfs
	case "goroot":
		return gorootfs
	}

	panic(fmt.Sprintf("Top dir should be goroot or gopath, got %s", dir))
}

// Expr for TypeSpec.Type: Should return *Ident, *ParenExpr, *SelectorExpr, *StarExpr, or any of the *XxxTypes
func (s *Session) typeToAstTypeSpec(t types.Type, path string, f *ast.File) ast.Expr {
	switch t := t.(type) {
	case *types.Basic:
		switch t.Kind() {
		case types.Bool, types.Int, types.Int8, types.Int16, types.Int32, types.Int64, types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr, types.Float32, types.Float64, types.Complex64, types.Complex128, types.String:
			return ast.NewIdent(t.Name())
		case types.UnsafePointer:
			panic("TODO: types.UnsafePointer not implemented")
		case types.UntypedBool:
			return ast.NewIdent("bool")
		case types.UntypedInt:
			return ast.NewIdent("int")
		case types.UntypedRune:
			return ast.NewIdent("rune")
		case types.UntypedFloat:
			return ast.NewIdent("float64")
		case types.UntypedComplex:
			return ast.NewIdent("complex64")
		case types.UntypedString:
			return ast.NewIdent("string")
		case types.UntypedNil:
			panic("TODO: types.UntypedNil not implemented")
		}
	case *types.Array:
		return &ast.ArrayType{
			Len: &ast.BasicLit{
				Kind:  token.INT,
				Value: fmt.Sprint(t.Len()),
			},
			Elt: s.typeToAstTypeSpec(t.Elem(), path, f),
		}
	case *types.Slice:
		return &ast.ArrayType{
			Elt: s.typeToAstTypeSpec(t.Elem(), path, f),
		}
	case *types.Struct:
		var fields []*ast.Field
		for i := 0; i < t.NumFields(); i++ {
			f := &ast.Field{
				Names: []*ast.Ident{ast.NewIdent(t.Field(i).Name())},
				Type:  s.typeToAstTypeSpec(t.Field(i).Type(), path, f),
			}
			fields = append(fields, f)
		}
		return &ast.StructType{
			Fields: &ast.FieldList{
				List: fields,
			},
		}

	case *types.Pointer:
		return &ast.StarExpr{
			X: s.typeToAstTypeSpec(t.Elem(), path, f),
		}
	case *types.Tuple:
		panic("tuple?")
	case *types.Signature:
		params := &ast.FieldList{}
		for i := 0; i < t.Params().Len(); i++ {
			f := &ast.Field{
				Names: []*ast.Ident{ast.NewIdent(t.Params().At(i).Name())},
				Type:  s.typeToAstTypeSpec(t.Params().At(i).Type(), path, f),
			}
			params.List = append(params.List, f)
		}
		var results *ast.FieldList
		if t.Results().Len() > 0 {
			results = &ast.FieldList{}
			for i := 0; i < t.Results().Len(); i++ {
				f := &ast.Field{
					Names: []*ast.Ident{ast.NewIdent(t.Results().At(i).Name())},
					Type:  s.typeToAstTypeSpec(t.Results().At(i).Type(), path, f),
				}
				results.List = append(results.List, f)
			}
		}
		return &ast.FuncType{
			Params:  params,
			Results: results,
		}
	case *types.Interface:
		methods := &ast.FieldList{}
		for i := 0; i < t.NumEmbeddeds(); i++ {
			f := &ast.Field{
				Type: s.typeToAstTypeSpec(t.Embedded(i), path, f),
			}
			methods.List = append(methods.List, f)
		}
		for i := 0; i < t.NumExplicitMethods(); i++ {
			f := &ast.Field{
				Names: []*ast.Ident{ast.NewIdent(t.ExplicitMethod(i).Name())},
				Type:  s.typeToAstTypeSpec(t.ExplicitMethod(i).Type(), path, f),
			}
			methods.List = append(methods.List, f)
		}

		return &ast.InterfaceType{
			Methods: methods,
		}
	case *types.Map:
		return &ast.MapType{
			Key:   s.typeToAstTypeSpec(t.Key(), path, f),
			Value: s.typeToAstTypeSpec(t.Elem(), path, f),
		}
	case *types.Chan:
		var dir ast.ChanDir
		switch t.Dir() {
		case types.SendOnly:
			dir = ast.SEND
		case types.RecvOnly:
			dir = ast.RECV
		}
		return &ast.ChanType{
			Dir:   dir,
			Value: s.typeToAstTypeSpec(t.Elem(), path, f),
		}
	case *types.Named:
		if t.Obj().Pkg() == nil || t.Obj().Pkg().Path() == path {
			// t.Obj().Pkg() == nil for "error"
			return ast.NewIdent(t.Obj().Name())
		}
		ih := progutils.NewImportsHelper(t.Obj().Pkg().Path(), f, s.prog)
		name, err := ih.RegisterImport(t.Obj().Pkg().Path())
		if err != nil {
			panic(err)
		}
		return &ast.SelectorExpr{
			X:   ast.NewIdent(name),
			Sel: ast.NewIdent(t.Obj().Name()),
		}
	}
	panic(fmt.Sprintf("unsupported type %T", t))
}
