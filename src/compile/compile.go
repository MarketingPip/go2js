package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"

	"github.com/gopherjs/gopherjs/compiler"
	"github.com/gopherjs/gopherjs/js"
	"honnef.co/go/js/xhr"
)

// ---------------------------------------------------------------------------
// Fake "os" package source — delegates everything to window.VFS via syscall/js
// Must mirror the real os API surface that user code can reference.
// ---------------------------------------------------------------------------
const fakeOsSource = `package os

import (
	"errors"
	"io"
	"syscall/js"
	"time"
)

// ── Core types ──────────────────────────────────────────────────────────────

type FileMode uint32
type Signal interface{ String() string; Signal() }

type FileInfo interface {
	Name() string
	Size() int64
	Mode() FileMode
	ModTime() time.Time
	IsDir() bool
	Sys() interface{}
}

type DirEntry interface {
	Name() string
	IsDir() bool
	Type() FileMode
	Info() (FileInfo, error)
}

type PathError struct {
	Op   string
	Path string
	Err  error
}
func (e *PathError) Error() string { return e.Op + " " + e.Path + ": " + e.Err.Error() }
func (e *PathError) Unwrap() error { return e.Err }

// ── Sentinel errors ──────────────────────────────────────────────────────────

var (
	ErrNotExist   = errors.New("file does not exist")
	ErrExist      = errors.New("file already exists")
	ErrPermission = errors.New("permission denied")
	ErrInvalid    = errors.New("invalid argument")
)

func IsNotExist(err error) bool {
	var pe *PathError
	if errors.As(err, &pe) { err = pe.Err }
	return err == ErrNotExist
}
func IsExist(err error)      bool { return err == ErrExist }
func IsPermission(err error) bool { return err == ErrPermission }

// ── Stdio / Args / Env ────────────────────────────────────────────────────────

var (
	Stdin  = &File{name: "<stdin>",  fd: 0}
	Stdout = &File{name: "<stdout>", fd: 1}
	Stderr = &File{name: "<stderr>", fd: 2}
	Args   = []string{"prog"}
)

func Exit(code int) { js.Global().Get("process").Call("exit", code) }

func Getenv(key string) string {
	v := js.Global().Get("process").Get("env").Get(key)
	if v.IsUndefined() || v.IsNull() { return "" }
	return v.String()
}
func Setenv(key, val string) error {
	js.Global().Get("process").Get("env").Set(key, val)
	return nil
}
func Environ() []string { return nil }

// ── File type ────────────────────────────────────────────────────────────────

type File struct {
	name string
	fd   int
	pos  int64
}

func (f *File) Name() string { return f.name }

func (f *File) Read(p []byte) (int, error) {
	vfs := js.Global().Get("VFS")
	if vfs.IsUndefined() { return 0, io.EOF }
	res := vfs.Call("read", f.fd, len(p))
	s := res.String()
	if len(s) == 0 { return 0, io.EOF }
	n := copy(p, []byte(s))
	f.pos += int64(n)
	return n, nil
}

func (f *File) Write(p []byte) (int, error) {
	// Route stdout/stderr to console
	if f.fd == 1 { js.Global().Get("console").Call("log", string(p)); return len(p), nil }
	if f.fd == 2 { js.Global().Get("console").Call("error", string(p)); return len(p), nil }
	js.Global().Get("VFS").Call("write", f.fd, string(p))
	return len(p), nil
}

func (f *File) WriteString(s string) (int, error) { return f.Write([]byte(s)) }

func (f *File) Close() error {
	if f.fd > 2 { js.Global().Get("VFS").Call("close", f.fd) }
	return nil
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	js.Global().Get("VFS").Call("seek", f.fd, offset, whence)
	return 0, nil
}

func (f *File) Stat() (FileInfo, error) { return Stat(f.name) }

// ── Top-level file ops ────────────────────────────────────────────────────────

func ReadFile(name string) ([]byte, error) {
	vfs := js.Global().Get("VFS")
	if vfs.IsUndefined() { return nil, &PathError{"readFile", name, ErrNotExist} }
	result := vfs.Call("readFile", name)
	return []byte(result.String()), nil
}

func WriteFile(name string, data []byte, perm FileMode) error {
	js.Global().Get("VFS").Call("writeFile", name, string(data))
	return nil
}

func Open(name string) (*File, error) {
	vfs := js.Global().Get("VFS")
	if vfs.IsUndefined() { return nil, &PathError{"open", name, ErrNotExist} }
	fd := vfs.Call("open", name).Int()
	return &File{name: name, fd: fd}, nil
}

func Create(name string) (*File, error) {
	vfs := js.Global().Get("VFS")
	if vfs.IsUndefined() { return nil, &PathError{"create", name, ErrPermission} }
	fd := vfs.Call("create", name).Int()
	return &File{name: name, fd: fd}, nil
}

func OpenFile(name string, flag int, perm FileMode) (*File, error) {
	if flag&1 != 0 { return Create(name) }
	return Open(name)
}

// ── Directory ops ─────────────────────────────────────────────────────────────

type fileDirEntry struct{ fi fileStatInfo }
func (d fileDirEntry) Name() string               { return d.fi.name }
func (d fileDirEntry) IsDir() bool                { return d.fi.isDir }
func (d fileDirEntry) Type() FileMode             { return d.fi.mode }
func (d fileDirEntry) Info() (FileInfo, error)    { return d.fi, nil }

type fileStatInfo struct {
	name  string
	size  int64
	mode  FileMode
	isDir bool
	mtime time.Time
}
func (fi fileStatInfo) Name() string       { return fi.name }
func (fi fileStatInfo) Size() int64        { return fi.size }
func (fi fileStatInfo) Mode() FileMode     { return fi.mode }
func (fi fileStatInfo) ModTime() time.Time { return fi.mtime }
func (fi fileStatInfo) IsDir() bool        { return fi.isDir }
func (fi fileStatInfo) Sys() interface{}   { return nil }

func Stat(name string) (FileInfo, error) {
	vfs := js.Global().Get("VFS")
	if vfs.IsUndefined() { return nil, &PathError{"stat", name, ErrNotExist} }
	st := vfs.Call("stat", name)
	return fileStatInfo{
		name:  st.Get("name").String(),
		size:  int64(st.Get("size").Int()),
		mode:  FileMode(st.Get("mode").Int()),
		isDir: st.Get("isDirectory").Bool(),
		mtime: time.Unix(0, int64(st.Get("mtimeMs").Int())*int64(time.Millisecond)),
	}, nil
}
func Lstat(name string) (FileInfo, error) { return Stat(name) }

func ReadDir(name string) ([]DirEntry, error) {
	vfs := js.Global().Get("VFS")
	if vfs.IsUndefined() { return nil, &PathError{"readDir", name, ErrNotExist} }
	arr := vfs.Call("readDir", name)
	out := make([]DirEntry, arr.Length())
	for i := range out {
		out[i] = fileDirEntry{fileStatInfo{name: arr.Index(i).String()}}
	}
	return out, nil
}

func MkdirAll(path string, perm FileMode) error {
	js.Global().Get("VFS").Call("mkdirAll", path)
	return nil
}
func Mkdir(name string, perm FileMode) error   { return MkdirAll(name, perm) }
func Remove(name string) error                 { js.Global().Get("VFS").Call("remove", name); return nil }
func RemoveAll(path string) error              { return Remove(path) }
func Rename(oldpath, newpath string) error     { js.Global().Get("VFS").Call("rename", oldpath, newpath); return nil }
func TempDir() string                         { return "/tmp" }
func UserHomeDir() (string, error)            { return "/home", nil }
func Getwd() (string, error)                  { return "/", nil }
func Chdir(dir string) error                  { return nil }
`

func main() {
	var output string
	packages := make(map[string]*compiler.Archive)
	var pkgsToLoad map[string]struct{}
	fileSet := token.NewFileSet()

	importContext := &compiler.ImportContext{
		Packages: make(map[string]*types.Package),
		Import: func(path string) (*compiler.Archive, error) {
			if pkg, found := packages[path]; found {
				return pkg, nil
			}
			pkgsToLoad[path] = struct{}{}
			return &compiler.Archive{}, nil
		},
	}

	// ── Pre-inject the fake "os" package ──────────────────────────────────────
	// Compile it once at startup so it's always in packages["os"] before any
	// user code triggers an XHR fetch for the real one.
	injectFakeOs := func() {
		osFile, err := parser.ParseFile(fileSet, "os.go", []byte(fakeOsSource), 0)
		if err != nil {
			js.Global().Get("console").Call("warn", "fake os parse error: "+err.Error())
			return
		}
		// Temporarily clear pkgsToLoad so the compilation of fake os itself
		// doesn't pollute the outer load state.
		savedPkgsToLoad := pkgsToLoad
		pkgsToLoad = make(map[string]struct{})
		archive, err := compiler.Compile("os", []*ast.File{osFile}, fileSet, importContext, false)
		pkgsToLoad = savedPkgsToLoad
		if err != nil {
			js.Global().Get("console").Call("warn", "fake os compile error: "+err.Error())
			return
		}
		packages["os"] = archive
		if err := archive.RegisterTypes(importContext.Packages); err != nil {
			js.Global().Get("console").Call("warn", "fake os RegisterTypes error: "+err.Error())
		}
	}
	// NOTE: injectFakeOs() itself depends on "errors", "io", "syscall/js", "time"
	// which need to be loaded first. Call it lazily on first compile instead.
	fakeOsInjected := false

	pkgsReceived := 0
	var compile func(string, string, func(...interface{}) *js.Object)
	compile = func(code string, baseUrl string, callback func(...interface{}) *js.Object) {
		output = ""
		pkgsToLoad = make(map[string]struct{})

		// Inject fake os once the stdlib (errors, io, time, syscall/js) is loaded
		if !fakeOsInjected && len(packages) > 0 {
			injectFakeOs()
			fakeOsInjected = true
		}

		file, err := parser.ParseFile(fileSet, "prog.go", []byte(code), parser.ParseComments)
		if err != nil {
			if list, ok := err.(scanner.ErrorList); ok {
				for _, entry := range list { output += entry.Error() + "\n" }
				callback(output, nil)
				return
			}
			callback("syntax error", nil)
			return
		}

		mainPkg, err := compiler.Compile("main", []*ast.File{file}, fileSet, importContext, false)
		packages["main"] = mainPkg
		if err != nil && len(pkgsToLoad) == 0 {
			if list, ok := err.(compiler.ErrorList); ok {
				for _, entry := range list { output += entry.Error() + "\n" }
				callback(output, nil)
				return
			}
			callback("compile error", nil)
			return
		}

		var allPkgs []*compiler.Archive
		if len(pkgsToLoad) == 0 {
			allPkgs, _ = compiler.ImportDependencies(mainPkg, importContext.Import)
		}
		if len(pkgsToLoad) != 0 {
			pkgsReceived = 0
			for path := range pkgsToLoad {
				req := xhr.NewRequest("GET", baseUrl+"/pkg/"+path+".a.js")
				req.ResponseType = xhr.ArrayBuffer
				go func(path string) {
					err := req.Send(nil)
					if err != nil || req.Status != 200 {
						callback(`failed to load package "`+path+`"`, nil)
						return
					}
					data := js.Global.Get("Uint8Array").New(req.Response).Interface().([]byte)
					packages[path], err = compiler.ReadArchive(path+".a", bytes.NewReader(data))
					if err != nil {
						callback(err.Error(), nil)
						return
					}
					if err := packages[path].RegisterTypes(importContext.Packages); err != nil {
						callback(err.Error(), nil)
						return
					}
					pkgsReceived++
					if pkgsReceived == len(pkgsToLoad) {
						// After loading stdlib, inject fake os if not done yet
						if !fakeOsInjected {
							injectFakeOs()
							fakeOsInjected = true
						}
						compile(code, baseUrl, callback)
					}
				}(path)
			}
			return
		}

		jsCode := bytes.NewBuffer(nil)
		jsCode.WriteString("try{\n")
		compiler.WriteProgramCode(allPkgs, &compiler.SourceMapFilter{Writer: jsCode}, "1.19.13")
		jsCode.WriteString("} catch (err) {\nconsole.error(err.message);\n}\n")

		// ── Strategy 1 safety net: JS-side $packages["os"] patch ──────────────
		// Appended INSIDE the try block so $packages is in scope.
		// This is a belt-and-suspenders patch in case type compat isn't 100%.
		jsCode.WriteString(`
;(function() {
  var osPkg = $packages["os"];
  if (!osPkg || !window.VFS) return;
  var _origReadFile = osPkg.ReadFile;
  osPkg.ReadFile = function(name) {
    try {
      var s = window.VFS.readFile(name);
      var enc = new TextEncoder().encode(s);
      return [enc, $ifaceNil];
    } catch(e) {
      return [new Uint8Array(0), osPkg.NewPathError ? 
        osPkg.NewPathError("readFile", name, e.message) : e];
    }
  };
  var _origWriteFile = osPkg.WriteFile;
  osPkg.WriteFile = function(name, data, perm) {
    try { window.VFS.writeFile(name, new TextDecoder().decode(data)); return $ifaceNil; }
    catch(e) { return e; }
  };
})();
`)
		js.Global.Set("$checkForDeadlock", true)
		callback(nil, jsCode.String())
	}

	js.Global.Set("go2jsCompile", compile)
}
