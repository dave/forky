package migraty

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/token"
	"go/types"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/dave/services/progutils"
	"golang.org/x/tools/go/ast/astutil"
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

			// Find all packages and the full dependency tree (only packages in s.paths considered)
			deps := map[string]bool{}
			var scan func(p *PackageInfo)
			scan = func(p *PackageInfo) {
				for _, p := range p.Info.Pkg.Imports() {
					if !strings.HasPrefix(p.Path(), s.destination+"/") {
						continue
					}
					relpath := strings.TrimPrefix(p.Path(), s.destination+"/")
					info, ok := s.paths[relpath]
					if !ok {
						continue
					}
					scan(info.Default)
					deps[relpath] = true
				}
			}
			for _, relpath := range m.Packages {
				info := s.paths[relpath].Default
				if info == nil {
					panic(fmt.Sprintf("no default package for %s", relpath))
				}
				scan(info)
				deps[relpath] = true
			}

			// 1) Find all package-level vars and funcs
			var count int
			for relpath := range deps {
				count++
				fmt.Fprintf(s.out, "\rLibify: %d/%d", count, len(deps))

				info := s.paths[relpath].Default
				vars := map[*ast.ValueSpec]bool{}
				varObjects := map[types.Object]bool{}
				funcObjects := map[types.Object]bool{}
				methodObjects := map[types.Object]bool{}

				for fname, file := range info.Files {
					f := func(c *astutil.Cursor) bool {
						switch n := c.Node().(type) {
						case *ast.GenDecl:
							if n.Tok != token.VAR {
								return true
							}
							if _, ok := c.Parent().(*ast.DeclStmt); ok {
								// for vars inside functions
								return true
							}

							for _, spec := range n.Specs {
								spec := spec.(*ast.ValueSpec)
								vars[spec] = true

								// look up the object in the types.Defs
								for _, id := range spec.Names {
									if id.Name == "_" {
										continue
									}
									def, ok := info.Info.Defs[id]
									if !ok {
										panic(fmt.Sprintf("can't find %s in defs", id.Name))
									}
									varObjects[def] = true
								}
							}

							// remove all package-level var declarations
							c.Delete()

						case *ast.FuncDecl:

							isMethod := n.Recv != nil && len(n.Recv.List) > 0

							if isMethod {
								// if method, add "psess *PackageSession" as the first parameter
								psess := &ast.Field{
									Names: []*ast.Ident{ast.NewIdent("psess")},
									Type: &ast.StarExpr{
										X: ast.NewIdent("PackageSession"),
									},
								}
								n.Type.Params.List = append([]*ast.Field{psess}, n.Type.Params.List...)

								def, ok := info.Info.Defs[n.Name]
								if !ok {
									panic(fmt.Sprintf("can't find %s in defs", n.Name.Name))
								}
								methodObjects[def] = true

							} else {
								// if func, add "psess *PackageSession" as the receiver
								n.Recv = &ast.FieldList{List: []*ast.Field{
									{
										Names: []*ast.Ident{ast.NewIdent("psess")},
										Type:  &ast.StarExpr{X: ast.NewIdent("PackageSession")},
									},
								}}
								def, ok := info.Info.Defs[n.Name]
								if !ok {
									panic(fmt.Sprintf("can't find %s in defs", n.Name.Name))
								}
								funcObjects[def] = true
							}
							c.Replace(n)
						}
						return true
					}
					result := astutil.Apply(file, f, nil)
					if result == nil {
						info.Files[fname] = nil
					} else {
						info.Files[fname] = result.(*ast.File)
					}
				}

				f := &ast.File{
					Name: ast.NewIdent(info.Name),
				}

				// make a list of *ast.Field corresponding to the names and types of the deleted vars
				var fields []*ast.Field
				type valueItem struct {
					name  string
					value ast.Expr
				}
				var values []valueItem
				for spec := range vars {

					for i, v := range spec.Values {
						// save any values
						if spec.Names[i].Name == "_" {
							continue
						}
						values = append(values, valueItem{name: spec.Names[i].Name, value: v})
					}
					if spec.Type != nil {
						// if a type is specified, we can add the names as one field
						infoType := info.Info.Types[spec.Type]
						if infoType.Type == nil {
							panic(fmt.Sprintf("no type for %v in %s", spec.Names, relpath))
						}
						f := &ast.Field{
							Names: spec.Names,
							Type:  s.typeToAstTypeSpec(infoType.Type, path.Join(s.destination, relpath), f),
						}
						fields = append(fields, f)
						continue
					}
					// if spec.Type is nil, we must separate the name / value pairs
					// TODO: determine type from value (will need to scan with type checker)
					for i := range spec.Names {
						name := spec.Names[i]
						if name.Name == "_" {
							continue
						}
						value := spec.Values[i]
						infoType := info.Info.Types[value]
						if infoType.Type == nil {
							panic("no type for " + name.Name + " in " + relpath)
						}
						f := &ast.Field{
							Names: []*ast.Ident{name},
							Type:  s.typeToAstTypeSpec(infoType.Type, path.Join(s.destination, relpath), f),
						}
						fields = append(fields, f)
					}

					/*
						t := spec.
						if t == nil {
							t = getType()
						}
						if len(spec.Names) > 0 && spec.Type != nil {

						} else {
							fmt.Println(spec.Names, spec.Type)
						}
					*/
				}

				// 3) Add a PackageSession struct with those fields and methods
				f.Decls = append(f.Decls, &ast.GenDecl{
					Tok: token.TYPE,
					Specs: []ast.Spec{
						&ast.TypeSpec{
							Name: ast.NewIdent("PackageSession"),
							Type: &ast.StructType{
								Fields: &ast.FieldList{
									List: fields,
								},
							},
						},
					},
				})

				var body []ast.Stmt

				body = append(body, &ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent("psess")},
					Tok: token.DEFINE,
					Rhs: []ast.Expr{
						&ast.UnaryExpr{
							Op: token.AND,
							X: &ast.CompositeLit{
								Type: ast.NewIdent("PackageSession"),
							},
						},
					},
				})

				for _, i := range info.Info.InitOrder {
					for _, v := range i.Lhs {
						if v.Name() == "_" {
							continue
						}
						body = append(body, &ast.AssignStmt{
							Lhs: []ast.Expr{
								&ast.SelectorExpr{
									X:   ast.NewIdent("psess"),
									Sel: ast.NewIdent(v.Name()),
								},
							},
							Tok: token.ASSIGN,
							Rhs: []ast.Expr{i.Rhs},
						})
					}
				}

				body = append(body, &ast.ReturnStmt{
					Results: []ast.Expr{
						ast.NewIdent("psess"),
					},
				})

				// Add NewPackageSession function with initialisation logic
				f.Decls = append(f.Decls, &ast.FuncDecl{
					Name: ast.NewIdent("NewPackageSession"),
					Type: &ast.FuncType{
						Params: &ast.FieldList{},
						Results: &ast.FieldList{
							List: []*ast.Field{
								{
									Type: &ast.StarExpr{
										X: ast.NewIdent("PackageSession"),
									},
								},
							},
						},
					},
					Body: &ast.BlockStmt{
						List: body,
					},
				})

				info.Files["package-session.go"] = f

				// *) All usages of package level var X get converted into psess.X

				for fname, file := range info.Files {
					f := astutil.Apply(file, func(c *astutil.Cursor) bool {
						switch n := c.Node().(type) {
						case *ast.Ident:
							use, ok := info.Info.Uses[n]
							if !ok {
								return true
							}
							if varObjects[use] || funcObjects[use] {
								c.Replace(&ast.SelectorExpr{
									X:   ast.NewIdent("psess"),
									Sel: n,
								})
							}
						case *ast.CallExpr:
							var id *ast.Ident
							switch fun := n.Fun.(type) {
							case *ast.Ident:
								id = fun
							case *ast.SelectorExpr:
								id = fun.Sel
							default:
								return true
							}
							use, ok := info.Info.Uses[id]
							if !ok {
								return true
							}
							if methodObjects[use] {
								n.Args = append([]ast.Expr{ast.NewIdent("psess")}, n.Args...)
							}
						}
						return true
					}, nil)
					info.Files[fname] = f.(*ast.File)
				}

				// *) Convert func "main" to method "Main".

				// *) All other packages that import this one get wired up somehow :/

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
		ih := progutils.NewImportsHelper(f, s.prog)
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
