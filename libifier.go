package forky

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"

	"github.com/dave/services/progutils"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/pointer"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

type Libifier struct {
	libify   Libify
	session  *Session
	packages map[string]*LibifyPackage
	ssa      *ssa.Program

	varObjects    map[types.Object]bool
	methodObjects map[types.Object]bool
	funcObjects   map[types.Object]bool

	varUses    map[types.Object]map[types.Object]bool // func object -> var objects
	funcUses   map[types.Object]map[types.Object]bool // func object -> func objects
	varMutated map[types.Object]bool
}

func NewLibifier(l Libify, s *Session) *Libifier {
	return &Libifier{
		libify:   l,
		session:  s,
		packages: map[string]*LibifyPackage{},

		varObjects:    map[types.Object]bool{},
		methodObjects: map[types.Object]bool{},
		funcObjects:   map[types.Object]bool{},

		varUses:    map[types.Object]map[types.Object]bool{},
		funcUses:   map[types.Object]map[types.Object]bool{},
		varMutated: map[types.Object]bool{},
	}
}

type LibifyPackage struct {
	*PackageInfo
	session     *Session
	libifier    *Libifier
	relpath     string
	path        string
	sessionFile *ast.File
	ssa         *ssa.Package

	vars    map[*ast.GenDecl]bool
	methods map[*ast.FuncDecl]bool
	funcs   map[*ast.FuncDecl]bool

	moved []declspec

	varObjects map[types.Object]bool
	varMutated map[types.Object]bool
}

type declspec struct {
	typ    ast.Expr
	names  []*ast.Ident
	values []ast.Expr
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

		varObjects: map[types.Object]bool{},
		varMutated: map[types.Object]bool{},
	}
}

func (l *Libifier) Run() error {

	fmt.Println("")

	fmt.Println("load()")
	l.session.load()

	fmt.Println("scanDeps()")
	if err := l.scanDeps(); err != nil {
		return err
	}

	fmt.Println("findVars()")
	// finds all package level vars, populates vars, varObjects
	if err := l.findVars(); err != nil {
		return err
	}

	fmt.Println("findVarUses()")
	if err := l.findVarUses(); err != nil {
		return err
	}

	fmt.Println("analyzeSSA")
	if err := l.analyzeSSA(); err != nil {
		return err
	}

	fmt.Println("findVarMutations")
	if err := l.findVarMutations(); err != nil {
		return err
	}

	fmt.Println("all vars:")
	for relpath, pkg := range l.packages {
		for ob := range pkg.varObjects {
			fmt.Print(relpath, " ", ob.Name())
			if l.varMutated[ob] {
				fmt.Println(" mutated")
			} else {
				fmt.Println("")
			}
		}
	}

	//if err := l.generateCallGraph(); err != nil {
	//	return err
	//}

	// finds all package level funcs and methods, populates methods, funcs, methodObjects, funcObjects
	if err := l.findFuncs(); err != nil {
		return err
	}

	// deletes all vars, adds receiver to all funcs and adds a new param to all methods.
	if err := l.updateDecls(); err != nil {
		return err
	}

	// creates package-state.go
	if err := l.createStateFiles(); err != nil {
		return err
	}

	// updates usage of vars and funcs to the field of the package state
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

func (l *Libifier) includeVar(ob types.Object) bool {
	if !l.varObjects[ob] {
		return false
	}
	if !l.varMutated[ob] {
		return false
	}
	return true
}

func (l *Libifier) scanDeps() error {
	// Find all packages and the full dependency tree (only packages in s.paths considered)
	var scan func(p *PackageInfo)
	scan = func(p *PackageInfo) {
		if p == nil {
			panic("p == nil!!!")
			return
		}
		if p.Info == nil {
			panic("p.Info == nil!!! " + p.Name)
			return
		}
		if p.Info.Pkg == nil {
			panic("p.Info.Pkg == nil!!! " + p.Name)
			return
		}
		for _, imported := range p.Info.Pkg.Imports() {
			relpath, ok := l.session.Rel(imported.Path())
			if !ok {
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

func (l *Libifier) findVars() error {

	// TODO: exclude vars that are never modified

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
							pkg.libifier.varObjects[def] = true
							pkg.varObjects[def] = true
						}
					}
				}
				return true
			}, nil)
		}
	}
	return nil
}

func (l *Libifier) findVarUses() error {
	for _, pkg := range l.packages {
		for _, file := range pkg.Files {
			astutil.Apply(file, func(c *astutil.Cursor) bool {
				switch decl := c.Node().(type) {
				case *ast.FuncDecl:
					obj, ok := pkg.Info.Defs[decl.Name]
					if !ok {
						panic("func not found in defs " + decl.Name.Name)
					}
					astutil.Apply(decl.Body, func(c *astutil.Cursor) bool {
						switch n := c.Node().(type) {
						case *ast.Ident:
							use, ok := pkg.Info.Uses[n]
							if !ok {
								return true
							}
							if l.varObjects[use] {
								if l.varUses[obj] == nil {
									l.varUses[obj] = map[types.Object]bool{}
								}
								l.varUses[obj][use] = true
								return true
							}
							// funcs?
							if _, ok := use.Type().Underlying().(*types.Signature); ok {
								if l.funcUses[obj] == nil {
									l.funcUses[obj] = map[types.Object]bool{}
								}
								l.funcUses[obj][use] = true
							}

						}
						return true
					}, nil)
				}
				return true
			}, nil)
		}
	}
	return nil
}

// analyze created the ssa program and performs pointer analysis
func (l *Libifier) analyzeSSA() error {
	l.ssa = ssautil.CreateProgram(l.session.prog, 0)
	for _, pkg := range l.ssa.AllPackages() {
		pkg.Build()
		p := l.packageFromPath(pkg.Pkg.Path())
		if p == nil {
			continue
			//panic(fmt.Sprintf("package %s not found", pkg.Pkg.Path()))
		}
		p.ssa = pkg
	}
	l.ssa.Build()
	return nil
}

func (l *Libifier) findVarMutations() error {

	// See: https://godoc.org/golang.org/x/tools/go/pointer#example-package

	// Make a list of the main packages to feed into the pointer analysis
	var mains []*ssa.Package
	for _, p := range l.libify.Packages {
		if l.packages[p].Name != "main" {
			continue
		}
		mains = append(mains, l.ssa.Package(l.packages[p].Info.Pkg))
	}

	// Configure the pointer analysis to build a call-graph.
	config := &pointer.Config{
		Mains:          mains,
		BuildCallGraph: true,
		Reflection:     true,
	}

	/*
		for _, pkg := range l.packages {
			for decl := range pkg.vars {
				for _, spec := range decl.Specs {
					spec := spec.(*ast.ValueSpec)
					for _, name := range spec.Names {
						val := pkg.ssa.Var(name.Name)
						if val == nil {
							panic("ssa nil " + name.Name)
						}
						//fmt.Println("adding query", val)
						config.AddQuery(val)
					}
				}
			}
		}
	*/

	var modified []ssa.Value

	for _, pkg := range l.ssa.AllPackages() {
		type info struct {
			*ssa.Function
			method bool
		}
		var functions []info

		for _, member := range pkg.Members {
			switch m := member.(type) {
			case *ssa.Function:
				functions = append(functions, info{m, false})
			case *ssa.Type:
				action := func(t types.Type) {
					ms := l.ssa.MethodSets.MethodSet(t)
					for i := 0; i < ms.Len(); i++ {
						f := l.ssa.MethodValue(ms.At(i))
						functions = append(functions, info{f, true})
					}
				}
				t, ok := m.Type().(*types.Named)
				if !ok {
					// TODO: not a named type - don't scan methods?
					break
				}
				action(t)
				action(types.NewPointer(t))
				// TODO: What about **T?
			}
		}

		for _, f := range functions {
			if f.Function == nil {
				continue
			}
			if f.Name() == "init" && !f.method {
				// skip init function (but not methods called init!)
				continue
			}
			var blocks []*ssa.BasicBlock
			blocks = append(blocks, f.Blocks...)
			if f.Recover != nil {
				blocks = append(blocks, f.Recover)
			}
			for _, block := range blocks {
				for _, ins := range block.Instrs {

					var action func(v ssa.Value)
					action = func(v ssa.Value) {
						switch v := v.(type) {
						case *ssa.Global:
							config.AddQuery(v)
							modified = append(modified, v)
						case *ssa.UnOp:
							if v.Op != token.MUL {
								panic(fmt.Sprintf("UnOp without *: %v", v.Op))
							}
							action(v.X)
						case *ssa.IndexAddr:
							action(v.X)
						case *ssa.FieldAddr:
							action(v.X)
						default:
							config.AddQuery(v)
						}
					}

					switch ins := ins.(type) {
					case *ssa.Store:
						action(ins.Addr)
					case *ssa.MapUpdate:
						action(ins.Map)
					case *ssa.UnOp:
						//fmt.Printf("ins.(type): %T %v\n", ins, ins.Op)
					case *ssa.Call, *ssa.Return:
						// noop
					default:
						//fmt.Printf("ins.(type): %T\n", ins)
					}
				}
			}
		}
	}

	// Run the pointer analysis.
	fmt.Print("pointer.Analyze...")
	result, err := pointer.Analyze(config)
	if err != nil {
		return err // internal error in pointer analysis
	}
	fmt.Println(" done")

	for _, q := range result.Queries {
		for _, label := range q.PointsTo().Labels() {
			var actionReferrer func(v ssa.Instruction, value ssa.Value)
			var actionValue func(v ssa.Value)
			actionReferrer = func(ins ssa.Instruction, value ssa.Value) {
				//fmt.Printf("REF: %T\n", ins)
				switch ins := ins.(type) {
				case *ssa.Store:
					if value == ins.Val {
						// only continue if the value is the Val operand
						actionValue(ins.Addr)
					}
				case *ssa.MapUpdate:
					if value == ins.Value {
						// only continue if the value is the Value operand
						actionValue(ins.Map)
					}
				//case *ssa.FieldAddr:
				// actionValue(ins.X)
				//	for _, r := range *ins.Referrers() {
				//		actionReferrer(r, ins)
				//	}
				case *ssa.IndexAddr:
					for _, r := range *ins.Referrers() {
						actionReferrer(r, ins)
					}
				case *ssa.Slice:
					for _, r := range *ins.Referrers() {
						actionReferrer(r, ins)
					}
				}
			}

			actionValue = func(v ssa.Value) {
				//fmt.Printf("VAL: %T\n", v)
				switch v := v.(type) {
				case *ssa.Global:
					modified = append(modified, v)
				case *ssa.MakeMap:
					// with maps, we get the makemap instruction where the map is created. To link this
					// to a Global we have to look in the Referrers list for a Store to a Global.
					for _, r := range *v.Referrers() {
						actionReferrer(r, v)
					}
				case *ssa.Alloc:
					for _, r := range *v.Referrers() {
						actionReferrer(r, v)
					}
				case *ssa.FieldAddr:
					actionValue(v.X)
				case *ssa.IndexAddr:
					actionValue(v.X)
				}
			}

			actionValue(label.Value())

		}
	}

	for _, v := range modified {
		switch v := v.(type) {
		case *ssa.Global:
			pkg := l.packageFromPath(v.Pkg.Pkg.Path())
			if pkg == nil {
				continue
			}
			l.varMutated[v.Object()] = true
			pkg.varMutated[v.Object()] = true
		default:
			panic(fmt.Sprintf("unsupported type in modified %T", v))
		}
	}

	/*
		for _, pkg := range l.packages {
			for _, file := range pkg.Files {
				astutil.Apply(file, func(c *astutil.Cursor) bool {
					switch decl := c.Node().(type) {
					case *ast.FuncDecl:

					}
					return true
				}, nil)
			}
		}
	*/
	return nil
}

/*
func (l *Libifier) generateCallGraph() error {
	prog := ssautil.CreateProgram(l.session.prog, 0)
	for _, p := range prog.AllPackages() {
		pkg := l.packageFromPath(p.Pkg.Path())
		if pkg == nil {
			continue
		}
		p.Build()
		pkg.ssa = p
	}
	l.session.ssa = prog
	l.session.graph = cha.CallGraph(prog)
	if false {
		var printNode func(int, *callgraph.Node)
		printNode = func(indent int, node *callgraph.Node) {
			fmt.Print(strings.Repeat(" ", indent))
			if node.Func == nil {
				fmt.Println("<nil>")
			} else {
				fmt.Println(node.Func.Name())
			}
			for _, edge := range node.Out {
				printNode(indent+4, edge.Callee)
			}
		}
		for f, n := range graph.Nodes {
			if f == nil {
				continue
			}
			fmt.Println("Node:", f.Name())
			printNode(0, n)
		}
	}
	return nil
}
*/

func (l *Libifier) findFuncs() error {

	// TODO: exclude funcs that don't need access to package level vars or funcs

	for _, pkg := range l.packages {
		for _, file := range pkg.Files {
			astutil.Apply(file, func(c *astutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *ast.FuncDecl:

					def, ok := pkg.Info.Defs[n.Name]
					if !ok {
						panic(fmt.Sprintf("can't find %s in defs", n.Name.Name))
					}

					// inspect the callgraph to see if this or any callees use package level vars
					var found bool
					done := map[types.Object]bool{}
					var inspect func(obj types.Object)
					inspect = func(obj types.Object) {
						if obj == nil {
							return
						}
						if done[obj] {
							return
						}
						done[obj] = true

						uses := l.varUses[obj]
						if uses != nil {
							for v := range uses {
								if l.includeVar(v) {
									found = true
									return
								}
							}
						}

						callees, ok := l.funcUses[obj]
						if !ok {
							return
						}
						for callee := range callees {
							inspect(callee)
						}
					}
					inspect(def)

					if !found {
						// call graph doesn't contain any var uses, so we can skip this function
						return true
					}

					if n.Recv != nil && len(n.Recv.List) > 0 {
						// method
						pkg.methods[n] = true
						pkg.libifier.methodObjects[def] = true
						/*
							// Print list of types that have methods that need package state
							recvTyp := pkg.Info.Types[n.Recv.List[0].Type].Type
							var name string
							for {
								if p, ok := recvTyp.(*types.Pointer); ok {
									recvTyp = p.Elem()
									continue
								}
								if n, ok := recvTyp.(*types.Named); ok {
									name = n.Obj().Pkg().Path() + "..." + n.Obj().Name()
									recvTyp = n.Underlying()
									continue
								}
								break
							}
							if !typesList[recvTyp] {

								typesList[recvTyp] = true

								if name != "" {
									fmt.Printf("Method: %T %s\n", recvTyp, name)
								} else {
									fmt.Printf("Method: %T %s\n", recvTyp, recvTyp.String())
								}

							}
						*/
					} else {
						// function
						pkg.funcs[n] = true
						pkg.libifier.funcObjects[def] = true
					}
					c.Replace(n)
				}
				return true
			}, nil)
		}
	}
	return nil
}

//var typesList = map[types.Type]bool{}

func (l *Libifier) updateDecls() error {
	for _, pkg := range l.packages {
		for fname, file := range pkg.Files {
			result := astutil.Apply(file, func(c *astutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *ast.GenDecl:
					if !pkg.vars[n] {
						break
					}
					var specs []ast.Spec
					for _, spec := range n.Specs {
						spec := spec.(*ast.ValueSpec)
						var names []*ast.Ident
						var values []ast.Expr
						var namesMoved []*ast.Ident
						var valuesMoved []ast.Expr
						for i, name := range spec.Names {
							ob := pkg.Info.Defs[name]
							if !l.includeVar(ob) {
								// definitions of vars that are included in the package state should
								// be deleted, so only append the name if it's not included
								names = append(names, name)
								if len(spec.Values) > 0 {
									values = append(values, spec.Values[i])
								}
							} else {
								namesMoved = append(namesMoved, name)
								if len(spec.Values) > 0 {
									valuesMoved = append(valuesMoved, spec.Values[i])
								}
							}
						}
						if len(names) > 0 {
							spec.Names = names
							spec.Values = values
							specs = append(specs, spec)
						}
						if len(namesMoved) > 0 {
							ds := declspec{
								names:  namesMoved,
								values: valuesMoved,
								typ:    spec.Type,
							}
							pkg.moved = append(pkg.moved, ds)
						}
					}
					if len(specs) > 0 {
						n.Specs = specs
					} else {
						// no specs -> delete node
						c.Delete()
					}

				case *ast.FuncDecl:
					switch {
					case pkg.methods[n]:
						// if method, add "pstate *PackageState" as the first parameter
						pstate := &ast.Field{
							Names: []*ast.Ident{ast.NewIdent("pstate")},
							Type: &ast.StarExpr{
								X: ast.NewIdent("PackageState"),
							},
						}
						n.Type.Params.List = append([]*ast.Field{pstate}, n.Type.Params.List...)
						c.Replace(n)
					case pkg.funcs[n]:
						// if func, add "pstate *PackageState" as the receiver
						n.Recv = &ast.FieldList{List: []*ast.Field{
							{
								Names: []*ast.Ident{ast.NewIdent("pstate")},
								Type:  &ast.StarExpr{X: ast.NewIdent("PackageState")},
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

func (l *Libifier) createStateFiles() error {
	for _, pkg := range l.packages {

		pkg.sessionFile = &ast.File{
			Name: ast.NewIdent(pkg.Info.Pkg.Name()),
		}
		pkg.Files["package-state.go"] = pkg.sessionFile

		if err := pkg.addPackageStateStruct(); err != nil {
			return err
		}

		if err := pkg.addNewPackageStateFunc(); err != nil {
			return err
		}

	}
	return nil
}

func (pkg *LibifyPackage) addPackageStateStruct() error {

	var fields []*ast.Field

	importFields, err := pkg.generatePackageStateImportFields()
	if err != nil {
		return err
	}
	sort.Slice(importFields, func(i, j int) bool {
		return importFields[i].Names[0].Name < importFields[j].Names[0].Name
	})
	fields = append(fields, importFields...)

	varFields, err := pkg.generatePackageStateVarFields()
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
				Name: ast.NewIdent("PackageState"),
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

func (pkg *LibifyPackage) generatePackageStateImportFields() ([]*ast.Field, error) {
	// foo *foo.PackageState
	var fields []*ast.Field
	for _, imp := range pkg.Info.Pkg.Imports() {
		impPkg := pkg.libifier.packageFromPath(imp.Path())
		if impPkg == nil {
			continue
		}
		pkgId := ast.NewIdent(imp.Name())
		f := &ast.Field{
			Names: []*ast.Ident{ast.NewIdent(imp.Name())},
			Type: &ast.StarExpr{
				X: &ast.SelectorExpr{
					X:   pkgId,
					Sel: ast.NewIdent("PackageState"),
				},
			},
		}
		fields = append(fields, f)
		// have to add this package usage to the info in order for ImportsHelper to pick them up
		pkg.Info.Uses[pkgId] = types.NewPkgName(0, pkg.Info.Pkg, imp.Name(), imp)
	}
	return fields, nil
}

func (pkg *LibifyPackage) generatePackageStateVarFields() ([]*ast.Field, error) {
	var fields []*ast.Field
	for _, ds := range pkg.moved {
		if ds.typ != nil {
			// if a type is specified, we can add the names as one field
			infoType := pkg.Info.Types[ds.typ]
			if infoType.Type == nil {
				return nil, fmt.Errorf("no type for %v in %s", ds.names, pkg.relpath)
			}
			f := &ast.Field{
				Names: ds.names,
				Type:  pkg.session.typeToAstTypeSpec(infoType.Type, pkg.path, pkg.sessionFile),
			}
			fields = append(fields, f)
			continue
		}
		// if spec.Type is nil, we must separate the name / value pairs
		for i, name := range ds.names {
			if name.Name == "_" {
				continue
			}
			value := ds.values[i]
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
	return fields, nil
}

func (pkg *LibifyPackage) addNewPackageStateFunc() error {
	params, err := pkg.generateNewPackageStateFuncParams()
	if err != nil {
		return err
	}

	body, err := pkg.generateNewPackageStateFuncBody()
	if err != nil {
		return err
	}

	pkg.sessionFile.Decls = append(pkg.sessionFile.Decls, &ast.FuncDecl{
		Name: ast.NewIdent("NewPackageState"),
		Type: &ast.FuncType{
			Params: &ast.FieldList{
				List: params,
			},
			Results: &ast.FieldList{
				List: []*ast.Field{
					{
						Type: &ast.StarExpr{
							X: ast.NewIdent("PackageState"),
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

func (pkg *LibifyPackage) generateNewPackageStateFuncParams() ([]*ast.Field, error) {
	var params []*ast.Field
	// b_pstate *b.PackageState
	for _, imp := range pkg.Info.Pkg.Imports() {
		impPkg := pkg.libifier.packageFromPath(imp.Path())
		if impPkg == nil {
			continue
		}
		pkgId := ast.NewIdent(imp.Name())
		f := &ast.Field{
			Names: []*ast.Ident{ast.NewIdent(fmt.Sprintf("%s_pstate", imp.Name()))},
			Type: &ast.StarExpr{
				X: &ast.SelectorExpr{
					X:   pkgId,
					Sel: ast.NewIdent("PackageState"),
				},
			},
		}
		params = append(params, f)
		// have to add this package usage to the info in order for ImportsHelper to pick them up
		pkg.Info.Uses[pkgId] = types.NewPkgName(0, pkg.Info.Pkg, imp.Name(), imp)
	}
	return params, nil
}

func (pkg *LibifyPackage) generateNewPackageStateFuncBody() ([]ast.Stmt, error) {
	var body []ast.Stmt

	// Create the package state
	// pstate := &PackageState{}
	body = append(body, &ast.AssignStmt{
		Lhs: []ast.Expr{ast.NewIdent("pstate")},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{
			&ast.UnaryExpr{
				Op: token.AND,
				X: &ast.CompositeLit{
					Type: ast.NewIdent("PackageState"),
				},
			},
		},
	})

	// Assign the injected package state for all imported packages
	// pstate.foo = foo_pstate
	for _, imp := range pkg.Info.Pkg.Imports() {
		impPkg := pkg.libifier.packageFromPath(imp.Path())
		if impPkg == nil {
			continue
		}
		body = append(body, &ast.AssignStmt{
			Lhs: []ast.Expr{
				&ast.SelectorExpr{
					X:   ast.NewIdent("pstate"),
					Sel: ast.NewIdent(imp.Name()),
				},
			},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{
				ast.NewIdent(fmt.Sprintf("%s_pstate", imp.Name())),
			},
		})
	}

	// Initialise the vars in init order
	for _, i := range pkg.Info.InitOrder {
		for _, v := range i.Lhs {
			if v.Name() == "_" {
				continue
			}
			if !pkg.libifier.includeVar(v) {
				continue
			}
			body = append(body, &ast.AssignStmt{
				Lhs: []ast.Expr{
					&ast.SelectorExpr{
						X:   ast.NewIdent("pstate"),
						Sel: ast.NewIdent(v.Name()),
					},
				},
				Tok: token.ASSIGN,
				Rhs: []ast.Expr{i.Rhs},
			})
		}
	}

	// Finally return the package state
	body = append(body, &ast.ReturnStmt{
		Results: []ast.Expr{
			ast.NewIdent("pstate"),
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
					// a -> pstate.a (only if a is a var or func in the current package)
					use, ok := pkg.Info.Uses[n]
					if !ok {
						return true
					}
					if pkg.libifier.includeVar(use) || pkg.libifier.funcObjects[use] {
						if use.Pkg().Path() != pkg.path {
							// This is only for if the object is in the local package. Without this,
							// we trigger on the "a" part of foo.a where foo is another package.
							return true
						}
						c.Replace(&ast.SelectorExpr{
							X:   ast.NewIdent("pstate"),
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

					if pkg.libifier.methodObjects[use] {
						if use.Pkg().Path() == pkg.path {
							n.Args = append([]ast.Expr{ast.NewIdent("pstate")}, n.Args...)
						} else {
							n.Args = append([]ast.Expr{&ast.SelectorExpr{
								X:   ast.NewIdent("pstate"),
								Sel: ast.NewIdent(use.Pkg().Name()),
							}}, n.Args...)
						}
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
					// a.B() -> pstate.a.B() (only if a is a package in the deps)
					packagePath, _, _, _ := progutils.QualifiedIdentifierInfo(n, pkg.Info.Pkg.Path(), l.session.prog)
					if packagePath == "" {
						return true
					}
					if pkg.libifier.packageFromPath(packagePath) == nil {
						return true
					}
					use, ok := pkg.Info.Uses[n.Sel]
					if !ok {
						return true
					}
					if pkg.libifier.varObjects[use] || pkg.libifier.funcObjects[use] {
						pkgName := n.X.(*ast.Ident).Name
						newNode := &ast.SelectorExpr{
							X: &ast.SelectorExpr{
								X:   ast.NewIdent("pstate"),
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

func (l *Libifier) packageFromPath(path string) *LibifyPackage {
	relpath, ok := l.session.Rel(path)
	if !ok {
		return nil
	}
	pkg, ok := l.packages[relpath]
	if !ok {
		return nil
	}
	return pkg
}
