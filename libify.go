package migraty

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dave/services/fsutil"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/loader"
	"gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/memfs"
)

type Libify struct {
	Packages map[string]map[string]bool // package path -> package name -> true
	Root     string
}

func (m Libify) Apply(s *Session) Applier {
	return Applier{
		Func: func() {

			// save all files to a memfs
			gopathfs := memfs.New()
			var count int
			for relpath, info := range s.paths {
				fmt.Fprintf(s.out, "\rScanning: %d/%d", count+1, len(s.paths))
				count++
				for _, pkg := range info.Packages {
					for fname, file := range pkg.Files {

						if file == nil {
							continue
						}

						rootrelfpath := filepath.Join("gopath", "src", m.Root, relpath, fname)

						buf := &bytes.Buffer{}
						if err := format.Node(buf, s.fset, file); err != nil {
							panic(fmt.Errorf("format.Node error in %s: %v", rootrelfpath, err))
						}

						if err := fsutil.WriteFile(gopathfs, rootrelfpath, 0666, buf); err != nil {
							panic(err)
						}
					}
				}
			}

			bc := buildContext(s.gorootfs, gopathfs, m.Root)
			lc := loader.Config{
				ParserMode: parser.ParseComments,
				Fset:       s.fset,
				Build:      bc,
				Cwd:        "/",
			}
			for relpath := range m.Packages {
				lc.Import(path.Join(m.Root, relpath))
			}
			p, err := lc.Load()
			if err != nil {
				panic(err)
			}
			s.prog = p
			s.packageNames = map[string]string{}
			for pkg, info := range p.AllPackages {
				s.packageNames[pkg.Path()] = pkg.Name()
				relpath := strings.TrimPrefix(pkg.Path(), m.Root+"/")
				if s.paths[relpath] == nil || s.paths[relpath].Packages[pkg.Name()] == nil {
					// only update packages that exist in s.paths (in infos we also have std lib etc).
					continue
				}
				files := map[string]*ast.File{}
				for _, f := range info.Files {
					_, fname := filepath.Split(s.fset.File(f.Pos()).Name())
					files[fname] = f
				}
				s.paths[relpath].Packages[pkg.Name()].Files = files
				s.paths[relpath].Packages[pkg.Name()].Info = info
			}

			// 1) Find all package-level vars and funcs
			for relpath, packageNames := range m.Packages {
				for packageName := range packageNames {
					info := s.paths[relpath].Packages[packageName]
					vars := map[*ast.ValueSpec]bool{}

					for fname, file := range info.Files {
						//_, fname := filepath.Split(fpath)
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

								for _, v := range n.Specs {
									vars[v.(*ast.ValueSpec)] = true
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
								} else {
									// if func, add "psess *PackageSession" as the receiver
									n.Recv = &ast.FieldList{List: []*ast.Field{
										{
											Names: []*ast.Ident{ast.NewIdent("psess")},
											Type:  &ast.StarExpr{X: ast.NewIdent("PackageSession")},
										},
									}}
								}
								c.Replace(n)
								//if fname == "elf.go" {
								//	fmt.Println("-----------------")
								//	format.Node(os.Stdout, s.fset, c.Node())
								//	fmt.Println("-----------------")
								//}

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
						Name: ast.NewIdent(packageName),
					}

					// make a list of *ast.Field corresponding to the names and types of the deleted vars
					var fields []*ast.Field
					for spec := range vars {
						if spec.Type != nil {
							// if a type is specified, we can add the names as one field
							infoType := info.Info.Types[spec.Type]
							if infoType.Type == nil {
								panic(fmt.Sprintf("no type for %v in %s", spec.Names, relpath))
							}
							f := &ast.Field{
								Names: spec.Names,
								Type:  s.typeToAstTypeSpec(infoType.Type, path.Join(m.Root, relpath), f),
							}
							fields = append(fields, f)
							continue
						}
						// if spec.Type is nil, we must separate the name / value pairs
						// TODO: determine type from value (will need to scan with type checker)
						for i := range spec.Names {
							name := spec.Names[i]
							value := spec.Values[i]
							infoType := info.Info.Types[value]
							if infoType.Type == nil {
								panic("no type for " + name.Name + " in " + relpath)
							}
							f := &ast.Field{
								Names: []*ast.Ident{name},
								Type:  s.typeToAstTypeSpec(infoType.Type, path.Join(m.Root, relpath), f),
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

					var importGenDecl *ast.GenDecl
					if len(f.Imports) > 0 {
						var importSpecs []ast.Spec
						for _, is := range f.Imports {
							importSpecs = append(importSpecs, is)
						}
						importGenDecl = &ast.GenDecl{
							Tok:    token.IMPORT,
							Lparen: token.Pos(1), // must be non-zero to render as a list
							Specs:  importSpecs,
							Rparen: token.Pos(1), // must be non-zero to render as a list
						}
					}

					// 3) Add a Package struct with those fields and methods
					f.Decls = []ast.Decl{
						importGenDecl,
						&ast.GenDecl{
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
						},
						&ast.FuncDecl{
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
								List: []ast.Stmt{
									&ast.ReturnStmt{
										Results: []ast.Expr{
											&ast.UnaryExpr{
												Op: token.AND,
												X: &ast.CompositeLit{
													Type: ast.NewIdent("PackageSession"),
													Elts: []ast.Expr{
													// TODO: Elements
													},
												},
											},
										},
									},
								},
							},
						},
					}

					info.Files["package-session.go"] = f
				}

				// TODO: vars

				// 2) Convert them to struct fields and methods. Convert func "main" to method "Main".

			}
			// 4) All methods of other types in the package get Package added as a param
			// 5) All other packages that import this one get wired up somehow :/

			// pkg := paths[m.Path].Packages[m.Name]
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
		if t.Obj().Pkg().Path() == path {
			return ast.NewIdent(t.Obj().Name())
		}
		var found bool
		// lookup package in file imports
		for _, is := range f.Imports {
			importpath, err := strconv.Unquote(is.Path.Value)
			if err != nil {
				panic(err)
			}
			if importpath == t.Obj().Pkg().Path() {
				if is.Name != nil && is.Name.Name != "" {
					// if current import is aliased, just use that name
					return &ast.SelectorExpr{
						X:   ast.NewIdent(is.Name.Name),
						Sel: ast.NewIdent(t.Obj().Name()),
					}
				}
				found = true
				break
			}
		}
		if !found {
			// if not found, add the import to the ast file imports
			f.Imports = append(f.Imports, &ast.ImportSpec{
				Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(t.Obj().Pkg().Path())},
			})
		}
		// look up the name of the package
		name, ok := s.packageNames[t.Obj().Pkg().Path()]
		if !ok {
			panic(fmt.Sprintf("package %s not found", t.Obj().Pkg().Path()))
		}
		return &ast.SelectorExpr{
			X:   ast.NewIdent(name),
			Sel: ast.NewIdent(t.Obj().Name()),
		}
	}
	panic(fmt.Sprintf("unsupported type %T", t))
}
