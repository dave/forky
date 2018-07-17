package migraty

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dave/services/fsutil"
	"gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/memfs"
	"gopkg.in/src-d/go-billy.v4/util"
)

func TestAll(t *testing.T) {
	tests := map[string]testspec{
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
			files:    `func Foo() {}`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": `func (psess *PackageSession) Foo() {}`,
				"package-session.go": `
					type PackageSession struct {
					}
					func NewPackageSession() *PackageSession {
						psess := &PackageSession{}
						return psess
					}`,
			},
		},
		"libify other methods": {
			files:    `type F string; func (f F) Foo() {}`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": `type F string; func (f F) Foo(psess *PackageSession) {}`,
				"package-session.go": `
					type PackageSession struct {
					}
					func NewPackageSession() *PackageSession {
						psess := &PackageSession{}
						return psess
					}`,
			},
		},
		"libify var": {
			files:    `var i int`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": ``,
				"package-session.go": `
					type PackageSession struct {
						i int
					}
					func NewPackageSession() *PackageSession {
						psess := &PackageSession{}
						return psess
					}`,
			},
		},
		"libify var init": {
			files:    `var i = 1`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": ``,
				"package-session.go": `
					type PackageSession struct {
						i int
					}
					func NewPackageSession() *PackageSession {
						psess := &PackageSession{}
						psess.i = 1
						return psess
					}`,
			},
		},
		"libify func": {
			files:    `func a() int{return 1}; func c() int {return a()}; var b = a()`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": `func (psess *PackageSession) a() int { return 1 }
				func (psess *PackageSession) c() int { return psess.a() }`,
				"package-session.go": `
					type PackageSession struct {
						b int
					}
					func NewPackageSession() *PackageSession {
						psess := &PackageSession{}
						psess.b = psess.a()
						return psess
					}`,
			},
		},
		"libify method": {
			// TODO: psess.b.a(psess) shouldn't span 2 lines.
			files: `type T struct{}
				func (T) a() int{ return 1 }
				var b = T{}
				var c = b.a()
				func d() int {
					return b.a()
				}
				`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": `type T struct{}
					
					func (T) a(psess *PackageSession) int { return 1 }

					func (psess *PackageSession) d() int {
						return psess.b.a(psess)
					}`,
				"package-session.go": `
					type PackageSession struct {
						b T
						c int
					}
					func NewPackageSession() *PackageSession {
						psess := &PackageSession{}
						psess.b = T{}
						psess.c = psess.
							b.a(psess)
						return psess
					}`,
			},
		},
		"libify var init order": {
			files:    `var a = b; var b = 1`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": ``,
				"package-session.go": `
					type PackageSession struct {
						a int
						b int
					}
					func NewPackageSession() *PackageSession {
						psess := &PackageSession{}
						psess.b = 1
						psess.a = psess.b
						return psess
					}`,
			},
		},
		"update imports": {
			files:    `import "fmt"; var a = fmt.Sprint("")`,
			mutators: Libify{[]string{"a"}},
			expected: map[string]string{
				"a.go": ``,
				"package-session.go": `
					import "fmt"
					type PackageSession struct {
						a string
					}
					func NewPackageSession() *PackageSession {
						psess := &PackageSession{}
						psess.a = fmt.Sprint("")
						return psess
					}`,
			},
		},
		"two packages": {
			files: map[string]map[string]string{
				"a": {"a.go": `package a; import "b"; func a(){b.B()}`},
				"b": {"b.go": `package b; func B(){}`},
			},
			mutators: Libify{[]string{"a"}},
			expected: map[string]map[string]string{
				"a": {
					"a.go": `func (psess *PackageSession) a(){
						psess.b.B()
					}`,
					"package-session.go": `
						import "b"
						type PackageSession struct {
							b *b.PackageSession
						}
						func NewPackageSession(b_psess *b.PackageSession) *PackageSession {
							psess := &PackageSession{}
							psess.b = b_psess
							return psess
						}`,
				},
				"b": {
					"b.go": `package b; func (psess *PackageSession) B(){}`,
					"package-session.go": `
						package b 
						type PackageSession struct {
						}
						func NewPackageSession() *PackageSession {
							psess := &PackageSession{}
							return psess
						}`,
				},
			},
		},
	}

	single := "" // during dev, set this to the name of a test case to just run that single case

	if single != "" {
		tests = map[string]testspec{single: tests[single]}
	}

	var skipped bool
	for name, spec := range tests {
		if spec.skip {
			skipped = true
			continue
		}
		if err := runTest(spec); err != nil {
			t.Fatalf("%s: %v", name, err)
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

	normalize := func(i interface{}) map[string]map[string]string {
		var m map[string]map[string]string
		switch v := i.(type) {
		case map[string]map[string]string:
			m = v
		case map[string]string:
			m = map[string]map[string]string{"a": v}
		case string:
			m = map[string]map[string]string{"a": {"a.go": v}}
		}
		for path, files := range m {
			for name, contents := range files {
				if !strings.HasPrefix(strings.TrimSpace(contents), "package ") {
					m[path][name] = "package a\n" + contents
				}
			}
		}
		return m
	}

	files := normalize(spec.files)
	expected := normalize(spec.expected)

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
