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
	All bool `json:"all,omitempty" jsonschema:"include the disabled accounts, which are held out of rotation and hidden by default"`
}

type configGetIn struct {
	Key string `json:"key" jsonschema:"the configuration key to read, for example threshold, strategy or credit.max_auto_spend"`
}

// autoStarts is the sentence a read carries when running it may detach ccdad's
// background process.
//
// Three of the five reads are on ccdad's auto-start list -- list, status and
// which -- because those are the commands a person runs while USING their
// accounts. doctor and config_get are deliberately not, doctor because it must
// not create the thing it is checking for, so this sentence is false on them
// and they do not carry it. "A read-only tool has no side effects" is not true
// of the three that do, and a description that implies it is a lie the model
// repeats to the person running it.
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
		Description: "List every account ccdad manages, with how much of each account's quota is " +
			"left and when it resets. Returns ccdad's own JSON document." + autoStarts,
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, ReadOut, error) {
		argv := []string{"list"}
		if in.All {
			argv = append(argv, "--all")
		}
		argv = append(argv, "--json")
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

	return nil
}
