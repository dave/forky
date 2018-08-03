package forky

import (
	"bytes"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gopkg.in/src-d/go-billy.v4/memfs"
	"gopkg.in/src-d/go-billy.v4/util"
)

func TestUsed(t *testing.T) {
	tests := map[string]usedspec{
		"simple": {
			files: `var a, b int
				func main(){ a = 1 }`,
			expected: []string{"a"},
		},
		"pointer": {
			files: `var a, b int
				func main(){ f(&b) }
				func f(v *int){ *v = 1 }`,
			expected: []string{"b"},
		},
		"struct": {
			files: `
				var a = T{ i: 1 }
				var b = T{ i: 2 }
				type T struct { i int }
				func main(){ a.i = 2 }`,
			expected: []string{"a"},
		},
		"struct pointer": {
			files: `
				var a = &T{ i: 1 }
				var b = &T{ i: 2 }
				type T struct { i int }
				func main(){ Fa(a); Fb(b); }
				func Fa(t *T){ t.i = 2 }
				func Fb(t *T){ println(t.i) }`,
			expected: []string{"a"},
		},
		"map": {
			files: `var a, b map[string]string
				func main(){ a["1"] = "1" }`,
			expected: []string{"a"},
		},
		"map pointer": {
			files: `var a = map[string]string{}
				var b = map[string]string{}
				func main(){ f(b) }
				func f(v map[string]string){ v["1"] = "1" }`,
			expected: []string{"b"},
		},
		"map pointer 2": {
			files: `var a = map[string]string{}
				var b = map[string]string{}
				func main(){ f(&a) }
				func f(v *map[string]string){ (*v)["1"] = "1" }`,
			expected: []string{"a"},
		},
		"slice": {
			files: `var a = []string{"a"}
				var b = []string{"b"}
				func main(){ a[0] = "c" }`,
			expected: []string{"a"},
		},
		"slice 2": {
			files: `var a, b []string
				func main(){ a[0] = "c" }`,
			expected: []string{"a"},
		},
		"slice 3": {
			files: `
				var a = []int{1}
				func main(){ println(a[0]); }`,
			expected: []string{},
		},
		"array": {
			files: `var a, b [5]string
				func main(){ b[0] = "c" }`,
			expected: []string{"b"},
		},
		"increment": {
			files: `var a, b int
				func main(){ b++ }`,
			expected: []string{"b"},
		},
		"method": {
			files: `var a, b int
				func main(){}
				type T struct{}
				func (T) F() { a = 1 }
				`,
			expected: []string{"a"},
		},
		"method pointer": {
			files: `var a, b int
				func main(){ t := &T{}; t.F() }
				type T struct{}
				func (*T) F() { a = 1 }
				`,
			expected: []string{"a"},
		},
		"pointer inside map": {
			files: `
				var a = map[string]*T{"a": &T{i: 1}}
				var b = map[string]*T{"b": &T{i: 2}}
				type T struct { i int }
				func main(){ Fa(a["a"]); Fb(b["b"]); }
				func Fa(t *T) { t.i++ }
				func Fb(t *T) { println(t.i) }
				`,
			expected: []string{"a"},
		},
		"pointer inside slice": {
			files: `
				var a = []*T{&T{i: 1}}
				var b = []*T{&T{i: 2}}
				type T struct { i int }
				func main(){ Fa(a[0]); Fb(b[0]); }
				func Fa(t *T) { t.i++ }
				func Fb(t *T) { println(t.i) }
				`,
			expected: []string{"a"},
		},
		"recursive": {
			skip: true,
			files: `
				type T struct { field *T }
				func main() { t := &T{}; t.field = t }
			`,
			expected: []string{"a"},
		},
	}

	var single bool
	for name, test := range tests {
		if test.single {
			if single {
				panic("two tests marked as single")
			}
			single = true
			tests = map[string]usedspec{name: test}
		}
	}

	var skipped bool
	for name, spec := range tests {
		if spec.skip && !spec.single {
			skipped = true
			continue
		}
		if err := runUsedTest(spec); err != nil {
			t.Fatalf("%s: %v", name, err)
			return
		}
	}

	if single {
		t.Fatal("test passed, but failed because single mode is set")
	}
	if skipped {
		t.Fatal("test passed, but skipped some")
	}
}

type usedspec struct {
	skip, single bool
	files        interface{} // either map[string]map[string]string, map[string]string or string
	expected     []string
}

func runUsedTest(spec usedspec) error {
	s := NewSession("", "")
	s.out = &bytes.Buffer{}
	s.gopathsrc = "/"
	s.fs = memfs.New()

	for path, files := range normalize("main", spec.files) {
		for fname, contents := range files {
			if err := util.WriteFile(s.fs, filepath.Join(path, fname), []byte(contents), 0666); err != nil {
				return err
			}
		}
	}

	files, err := s.getFiles()
	if err != nil {
		return err
	}

	if err := s.parse(files); err != nil {
		return err
	}

	l := NewLibifier(Libify{[]string{"main"}}, s)

	l.session.load()

	if err := l.scanDeps(); err != nil {
		return err
	}

	if err := l.analyzeSSA(); err != nil {
		return err
	}

	if err := l.findVarMutations(); err != nil {
		return err
	}

	pkg := l.packages["main"]

	var found []string
	for k := range pkg.varMutated {
		found = append(found, k.Name())
	}
	sort.Strings(found)

	var expected []string
	for _, v := range spec.expected {
		expected = append(expected, v)
	}
	sort.Strings(expected)

	if !reflect.DeepEqual(found, expected) {
		return fmt.Errorf("found %#v\nexpected %#v", found, spec.expected)
	}

	return nil
}
