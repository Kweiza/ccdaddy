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
// copy, because the four refusals below are the kind of thing two
// independent copies drift on silently.
//
// parent is the command whose Context() the fresh root inherits, so a
// cancelled session (an MCP client that goes away mid-call) propagates into
// whatever the fresh root spawns.
//
// The four refusals, stated once here rather than repeated by every future
// caller of this seam: never omit SetArgs; never omit SetOut/SetErr --
// cmd.OutOrStdout() defaults to os.Stdout, and most of this package's output
// goes through OutOrStdout/ErrOrStderr, which will take it; never call
// cli.Execute() in a handler -- it reads os.Args, calls ignoreSIGPIPE(), and
// passes os.Stdout to enableConsoleVT, all three of which are process-wide;
// never let a rendering under this root resolve colour -- the annotation is the
// exclusion, because an environment that exports CLICOLOR_FORCE colours a
// bytes.Buffer and NO_COLOR does not take it back off one.
func freshRootExec(parent *cobra.Command) view.Exec {
	return func(argv []string) (code int, stdout, stderr string) {
		root := NewRootCmd()
		root.SetContext(parent.Context())
		// The fourth refusal, and the only structural one: never let this seam
		// resolve colour. Its output is a tool result and a keypress's echo,
		// never a terminal, and the annotation makes every renderer under this
		// root take the None theme regardless of writer and regardless of
		// environment. Deciding it from the writer would not work -- the buffer
		// below is exactly the destination colorprofile will happily colour
		// when CLICOLOR_FORCE is set, because it gates NO_COLOR on the
		// destination being a terminal and raises the profile when it is told
		// one is being forced. Measured: NO_COLOR=1 CLICOLOR_FORCE=1 into a
		// bytes.Buffer resolves ANSI256 and writes "\x1b[38;5;173mX\x1b[m".
		root.Annotations = map[string]string{colourlessRoot: "1"}
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
