package cli

import (
	"bytes"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// freshRootExec builds a view.Exec that runs one ccdad command through a fresh
// cobra root and reports what it wrote and what it exited with. It is the
// only implementation of this seam in the tree -- the TUI's mutating keys and
// every MCP tool handler both consume it rather than each declaring their own
// copy, because the three refusals below are the kind of thing two
// independent copies drift on silently.
//
// parent is the command whose Context() the fresh root inherits, so a
// cancelled session (an MCP client that goes away mid-call) propagates into
// whatever the fresh root spawns.
//
// The three refusals, stated once here rather than repeated by every future
// caller of this seam: never omit SetArgs; never omit SetOut/SetErr --
// cmd.OutOrStdout() defaults to os.Stdout, and there are 27
// OutOrStdout/ErrOrStderr call sites that will take it; never call
// cli.Execute() in a handler -- it reads os.Args, calls ignoreSIGPIPE(), and
// passes os.Stdout to enableConsoleVT, all three of which are process-wide.
func freshRootExec(parent *cobra.Command) view.Exec {
	return func(argv []string) (code int, stdout, stderr string) {
		root := NewRootCmd()
		root.SetContext(parent.Context())
		var out, errOut, top bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&errOut)
		// NEVER nil, NEVER omitted: cobra falls back to os.Args[1:] when
		// c.args is nil, which for a long-lived process re-runs its own argv
		// on every call -- a nested `ccdad mcp` on the same stdio, or a
		// re-entrant TUI, per keypress or per tool call.
		root.SetArgs(append([]string{}, argv...))
		code = int(ExecuteWith(root, &top))
		return code, out.String(), errOut.String() + top.String()
	}
}
