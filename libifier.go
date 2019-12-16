package forky

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"

	"github.com/dave/dst"
	"github.com/dave/dst/dstutil"
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
	sessionFile *dst.File
	ssa         *ssa.Package

	vars    map[*dst.GenDecl]bool
	methods map[*dst.FuncDecl]bool
	funcs   map[*dst.FuncDecl]bool

	moved []declspec

	varObjects map[types.Object]bool
	varMutated map[types.Object]bool
}

type declspec struct {
	typ    dst.Expr
	names  []*dst.Ident
	values []dst.Expr
}

func (l *Libifier) NewLibifyPackage(rel, path string, info *PackageInfo) *LibifyPackage {
	return &LibifyPackage{
		PackageInfo: info,
		session:     l.session,
		libifier:    l,
		relpath:     rel,
		path:        path,

		vars:    map[*dst.GenDecl]bool{},
		methods: map[*dst.FuncDecl]bool{},
		funcs:   map[*dst.FuncDecl]bool{},

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

	/*
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
	*/

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

	return nil
}

func (l *Libifier) includeVar(ob types.Object) bool {
	if !l.varObjects[ob] {
		return false
	}
	//if !l.varMutated[ob] {
	//	return false
	//}
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
		if info.Name == "main_test" {
			continue
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
			dstutil.Apply(file, func(c *dstutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *dst.GenDecl:
					if n.Tok != token.VAR {
						return true
					}
					if _, ok := c.Parent().(*dst.DeclStmt); ok {
						// skip vars inside functions
						return true
					}

					pkg.vars[n] = true

					for _, spec := range n.Specs {
						spec := spec.(*dst.ValueSpec)
						// look up the object in the types.Defs
						for _, id := range spec.Names {
							if id.Name == "_" {
								continue
							}
							def, ok := pkg.Info.Defs[pkg.NodesAst[id].(*ast.Ident)]
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
			dstutil.Apply(file, func(c *dstutil.Cursor) bool {
				switch decl := c.Node().(type) {
				case *dst.FuncDecl:
					obj, ok := pkg.Info.Defs[pkg.NodesAst.Ident(decl.Name)]
					if !ok {
						panic("func not found in defs " + decl.Name.Name)
					}
					dstutil.Apply(decl.Body, func(c *dstutil.Cursor) bool {
						switch n := c.Node().(type) {
						case *dst.Ident:
							if n.Path != "" {
								return true
							}
							use, ok := pkg.Info.Uses[pkg.NodesAst.Ident(n)]
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
					spec := spec.(*dst.ValueSpec)
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
				dstutil.Apply(file, func(c *dstutil.Cursor) bool {
					switch decl := c.Node().(type) {
					case *dst.FuncDecl:

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
			dstutil.Apply(file, func(c *dstutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *dst.FuncDecl:

					def, ok := pkg.Info.Defs[pkg.NodesAst.Ident(n.Name)]
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
			result := dstutil.Apply(file, func(c *dstutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *dst.GenDecl:
					if !pkg.vars[n] {
						break
					}
					var specs []dst.Spec
					for _, spec := range n.Specs {
						spec := spec.(*dst.ValueSpec)
						var names []*dst.Ident
						var values []dst.Expr
						var namesMoved []*dst.Ident
						var valuesMoved []dst.Expr
						for i, name := range spec.Names {
							ob := pkg.Info.Defs[pkg.NodesAst.Ident(name)]
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

				case *dst.FuncDecl:
					switch {
					case pkg.methods[n]:
						// if method, add "pstate *PackageState" as the first parameter
						pstate := &dst.Field{
							Names: []*dst.Ident{dst.NewIdent("pstate")},
							Type: &dst.StarExpr{
								X: dst.NewIdent("PackageState"),
							},
						}
						n.Type.Params.List = append([]*dst.Field{pstate}, n.Type.Params.List...)
						c.Replace(n)
					case pkg.funcs[n]:
						// if func, add "pstate *PackageState" as the receiver
						n.Recv = &dst.FieldList{List: []*dst.Field{
							{
								Names: []*dst.Ident{dst.NewIdent("pstate")},
								Type:  &dst.StarExpr{X: dst.NewIdent("PackageState")},
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
				pkg.Files[fname] = result.(*dst.File)
			}
		}
	}
	return nil
}

func (l *Libifier) createStateFiles() error {
	for _, pkg := range l.packages {

		pkg.sessionFile = &dst.File{
			Name: dst.NewIdent(pkg.Info.Pkg.Name()),
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

	var fields []*dst.Field

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

	pkg.sessionFile.Decls = append(pkg.sessionFile.Decls, &dst.GenDecl{
		Tok: token.TYPE,
		Specs: []dst.Spec{
			&dst.TypeSpec{
				Name: dst.NewIdent("PackageState"),
				Type: &dst.StructType{
					Fields: &dst.FieldList{
						List: fields,
					},
				},
			},
		},
	})
	return nil
}

func (pkg *LibifyPackage) generatePackageStateImportFields() ([]*dst.Field, error) {
	// foo *foo.PackageState
	var fields []*dst.Field
	for _, imp := range pkg.Info.Pkg.Imports() {
		f := &dst.Field{
			Names: []*dst.Ident{dst.NewIdent(imp.Name())},
			Type: &dst.StarExpr{
				X: &dst.Ident{
					Name: "PackageState",
					Path: imp.Path(),
				},
			},
		}
		fields = append(fields, f)
	}
	return fields, nil
}

func (pkg *LibifyPackage) generatePackageStateVarFields() ([]*dst.Field, error) {
	var fields []*dst.Field
	for _, ds := range pkg.moved {
		if ds.typ != nil {
			// if a type is specified, we can add the names as one field
			infoType := pkg.Info.Types[pkg.NodesAst.Expr(ds.typ)]
			if infoType.Type == nil {
				return nil, fmt.Errorf("1 no type for %v in %s", ds.names, pkg.relpath)
			}
			var names []*dst.Ident
			for _, v := range ds.names {
				names = append(names, dst.Clone(v).(*dst.Ident))
			}
			f := &dst.Field{
				Names: names,
				Type:  pkg.typeToAstTypeSpec(infoType.Type, pkg.path, pkg.sessionFile),
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
			infoType := pkg.Info.Types[pkg.NodesAst.Expr(value)]
			if infoType.Type == nil {
				return nil, fmt.Errorf("2 no type for " + name.Name + " in " + pkg.relpath)
			}
			f := &dst.Field{
				Names: []*dst.Ident{dst.Clone(name).(*dst.Ident)},
				Type:  pkg.typeToAstTypeSpec(infoType.Type, pkg.path, pkg.sessionFile),
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

	pkg.sessionFile.Decls = append(pkg.sessionFile.Decls, &dst.FuncDecl{
		Name: dst.NewIdent("NewPackageState"),
		Type: &dst.FuncType{
			Params: &dst.FieldList{
				List: params,
			},
			Results: &dst.FieldList{
				List: []*dst.Field{
					{
						Type: &dst.StarExpr{
							X: dst.NewIdent("PackageState"),
						},
					},
				},
			},
		},
		Body: &dst.BlockStmt{
			List: body,
		},
	})

	return nil
}

func (pkg *LibifyPackage) generateNewPackageStateFuncParams() ([]*dst.Field, error) {
	var params []*dst.Field
	// b_pstate *b.PackageState
	for _, imp := range pkg.Info.Pkg.Imports() {
		f := &dst.Field{
			Names: []*dst.Ident{dst.NewIdent(fmt.Sprintf("%s_pstate", imp.Name()))},
			Type: &dst.StarExpr{
				X: &dst.Ident{Name: "PackageState", Path: imp.Path()},
			},
		}
		params = append(params, f)
	}
	return params, nil
}

func (pkg *LibifyPackage) generateNewPackageStateFuncBody() ([]dst.Stmt, error) {
	var body []dst.Stmt

	// Create the package state
	// pstate := &PackageState{}
	body = append(body, &dst.AssignStmt{
		Lhs: []dst.Expr{dst.NewIdent("pstate")},
		Tok: token.DEFINE,
		Rhs: []dst.Expr{
			&dst.UnaryExpr{
				Op: token.AND,
				X: &dst.CompositeLit{
					Type: dst.NewIdent("PackageState"),
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
		body = append(body, &dst.AssignStmt{
			Lhs: []dst.Expr{
				&dst.SelectorExpr{
					X:   dst.NewIdent("pstate"),
					Sel: dst.NewIdent(imp.Name()),
				},
			},
			Tok: token.ASSIGN,
			Rhs: []dst.Expr{
				dst.NewIdent(fmt.Sprintf("%s_pstate", imp.Name())),
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
			body = append(body, &dst.AssignStmt{
				Lhs: []dst.Expr{
					&dst.SelectorExpr{
						X:   dst.NewIdent("pstate"),
						Sel: dst.NewIdent(v.Name()),
					},
				},
				Tok: token.ASSIGN,
				Rhs: []dst.Expr{dst.Clone(pkg.NodesDst.Expr(i.Rhs)).(dst.Expr)},
			})
		}
	}

	// Finally return the package state
	body = append(body, &dst.ReturnStmt{
		Results: []dst.Expr{
			dst.NewIdent("pstate"),
		},
	})

	return body, nil
}

func (l *Libifier) updateVarFuncUsage() error {
	for _, pkg := range l.packages {
		for fname, file := range pkg.Files {
			result := dstutil.Apply(file, func(c *dstutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *dst.Ident:
					// a -> pstate.a (only if a is a var or func in the current package)
					use, ok := pkg.Info.Uses[pkg.NodesAst.Ident(n)]
					if !ok {
						return true
					}
					if pkg.libifier.includeVar(use) || pkg.libifier.funcObjects[use] {
						if use.Pkg().Path() != pkg.path {
							// This is only for if the object is in the local package. Without this,
							// we trigger on the "a" part of foo.a where foo is another package.
							return true
						}
						c.Replace(&dst.SelectorExpr{
							X:   dst.NewIdent("pstate"),
							Sel: n,
						})
					}
				}
				return true
			}, nil)
			pkg.Files[fname] = result.(*dst.File)
		}
	}
	return nil
}

func (l *Libifier) updateMethodUsage() error {
	for _, pkg := range l.packages {
		for fname, file := range pkg.Files {
			result := dstutil.Apply(file, func(c *dstutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *dst.CallExpr:
					var id *dst.Ident
					switch fun := n.Fun.(type) {
					case *dst.Ident:
						id = fun
					case *dst.SelectorExpr:
						id = fun.Sel
					default:
						return true
					}
					use, ok := pkg.Info.Uses[pkg.NodesAst.Ident(id)]
					if !ok {
						return true
					}

					if pkg.libifier.methodObjects[use] {
						if use.Pkg().Path() == pkg.path {
							n.Args = append([]dst.Expr{dst.NewIdent("pstate")}, n.Args...)
						} else {
							n.Args = append([]dst.Expr{&dst.SelectorExpr{
								X:   dst.NewIdent("pstate"),
								Sel: dst.NewIdent(use.Pkg().Name()),
							}}, n.Args...)
						}
					}

					c.Replace(n)
				}
				return true
			}, nil)
			pkg.Files[fname] = result.(*dst.File)
		}
	}
	return nil
}

func (l *Libifier) updateSelectorUsage() error {
	for _, pkg := range l.packages {
		for fname, file := range pkg.Files {
			result := dstutil.Apply(file, func(c *dstutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *dst.Ident:
					// a.B() -> pstate.a.B() (only if a is a package in the deps)
					if n.Path == "" {
						return true
					}
					pkg := pkg.libifier.packageFromPath(n.Path)
					if pkg == nil {
						return true
					}
					use, ok := pkg.Info.Uses[pkg.NodesAst.Ident(n)]
					if !ok {
						return true
					}
					if pkg.libifier.varObjects[use] || pkg.libifier.funcObjects[use] {
						pkgName := pkg.Name
						newNode := &dst.SelectorExpr{
							X: &dst.SelectorExpr{
								X:   dst.NewIdent("pstate"),
								Sel: dst.NewIdent(pkgName),
							},
							Sel: dst.NewIdent(n.Name),
						}
						c.Replace(newNode)
					}
				}
				return true
			}, nil)
			pkg.Files[fname] = result.(*dst.File)
		}
	}
	return nil
}

/*
func (l *Libifier) addPstateToTypes() error {
	for _, pkg := range l.packages {
		for fname, file := range pkg.Files {
			result := dstutil.Apply(file, func(c *dstutil.Cursor) bool {
				switch n := c.Node().(type) {
				case *dst.GenDecl:
					if n.Tok == token.TYPE {
						for _, s := range n.Specs {
							s := s.(*dst.TypeSpec)
							switch t := s.Type.(type) {
							case *dst.StructType:
								t.Fields =
							}
						}
					}
				}
				return true
			}, nil)
			pkg.Files[fname] = result.(*dst.File)
		}
	}
	return nil
}
*/

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
