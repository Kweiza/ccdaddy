package mcpsrv

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolKind is the security class of one tool.
type toolKind int

const (
	classRead       toolKind = iota + 1 // answers a question and changes nothing ccdad owns
	classStore                          // writes ccdad's own account file
	classCredential                     // rewrites the live Claude Code login
	classDaemon                         // starts, stops or reports on the background process
)

// toolClass is the verdict for every tool this server may run, and the refusal
// for every tool it may not.
//
// It is complete: all fifteen entries are written together, because the
// classification IS the boundary and splitting it across commits splits the
// review of it. Eight ccdad verbs are deliberately absent -- add, add-token,
// run, export, import, uninstall, setup-path and bootstrap -- and their absence
// is the enforcement rather than a note: a handler registered under any of
// those names is refused by the middleware before it runs. add and add-token
// need a terminal, open a browser or read a secret from one, and block for
// minutes; run replaces the process; export and import move refresh tokens off
// and onto the machine through text a model can read; uninstall deletes the
// thing holding the user's logins; setup-path edits shell startup files; and
// bootstrap imports a secret document as a container entrypoint concern.
var toolClass = map[string]toolKind{
	"list":       classRead,
	"status":     classRead,
	"which":      classRead,
	"doctor":     classRead,
	"config_get": classRead,

	"enable":  classStore,
	"disable": classStore,
	"alias":   classStore,
	"move":    classStore,
	"primary": classStore,

	"switch": classCredential,

	"daemon_start":   classDaemon,
	"daemon_stop":    classDaemon,
	"daemon_restart": classDaemon,
	"daemon_status":  classDaemon,
}

func classOf(name string) (toolKind, bool) {
	k, ok := toolClass[name]
	return k, ok
}

// toolError is a refusal the MODEL can read.
//
// The Go error is nil on purpose. A non-nil error here becomes a JSON-RPC
// protocol error, which the client surfaces as a transport failure rather than
// as a tool result -- the model never sees the text and cannot correct itself
// from it.
func toolError(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}},
	}
}

// installGate adds the class check as RECEIVING middleware, and it must be
// added after the server is constructed.
//
// Two properties depend on that placement, and both were measured. The
// constructor installs middleware of its own, and AddReceivingMiddleware wraps
// whatever is current -- so one added afterwards ends up outermost. And a gate
// in receiving middleware runs EXACTLY ONCE per tools/call on the wire, while a
// tool handler can run TWICE: once returning a request for input, once with the
// answer filled in. A gate written inside a handler therefore fires twice, and
// so does any side effect the handler performs before its confirm branch.
func installGate(srv *mcp.Server) {
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			call, ok := req.(*mcp.CallToolRequest)
			if !ok {
				return next(ctx, method, req)
			}
			// The NAME and nothing else. This runs BEFORE the schema
			// validation, so the arguments here are raw untrusted JSON: a gate
			// that decoded them into a typed value and ignored the error would
			// be deciding on a zero-valued account name.
			if _, known := classOf(call.Params.Name); !known {
				return toolError(
					"ccdad has no gate verdict for the tool %q and will not run it. "+
						"Every ccdad MCP tool must be classified read, store-mutating, "+
						"credential-mutating or daemon-controlling.", call.Params.Name), nil
			}
			return next(ctx, method, req)
		}
	})
}
