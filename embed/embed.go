// Package embed implements all file embedding logic for github.com/mjibson/esc.
package embed

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	"golang.org/x/tools/imports"
)

// Config contains all information needed to run esc.
type Config struct {
	// OutputFile is the file name to write output, else stdout.
	OutputFile string
	// Package name for the generated file.
	Package string
	// Prefix is stripped from filenames.
	Prefix string
	// Ignore is the regexp for files we should ignore (for example `\.DS_Store`).
	Ignore string
	// Include is the regexp for files to include. If provided, only files that
	// match will be included.
	Include string
	// ModTime is the Unix timestamp to override as modification time for all files.
	ModTime string
	// Private, if true, causes autogenerated functions to be unexported.
	Private bool
	// NoCompression, if true, stores the files without compression.
	NoCompression bool
	// Invocation, if set, is added to the invocation string in the generated template.
	Invocation string

	// Files is the list of files or directories to embed.
	Files []string
}

var modTime *int64

var tmpl = template.Must(template.New("").Parse(fileTemplate))

type templateParams struct {
	Invocation     string
	PackageName    string
	FunctionPrefix string
	Files          []*_escFile
	Dirs           []*_escDir
}

type _escFile struct {
	Name       string
	BaseName   string
	Data       []byte
	Local      string
	ModTime    int64
	Compressed string

	fileinfo os.FileInfo
}

type _escDir struct {
	Name           string
	BaseName       string
	Local          string
	ChildFileNames []string
}

// Run executes a Config.
func Run(conf *Config, out io.Writer) error {
	var err error
	if conf.ModTime != "" {
		i, err := strconv.ParseInt(conf.ModTime, 10, 64)
		if err != nil {
			return fmt.Errorf("modtime must be an integer: %v", err)
		}
		modTime = &i
	}

	alreadyPrepared := make(map[string]bool, 10)
	escFiles := make([]*_escFile, 0, 10)
	prefix := filepath.ToSlash(conf.Prefix)
	var ignoreRegexp *regexp.Regexp
	if conf.Ignore != "" {
		ignoreRegexp, err = regexp.Compile(conf.Ignore)
		if err != nil {
			return err
		}
	}
	var includeRegexp *regexp.Regexp
	if conf.Include != "" {
		includeRegexp, err = regexp.Compile(conf.Include)
		if err != nil {
			return err
		}
	}
	gzipLevel := gzip.BestCompression
	if conf.NoCompression {
		gzipLevel = gzip.NoCompression
	}
	directories := make([]*_escDir, 0, 10)
	for _, base := range conf.Files {
		files := []string{base}
		for len(files) > 0 {
			fname := files[0]
			files = files[1:]
			if ignoreRegexp != nil && ignoreRegexp.MatchString(fname) {
				continue
			}
			f, err := os.Open(fname)
			if err != nil {
				return err
			}
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			fpath := filepath.ToSlash(fname)
			n := canonicFileName(fname, prefix)
			if fi.IsDir() {
				fis, err := f.Readdir(0)
				if err != nil {
					return err
				}
				dir := &_escDir{
					Name:           n,
					BaseName:       path.Base(n),
					Local:          fpath,
					ChildFileNames: make([]string, 0, len(fis)),
				}
				for _, fi := range fis {
					childFName := filepath.Join(fname, fi.Name())
					files = append(files, childFName)
					if ignoreRegexp != nil && ignoreRegexp.MatchString(childFName) {
						continue
					}
					if includeRegexp == nil || includeRegexp.MatchString(childFName) {
						dir.ChildFileNames = append(dir.ChildFileNames, canonicFileName(filepath.Join(fname, fi.Name()), prefix))
					}
				}
				sort.Strings(dir.ChildFileNames)
				directories = append(directories, dir)
			} else if includeRegexp == nil || includeRegexp.MatchString(fname) {
				b, err := ioutil.ReadAll(f)
				if err != nil {
					return errors.Wrap(err, "readAll return err")
				}
				if alreadyPrepared[n] {
					return fmt.Errorf("%s, %s: duplicate Name after prefix removal", n, fpath)
				}
				escFile := &_escFile{
					Name:     n,
					BaseName: path.Base(n),
					Data:     b,
					Local:    fpath,
					fileinfo: fi,
					ModTime:  fi.ModTime().Unix(),
				}
				if modTime != nil {
					escFile.ModTime = *modTime
				}
				if err := escFile.fillCompressed(gzipLevel); err != nil {
					return err
				}
				escFiles = append(escFiles, escFile)
				alreadyPrepared[n] = true
			}
			f.Close()
		}
	}

	sort.Slice(escFiles, func(i, j int) bool { return strings.Compare(escFiles[i].Name, escFiles[j].Name) == -1 })
	sort.Slice(directories, func(i, j int) bool { return strings.Compare(directories[i].Name, directories[j].Name) == -1 })

	functionPrefix := ""
	if conf.Private {
		functionPrefix = "_esc"
	}

	buf := bytes.NewBuffer(nil)
	tmpl.Execute(buf, templateParams{
		Invocation:     conf.Invocation,
		PackageName:    conf.Package,
		FunctionPrefix: functionPrefix,
		Files:          escFiles,
		Dirs:           directories,
	})

	fakeOutFileName := "static.go"
	if conf.OutputFile != "" {
		fakeOutFileName = conf.OutputFile
	}

	data, err := imports.Process(fakeOutFileName, buf.Bytes(), nil)
	if err != nil {
		return errors.Wrap(err, "imports.Process return error")
	}

	fmt.Fprint(out, string(data))

	return nil
}

func canonicFileName(fname, prefix string) string {
	fpath := filepath.ToSlash(fname)
	return path.Join("/", strings.TrimPrefix(fpath, prefix))
}

func (f *_escFile) fillCompressed(gzipLevel int) error {
	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, gzipLevel)
	if err != nil {
		return err
	}
	if _, err := gw.Write(f.Data); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	var b bytes.Buffer
	b64 := base64.NewEncoder(base64.StdEncoding, &b)
	b64.Write(buf.Bytes())
	b64.Close()
	res := "\n"
	chunk := make([]byte, 80)
	for n, _ := b.Read(chunk); n > 0; n, _ = b.Read(chunk) {
		res += string(chunk[0:n]) + "\n"
	}

	f.Compressed = res
	return nil
}

const (
	fileTemplate = `// Code generated by "esc{{with .Invocation}} {{.}}{{end}}"; DO NOT EDIT.

package {{.PackageName}}

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"sync"
	"time"
)

type _escLocalFS struct{}

var _escLocal _escLocalFS

type _escStaticFS struct{}

var _escStatic _escStaticFS

type _escDirectory struct {
	fs   http.FileSystem
	name string
}

type _escFile struct {
	compressed string
	size       int64
	modtime    int64
	local      string
	isDir      bool

	once sync.Once
	data []byte
	name string
}

func (_escLocalFS) Open(name string) (http.File, error) {
	f, present := _escData[path.Clean(name)]
	if !present {
		return nil, os.ErrNotExist
	}
	return os.Open(f.local)
}

func (_escStaticFS) prepare(name string) (*_escFile, error) {
	f, present := _escData[path.Clean(name)]
	if !present {
		return nil, os.ErrNotExist
	}
	var err error
	f.once.Do(func() {
		f.name = path.Base(name)
		if f.size == 0 {
			return
		}
		var gr *gzip.Reader
		b64 := base64.NewDecoder(base64.StdEncoding, bytes.NewBufferString(f.compressed))
		gr, err = gzip.NewReader(b64)
		if err != nil {
			return
		}
		f.data, err = ioutil.ReadAll(gr)
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (fs _escStaticFS) Open(name string) (http.File, error) {
	f, err := fs.prepare(name)
	if err != nil {
		return nil, err
	}
	return f.File()
}

func (dir _escDirectory) Open(name string) (http.File, error) {
	return dir.fs.Open(dir.name + name)
}

func (f *_escFile) File() (http.File, error) {
	type httpFile struct {
		*bytes.Reader
		*_escFile
	}
	return &httpFile{
		Reader:   bytes.NewReader(f.data),
		_escFile: f,
	}, nil
}

func (f *_escFile) Close() error {
	return nil
}

func (f *_escFile) Readdir(count int) ([]os.FileInfo, error) {
	if !f.isDir {
		return nil, fmt.Errorf(" escFile.Readdir: '%s' is not directory", f.name)
	}

	fis, ok := _escDirs[f.local]
	if !ok {
		return nil, fmt.Errorf(" escFile.Readdir: '%s' is directory, but we have no info about content of this dir, local=%s", f.name, f.local)
	}
	limit := count
	if count <= 0 || limit > len(fis) {
		limit = len(fis)
	}

	if len(fis) == 0 && count > 0 {
		return nil, io.EOF
	}

	return fis[0:limit], nil
}


func (f *_escFile) Stat() (os.FileInfo, error) {
	return f, nil
}

func (f *_escFile) Name() string {
	return f.name
}

func (f *_escFile) Size() int64 {
	return f.size
}

func (f *_escFile) Mode() os.FileMode {
	return 0
}

func (f *_escFile) ModTime() time.Time {
	return time.Unix(f.modtime, 0)
}

func (f *_escFile) IsDir() bool {
	return f.isDir
}

func (f *_escFile) Sys() interface{} {
	return f
}

// {{.FunctionPrefix}}FS returns a http.Filesystem for the embedded assets. If useLocal is true,
// the filesystem's contents are instead used.
func {{.FunctionPrefix}}FS(useLocal bool) http.FileSystem {
	if useLocal {
		return _escLocal
	}
	return _escStatic
}

// {{.FunctionPrefix}}Dir returns a http.Filesystem for the embedded assets on a given prefix dir.
// If useLocal is true, the filesystem's contents are instead used.
func {{.FunctionPrefix}}Dir(useLocal bool, name string) http.FileSystem {
	if useLocal {
		return _escDirectory{fs: _escLocal, name: name}
	}
	return _escDirectory{fs: _escStatic, name: name}
}

// {{.FunctionPrefix}}FSByte returns the named file from the embedded assets. If useLocal is
// true, the filesystem's contents are instead used.
func {{.FunctionPrefix}}FSByte(useLocal bool, name string) ([]byte, error) {
	if useLocal {
		f, err := _escLocal.Open(name)
		if err != nil {
			return nil, err
		}
		b, err := ioutil.ReadAll(f)
		_ = f.Close()
		return b, err
	}
	f, err := _escStatic.prepare(name)
	if err != nil {
		return nil, err
	}
	return f.data, nil
}

// {{.FunctionPrefix}}FSMustByte is the same as {{.FunctionPrefix}}FSByte, but panics if name is not present.
func {{.FunctionPrefix}}FSMustByte(useLocal bool, name string) []byte {
	b, err := {{.FunctionPrefix}}FSByte(useLocal, name)
	if err != nil {
		panic(err)
	}
	return b
}

// {{.FunctionPrefix}}FSString is the string version of {{.FunctionPrefix}}FSByte.
func {{.FunctionPrefix}}FSString(useLocal bool, name string) (string, error) {
	b, err := {{.FunctionPrefix}}FSByte(useLocal, name)
	return string(b), err
}

// {{.FunctionPrefix}}FSMustString is the string version of {{.FunctionPrefix}}FSMustByte.
func {{.FunctionPrefix}}FSMustString(useLocal bool, name string) string {
	return string({{.FunctionPrefix}}FSMustByte(useLocal, name))
}

var _escData = map[string]*_escFile{
{{ range .Files }}
	"{{ .Name }}": {
		name:    "{{ .BaseName }}",
		local:   "{{ .Local }}",
		size:    {{ .Data | len  }},
		modtime: {{ .ModTime }},
		compressed: ` + "`" + `{{ .Compressed }}` + "`" + `,
	},
{{ end -}}
{{ range .Dirs }}
	"{{ .Name }}": {
		name:  "{{ .BaseName }}",
		local: ` + "`" + `{{ .Local }}` + "`" + `,
		isDir: true,
	},
  {{ end }}
}

var _escDirs = map[string][]os.FileInfo{
  {{ range .Dirs }}
	"{{ .Local }}": {
		{{ range .ChildFileNames -}}
		_escData["{{.}}"],
		{{ end }}
	},
  {{ end }}
}

`
)
