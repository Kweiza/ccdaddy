package mcpsrv

import (
	"context"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// storeOnly is the sentence all five store tools carry.
//
// It is on every one of them because it is true of every one of them, and the
// repetition is the point: this server also holds a tool that DOES rewrite the
// live login, and the two classes are one word apart in a sentence a person
// might type. A model that has been told once, at the top of the session, that
// ccdad rewrites credentials will otherwise carry that sentence onto whichever
// ccdad tool it is looking at.
//
// What each verb costs the ENGINE is different, and that half is written per
// tool below rather than shared -- on `alias` it is not the engine at all.
const storeOnly = " This writes ccdad's own account file; the live Claude Code login is untouched."

// storeMutator is the annotation set the five store verbs carry.
//
// DestructiveHint is TRUE, and not because any of these deletes an account --
// none of them does. The hint's other reading is "performs only additive
// updates", and that is false for a verb that overwrites the flag, the label or
// the ordinal it finds. The two errors are not symmetric: a wrong `true` costs
// a confirmation a client did not have to ask for, and a wrong `false` is a
// client that never asks before something is overwritten.
//
// IdempotentHint is true, and it is the same fact ccdad's exit 3 states from
// the other side: calling any of these twice with the same arguments leaves the
// same world, and the second call says so rather than doing it again.
//
// Both POINTER fields are set explicitly, for the reason read.go's readOnly()
// gives: a nil pointer here is not false, it is the protocol's default of true.
func storeMutator() *mcp.ToolAnnotations {
	yes, no := true, false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		IdempotentHint:  true,
		DestructiveHint: &yes,
		OpenWorldHint:   &no,
	}
}

type accountIn struct {
	Account string `json:"account" jsonschema:"the account to enable or disable: a display index, an alias, an email address, or a uuid prefix of at least eight characters, as ccdad list shows them"`
}

type aliasIn struct {
	Account string `json:"account" jsonschema:"the account whose alias is being set or cleared: a display index, an alias, an email address, or a uuid prefix of at least eight characters"`
	Alias   string `json:"alias,omitempty" jsonschema:"the handle to give it: a-z, 0-9, '.', '_' and '-', never purely numeric and never starting with '-'. Omit it and pass clear to remove the alias the account already has"`
	Clear   bool   `json:"clear,omitempty" jsonschema:"remove the alias this account already has, instead of setting one"`
}

// moveIn takes a typed integer, and primaryIn a typed boolean, rather than the
// strings the command line spells them as.
//
// The conversion back to a command-line word is one line in each handler, and
// what it buys is that a position of "second" or a primary of "maybe" is
// refused by the schema BEFORE the handler runs -- with a message naming the
// argument, from a validator the model already knows how to read. Taking a
// string and parsing it here would move that refusal after the fact and give it
// a second spelling.
type moveIn struct {
	Account  string `json:"account" jsonschema:"the account to move: a display index, an alias, an email address, or a uuid prefix of at least eight characters"`
	Position int    `json:"position" jsonschema:"the 1-based display position to put it at. A position past the end means last"`
}

type primaryIn struct {
	Account string `json:"account" jsonschema:"the account whose primary flag is being set: a display index, an alias, an email address, or a uuid prefix of at least eight characters"`
	// No omitempty, and that is the field doing the work. An optional boolean
	// defaults to false, so a caller that forgot to say which way it meant
	// would be read as having asked for `off` -- and `off` is a real state this
	// tool can put an account into. Required, the same call is refused.
	Primary bool `json:"primary" jsonschema:"true ranks this credit-metered seat with the subscription accounts and lifts the max_auto_spend ceiling from it; false puts it back to being a last resort under that ceiling"`
}

func addStoreTools(srv *mcp.Server, e view.Exec) error {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "enable",
		Title: "Return an account to automatic rotation",
		Description: "Return one managed account to ccdad's automatic rotation, so the switching engine " +
			"may choose it again. Nothing switches at this moment: the engine reaches for the " +
			"account on a later tick, and only if its ranking picks it." + storeOnly,
		Annotations: storeMutator(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in accountIn) (*mcp.CallToolResult, ActionOut, error) {
		out, isErr := actionResult(e, "enable", in.Account)
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "disable",
		Title: "Hold an account out of automatic rotation",
		Description: "Hold one managed account out of ccdad's automatic rotation. It is a policy for the " +
			"engine rather than a lock: a switch that names the account explicitly still " +
			"activates it." + storeOnly + " Disabling the account that is live right now leaves it " +
			"live: what changes is that nothing will switch back to it once something switches away.",
		Annotations: storeMutator(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in accountIn) (*mcp.CallToolResult, ActionOut, error) {
		out, isErr := actionResult(e, "disable", in.Account)
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "alias",
		Title: "Give an account a short handle, or clear the one it has",
		Description: "Give one managed account a short unique handle, or pass clear to remove the one it " +
			"has. The handle is lowercased and trimmed on the way in, and the stored form is " +
			"what every later reference has to use. An alias is one of the ways an account is " +
			"named, so changing or clearing one changes what a later call naming that handle " +
			"resolves to." + storeOnly,
		Annotations: storeMutator(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in aliasIn) (*mcp.CallToolResult, ActionOut, error) {
		// The argument vector is assembled the way a person would type it, and
		// every refusal in it is left to the command tree: an alias given
		// TOGETHER with clear, and a call that gives neither, are already usage
		// errors there. A second copy of those two rules here is the copy that
		// drifts, and this package owns no logic of its own by design.
		//
		// An empty alias is passed as if it were absent rather than as an empty
		// word, because the two are the same value in Go and the refusal for
		// the absent one -- "alias needs an alias to set, or --clear" -- is the
		// true sentence for both.
		argv := []string{"alias", in.Account}
		if in.Alias != "" {
			argv = append(argv, in.Alias)
		}
		if in.Clear {
			argv = append(argv, "--clear")
		}
		out, isErr := actionResult(e, argv...)
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "move",
		Title: "Put an account at a display position",
		Description: "Put one managed account at a 1-based display position and renumber the rest. The " +
			"position is a display ordinal rather than a key: every account between the old " +
			"and the new one changes number, so a later call should name accounts by alias or " +
			"uuid instead. This order also settles ties -- the engine's ranking sort is " +
			"stable, so between two accounts that rank equally it decides." + storeOnly,
		Annotations: storeMutator(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in moveIn) (*mcp.CallToolResult, ActionOut, error) {
		out, isErr := actionResult(e, "move", in.Account, strconv.Itoa(in.Position))
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "primary",
		Title: "Rank a credit-metered seat with the accounts whose quota is paid for",
		Description: "Turn the primary flag on or off for one managed account. A credit-metered seat is " +
			"normally a last resort: the engine reaches it only once every other account is " +
			"spent, and only under the max_auto_spend ceiling. Turning primary ON ranks it " +
			"alongside the subscription accounts and STOPS that ceiling applying to it, so the " +
			"engine may spend that account's credits automatically. The flag is read only for " +
			"an account ccdad has classified as credit-metered, and it is stored either " +
			"way." + storeOnly,
		Annotations: storeMutator(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in primaryIn) (*mcp.CallToolResult, ActionOut, error) {
		// on|off rather than a boolean, because that is the grammar the command
		// takes: `ccdad primary` deliberately refuses strconv.ParseBool's "1"
		// and "t", so "true" is not a word this verb accepts.
		verb := "off"
		if in.Primary {
			verb = "on"
		}
		out, isErr := actionResult(e, "primary", in.Account, verb)
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	return nil
}
