package forky

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dave/services/fsutil"
	"gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/memfs"
	"gopkg.in/src-d/go-billy.v4/util"
)

func TestAll(t *testing.T) {
	tests := map[string]testspec{
		"callgraph": {
			skip: true,
			files: `package main
				var i, j int
				var m, n map[string]string
				func main() {
					pointer_assign(&i)
					map_update_param(n)
				}
				func int_assign() {
					i = 1
				}
				func map_update() {
					m["a"] = "b"
				}
				func map_update_param(v map[string]string) {
					v["a"] = "b"
				}
				func pointer_assign(v *int) {
					*v = 1
				}`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": `package main
		
					func (pstate *PackageState) main() {
						pstate.
							a()
					}
					func (pstate *PackageState) a() {
						pstate.
							v++
						b()
					}
					func b() {
						c()
					}
					func c() {}`,
				"package-state.go": `
					package main
					type PackageState struct {
						v int
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						return pstate
					}`,
			},
		},
		"simple string replace": {
			files: `var a = "foo"`,
			mutators: ModifyStrings(func(s string) string {
				if s == "foo" {
					return "bar"
				}
				return s
			}),
			expected: `var a = "bar"`,
		},
		"libify simple": {
			files:    `func main(){}; func Foo() {}`,
			mutators: Libify{[]string{"main"}},
			expected: map[string]string{
				"main.go": `func main(){}; func Foo() {}`,
				"package-state.go": `
					type PackageState struct {
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						return pstate
					}`,
			},
		},
		"libify other methods": {
			files:    `func main(){}; type F string; func (F) Foo() { a++ }; var a int`,
			mutators: Libify{[]string{"main"}},
			expected: map[string]string{
				"main.go": `func main(){}; type F string; func (F) Foo(pstate *PackageState) {
					pstate.a++
				}`,
				"package-state.go": `
					type PackageState struct {
						a int
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						return pstate
					}`,
			},
		},
		"libify var": {
			files:    `var i int`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": ``,
				"package-state.go": `
					type PackageState struct {
						i int
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						return pstate
					}`,
			},
		},
		"libify var init": {
			files:    `var i = 1`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": ``,
				"package-state.go": `
					type PackageState struct {
						i int
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						pstate.i = 1
						return pstate
					}`,
			},
		},
		"libify func": {
			files:    `func a() int{return 1}; func c() int {return b}; var b = a()`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": `
					func a() int { return 1 }
					func (pstate *PackageState) c() int { return pstate.b }`,
				"package-state.go": `
					type PackageState struct {
						b int
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						pstate.b = a()
						return pstate
					}`,
			},
		},
		"libify method": {
			// TODO: pstate.b.a(pstate) shouldn't span 2 lines.
			files: `type T struct{}
				func (T) a() int{ return i }
				var b = T{}
				var c = b.a()
				var i int
				func d() int {
					return b.a()
				}
				`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": `type T struct{}
					
					func (T) a(pstate *PackageState) int { return pstate.i }

					func (pstate *PackageState) d() int {
						return pstate.b.a(pstate)
					}`,
				"package-state.go": `
					type PackageState struct {
						b T
						c int
						i int
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						pstate.b = T{}
						pstate.c = pstate.
							b.a(pstate)
						return pstate
					}`,
			},
		},
		"libify var init order": {
			files:    `var a = b; var b = 1`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": ``,
				"package-state.go": `
					type PackageState struct {
						a int
						b int
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						pstate.b = 1
						pstate.a = pstate.b
						return pstate
					}`,
			},
		},
		"update imports": {
			files:    `import "fmt"; var a = fmt.Sprint("")`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": ``,
				"package-state.go": `
					import "fmt"
					type PackageState struct {
						a string
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						pstate.a = fmt.Sprint("")
						return pstate
					}`,
			},
		},
		"two packages": {
			files: map[string]map[string]string{
				"a": {"a.go": `package a; import "b"; func a(){b.B()}`},
				"b": {"b.go": `package b; func B(){a++}; var a = 1`},
			},
			mutators: Libify{[]string{"a"}},
			expected: map[string]map[string]string{
				"a": {
					"a.go": `func (pstate *PackageState) a(){
						pstate.b.B()
					}`,
					"package-state.go": `
						import "b"
						type PackageState struct {
							b *b.PackageState
						}
						func NewPackageState(b_pstate *b.PackageState) *PackageState {
							pstate := &PackageState{}
							pstate.b = b_pstate
							return pstate
						}`,
				},
				"b": {
					"b.go": `package b; func (pstate *PackageState) B(){
							pstate.a++
						}`,
					"package-state.go": `
						package b 
						type PackageState struct {
							a int
						}
						func NewPackageState() *PackageState {
							pstate := &PackageState{}
							pstate.a = 1
							return pstate
						}`,
				},
			},
		},
	}

	single := "libify other methods" // during dev, set this to the name of a test case to just run that single case

	if single != "" {
		tests = map[string]testspec{single: tests[single]}
	}

	type named struct {
		testspec
		name string
	}
	var ordered []named
	for name, spec := range tests {
		ordered = append(ordered, named{spec, name})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].name < ordered[j].name })

	var skipped bool
	for _, spec := range ordered {
		if spec.skip {
			skipped = true
			continue
		}
		if err := runTest(spec.testspec); err != nil {
			t.Fatalf("%s: %v", spec.name, err)
			return
		}
	}

	if single != "" {
		t.Fatal("test passed, but failed because single mode is set")
	}
	if skipped {
		t.Fatal("test passed, but skipped some")
	}
}

type testspec struct {
	skip     bool
	files    interface{} // either map[string]map[string]string, map[string]string or string
	mutators interface{} // either Mutator or []Mutator
	expected interface{} // either map[string]map[string]string, map[string]string or string
}

func runTest(spec testspec) error {
	s := NewSession("", "")
	s.out = &bytes.Buffer{}
	s.gopathsrc = "/"
	s.fs = memfs.New()

	files := normalize("main", spec.files)
	expected := normalize("main", spec.expected)

	for path, files := range files {
		for fname, contents := range files {
			if err := util.WriteFile(s.fs, filepath.Join(path, fname), []byte(contents), 0666); err != nil {
				return err
			}
		}
	}
	var mutators []Mutator
	switch m := spec.mutators.(type) {
	case []Mutator:
		mutators = m
	case Mutator:
		mutators = []Mutator{m}
	}
	if err := s.Run(mutators); err != nil {
		return err
	}
	if err := s.Save(); err != nil {
		return err
	}

	// first count the files in the expected
	var count int
	for _, files := range expected {
		count += len(files)
	}

	// then walk the actual output filesystem, and compare every file
	var actual int
	if err := fsutil.Walk(s.fs, "/", func(fs billy.Filesystem, fpath string, finfo os.FileInfo, err error) error {
		if finfo == nil {
			return nil
		}
		if !finfo.IsDir() {
			dir, fname := filepath.Split(fpath)
			path := dirToPath(dir)
			if expected[path] == nil {
				return fmt.Errorf("path %s not expected", path)
			}
			if expected[path][fname] == "" {
				return fmt.Errorf("file %s not expected", fpath)
			}
			actual++
			exp := expected[path][fname]
			found, err := readFile(s.fs, fpath)
			if err != nil {
				return err
			}
			expectedBytes, err := format.Source([]byte(exp))
			if err != nil {
				return err
			}
			foundBytes, err := format.Source(found)
			if err != nil {
				fmt.Println(string(found))
				return err
			}
			if string(expectedBytes) != string(foundBytes) {
				return fmt.Errorf("unexpected contents in %s - expected:\n------------------------------------\n%s\n------------------------------------\nactual:\n------------------------------------\n%s\n------------------------------------\n", fpath, string(expectedBytes), string(foundBytes))
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if actual != count {
		return fmt.Errorf("wrong number of files - expected %d, found %d", count, actual)
	}

	return nil
}

func normalize(name string, i interface{}) map[string]map[string]string {
	var m map[string]map[string]string
	switch v := i.(type) {
	case map[string]map[string]string:
		m = v
	case map[string]string:
		m = map[string]map[string]string{name: v}
	case string:
		m = map[string]map[string]string{name: {name + ".go": v}}
	}
	for path, files := range m {
		for fname, contents := range files {
			if !strings.HasPrefix(strings.TrimSpace(contents), "package ") {
				m[path][fname] = "package " + name + "\n" + contents
			}
		}
	}
	return m
}
