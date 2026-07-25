package api

import (
	"context"

	"github.com/microsoft/typescript-go/internal/json"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
	"github.com/microsoft/typescript-go/internal/project"
	"github.com/microsoft/typescript-go/internal/vfs"
)

// InProcessServer runs an API session in-process, without any transport.
//
// It is the transport-less analogue of StdioServer: instead of reading framed
// messages off a pipe, callers dispatch one request at a time synchronously via
// HandleRequest. Filesystem operations are delegated to the supplied Conn (for
// example, a WebAssembly host bridge that answers reads from an in-memory file
// system), matching the callback protocol used by the STDIO server.
type InProcessServer struct {
	session *Session
}

// InProcessServerOptions configures an InProcessServer.
type InProcessServerOptions struct {
	// FS is the base filesystem. Bundled library paths should already be
	// redirected to the embedded FS (e.g. via bundled.WrapFS).
	FS vfs.FS
	// Cwd is the current working directory. Required.
	Cwd string
	// DefaultLibraryPath is the directory under which the bundled lib.*.d.ts
	// files live.
	DefaultLibraryPath string
	// Callbacks names the filesystem operations delegated to the client via
	// Conn.Call. Empty means the base FS is used directly with no delegation.
	Callbacks []string
	// Conn receives filesystem callbacks. Required when Callbacks is non-empty.
	Conn Conn
}

// NewInProcessServer creates a new in-process API server. The provided context
// is used as the session's background context and for filesystem callbacks.
func NewInProcessServer(ctx context.Context, options *InProcessServerOptions) *InProcessServer {
	if options.Cwd == "" {
		panic("InProcessServerOptions.Cwd is required")
	}

	fs := options.FS

	// Wrap the base FS with callbackFS if callbacks are requested.
	var callbackFS *callbackFS
	if len(options.Callbacks) > 0 {
		callbackFS = newCallbackFS(fs, options.Callbacks)
		fs = callbackFS
	}

	projectSession := project.NewSession(&project.SessionInit{
		BackgroundCtx: ctx,
		FS:            fs,
		Options: &project.SessionOptions{
			CurrentDirectory:   options.Cwd,
			DefaultLibraryPath: options.DefaultLibraryPath,
			PositionEncoding:   lsproto.PositionEncodingKindUTF8,
			LoggingEnabled:     false,
		},
	})

	session := NewSession(projectSession, &SessionOptions{
		UseBinaryResponses: true,
	})

	if callbackFS != nil {
		callbackFS.SetConnection(ctx, options.Conn)
	}

	return &InProcessServer{session: session}
}

// HandleRequest dispatches a single API request synchronously and returns its
// encoded response. When isBinary is true the response is raw binary (e.g.
// encoded node data); otherwise it is JSON. Filesystem callbacks made while
// handling the request are routed through the configured Conn.
func (s *InProcessServer) HandleRequest(ctx context.Context, method string, params json.Value) (response []byte, isBinary bool, err error) {
	result, err := s.session.HandleRequest(ctx, method, params)
	if err != nil {
		return nil, false, err
	}
	if raw, ok := result.(RawBinary); ok {
		return []byte(raw), true, nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, false, err
	}
	return encoded, false, nil
}

// Close releases the underlying session's resources.
func (s *InProcessServer) Close() {
	s.session.Close()
}
