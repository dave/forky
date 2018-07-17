package migraty

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"

	"github.com/dave/services/progutils"
	"golang.org/x/tools/go/ast/astutil"
)

type Libifier struct {
	libify   Libify
	session  *Session
	packages map[string]*LibifyPackage
}

func NewLibifier(l Libify, s *Session) *Libifier {
	return &Libifier{
		libify:   l,
		session:  s,
		packages: map[string]*LibifyPackage{},
	}
}

type LibifyPackage struct {
	*PackageInfo
	session  *Session
	libifier *Libifier
	relpath  string
	path     string

	vars    map[*ast.GenDecl]bool
	methods map[*ast.FuncDecl]bool
	funcs   map[*ast.FuncDecl]bool

	varObjects    map[types.Object]bool
	methodObjects map[types.Object]bool
	funcObjects   map[types.Object]bool

	sessionFile *ast.File
}

func (l *Libifier) NewLibifyPackage(rel, path string, info *PackageInfo) *LibifyPackage {
	return &LibifyPackage{
		PackageInfo: info,
		session:     l.session,
		libifier:    l,
		relpath:     rel,
		path:        path,

		vars:    map[*ast.GenDecl]bool{},
		methods: map[*ast.FuncDecl]bool{},
		funcs:   map[*ast.FuncDecl]bool{},

		varObjects:    map[types.Object]bool{},
		methodObjects: map[types.Object]bool{},
		funcObjects:   map[types.Object]bool{},
	}
}

func (l *Libifier) Run() error {

	if err := l.scanDeps(); err != nil {
		return err
	}

	// finds all package level vars, funcs and methods, populates vars, methods, funcs, varObjects, methodObjects, funcObjects
	if err := l.findDecls(); err != nil {
		return err
	}

	// deletes all vars, adds receiver to all funcs and adds a new param to all methods.
	if err := l.updateDecls(); err != nil {
		return err
	}

	// creates package-session.go
	if err := l.createSessionFiles(); err != nil {
		return err
	}

	// updates usage of vars and funcs to the field of the package session
	if err := l.updateVarFuncUsage(); err != nil {
		return err
	}

	if err := l.updateMethodUsage(); err != nil {
		return err
	}

	if err := l.updateSelectorUsage(); err != nil {
		return err
	}

	if err := l.refreshImports(); err != nil {
		return err
	}

	return nil
}

func (l *Libifier) scanDeps() error {
	// Find all packages and the full dependency tree (only packages in s.paths considered)
	var scan func(p *PackageInfo)
	scan = func(p *PackageInfo) {
		for _, imported := range p.Info.Pkg.Imports() {
			isrel, relpath := l.session.Rel(imported.Path())
			if !isrel {
				continue
			}
			info, ok := l.session.paths[relpath]
			if !ok {
				continue
			}
			scan(info.Default)
			l.packages[relpath] = l.NewLibifyPackage(relpath, imported.Path(), info.Default)
		}
	}
	for _, relpath := range l.libify.Packages {
		info := l.session.paths[relpath].Default
		if info == nil {
			return fmt.Errorf("no default package for %s", relpath)
		}
		scan(info)
		l.packages[relpath] = l.NewLibifyPackage(relpath, info.Info.Pkg.Path(), info)
	}
	return nil
}

func (l *Libifier) findDecls() error {
	for _, pkg := range l.packages {
		for _, file := range pkg.Files {
			astutil.Apply(file, func(c *astutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *ast.GenDecl:
					if n.Tok != token.VAR {
						return true
					}
					if _, ok := c.Parent().(*ast.DeclStmt); ok {
						// skip vars inside functions
						return true
					}

					pkg.vars[n] = true

					for _, spec := range n.Specs {
						spec := spec.(*ast.ValueSpec)
						// look up the object in the types.Defs
						for _, id := range spec.Names {
							if id.Name == "_" {
								continue
							}
							def, ok := pkg.Info.Defs[id]
							if !ok {
								panic(fmt.Sprintf("can't find %s in defs", id.Name))
							}
							pkg.varObjects[def] = true
						}
					}

				case *ast.FuncDecl:

					def, ok := pkg.Info.Defs[n.Name]
					if !ok {
						panic(fmt.Sprintf("can't find %s in defs", n.Name.Name))
					}

					if n.Recv != nil && len(n.Recv.List) > 0 {
						// method
						pkg.methods[n] = true
						pkg.methodObjects[def] = true
					} else {
						// function
						pkg.funcs[n] = true
						pkg.funcObjects[def] = true
					}
					c.Replace(n)
				}
				return true
			}, nil)
		}
	}
	return nil
}

func (l *Libifier) updateDecls() error {
	for _, pkg := range l.packages {
		for fname, file := range pkg.Files {
			result := astutil.Apply(file, func(c *astutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *ast.GenDecl:
					if pkg.vars[n] {
						// remove all package-level var declarations
						c.Delete()
					}
				case *ast.FuncDecl:
					switch {
					case pkg.methods[n]:
						// if method, add "psess *PackageSession" as the first parameter
						psess := &ast.Field{
							Names: []*ast.Ident{ast.NewIdent("psess")},
							Type: &ast.StarExpr{
								X: ast.NewIdent("PackageSession"),
							},
						}
						n.Type.Params.List = append([]*ast.Field{psess}, n.Type.Params.List...)
						c.Replace(n)
					case pkg.funcs[n]:
						// if func, add "psess *PackageSession" as the receiver
						n.Recv = &ast.FieldList{List: []*ast.Field{
							{
								Names: []*ast.Ident{ast.NewIdent("psess")},
								Type:  &ast.StarExpr{X: ast.NewIdent("PackageSession")},
							},
						}}
						c.Replace(n)
					}
				}
				return true
			}, nil)
			if result == nil {
				pkg.Files[fname] = nil
			} else {
				pkg.Files[fname] = result.(*ast.File)
			}
		}
	}
	return nil
}

func (l *Libifier) createSessionFiles() error {
	for _, pkg := range l.packages {

		pkg.sessionFile = &ast.File{
			Name: ast.NewIdent(pkg.Info.Pkg.Name()),
		}
		pkg.Files["package-session.go"] = pkg.sessionFile

		if err := pkg.addPackageSessionStruct(); err != nil {
			return err
		}

		if err := pkg.addNewPackageSessionFunc(); err != nil {
			return err
		}

	}
	return nil
}

func (pkg *LibifyPackage) addPackageSessionStruct() error {

	var fields []*ast.Field

	importFields, err := pkg.generatePackageSessionImportFields()
	if err != nil {
		return err
	}
	sort.Slice(importFields, func(i, j int) bool {
		return importFields[i].Names[0].Name < importFields[j].Names[0].Name
	})
	fields = append(fields, importFields...)

	varFields, err := pkg.generatePackageSessionVarFields()
	if err != nil {
		return err
	}
	sort.Slice(varFields, func(i, j int) bool {
		return varFields[i].Names[0].Name < varFields[j].Names[0].Name
	})
	fields = append(fields, varFields...)

	pkg.sessionFile.Decls = append(pkg.sessionFile.Decls, &ast.GenDecl{
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
	return nil
}

func (pkg *LibifyPackage) generatePackageSessionImportFields() ([]*ast.Field, error) {
	// foo *foo.PackageSession
	var fields []*ast.Field
	for _, imp := range pkg.Info.Pkg.Imports() {
		found, relpath := pkg.session.Rel(imp.Path())
		if !found {
			continue
		}
		if pkg.libifier.packages[relpath] == nil {
			continue
		}
		pkgId := ast.NewIdent(imp.Name())
		f := &ast.Field{
			Names: []*ast.Ident{ast.NewIdent(imp.Name())},
			Type: &ast.StarExpr{
				X: &ast.SelectorExpr{
					X:   pkgId,
					Sel: ast.NewIdent("PackageSession"),
				},
			},
		}
		fields = append(fields, f)
		// have to add this package usage to the info in order for ImportsHelper to pick them up
		pkg.Info.Uses[pkgId] = types.NewPkgName(0, pkg.Info.Pkg, imp.Name(), imp)
	}
	return fields, nil
}

func (pkg *LibifyPackage) generatePackageSessionVarFields() ([]*ast.Field, error) {
	var fields []*ast.Field
	for decl := range pkg.vars {
		for _, spec := range decl.Specs {
			spec := spec.(*ast.ValueSpec)
			if spec.Type != nil {
				// if a type is specified, we can add the names as one field
				infoType := pkg.Info.Types[spec.Type]
				if infoType.Type == nil {
					return nil, fmt.Errorf("no type for %v in %s", spec.Names, pkg.relpath)
				}
				f := &ast.Field{
					Names: spec.Names,
					Type:  pkg.session.typeToAstTypeSpec(infoType.Type, pkg.path, pkg.sessionFile),
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
				infoType := pkg.Info.Types[value]
				if infoType.Type == nil {
					return nil, fmt.Errorf("no type for " + name.Name + " in " + pkg.relpath)
				}
				f := &ast.Field{
					Names: []*ast.Ident{name},
					Type:  pkg.session.typeToAstTypeSpec(infoType.Type, pkg.path, pkg.sessionFile),
				}
				fields = append(fields, f)
			}
		}
	}
	return fields, nil
}

func (pkg *LibifyPackage) addNewPackageSessionFunc() error {
	params, err := pkg.generateNewPackageSessionFuncParams()
	if err != nil {
		return err
	}

	body, err := pkg.generateNewPackageSessionFuncBody()
	if err != nil {
		return err
	}

	pkg.sessionFile.Decls = append(pkg.sessionFile.Decls, &ast.FuncDecl{
		Name: ast.NewIdent("NewPackageSession"),
		Type: &ast.FuncType{
			Params: &ast.FieldList{
				List: params,
			},
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

	return nil
}

func (pkg *LibifyPackage) generateNewPackageSessionFuncParams() ([]*ast.Field, error) {
	var params []*ast.Field
	// b_psess *b.PackageSession
	for _, imp := range pkg.Info.Pkg.Imports() {
		found, relpath := pkg.session.Rel(imp.Path())
		if !found {
			continue
		}
		if pkg.libifier.packages[relpath] == nil {
			continue
		}
		pkgId := ast.NewIdent(imp.Name())
		f := &ast.Field{
			Names: []*ast.Ident{ast.NewIdent(fmt.Sprintf("%s_psess", imp.Name()))},
			Type: &ast.StarExpr{
				X: &ast.SelectorExpr{
					X:   pkgId,
					Sel: ast.NewIdent("PackageSession"),
				},
			},
		}
		params = append(params, f)
		// have to add this package usage to the info in order for ImportsHelper to pick them up
		pkg.Info.Uses[pkgId] = types.NewPkgName(0, pkg.Info.Pkg, imp.Name(), imp)
	}
	return params, nil
}

func (pkg *LibifyPackage) generateNewPackageSessionFuncBody() ([]ast.Stmt, error) {
	var body []ast.Stmt

	// Create the package session
	// psess := &PackageSession{}
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

	// Assign the injected package session for all imported packages
	// psess.foo = foo_psess
	for _, imp := range pkg.Info.Pkg.Imports() {
		found, relpath := pkg.session.Rel(imp.Path())
		if !found {
			continue
		}
		if pkg.libifier.packages[relpath] == nil {
			continue
		}
		body = append(body, &ast.AssignStmt{
			Lhs: []ast.Expr{
				&ast.SelectorExpr{
					X:   ast.NewIdent("psess"),
					Sel: ast.NewIdent(imp.Name()),
				},
			},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{
				ast.NewIdent(fmt.Sprintf("%s_psess", imp.Name())),
			},
		})
	}

	// Initialise the vars in init order
	for _, i := range pkg.Info.InitOrder {
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

	// Finally return the package session
	body = append(body, &ast.ReturnStmt{
		Results: []ast.Expr{
			ast.NewIdent("psess"),
		},
	})

	return body, nil
}

func (l *Libifier) updateVarFuncUsage() error {
	for _, pkg := range l.packages {
		for fname, file := range pkg.Files {
			result := astutil.Apply(file, func(c *astutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *ast.Ident:
					use, ok := pkg.Info.Uses[n]
					if !ok {
						return true
					}
					if pkg.varObjects[use] || pkg.funcObjects[use] {
						c.Replace(&ast.SelectorExpr{
							X:   ast.NewIdent("psess"),
							Sel: n,
						})
					}
				}
				return true
			}, nil)
			pkg.Files[fname] = result.(*ast.File)
		}
	}
	return nil
}

func (l *Libifier) updateMethodUsage() error {
	for _, pkg := range l.packages {
		for fname, file := range pkg.Files {
			result := astutil.Apply(file, func(c *astutil.Cursor) bool {
				switch n := c.Node().(type) {
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
					use, ok := pkg.Info.Uses[id]
					if !ok {
						return true
					}
					if pkg.methodObjects[use] {
						n.Args = append([]ast.Expr{ast.NewIdent("psess")}, n.Args...)
					}
					c.Replace(n)
				}
				return true
			}, nil)
			pkg.Files[fname] = result.(*ast.File)
		}
	}
	return nil
}

func (l *Libifier) updateSelectorUsage() error {
	for _, pkg := range l.packages {
		for fname, file := range pkg.Files {
			result := astutil.Apply(file, func(c *astutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *ast.SelectorExpr:
					// a.B() -> psess.a.B() (only if a is a package in the deps)
					packagePath, _, _, _ := progutils.PackageSelector(n, pkg.Info.Pkg.Path(), l.session.prog)
					if packagePath == "" {
						return true
					}
					found, relpath := l.session.Rel(packagePath)
					if !found {
						return true
					}
					imp, ok := l.packages[relpath]
					if !ok {
						return true
					}
					use, ok := pkg.Info.Uses[n.Sel]
					if !ok {
						return true
					}
					if imp.varObjects[use] || imp.funcObjects[use] {
						pkgName := n.X.(*ast.Ident).Name
						newNode := &ast.SelectorExpr{
							X: &ast.SelectorExpr{
								X:   ast.NewIdent("psess"),
								Sel: ast.NewIdent(pkgName),
							},
							Sel: n.Sel,
						}
						c.Replace(newNode)

					}
				}
				return true
			}, nil)
			pkg.Files[fname] = result.(*ast.File)
		}
	}
	return nil
}

func (l *Libifier) refreshImports() error {
	for _, pkg := range l.packages {
		for _, file := range pkg.Files {
			ih := progutils.NewImportsHelper(pkg.Info.Pkg.Path(), file, l.session.prog)
			ih.RefreshFromCode()
		}
	}
	return nil
}
