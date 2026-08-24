package mcpsrv

import (
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// ServerName is the one spelling of ccdad's MCP server name.
//
// It is the name in mcp.Implementation, the key in Claude Code's mcpServers
// object, the name in the plugin's own server config, and the middle segment of
// every tool name a client derives. Claude Code de-duplicates plugin and
// connector servers by ENDPOINT -- the command plus its arguments -- rather
// than by name, so this constant paired with the bare command string "ccdad" is
// what makes a direct registration and a plugin registration collapse into ONE
// server instead of running two full tool sets side by side.
const ServerName = "ccdad"

// Options is everything `ccdad mcp` hands the server.
type Options struct {
	// Exec runs one ccdad command line through a FRESH command tree and reports
	// its exit code, its stdout and its stderr. Required. It is a function
	// value rather than a direct call so this package never imports the one
	// that builds the tree.
	Exec view.Exec

	// Version is what the initialize handshake reports. Production passes the
	// build version; a test passes a fixed constant, because a version that
	// moves every release reddens any assertion written against it.
	Version string

	// Logger receives the SDK's own diagnostics, and it must write to STDERR.
	// For a stdio server stdout IS the protocol. It is also required rather
	// than optional: with no logger, a tool registered under an invalid name
	// fails SILENTLY, because the error has nowhere else to go.
	Logger *slog.Logger
}

// implementation is the identity the server reports at initialize.
func implementation(version string) *mcp.Implementation {
	return &mcp.Implementation{Name: ServerName, Version: version}
}
