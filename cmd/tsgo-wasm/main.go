//go:build wasip1

// Command tsgo-wasm builds the TypeScript API server as a WebAssembly reactor
// module (GOOS=wasip1, -buildmode=c-shared) that runs entirely in-process.
//
// Instead of talking to a spawned tsgo process over a pipe, a JS host drives the
// module synchronously:
//
//   - create_session(cwd) once, to build the API session.
//   - get_request_buffer(size) to obtain a shared buffer, write the method bytes
//     followed by the JSON payload into it, then call handle_request(methodLen,
//     payloadLen). The status is the return value (0 ok, 1 error); the response
//     bytes are read from response_ptr()/response_len(), and response_is_binary()
//     reports whether they are raw binary or JSON.
//
// Filesystem access is delegated back to the host via the ts_host.callback /
// ts_host.read_result imports, so the host provides a virtual file system (the
// same callback contract the STDIO server uses). Bundled lib.*.d.ts files are
// embedded in the module itself and never hit the host.
//
// The module is single-threaded: one request runs to completion per call.
package main

import (
	"context"
	"sync"
	"unsafe"

	"github.com/microsoft/typescript-go/internal/api"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/json"
	"github.com/microsoft/typescript-go/internal/vfs/vfstest"
)

func main() {}

// Host imports. The host answers a filesystem callback by stashing the result
// and returning its length; the module then allocates a buffer and asks the
// host to copy the bytes in. Splitting it into two calls lets the result be any
// size while keeping ownership of the destination buffer on the Go side.
//
//go:wasmimport ts_host callback
func hostCallback(namePtr, nameLen, argPtr, argLen uint32) uint32

//go:wasmimport ts_host read_result
func hostReadResult(destPtr uint32)

// allFSCallbacks are the filesystem operations delegated to the host. Anything
// not delegated (and not an embedded lib) resolves against the empty base FS.
var allFSCallbacks = []string{"readFile", "fileExists", "directoryExists", "getAccessibleEntries", "realpath", "writeFile"}

var (
	ctx    = context.Background()
	server *api.InProcessServer

	// reqBuf holds the incoming method + payload for the current request.
	reqBuf []byte

	// respBuf holds the outgoing response until the next request overwrites it;
	// it is kept referenced so the pointer handed to the host stays valid.
	respBuf    []byte
	respPtr    uint32
	respLen    uint32
	respBinary uint32

	// callMu serializes host callbacks so two concurrently-scheduled goroutines
	// cannot interleave the callback/read_result pair and read each other's
	// stashed result. The module is single-threaded, so this only ever blocks a
	// goroutine until the in-flight callback completes.
	callMu sync.Mutex
)

// create_session builds the API session with the given current working
// directory. It must be called once before handle_request.
//
//go:wasmexport create_session
func createSession(cwdPtr, cwdLen uint32) {
	cwd := string(readMem(cwdPtr, cwdLen))
	if cwd == "" {
		cwd = "/"
	}
	base := bundled.WrapFS(vfstest.FromMap(map[string]string{}, true))
	server = api.NewInProcessServer(ctx, &api.InProcessServerOptions{
		FS:                 base,
		Cwd:                cwd,
		DefaultLibraryPath: bundled.LibPath(),
		Callbacks:          allFSCallbacks,
		Conn:               wasmConn{},
	})
}

// get_request_buffer ensures the shared request buffer holds at least size bytes
// and returns a pointer to its start for the host to write the request into.
//
//go:wasmexport get_request_buffer
func getRequestBuffer(size uint32) uint32 {
	if uint32(cap(reqBuf)) < size {
		reqBuf = make([]byte, size)
	}
	reqBuf = reqBuf[:size]
	return bytesPtr(reqBuf)
}

// handle_request dispatches the request currently in the shared buffer: the
// first methodLen bytes are the method name, the next payloadLen bytes are the
// JSON payload. It returns 0 on success and 1 on error; the response is then
// available via response_ptr/response_len/response_is_binary.
//
//go:wasmexport handle_request
func handleRequest(methodLen, payloadLen uint32) uint32 {
	method := string(reqBuf[:methodLen])
	payload := make([]byte, payloadLen)
	copy(payload, reqBuf[methodLen:methodLen+payloadLen])

	response, isBinary, err := server.HandleRequest(ctx, method, json.Value(payload))
	if err != nil {
		setResponse([]byte(err.Error()), false)
		return 1
	}
	setResponse(response, isBinary)
	return 0
}

//go:wasmexport response_ptr
func responsePtr() uint32 { return respPtr }

//go:wasmexport response_len
func responseLen() uint32 { return respLen }

//go:wasmexport response_is_binary
func responseIsBinary() uint32 { return respBinary }

func setResponse(data []byte, isBinary bool) {
	respBuf = data
	respPtr = bytesPtr(respBuf)
	respLen = uint32(len(respBuf))
	if isBinary {
		respBinary = 1
	} else {
		respBinary = 0
	}
}

// wasmConn implements api.Conn, delegating filesystem callbacks to the host and
// treating every other connection operation as a no-op (the module never runs a
// message loop or calls back into the client for anything but the FS).
type wasmConn struct{}

func (wasmConn) Run(ctx context.Context) error                               { return nil }
func (wasmConn) Notify(ctx context.Context, method string, params any) error { return nil }

func (wasmConn) Call(ctx context.Context, method string, params any) (json.Value, error) {
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	name := []byte(method)

	callMu.Lock()
	defer callMu.Unlock()

	resLen := hostCallback(bytesPtr(name), uint32(len(name)), bytesPtr(payload), uint32(len(payload)))
	if resLen == 0 {
		return json.Value(nil), nil
	}
	res := make([]byte, resLen)
	hostReadResult(bytesPtr(res))
	return json.Value(res), nil
}

// readMem returns a view over length bytes of linear memory at ptr.
func readMem(ptr, length uint32) []byte {
	if length == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)
}

// bytesPtr returns the linear-memory offset of b's backing array. b must be kept
// alive by the caller for as long as the host uses the pointer.
func bytesPtr(b []byte) uint32 {
	if len(b) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}
