package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// freshRootExec's whole job is running one command through a tree of its own,
// so the first thing worth pinning is that it actually does: a real command
// runs, and its stdout, stderr and exit code come back exactly as ExecuteWith
// reports them.
func TestFreshRootExecRunsACommandAndReportsWhatItWroteAndExited(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	parent := &cobra.Command{}
	parent.SetContext(context.Background())
	exec := freshRootExec(parent)

	code, stdout, _ := exec([]string{"list"})
	if code != int(ExitOK) {
		t.Fatalf("code = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "a@example.com") {
		t.Fatalf("stdout = %q, does not mention the seeded account", stdout)
	}
}

// The negative half: an exit code the caller can act on, and the failure
// text on the stderr half rather than mixed into stdout.
func TestFreshRootExecReportsANonZeroExitAndKeepsStderrSeparate(t *testing.T) {
	isolate(t)

	parent := &cobra.Command{}
	parent.SetContext(context.Background())
	exec := freshRootExec(parent)

	code, stdout, stderr := exec([]string{"status", "extra"})
	if code != int(ExitUsage) {
		t.Fatalf("code = %d, want %d for an unexpected positional argument", code, ExitUsage)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing: the usage error belongs on stderr", stdout)
	}
	if stderr == "" {
		t.Fatal("stderr is empty; nothing says why the command was refused")
	}
}

// The refusal this seam exists to enforce: argv is never nil, so a fresh root
// never falls back to cobra's os.Args[1:] default -- which for a long-lived
// process (an MCP server, a re-entrant TUI keypress) would re-run the parent
// process's own command line on every call.
func TestFreshRootExecNeverFallsBackToTheProcesssArgv(t *testing.T) {
	isolate(t)
	stubTTYs(t, false, false)
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	os.Args = []string{saved[0], "list", "--json"}

	parent := &cobra.Command{}
	parent.SetContext(context.Background())
	exec := freshRootExec(parent)

	// A bare invocation -- what argv looks like when nothing follows the
	// command name -- must be the empty command, and not what os.Args says.
	code, stdout, _ := exec(nil)
	if strings.Contains(stdout, "schemaVersion") {
		t.Fatalf("stdout = %q: freshRootExec ran the process's own argv instead of the empty one it was given", stdout)
	}
	if code != int(ExitUsage) {
		t.Fatalf("code = %d, want %d for a bare invocation with no terminal", code, ExitUsage)
	}
}

// parent.Context() is what a cancelled caller -- an MCP client that goes away
// mid-call -- has to reach the fresh root through, since nothing else connects
// the two.
func TestFreshRootExecPropagatesTheParentsContext(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	type markerKey struct{}
	parent := &cobra.Command{}
	parent.SetContext(context.WithValue(context.Background(), markerKey{}, "from-the-parent"))

	savedEngine := newEngine
	t.Cleanup(func() { newEngine = savedEngine })
	var gotCtx context.Context
	newEngine = func() *daemon.Engine {
		e := daemon.NewEngine()
		e.AccessToken = func(context.Context, string) (string, error) { return "AT", nil }
		e.FetchUsage = func(ctx context.Context, _ string) (*usage.Snapshot, error) {
			gotCtx = ctx
			return nil, errors.New("stop here; only the context matters")
		}
		return e
	}

	exec := freshRootExec(parent)
	exec([]string{"list", "--refresh"})

	if gotCtx == nil {
		t.Fatal("the engine was never asked, so context propagation went unverified")
	}
	if got, _ := gotCtx.Value(markerKey{}).(string); got != "from-the-parent" {
		t.Error("the fresh root's context does not carry a value set on the parent's")
	}
}
