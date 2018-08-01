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
		"libify var unused": {
			files:    `func main(){}; var i int`,
			mutators: Libify{[]string{"main"}},
			expected: map[string]string{
				"main.go": `func main(){}; var i int`,
				"package-state.go": `
					type PackageState struct {
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						return pstate
					}`,
			},
		},
		"libify var used": {
			files:    `func main(){}; func a(){ i = 1 }; var i int`,
			mutators: Libify{[]string{"main"}},
			expected: map[string]string{
				"main.go": `func main(){}; func (pstate *PackageState) a() {
					pstate.i = 1
				}`,
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
			files:    `func main(){}; var i, j = 1, 2; func a(){ i = 2; print(j) }`,
			mutators: Libify{[]string{"main"}},
			expected: map[string]string{
				"main.go": `func main(){}; var j = 2; func (pstate *PackageState) a() {
					pstate.i = 2
					print(j)
				}`,
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
		"libify func unused": {
			files:    `func main(){}; func a() int{return 1}; func c() int {return b}; var b = a()`,
			mutators: Libify{[]string{"main"}},
			expected: map[string]string{
				"main.go": `
					func main(){}
					func a() int { return 1 }
					func c() int { return b }
					var b = a()`,
				"package-state.go": `
					type PackageState struct {
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						return pstate
					}`,
			},
		},
		"libify func used": {
			// only a is written to... b is unchanged so remains package-level
			files: `
				func main(){}
				func f1() int { return 1 }
				func f2() int { return a }
				var a, b = f1(), 1
				func f3(){ a++ }
				func f4() int { return b }`,
			mutators: Libify{[]string{"main"}},
			expected: map[string]string{
				"main.go": `
					func main(){}
					func f1() int { return 1 }
					func (pstate *PackageState) f2() int { return pstate.a }
					var b = 1

					func (pstate *PackageState) f3() {
						pstate.a++
					}
					func f4() int { return b }`,
				"package-state.go": `
					type PackageState struct {
						a int
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						pstate.a = f1()
						return pstate
					}`,
			},
		},
		"libify method": {
			// TODO: pstate... shouldn't span 2 lines.
			files: `
				func main(){}
				type T string
				func (T) m1() int{ return v3 }
				var v1 = T("1")
				var v2 = v1.m1()
				var v3 int
				func f1() int {
					return v1.m1()
				}
				func f2() {
					v1 = T("2")
					v2++
					v3--
				}
				`,
			mutators: Libify{[]string{"main"}},
			expected: map[string]string{
				"main.go": `
					func main(){}
					type T string
					
					func (T) m1(pstate *PackageState) int { return pstate.v3 }

					func (pstate *PackageState) f1() int {
						return pstate.v1.m1(pstate)
					}
					func (pstate *PackageState) f2() {
						pstate.
							v1 = T("2")
						pstate.
							v2++
						pstate.
							v3--
					}`,
				"package-state.go": `
					type PackageState struct {
						v1 T
						v2 int
						v3 int
					}
					func NewPackageState() *PackageState {
						pstate := &PackageState{}
						pstate.v1 = T("1")
						pstate.v2 = pstate.
							v1.m1(pstate)
						return pstate
					}`,
			},
		},
		"libify var init order": {
			files:    `func main(){}; var a = b; var b = 1; func f1() {a, b = 1, 2}`,
			mutators: Libify{[]string{"main"}},
			expected: map[string]string{
				"main.go": `
					func main(){}

					func (pstate *PackageState) f1() {
						pstate.a, pstate.b = 1, 2
					}
				`,
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
		"libify var init order pstate": {
			// a is never changed, but initialising relies on a pstate variable b, so it must be in
			// the pstate.
			// TODO: FIX THIS
			skip:     true,
			files:    `func main(){}; var a = b; var b = 1; func f1() { b = 2 }`,
			mutators: Libify{[]string{"main"}},
			expected: map[string]string{
				"main.go": `
					func main(){}

					func (pstate *PackageState) f1() {
						pstate.b = 2
					}
				`,
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
			files: map[string]map[string]string{
				"main": {"main.go": `package main; import "b"; func main(){}; var a = b.B(); func f(){ a = "c" }`},
				"b":    {"b.go": `package b; func B() string { return "b" }`},
			},
			mutators: Libify{[]string{"main"}},
			expected: map[string]map[string]string{
				"main": {
					"main.go": `
						package main
						func main(){}

						func (pstate *PackageState) f() {
							pstate.a = "c"
						}`,
					"package-state.go": `
						package main
						import "b"
						type PackageState struct {
							b *b.PackageState

							a string
						}
						func NewPackageState(b_pstate *b.PackageState) *PackageState {
							pstate := &PackageState{}
							pstate.b = b_pstate
							pstate.a = b.B()
							return pstate
						}`,
				},
				"b": {
					"b.go": `
						package b
						func B() string { return "b" }`,
					"package-state.go": `
						package b
						type PackageState struct {
						}
						func NewPackageState() *PackageState {
							pstate := &PackageState{}
							return pstate
						}`,
				},
			},
		},
		"two packages": {
			files: map[string]map[string]string{
				"main": {"main.go": `package main; import "b"; func main(){}; func a(){b.B()}`},
				"b":    {"b.go": `package b; func B(){a++}; var a = 1`},
			},
			mutators: Libify{[]string{"main"}},
			expected: map[string]map[string]string{
				"main": {
					"main.go": `
						package main 
						func main(){} 
						func (pstate *PackageState) a(){
							pstate.b.B()
						}`,
					"package-state.go": `
						package main
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
					"b.go": `
						package b
						func (pstate *PackageState) B(){
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

	single := "" // during dev, set this to the name of a test case to just run that single case

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
