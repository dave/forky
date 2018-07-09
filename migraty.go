package migraty

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dave/services/fsutil"
	"golang.org/x/tools/go/ast/astutil"
	"gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/memfs"
	"gopkg.in/src-d/go-billy.v4/osfs"
)

type Session struct {
	fs          billy.Filesystem
	fset        *token.FileSet
	root        string               // root path
	paths       map[string]*PathInfo // relative path from root -> path info (may include several packages)
	gopathsrc   string
	out         io.Writer
	ParseFilter func(relpath string, file os.FileInfo) bool
}

func NewSession(rootpath string) *Session {
	return &Session{
		fs:        osfs.New("/"),
		gopathsrc: filepath.Join(build.Default.GOPATH, "src"),
		fset:      token.NewFileSet(),
		root:      rootpath,
		paths:     map[string]*PathInfo{},
		out:       os.Stdout,
	}
}

func (s *Session) Run(mutations []Mutator) error {

	rootDir := filepath.Join(s.gopathsrc, s.root)

	var appliers []Applier
	for _, mutation := range mutations {
		appliers = append(appliers, mutation.Apply(s))
	}

	// make list of files by relpath
	files := map[string]map[string]bool{} // full file path -> true

	if err := fsutil.Walk(s.fs, rootDir, func(fs billy.Filesystem, fpath string, finfo os.FileInfo, err error) error {
		if finfo == nil {
			return nil
		}
		if !finfo.IsDir() {
			dir, fname := filepath.Split(fpath)
			reldir, err := filepath.Rel(rootDir, dir)
			if err != nil {
				return err
			}
			relpath := dirToPath(reldir)
			if files[relpath] == nil {
				files[relpath] = map[string]bool{}
			}
			files[relpath][fname] = true
		}
		return nil
	}); err != nil {
		return err
	}

	for i, applier := range appliers {
		if applier.FileFilter != nil {
			var count int
			for relpath := range files {

				count++
				fmt.Fprintf(s.out, "\rFiltering (%d/%d): %d/%d", i+1, len(appliers), count, len(files))

				for fname := range files[relpath] {

					// If FileFilter == true, keep file - don't delete
					if applier.FileFilter(relpath, fname) {
						continue
					}

					// Delete file
					delete(files[relpath], fname)

					// If path info does not exist - not parsed yet, so continue
					if s.paths[relpath] == nil {
						continue
					}

					// If file is in extras, no need to search for it in packages
					if s.paths[relpath].Extras[fname] {
						delete(s.paths[relpath].Extras, fname)
						continue
					}

					// If the file wasn't in extras, search for it in the pacakges. Note pkg.Files key
					// is the full filesystem file path, so we need to split to get the filename.
					for _, pkg := range s.paths[relpath].Packages {
						for fpath := range pkg.Files {
							_, fn := filepath.Split(fpath)
							if fn == fname {
								delete(pkg.Files, fpath)
							}
						}
					}
				}
			}
		}

		if (applier.Apply != nil || applier.Func != nil) && len(s.paths) == 0 {
			// If we haven't parsed yet, parse now.
			if err := s.parse(files, rootDir); err != nil {
				return err
			}
		}

		if applier.Apply != nil {
			var count int
			for relpath, info := range s.paths {
				count++
				fmt.Fprintf(s.out, "\rApplying (%d/%d): %d/%d", i+1, len(appliers), count, len(s.paths))
				for _, pkg := range info.Packages {
					for fpath, file := range pkg.Files {
						fname := s.fset.File(file.Pos()).Name()
						applyFunc := applier.Apply(relpath, fname)
						if applyFunc == nil {
							continue
						}
						result := astutil.Apply(file, applyFunc, nil)
						if result == nil {
							pkg.Files[fpath] = nil
						} else {
							pkg.Files[fpath] = result.(*ast.File)
						}
					}
				}
			}
		}

		if applier.Func != nil {
			applier.Func()
		}
	}

	// If we haven't parsed yet, parse now.
	if len(s.paths) == 0 {
		s.parse(files, rootDir)
	}

	return nil
}

func (s *Session) parse(files map[string]map[string]bool, rootDir string) error {
	var count int
	for relpath := range files {

		count++
		fmt.Fprintf(s.out, "\rParsing: %d/%d", count, len(files))

		dir := filepath.Join(rootDir, relpath)
		path := dirToPath(filepath.Join(s.root, relpath))

		info := &PathInfo{
			Dir:      dir,
			Path:     path,
			Relpath:  relpath,
			Packages: map[string]*ast.Package{},
			Extras:   map[string]bool{},
		}
		s.paths[relpath] = info

		filter := func(file os.FileInfo) bool {
			if !files[relpath][file.Name()] {
				// file has already been filtered
				return false
			}
			if s.ParseFilter != nil && !s.ParseFilter(relpath, file) {
				return false
			}
			return true
		}

		packages, err := parseDir(s.fs, s.fset, dir, filter, parser.ParseComments)
		if err != nil {
			return err
		}

		for _, pkg := range packages {
			for _, f := range pkg.Files {
				astutil.Apply(f, func(c *astutil.Cursor) bool {
					switch n := c.Node().(type) {
					case *ast.CommentMap:
						fmt.Println("CommentMap:", n)
					}
					return true
				}, nil)
			}
		}

		info.Packages = packages

		// build a list of all the parsed files
		gofiles := map[string]bool{}
		for _, p := range packages {
			for fpath := range p.Files {
				_, fname := filepath.Split(fpath)
				gofiles[fname] = true
			}
		}

		// any files in the dir that have not been parsed, add to the extras collection
		for fname := range files[relpath] {
			if !gofiles[fname] {
				info.Extras[fname] = true
			}
		}

	}
	return nil
}

func parseDir(fs billy.Filesystem, fset *token.FileSet, dir string, filter func(os.FileInfo) bool, mode parser.Mode) (pkgs map[string]*ast.Package, first error) {
	list, err := fs.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	pkgs = make(map[string]*ast.Package)
	for _, d := range list {
		if strings.HasSuffix(d.Name(), ".go") && (filter == nil || filter(d)) {
			fpath := filepath.Join(dir, d.Name())
			b, err := readFile(fs, fpath)
			if err != nil {
				return nil, err
			}
			if src, err := parser.ParseFile(fset, fpath, b, mode); err == nil {
				name := src.Name.Name
				pkg, found := pkgs[name]
				if !found {
					pkg = &ast.Package{
						Name:  name,
						Files: make(map[string]*ast.File),
					}
					pkgs[name] = pkg
				}
				pkg.Files[fpath] = src
			} else if first == nil {
				first = err
			}
		}
	}

	return
}

func readFile(fs billy.Filesystem, fpath string) ([]byte, error) {
	f, err := fs.Open(fpath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := &bytes.Buffer{}
	if _, err := io.Copy(buf, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Session) Save(path string) error {
	tempfs := memfs.New()

	var count int
	for relpath, info := range s.paths {

		count++
		fmt.Fprintf(s.out, "\rSaving: %d/%d", count, len(s.paths))

		// go packages
		for _, pkg := range info.Packages {
			for fpath, file := range pkg.Files {
				if file == nil {
					continue
				}
				writeFile := func(to string, file *ast.File) error {
					dir, _ := filepath.Split(to)
					if err := tempfs.MkdirAll(dir, 0777); err != nil {
						return err
					}
					dst, err := tempfs.Create(to)
					if err != nil {
						return err
					}
					defer dst.Close()
					if err := format.Node(dst, s.fset, file); err != nil {
						//return fmt.Errorf("format.Node error in %s: %v", to, err)
					}
					return nil
				}
				_, fname := filepath.Split(fpath)
				if err := writeFile(filepath.Join(relpath, fname), file); err != nil {
					return err
				}
			}
		}
		// extras
		for fname := range info.Extras {
			copyFile := func(to, from string) error {
				fi, err := s.fs.Stat(from)
				if err != nil {
					return err
				}
				dir, _ := filepath.Split(to)
				if err := tempfs.MkdirAll(dir, 0777); err != nil {
					return err
				}
				dst, err := tempfs.OpenFile(to, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fi.Mode())
				if err != nil {
					return err
				}
				defer dst.Close()
				src, err := s.fs.Open(from)
				if err != nil {
					return err
				}
				defer src.Close()
				if _, err := io.Copy(dst, src); err != nil {
					return err
				}
				return nil
			}
			from := filepath.Join(s.gopathsrc, s.root, relpath, fname)
			to := filepath.Join(relpath, fname)
			if err := copyFile(to, from); err != nil {
				return err
			}
		}
	}
	destinationDir := filepath.Join(s.gopathsrc, path)
	if err := removeContents(s.fs, destinationDir); err != nil {
		return err
	}
	if err := fsutil.Copy(s.fs, destinationDir, tempfs, "/"); err != nil {
		return err
	}
	return nil
}

func removeContents(fs billy.Filesystem, dir string) error {
	fis, err := fs.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		fpath := filepath.Join(dir, fi.Name())
		if fi.IsDir() {
			if err := removeContents(fs, fpath); err != nil {
				return err
			}
		} else {
			if err := fs.Remove(fpath); err != nil {
				return err
			}
		}
	}
	return nil
}

type Mutator interface {
	Apply(s *Session) Applier
}

type PathInfo struct {
	Dir      string
	Path     string                  // full go path
	Relpath  string                  // go path relative to root
	Packages map[string]*ast.Package // all named packages in dir - e.g. foo, foo_test, main
	Extras   map[string]bool         // filenames of all files not included in packages (non-go files, filtered go files etc.)
}

type TestSkipper []TestSkip

func (m TestSkipper) Apply(s *Session) Applier {
	return Applier{
		Apply: func(relpath, fname string) func(*astutil.Cursor) bool {
			if !strings.HasSuffix(fname, "_test.go") {
				return nil
			}
			return func(c *astutil.Cursor) bool {
				fd, ok := c.Node().(*ast.FuncDecl)
				if !ok {
					return true
				}
				if !strings.HasPrefix(fd.Name.Name, "Test") {
					return true
				}
				var test TestSkip
				for _, ts := range m {
					if ts.Path == relpath && ts.Name == fd.Name.Name {
						test = ts
						break
					}
				}
				if test == (TestSkip{}) {
					return true
				}

				// check name of testing param (usually t)
				name := fd.Type.Params.List[0].Names[0].Name

				// create the skip statement
				skip := &ast.ExprStmt{
					X: &ast.CallExpr{
						Fun: &ast.SelectorExpr{
							X:   ast.NewIdent(name),
							Sel: ast.NewIdent("Skip"),
						},
						Args: []ast.Expr{&ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(test.Comment)}},
					},
				}

				fd.Body.List = append([]ast.Stmt{skip}, fd.Body.List...)

				return true
			}
		},
	}
}

type TestSkip struct {
	Path, Name, Comment string
}

type Manual func(relpath, fname string) func(c *astutil.Cursor) bool

func (m Manual) Apply(s *Session) Applier {
	return Applier{
		Apply: m,
	}
}

type DeleteNodes func(relpath, fname string, node, parent ast.Node) bool

func (m DeleteNodes) Apply(s *Session) Applier {
	return Applier{
		Apply: func(relpath, fname string) func(c *astutil.Cursor) bool {
			return func(c *astutil.Cursor) bool {
				if m(relpath, fname, c.Node(), c.Parent()) {
					c.Delete()
					return false
				}
				return true
			}
		},
	}
}

type PathReplacer struct {
	Matchers    []string
	Replacement string
	matchers    []*regexp.Regexp
	initialised bool
}

func (m *PathReplacer) init() {
	if m.initialised {
		return
	}
	for _, s := range m.Matchers {
		m.matchers = append(m.matchers, regexp.MustCompile(fmt.Sprintf(`(^|\W)(%s)($|\W)`, regexp.QuoteMeta(s))))
	}
}

func (m *PathReplacer) Apply(s *Session) Applier {
	m.init()
	return Applier{
		Apply: func(relpath, fname string) func(c *astutil.Cursor) bool {
			return func(c *astutil.Cursor) bool {
				if bl, ok := c.Node().(*ast.BasicLit); ok && bl.Kind == token.STRING {
					s, err := strconv.Unquote(bl.Value)
					if err != nil {
						panic(err)
					}
					for _, reg := range m.matchers {
						s = reg.ReplaceAllString(s, m.Replacement)
					}
					if strconv.Quote(s) == bl.Value {
						return false
					}
					c.Replace(&ast.BasicLit{
						ValuePos: bl.ValuePos,
						Kind:     token.STRING,
						Value:    strconv.Quote(s),
					})
				}
				return true
			}
		},
	}
}

type ModifyStrings func(s string) string

func (m ModifyStrings) Apply(s *Session) Applier {
	return Applier{
		Apply: func(relpath, fname string) func(c *astutil.Cursor) bool {
			return func(c *astutil.Cursor) bool {
				if bl, ok := c.Node().(*ast.BasicLit); ok && bl.Kind == token.STRING {
					s, err := strconv.Unquote(bl.Value)
					if err != nil {
						panic(err)
					}
					s = m(s)
					c.Replace(&ast.BasicLit{
						ValuePos: bl.ValuePos,
						Kind:     token.STRING,
						Value:    strconv.Quote(s),
					})
				}
				return true
			}
		},
	}
}

type FilterFiles func(relpath, fname string) bool

func (m FilterFiles) Apply(s *Session) Applier {
	return Applier{
		FileFilter: m,
	}
}

type Applier struct {
	FileFilter func(relpath, fname string) bool
	Apply      func(relpath, fname string) func(*astutil.Cursor) bool
	Func       func()
}

func dirToPath(dir string) string {
	return strings.Trim(filepath.ToSlash(dir), "/")
}

func MatchPath(dir string, specs ...string) bool {
	for _, spec := range specs {
		if strings.HasSuffix(spec, "/**") {
			if strings.HasPrefix(dir, strings.TrimSuffix(spec, "**")) {
				return true
			}
		} else {
			if dir == spec {
				return true
			}
		}
	}
	return false
}
