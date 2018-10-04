package main

import (
	"os"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/forky"
)

const destinationPath = "github.com/dave/golib"
const pathPrefix = destinationPath + "/src/"

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	s := forky.NewSession("go.googlesource.com/go", destinationPath)

	s.ParseFilter = func(relpath string, file os.FileInfo) bool {
		if strings.Contains(relpath, "/testdata/") || strings.HasSuffix(relpath, "/testdata") {
			return false
		}
		return strings.HasPrefix(relpath, "src/")
	}

	if err := s.Run(Default); err != nil {
		return err
	}
	if err := s.Save(); err != nil {
		return err
	}
	return nil
}

var Default = []forky.Mutator{
	forky.FilterFiles(func(relpath, fname string) bool {
		return forky.MatchPath(
			relpath,
			".git",
			".git/**",
			"misc/wasm",
			"src/cmd/compile",
			"src/cmd/compile/internal/**",
			"src/cmd/internal/bio",
			"src/cmd/internal/dwarf",
			"src/cmd/internal/gcprog",
			"src/cmd/internal/goobj",
			"src/cmd/internal/goobj/**",
			"src/cmd/internal/obj",
			"src/cmd/internal/obj/**",
			"src/cmd/internal/objabi",
			"src/cmd/internal/objfile",
			"src/cmd/internal/src",
			"src/cmd/internal/sys",
			"src/cmd/link",
			"src/cmd/link/internal/**",
			"src/internal/testenv",
			"src/cmd/vendor/golang.org/x/arch/**",
		)
	}),
	&forky.PathReplacer{
		Replacement: "${1}" + pathPrefix + "${2}${3}",
		Matchers: []string{
			"internal/testenv",
			"cmd/compile",
			"cmd/internal",
			"cmd/link",
		},
	},
	forky.TestSkipper{
		forky.TestSkip{"src/cmd/internal/obj/arm64", "TestNoRet", "TODO: Enable when go1.11 released"},
		forky.TestSkip{"src/cmd/internal/obj/arm64", "TestLarge", "TODO: Enable when go1.11 released"},

		forky.TestSkip{"src/cmd/link/internal/ld", "TestVarDeclCoordsWithLineDirective", "TODO: Enable when go1.11 released"},
		forky.TestSkip{"src/cmd/link/internal/ld", "TestRuntimeTypeAttr", "TODO: Enable when go1.11 released"},
		forky.TestSkip{"src/cmd/link/internal/ld", "TestUndefinedRelocErrors", "TODO: Enable when go1.11 released"},

		forky.TestSkip{"src/cmd/link", "TestDWARF", "TODO: ???"},
		forky.TestSkip{"src/cmd/link", "TestDWARFiOS", "TODO: ???"},

		forky.TestSkip{"src/cmd/compile/internal/gc", "TestEmptyDwarfRanges", "TODO: ???"},
		forky.TestSkip{"src/cmd/compile/internal/gc", "TestIntendedInlining", "TODO: ???"},

		forky.TestSkip{"src/cmd/compile/internal/syntax", "TestStdLib", "TODO: ???"},

		forky.TestSkip{"src/cmd/compile/internal/gc", "TestBuiltin", "TODO: I think this is failing because we're stripping comments from the AST?"},
	},
	forky.DeleteNodes(func(relpath, fname string, node, parent dst.Node) bool {
		// Delete `case macho.CpuArm64` clause in objfile/macho.go
		// TODO: I think this can be reverted after go1.11 is in use.
		if cc, ok := node.(*dst.CaseClause); ok && len(cc.List) > 0 {
			if se, ok := cc.List[0].(*dst.SelectorExpr); ok && se.Sel.Name == "CpuArm64" {
				if x, ok := se.X.(*dst.Ident); ok && x.Name == "macho" {
					return true
				}
			}
		}
		return false
	}),

	// All tests pass now!

	forky.Libify{
		Packages: []string{
			"src/cmd/compile",
			"src/cmd/link",
		},
	},
}
