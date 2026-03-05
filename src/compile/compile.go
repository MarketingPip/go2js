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

// fakeOsSource is compiled and force-registered as packages["os"].
// It ONLY imports syscall/js so it can be injected as soon as that
// package is available — no dependency on errors/io/time/etc.
const fakeOsSource = `package os

import "syscall/js"

// ── Types ─────────────────────────────────────────────────────────────────────

type FileMode uint32
const (
	ModeDir        FileMode = 1 << (32 - 1 - iota)
	ModeAppend
	ModeExclusive
	ModeTemporary
	ModeSymlink
	ModeDevice
	ModeNamedPipe
	ModeSocket
	ModeSetuid
	ModeSetgid
	ModeCharDevice
	ModeSticky
	ModeIrregular
	ModeType = ModeDir | ModeSymlink | ModeNamedPipe | ModeSocket | ModeDevice | ModeCharDevice | ModeIrregular
	ModePerm FileMode = 0777
)

// PathError records an error and the operation and path that caused it.
type PathError struct {
	Op   string
	Path string
	Err  error
}
func (e *PathError) Error() string {
	return e.Op + " " + e.Path + ": " + e.Err.Error()
}

// ── Sentinel errors ────────────────────────────────────────────────────────────

type errorString struct{ s string }
func (e *errorString) Error() string { return e.s }
func newError(s string) error { return &errorString{s} }

var (
	ErrInvalid    = newError("invalid argument")
	ErrPermission = newError("permission denied")
	ErrExist      = newError("file already exists")
	ErrNotExist   = newError("file does not exist")
	ErrClosed     = newError("file already closed")
)

func IsNotExist(err error) bool {
	if pe, ok := err.(*PathError); ok { err = pe.Err }
	return err == ErrNotExist
}
func IsExist(err error) bool {
	if pe, ok := err.(*PathError); ok { err = pe.Err }
	return err == ErrExist
}
func IsPermission(err error) bool {
	if pe, ok := err.(*PathError); ok { err = pe.Err }
	return err == ErrPermission
}

// ── File ──────────────────────────────────────────────────────────────────────

type File struct {
	fd   int
	name string
}

func (f *File) Name() string { return f.name }

func (f *File) Read(b []byte) (int, error) {
	jsBuf := js.Global().Get("Uint8Array").New(len(b))
	ret := js.Global().Get("fs").Call("readSync", f.fd, jsBuf, 0, len(b), js.Null())
	n := ret.Int()
	if n == 0 {
		return 0, &PathError{"read", f.name, ErrClosed}
	}
	js.InternalObject(b).Call("set", jsBuf.Call("subarray", 0, n))
	return n, nil
}

func (f *File) Write(b []byte) (int, error) {
	jsBuf := js.Global().Get("Uint8Array").New(len(b))
	js.InternalObject(jsBuf).Call("set", js.InternalObject(b))
	ret := js.Global().Get("fs").Call("writeSync", f.fd, jsBuf)
	return ret.Int(), nil
}

func (f *File) WriteString(s string) (int, error) { return f.Write([]byte(s)) }

func (f *File) Close() error {
	js.Global().Get("fs").Call("closeSync", f.fd)
	return nil
}

// ── stdio ─────────────────────────────────────────────────────────────────────

var (
	Stdin  = &File{fd: 0, name: "/dev/stdin"}
	Stdout = &File{fd: 1, name: "/dev/stdout"}
	Stderr = &File{fd: 2, name: "/dev/stderr"}
	Args   = []string{"js"}
)

// ── Top-level file operations ─────────────────────────────────────────────────

// jsFsCallback executes a window.fs callback-based call synchronously by
// blocking a goroutine — the JS event loop still runs so the cb fires.
func openSync(path string, flags int, perm FileMode) (int, error) {
	done := make(chan struct{ fd int; err string }, 1)
	cb := js.MakeFunc(func(this *js.Object, args []*js.Object) interface{} {
		if args[0] != nil && args[0] != js.Undefined {
			done <- struct{ fd int; err string }{0, args[0].Get("message").String()}
		} else {
			done <- struct{ fd int; err string }{args[1].Int(), ""}
		}
		return nil
	})
	js.Global().Get("fs").Call("open", path, flags, int(perm), cb)
	r := <-done
	if r.err != "" {
		return 0, &PathError{"open", path, newError(r.err)}
	}
	return r.fd, nil
}

func ReadFile(name string) ([]byte, error) {
	const O_RDONLY = 0
	fd, err := openSync(name, O_RDONLY, 0)
	if err != nil { return nil, err }

	var out []byte
	buf := make([]byte, 4096)
	jsBuf := js.Global().Get("Uint8Array").New(4096)
	f := &File{fd: fd, name: name}
	for {
		done := make(chan struct{ n int; err string }, 1)
		cb := js.MakeFunc(func(this *js.Object, args []*js.Object) interface{} {
			if args[0] != nil && args[0] != js.Undefined {
				done <- struct{ n int; err string }{0, args[0].Get("message").String()}
			} else {
				done <- struct{ n int; err string }{args[1].Int(), ""}
			}
			return nil
		})
		js.Global().Get("fs").Call("read", f.fd, jsBuf, 0, 4096, js.Null(), cb)
		r := <-done
		if r.err != "" { break }
		if r.n == 0 { break }
		js.InternalObject(buf).Call("set", jsBuf.Call("subarray", 0, r.n))
		out = append(out, buf[:r.n]...)
	}
	f.Close()
	return out, nil
}

func WriteFile(name string, data []byte, perm FileMode) error {
	constants := js.Global().Get("fs").Get("constants")
	flags := constants.Get("O_WRONLY").Int() |
		constants.Get("O_CREAT").Int() |
		constants.Get("O_TRUNC").Int()

	fd, err := openSync(name, flags, perm)
	if err != nil { return err }
	f := &File{fd: fd, name: name}

	jsBuf := js.Global().Get("Uint8Array").New(len(data))
	js.InternalObject(jsBuf).Call("set", js.InternalObject(data))

	done := make(chan string, 1)
	cb := js.MakeFunc(func(this *js.Object, args []*js.Object) interface{} {
		if args[0] != nil && args[0] != js.Undefined {
			done <- args[0].Get("message").String()
		} else {
			done <- ""
		}
		return nil
	})
	js.Global().Get("fs").Call("write", f.fd, jsBuf, 0, len(data), js.Null(), cb)
	if msg := <-done; msg != "" {
		return &PathError{"write", name, newError(msg)}
	}
	f.Close()
	return nil
}

func Open(name string) (*File, error) {
	fd, err := openSync(name, 0 /*O_RDONLY*/, 0)
	if err != nil { return nil, err }
	return &File{fd: fd, name: name}, nil
}

func Create(name string) (*File, error) {
	constants := js.Global().Get("fs").Get("constants")
	flags := constants.Get("O_WRONLY").Int() |
		constants.Get("O_CREAT").Int() |
		constants.Get("O_TRUNC").Int()
	fd, err := openSync(name, flags, 0o666)
	if err != nil { return nil, err }
	return &File{fd: fd, name: name}, nil
}

func Remove(name string) error {
	done := make(chan string, 1)
	cb := js.MakeFunc(func(this *js.Object, args []*js.Object) interface{} {
		if args[0] != nil && args[0] != js.Undefined { done <- args[0].Get("message").String() } else { done <- "" }
		return nil
	})
	js.Global().Get("fs").Call("unlink", name, cb)
	if msg := <-done; msg != "" { return &PathError{"remove", name, newError(msg)} }
	return nil
}

func MkdirAll(path string, perm FileMode) error {
	done := make(chan string, 1)
	cb := js.MakeFunc(func(this *js.Object, args []*js.Object) interface{} {
		if args[0] != nil && args[0] != js.Undefined { done <- args[0].Get("message").String() } else { done <- "" }
		return nil
	})
	js.Global().Get("fs").Call("mkdir", path, int(perm), cb)
	if msg := <-done; msg != "" && msg != "EEXIST" { return &PathError{"mkdirAll", path, newError(msg)} }
	return nil
}

func Getenv(key string) string {
	v := js.Global().Get("process").Get("env").Get(key)
	if v == nil || v == js.Undefined { return "" }
	return v.String()
}

func Exit(code int) {
	js.Global().Get("process").Call("exit", code)
}

func TempDir() string { return "/tmp" }
func Getwd() (string, error) { return "/", nil }
`

func main() {
	packages := make(map[string]*compiler.Archive)
	var pkgsToLoad map[string]struct{}
	fileSet := token.NewFileSet()
	importContext := &compiler.ImportContext{
		Packages: make(map[string]*types.Package),
		Import: func(path string) (*compiler.Archive, error) {
			// ── THE KEY: packages["os"] is pre-registered below, so this
			// branch is hit immediately — no XHR for os.a.js ever fires ──
			if pkg, found := packages[path]; found {
				return pkg, nil
			}
			pkgsToLoad[path] = struct{}{}
			return &compiler.Archive{}, nil
		},
	}

	var output string
	pkgsReceived := 0
	fakeOsInjected := false

	// injectFakeOs compiles fakeOsSource and force-registers it as packages["os"].
	// Must be called AFTER syscall/js is loaded (it's a dependency of fakeOsSource).
	injectFakeOs := func() {
		if fakeOsInjected {
			return
		}
		osFile, parseErr := parser.ParseFile(fileSet, "os.go", []byte(fakeOsSource), 0)
		if parseErr != nil {
			js.Global().Get("console").Call("warn", "[go2js] fake os parse error: "+parseErr.Error())
			return
		}
		// Save/restore pkgsToLoad so compiling fake os doesn't pollute the
		// outer load-set with its own dependencies (syscall/js etc. already loaded)
		savedPkgs := pkgsToLoad
		pkgsToLoad = make(map[string]struct{})

		archive, compErr := compiler.Compile("os", []*ast.File{osFile}, fileSet, importContext, false)

		// Any new deps the fake os needed — merge back so they get fetched
		for dep := range pkgsToLoad {
			savedPkgs[dep] = struct{}{}
		}
		pkgsToLoad = savedPkgs

		if compErr != nil {
			js.Global().Get("console").Call("warn", "[go2js] fake os compile error: "+compErr.Error())
			return
		}
		if regErr := archive.RegisterTypes(importContext.Packages); regErr != nil {
			js.Global().Get("console").Call("warn", "[go2js] fake os RegisterTypes error: "+regErr.Error())
			return
		}

		// ── Force-register — this is the line that matters ─────────────────
		packages["os"] = archive
		fakeOsInjected = true
		js.Global().Get("console").Call("log", "[go2js] fake os package registered ✓")
	}

	var compile func(string, string, func(...interface{}) *js.Object)
	compile = func(code string, baseUrl string, callback func(...interface{}) *js.Object) {
		output = ""
		pkgsToLoad = make(map[string]struct{})

		file, err := parser.ParseFile(fileSet, "prog.go", []byte(code), parser.ParseComments)
		if err != nil {
			if list, ok := err.(scanner.ErrorList); ok {
				for _, entry := range list {
					output += entry.Error() + "\n"
				}
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
				for _, entry := range list {
					output += entry.Error() + "\n"
				}
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
			totalToLoad := len(pkgsToLoad)
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
					if pkgsReceived == totalToLoad {
						// ── Inject fake os now that syscall/js (and friends)
						//    are guaranteed to be in packages ─────────────────
						injectFakeOs()
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
		js.Global.Set("$checkForDeadlock", true)
		callback(nil, jsCode.String())
	}

	js.Global.Set("go2jsCompile", compile)
}
