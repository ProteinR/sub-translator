package main

/*
#cgo CFLAGS: -fobjc-arc -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#include <stdlib.h>
#include "webkit_darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Cocoa работает только на главном потоке процесса: main() должна остаться на нём.
func init() { runtime.LockOSThread() }

const hasNativeWindow = true

type evalResult struct {
	value string
	err   error
}

type webkitEngine struct {
	mu      sync.Mutex
	nextReq atomic.Int64
	pending map[int64]chan evalResult
}

var webkit = &webkitEngine{pending: map[int64]chan evalResult{}}

func defaultEngine() tabEngine { return webkit }

// runNativeWindow открывает окно с панелью и не возвращается до выхода из приложения.
func runNativeWindow(panelURL string) {
	cs := C.CString(panelURL)
	defer C.free(unsafe.Pointer(cs))
	C.loka_run(cs)
}

func withCString(s string, fn func(*C.char)) {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	fn(cs)
}

func (e *webkitEngine) newTab(id int, title string) {
	withCString(title, func(t *C.char) { C.loka_tab_new(C.int(id), t) })
}

func (e *webkitEngine) navigate(id int, url string) {
	withCString(url, func(u *C.char) { C.loka_tab_navigate(C.int(id), u) })
}

func (e *webkitEngine) setTitle(id int, title string) {
	withCString(title, func(t *C.char) { C.loka_tab_title(C.int(id), t) })
}

func quitApp() { C.loka_quit() }

func (e *webkitEngine) selectTab(id int) { C.loka_tab_select(C.int(id)) }
func (e *webkitEngine) closeTab(id int)  { C.loka_tab_close(C.int(id)) }
func (e *webkitEngine) activate()        { C.loka_activate() }
func (e *webkitEngine) setBusy(busy bool) {
	v := 0
	if busy {
		v = 1
	}
	C.loka_set_busy(C.int(v))
}

func (e *webkitEngine) eval(ctx context.Context, id int, body, argsJSON string) (string, error) {
	req := e.nextReq.Add(1)
	ch := make(chan evalResult, 1)
	e.mu.Lock()
	e.pending[req] = ch
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.pending, req)
		e.mu.Unlock()
	}()

	cb, ca := C.CString(body), C.CString(argsJSON)
	C.loka_tab_eval(C.int(id), cb, ca, C.longlong(req))
	C.free(unsafe.Pointer(cb))
	C.free(unsafe.Pointer(ca))

	select {
	case r := <-ch:
		return r.value, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

//export lokaEvalDone
func lokaEvalDone(req C.longlong, result *C.char, errMsg *C.char) {
	r := evalResult{}
	if errMsg != nil {
		r.err = errors.New(C.GoString(errMsg))
	} else if result != nil {
		r.value = C.GoString(result)
	}
	webkit.mu.Lock()
	ch := webkit.pending[int64(req)]
	webkit.mu.Unlock()
	if ch != nil {
		select {
		case ch <- r:
		default:
		}
	}
}
