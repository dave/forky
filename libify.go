package migraty

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/ast/astutil"
)

type Libify struct {
	Path, Name string
}

func (m Libify) Apply(s *Session) Applier {
	return Applier{
		Func: func() {
			// 1) Find all package-level vars and funcs
			for relpath, info := range s.paths {
				for name, pkg := range info.Packages {
					for fpath, file := range pkg.Files {
						//_, fname := filepath.Split(fpath)
						_ = relpath
						_ = name
						f := func(c *astutil.Cursor) bool {
							switch n := c.Node().(type) {
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
				}
			}
			// TODO: vars

			// 2) Convert them to struct fields and methods. Convert func "main" to method "Main".
			// 3) Add a Package struct with those fields and methods
			f := &ast.File{
				Name: ast.NewIdent(m.Name),
				Decls: []ast.Decl{
					&ast.GenDecl{
						Tok: token.TYPE,
						Specs: []ast.Spec{
							&ast.TypeSpec{
								Name: ast.NewIdent("PackageSession"),
								Type: &ast.StructType{
									Fields: &ast.FieldList{
									// TODO: Fields
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
			s.paths[m.Path].Packages[m.Name].Files["package.go"] = f

			// 4) All methods of other types in the package get Package added as a param
			// 5) All other packages that import this one get wired up somehow :/

			// pkg := paths[m.Path].Packages[m.Name]
		},
	}
}
