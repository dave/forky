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
		"libify simple": {
			files:    `func Foo() {}`,
			mutators: Libify{map[string]map[string]bool{"a": {"a": true}}},
			expected: map[string]string{
				"a.go": `func (psess *PackageSession) Foo() {}`,
				"package-session.go": `
					type PackageSession struct {
					}
					func NewPackageSession() *PackageSession {
						return &PackageSession{}
					}`,
			},
		},
		"libify other methods": {
			files:    `func (f F) Foo() {}`,
			mutators: Libify{map[string]map[string]bool{"a": {"a": true}}},
			expected: map[string]string{
				"a.go": `func (f F) Foo(psess *PackageSession) {}`,
				"package-session.go": `
					type PackageSession struct {
					}
					func NewPackageSession() *PackageSession {
						return &PackageSession{}
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
	}

	single := "" // during dev, set this to the name of a test case to just run that single case

	if single != "" {
		tests = map[string]testspec{single: tests[single]}
	}

	for _, spec := range tests {
		if err := runTest(spec); err != nil {
			t.Fatal(err)
			return
		}
	}

	if single != "" {
		t.Fatal("test passed, but failed because single mode is set")
	}
}

type testspec struct {
	name     string
	files    interface{} // either map[string]map[string]string, map[string]string or string
	mutators interface{} // either Mutator or []Mutator
	expected interface{} // either map[string]map[string]string, map[string]string or string
}

func runTest(spec testspec) error {
	s := NewSession("/")
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
	if err := s.Save("/"); err != nil {
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
			foundBytes, err := format.Source([]byte(found))
			if err != nil {
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

/*
	out := map[string]map[string]string{
		"foo/bar": {
			"bar.go": ` package bar`,
			"session.go": `	package bar

							func NewPackage() *Package {
								return &Package{
									i: 1,
								}
							}

							type Package struct {
								i int
							}

							func (p *Package) foo() string {
								return "foo"
							}`,
		},
	}*/
