package mcpsrv

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// noArgs is the input for a tool that takes none. A named empty struct rather
// than an inline one so the derived schema is an object with no properties and
// no additional ones, which is what refuses a caller that invents an argument.
type noArgs struct{}

type listIn struct {
	All bool `json:"all,omitempty" jsonschema:"accepted for compatibility; unified status always includes disabled accounts"`
}

type configGetIn struct {
	Key string `json:"key" jsonschema:"the configuration key to read, for example threshold, strategy or credit.max_auto_spend"`
}

// autoStarts is the sentence a read carries when running it may detach ccdad's
// background process.
//
// Three of the six reads are on ccdad's auto-start list -- list, status and
// which -- because those are the commands a person runs while USING their
// accounts. doctor, config_get and runway are deliberately not: doctor must not
// create the thing it is checking for, and runway answers a question about
// readings already on disk, which a poll started to answer it cannot change. So
// this sentence is false on those three and they do not carry it. "A read-only
// tool has no side effects" is not true of the three that do, and a description
// that implies it is a lie the model repeats to the person running it.
const autoStarts = " Note: like the ccdad command it runs, this may start ccdad's background daemon if none is running."

// readOnly is the annotation set every read tool carries.
//
// The two POINTER fields are set explicitly and that is not tidiness. This
// type mixes plain booleans with pointers whose protocol default is TRUE, so a
// nil pointer is not false -- an author who fills in only the plain fields has
// declared a destructive, open-world tool and the protocol will believe them.
func readOnly() *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		IdempotentHint:  true,
		DestructiveHint: &no,
		OpenWorldHint:   &no,
	}
}

func addReadTools(srv *mcp.Server, e view.Exec) error {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "list",
		Title: "List the managed accounts",
		Description: "Compatibility name for ccdad status. Reports every managed account, its " +
			"quota, the selected strategy and the daemon state. Returns ccdad's own JSON document." + autoStarts,
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, ReadOut, error) {
		argv := []string{"status", "--json"}
		out, isErr := readResult(e, argv...)
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "status",
		Title: "Report which account is live and what the engine is doing",
		Description: "Report which managed account Claude Code is currently authenticated as, what " +
			"ccdad's switching strategy is, and whether the background engine is running. " +
			"Returns ccdad's own JSON document." + autoStarts,
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in noArgs) (*mcp.CallToolResult, ReadOut, error) {
		out, isErr := readResult(e, "status", "--json")
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "which",
		Title: "Say whose account the live login belongs to",
		Description: "Say which of the managed accounts the live Claude Code login belongs to. The " +
			"answer may be that it belongs to none of them, which is a complete answer and " +
			"not a failure: the document says so and the exit code is 5." + autoStarts,
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in noArgs) (*mcp.CallToolResult, ReadOut, error) {
		out, isErr := readResult(e, "which", "--json")
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "doctor",
		Title: "Check this machine's ccdad installation",
		Description: "Check ccdad's installation on this machine -- its store, its credential home, " +
			"its daemon and the parts of Claude Code it writes to -- and report every check " +
			"with its verdict. Returns ccdad's own JSON document. It creates nothing: a " +
			"check that started what it is checking for could never report it missing.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in noArgs) (*mcp.CallToolResult, ReadOut, error) {
		out, isErr := readResult(e, "doctor", "--json")
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "config_get",
		Title: "Read one ccdad configuration key",
		Description: "Read one key from ccdad's configuration and report its value together with " +
			"where that value came from. A key that is not set is a complete answer rather " +
			"than a failure. Returns ccdad's own JSON document.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in configGetIn) (*mcp.CallToolResult, ReadOut, error) {
		out, isErr := readResult(e, "config", "get", in.Key, "--json")
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	// The description spends most of its length on what an empty answer means,
	// because that is the answer a fresh install gives and the one a model is
	// most likely to misreport. "No rate could be measured" arrives as exit 0
	// with a document whose basis says zero readings -- not an error, and not a
	// fleet that burns nothing. A model told only "reports the burn rate" reads
	// the missing figure as zero and tells the person their quota will last
	// forever.
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "runway",
		Title: "Measure how fast the accounts are spending quota, and say when it runs out",
		Description: "Measure how fast ccdad's accounts are spending their quota, using readings " +
			"already recorded on this machine, and report whether the five-hour and weekly " +
			"windows hold at that rate, when each one runs dry, and when paid credits run " +
			"out. It fetches nothing and costs no request against the usage endpoint. " +
			"Rates are percentage points per hour and are reported per window: the two " +
			"windows are different quantities and adding them gives a number with no unit. " +
			"When too little has been recorded to measure a rate, the answer says so and is " +
			"complete rather than failed -- a missing rate is unknown and never zero. " +
			"Returns ccdad's own JSON document.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in noArgs) (*mcp.CallToolResult, ReadOut, error) {
		out, isErr := readResult(e, "runway", "--json")
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	return nil
}
