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
	}

	single := ""

	if single != "" {
		tests = map[string]usedspec{single: tests[single]}
	}

	var skipped bool
	for name, spec := range tests {
		if spec.skip {
			skipped = true
			continue
		}
		if err := runUsedTest(spec); err != nil {
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

type usedspec struct {
	skip     bool
	files    interface{} // either map[string]map[string]string, map[string]string or string
	expected []string
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
