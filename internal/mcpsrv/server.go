package mcpsrv

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// instructions is what the client is told at initialize.
//
// It is the one place a person's client learns, before any tool is called, that
// this server is holding the keys to their account. Everything in it was
// measured: the credentials file is re-read on the next request because the
// token accessor's cache is cleared by an mtime check, and a detached daemon
// keeps running after the session that started it has gone.
const instructions = "ccdad manages the live Claude Code login on this machine. " +
	"It holds every managed account's OAuth refresh token and it rewrites the credentials " +
	"file Claude Code authenticates with.\n\n" +
	"The switch tool changes which account THIS conversation is billed to, from its very " +
	"next request, with no restart. It asks the person at the keyboard before it does.\n\n" +
	"The daemon tools start and stop a background process that outlives this session.\n\n" +
	"The read tools have one side effect worth knowing: they run the same ccdad commands a " +
	"person would, and some of those start that background process if it is not running."

// New builds the server, registers every tool and installs the class gate.
func New(opts Options) (*mcp.Server, error) {
	if opts.Exec == nil {
		return nil, errors.New("mcpsrv: no executor; every tool runs a ccdad command and there is nothing to run it with")
	}
	if opts.Logger == nil {
		// Not a default: with no logger, a tool registered under an invalid
		// name fails SILENTLY, because the registration error has nowhere else
		// to go. Refusing here makes that impossible rather than unlikely.
		return nil, errors.New("mcpsrv: no logger; without one an invalid tool name registers silently")
	}
	srv := mcp.NewServer(implementation(opts.Version), &mcp.ServerOptions{
		// Explicit and empty. Left nil, the server advertises a logging
		// capability it does not implement, and logging is deprecated in the
		// newer protocol revisions anyway. The tools capability still arrives:
		// the server derives that one from the tools it actually holds.
		Capabilities: &mcp.ServerCapabilities{},
		// STDERR. For a stdio server stdout is the protocol, and one stray line
		// on it ends the session.
		Logger:       opts.Logger,
		Instructions: instructions,
	})
	if err := Register(srv, opts.Exec); err != nil {
		return nil, err
	}
	// AFTER the constructor, so this ends up outermost and runs once per call.
	installGate(srv)
	return srv, nil
}

// Serve runs the server over stdio until stdin reaches EOF or ctx is done.
//
// The transport is a pipe, not a listener: nothing here opens a port.
func Serve(ctx context.Context, srv *mcp.Server) error {
	return srv.Run(ctx, &mcp.StdioTransport{})
}
