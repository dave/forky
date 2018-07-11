package migraty

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"gopkg.in/src-d/go-billy.v4"
)

type Libify struct {
	Packages map[string]map[string]bool // package path -> package name -> true
}

func (m Libify) Apply(s *Session) Applier {
	return Applier{
		Func: func() {

			/*
				lc := loader.Config{
					Fset:  s.fset,
					Build: buildContext(s.gorootfs, memfs.New()),
				}
				for relpath, pathInfo := range s.paths {
					for _, pkg := range pathInfo.Packages {
						var files []*ast.File
						for _, file := range pkg.Files {
							files = append(files, file)
						}
						lc.CreateFromFiles(path.Join(s.root, relpath), files...)
					}
				}
				p, err := lc.Load()
				if err != nil {
					panic(err)
				}
				for pkg, info := range p.AllPackages {
					fmt.Println(pkg.Path(), pkg.String(), len(info.Files))
				}
			*/

			// 1) Find all package-level vars and funcs
			for relpath, packageNames := range m.Packages {
				for packageName := range packageNames {
					pkg := s.paths[relpath].Packages[packageName]
					vars := map[*ast.ValueSpec]bool{}
					for fpath, file := range pkg.Files {
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
							pkg.Files[fpath] = nil
						} else {
							pkg.Files[fpath] = result.(*ast.File)
						}
					}

					// make a list of *ast.Field corresponding to the names and types of the deleted vars
					var fields []*ast.Field
					for spec := range vars {
						if spec.Type != nil {
							// if a type is specified, we can add the names as one field
							f := &ast.Field{
								Names: spec.Names,
								Type:  spec.Type,
							}
							fields = append(fields, f)
							continue
						}
						// if spec.Type is nil, we must separate the name / value pairs and guess the
						// types from the values (e.g. var a, b = "a", 1)
						// TODO: determine type from value (will need to scan with type checker)

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

					// 3) Add a Package struct with those fields and methods
					f := &ast.File{
						Name: ast.NewIdent(packageName),
						Decls: []ast.Decl{
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
						},
					}
					pkg.Files["package-session.go"] = f
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

func buildContext(gorootfs, gopathfs billy.Filesystem, tags ...string) *build.Context {
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
			newpath, fs := filesystem(path, gorootfs, gopathfs)
			fi, err := fs.Stat(newpath)
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
			newdir, fs := filesystem(dir, gorootfs, gopathfs)
			return fs.ReadDir(newdir)
		},

		// OpenFile opens a file (not a directory) for reading.
		// If OpenFile is nil, Import uses os.Open.
		OpenFile: func(path string) (io.ReadCloser, error) {
			dir, fname := filepath.Split(path)
			newdir, fs := filesystem(dir, gorootfs, gopathfs)
			return fs.Open(filepath.Join(newdir, fname))
		},
	}
	return b
}

// Gets either sourcefs, rootfs or pathfs depending on the path, and if the package is part of source
func filesystem(dir string, gorootfs, gopathfs billy.Filesystem) (string, billy.Filesystem) {

	dir = strings.Trim(filepath.Clean(dir), string(filepath.Separator))
	parts := strings.Split(dir, string(filepath.Separator))

	switch parts[0] {
	case "gopath":
		return filepath.Join(parts[1:]...), gopathfs
	case "goroot":
		return filepath.Join(parts[1:]...), gorootfs
	}

	panic(fmt.Sprintf("Top dir should be goroot or gopath, got %s", dir))
}
