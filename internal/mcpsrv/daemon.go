package mcpsrv

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// outlivesTheSession is the sentence the three daemon tools that act carry.
//
// The process this group starts is DETACHED: it keeps polling, keeps ranking
// and keeps rewriting the live login after the conversation that started it has
// ended. That is the whole point of it and it is also the one fact a model
// cannot infer from a verb called "start", so it is said on every tool that
// leaves one running.
const outlivesTheSession = " The daemon is a detached background process: it keeps running, and keeps " +
	"switching the live login, after this session has ended."

// daemonControl is the annotation set for a daemon verb that ACTS.
//
// destructive says whether this verb can end a process that is running, and it
// is the argument for taking these one at a time rather than sharing one
// helper's answer: `start` only ever adds a process, while `stop` and `restart`
// end one that may be mid-tick.
//
// idempotent is the other axis and it does NOT follow the exit code. `start`
// and `stop` answer exit 3 the second time and leave the same world. `restart`
// answers 0 every time and leaves a DIFFERENT process each time -- same end
// state, new pid, one tick's worth of work abandoned -- so it is the one verb
// here that a client must not quietly retry.
//
// Both POINTER fields are set explicitly, for the reason read.go's readOnly()
// gives: a nil pointer here is not false, it is the protocol's default of true.
//
// OpenWorldHint is false on all four, and the daemon's own network traffic is
// not the counter-argument it looks like. What this tool interacts with is the
// singleton lock and the pid file on this machine; the polling belongs to the
// process it starts -- the same process three of the five READ tools already
// start on their own, where the hint is false for the same reason.
func daemonControl(destructive, idempotent bool) *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		IdempotentHint:  idempotent,
		DestructiveHint: &destructive,
		OpenWorldHint:   &no,
	}
}

func addDaemonTools(srv *mcp.Server, e view.Exec) error {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "daemon_start",
		Title: "Start ccdad's background engine",
		Description: "Start ccdad's background daemon, the process that polls each account's usage and " +
			"switches the live login on its own." + outlivesTheSession + " It reports nothing " +
			"changed if one is already running, and refuses to start if another ccdad store's " +
			"engine is already driving Claude Code's credential home -- two engines on one " +
			"login undo each other's switches.",
		Annotations: daemonControl(false, true),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in noArgs) (*mcp.CallToolResult, ActionOut, error) {
		out, isErr := actionResult(e, "daemon", "start")
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "daemon_stop",
		Title: "Stop ccdad's background engine",
		Description: "Ask ccdad's background daemon to finish the tick it is in and shut down, and wait " +
			"until it has. It reports nothing changed if none was running. It never escalates " +
			"to a kill on a daemon that answered: a tick cut short mid-swap abandons Claude " +
			"Code's lock directories. One answer to read carefully -- a daemon that holds the " +
			"lock while listening for nothing comes back as a negative answer rather than an " +
			"error, and that daemon is STILL RUNNING.",
		Annotations: daemonControl(true, true),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in noArgs) (*mcp.CallToolResult, ActionOut, error) {
		out, isErr := actionResult(e, "daemon", "stop")
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "daemon_restart",
		Title: "Replace the running engine with a fresh one",
		Description: "Stop any running ccdad daemon, wait for its lock to clear, and start one. This is " +
			"not stop-then-start: the new process must not reach for the lock while the old one " +
			"still holds it, so the wait between them is part of the verb. Its contract is the " +
			"END state -- success means a daemon is running now, whether or not one was " +
			"before." + outlivesTheSession + " Calling it twice is not free: it replaces a live " +
			"process each time.",
		Annotations: daemonControl(true, false),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in noArgs) (*mcp.CallToolResult, ActionOut, error) {
		out, isErr := actionResult(e, "daemon", "restart")
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "daemon_status",
		Title: "Say whether ccdad's background engine is running",
		Description: "Say whether ccdad's background daemon is running, and what it last reported. It " +
			"is a probe rather than a dashboard: \"not running\" is a complete answer and not a " +
			"failure, and it is reported as one. Distinguish it from the third answer, which is " +
			"that ccdad could not TELL -- that one is a failure, and the document says why. For " +
			"what the engine is deciding, read the status tool instead. It starts nothing: a " +
			"probe that started what it probes could never report it missing.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in noArgs) (*mcp.CallToolResult, ReadOut, error) {
		out, isErr := readResult(e, "daemon", "status", "--json")
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	return nil
}
