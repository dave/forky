package forky

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
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"honnef.co/go/tools/ssa"

	"github.com/dave/services/fsutil"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/loader"
	"gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/helper/mount"
	"gopkg.in/src-d/go-billy.v4/helper/polyfill"
	"gopkg.in/src-d/go-billy.v4/memfs"
	"gopkg.in/src-d/go-billy.v4/osfs"
)

type Session struct {
	fs                  billy.Filesystem
	gorootfs            billy.Filesystem
	fset                *token.FileSet
	source, destination string               // root path of source and destination
	paths               map[string]*PathInfo // relative path from root -> path info (may include several packages)
	gopathsrc           string
	out                 io.Writer
	ParseFilter         func(relpath string, file os.FileInfo) bool
	prog                *loader.Program
	ssa                 *ssa.Program
}

func NewSession(source, destination string) *Session {
	return &Session{
		fs:          osfs.New("/"),
		gorootfs:    polyfill.New(mount.New(memfs.New(), "/goroot", osfs.New(build.Default.GOROOT))),
		gopathsrc:   filepath.Join(build.Default.GOPATH, "src"),
		fset:        token.NewFileSet(),
		source:      source,
		destination: destination,
		paths:       map[string]*PathInfo{},
		out:         os.Stdout,
	}
}

func (s *Session) Run(mutations []Mutator) error {

	rootDir := filepath.Join(s.gopathsrc, s.source)

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
					for _, info := range s.paths[relpath].Packages {
						for fn := range info.Files {
							if fn == fname {
								delete(info.Files, fname)
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
			for relpath, pathInfo := range s.paths {
				count++
				fmt.Fprintf(s.out, "\rApplying (%d/%d): %d/%d", i+1, len(appliers), count, len(s.paths))
				for _, pkgInfo := range pathInfo.Packages {
					for fname, file := range pkgInfo.Files {
						applyFunc := applier.Apply(relpath, fname)
						if applyFunc == nil {
							continue
						}
						result := astutil.Apply(file, applyFunc, nil)
						if result == nil {
							pkgInfo.Files[fname] = nil
						} else {
							pkgInfo.Files[fname] = result.(*ast.File)
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
		path := dirToPath(filepath.Join(s.source, relpath))

		info := &PathInfo{
			Dir:      dir,
			Path:     path,
			Relpath:  relpath,
			Packages: map[string]*PackageInfo{},
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

		astpackages, err := parseDir(s.fs, s.fset, dir, filter, parser.ParseComments)
		if err != nil {
			return err
		}

		var name string
		packages := map[string]*PackageInfo{} // package name -> file name -> ast file
		var hasFiles bool
		for pkgname, pkg := range astpackages {
			packages[pkgname] = &PackageInfo{Name: pkgname, Files: map[string]*ast.File{}}
			if strings.HasSuffix(pkgname, "_test") {
				if name == "" {
					name = pkgname // only set name to x_test if it doesn't already have a value
				}
			} else if pkgname == "main" {
				if name == "" || strings.HasSuffix(pkgname, "_test") {
					name = pkgname // only set name to main if it doesn't already have a value, or is a test package
				}
			} else {
				name = pkgname
			}

			for fpath, file := range pkg.Files {
				hasFiles = true
				_, fname := filepath.Split(fpath)
				packages[pkgname].Files[fname] = file
			}
		}
		if name == "" && hasFiles {
			panic("no name for " + relpath)
		}
		if name != "" {
			info.Default = packages[name]
		}

		// Manipulating the AST breaks "lossy" comments - e.g. comments not attached to a node. They
		// end up being rendered in the wrong place which breaks things. I don't think there's any other
		// option right now except deleting them all.
		for _, pkgInfo := range packages {
			for _, f := range pkgInfo.Files {
				comments := map[*ast.CommentGroup]bool{}
				ast.Inspect(f, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.CommentGroup:
						comments[n] = true
					}
					return true
				})
				// delete all comments that don't occur in the inspect
				var fc []*ast.CommentGroup
				for _, cg := range f.Comments {
					if comments[cg] {
						fc = append(fc, cg)
						continue
					}
					for _, c := range cg.List {
						// don't delete build tags (they are at the start of the file, so won't get broken
						// by manipulating AST
						if strings.HasPrefix(c.Text, "// +build ") {
							fc = append(fc, cg)
							break
						}
					}
				}
				f.Comments = fc
			}
		}

		info.Packages = packages

		// build a list of all the parsed files
		gofiles := map[string]bool{}
		for _, files := range packages {
			for fname := range files.Files {
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

// load the program and scan types
func (s *Session) load() {
	// save all files to a memfs
	gopathfs := memfs.New()
	var count int
	for relpath, info := range s.paths {
		fmt.Fprintf(s.out, "\rScanning: %d/%d", count+1, len(s.paths))
		count++
		for _, pkg := range info.Packages {
			for fname, file := range pkg.Files {

				if file == nil {
					continue
				}

				rootrelfpath := filepath.Join("gopath", "src", s.destination, relpath, fname)

				buf := &bytes.Buffer{}
				if err := format.Node(buf, s.fset, file); err != nil {
					panic(fmt.Errorf("format.Node error in %s: %v", rootrelfpath, err))
				}

				if err := fsutil.WriteFile(gopathfs, rootrelfpath, 0666, buf); err != nil {
					panic(err)
				}
			}
		}
	}

	bc := buildContext(s.gorootfs, gopathfs, s.destination)
	lc := loader.Config{
		ParserMode: parser.ParseComments,
		Fset:       s.fset,
		Build:      bc,
		Cwd:        "/",
	}
	for relpath, pathInfo := range s.paths {
		if len(pathInfo.Packages) == 0 {
			continue
		}
		lc.Import(path.Join(s.destination, relpath))
	}
	p, err := lc.Load()
	if err != nil {
		panic(err)
	}
	s.prog = p
	for pkg, info := range p.AllPackages {
		relpath := strings.TrimPrefix(pkg.Path(), s.destination+"/")
		if s.paths[relpath] == nil || s.paths[relpath].Packages[pkg.Name()] == nil {
			// only update packages that exist in s.paths (in infos we also have std lib etc).
			continue
		}
		files := map[string]*ast.File{}
		for _, f := range info.Files {
			_, fname := filepath.Split(s.fset.File(f.Pos()).Name())
			files[fname] = f
		}
		s.paths[relpath].Packages[pkg.Name()].Files = files
		s.paths[relpath].Packages[pkg.Name()].Info = info
	}
}

// analyze created the ssa program and performs pointer analysis
func (s *Session) analyze() {
	/*
		s.ssa = ssautil.CreateProgram(s.prog, 0)

		prog := ssautil.CreateProgram(l.session.prog, 0)
		for _, p := range prog.AllPackages() {
			pkg := s.packageFromPath(p.Pkg.Path())
			if pkg == nil {
				continue
			}
			p.Build()
			pkg.ssa = p
		}
		l.session.ssa = prog
	*/
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

func (s *Session) Rel(path string) (rel string, found bool) {
	if s.destination == "" {
		return path, true
	}
	if strings.HasPrefix(path, s.destination+"/") {
		return strings.TrimPrefix(path, s.destination+"/"), true
	}
	return "", false
}

func (s *Session) Save() error {
	tempfs := memfs.New()

	var count int
	for relpath, pathInfo := range s.paths {

		count++
		fmt.Fprintf(s.out, "\rSaving: %d/%d", count, len(s.paths))

		// go packages
		for _, pkgInfo := range pathInfo.Packages {
			for fname, file := range pkgInfo.Files {
				if file == nil {
					continue
				}
				relfpath := filepath.Join(relpath, fname)

				buf := &bytes.Buffer{}
				if err := format.Node(buf, s.fset, file); err != nil {
					return fmt.Errorf("format.Node error in %s: %v", relfpath, err)
				}

				if err := fsutil.WriteFile(tempfs, relfpath, 0666, buf); err != nil {
					return err
				}
			}
		}
		// extras
		for fname := range pathInfo.Extras {
			from := filepath.Join(s.gopathsrc, s.source, relpath, fname)
			to := filepath.Join(relpath, fname)
			if err := fsutil.Copy(tempfs, to, s.fs, from); err != nil {
				return err
			}
		}
	}
	destinationDir := filepath.Join(s.gopathsrc, s.destination)
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
	Default  *PackageInfo            // default package (e.g. not x_test or main)
	Packages map[string]*PackageInfo // all named packages in dir - e.g. foo, foo_test, main: package name -> package info
	Extras   map[string]bool         // filenames of all files not included in packages (non-go files, filtered go files etc.)
}

type PackageInfo struct {
	Name  string
	Files map[string]*ast.File // file name -> ast file
	Info  *loader.PackageInfo
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
