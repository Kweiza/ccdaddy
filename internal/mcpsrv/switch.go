package mcpsrv

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// EnvSwitchWithoutElicitation is the environment half of the permission
// config.toml spells as mcp_switch_without_elicitation.
//
// It exists because the client is what launches `ccdad mcp`, so its own
// environment is the one place an operator can grant this per client rather
// than per machine. Set on the process, it decides -- INCLUDING when it is set
// to false, which is how a machine that granted the permission in config.toml
// takes it back for one client without editing the file.
const EnvSwitchWithoutElicitation = "CCDAD_MCP_SWITCH_WITHOUT_ELICITATION"

// confirmID is the id the confirm travels under, from the input request the
// handler returns to the response the client hands back. It is a label this
// server chooses; the client only has to echo it.
const confirmID = "confirm_switch"

type switchIn struct {
	Account string `json:"account" jsonschema:"the account to make live: a display index, an alias, an email address, or a uuid prefix of at least eight characters, as ccdad status shows them"`
}

// credentialMutator is the annotation set for the one tool in this server that
// rewrites the live Claude Code login.
//
// IdempotentHint is true and it is not a contradiction: switching to the
// account that is already live is ccdad's exit 3, and it rewrites nothing.
//
// OpenWorldHint is false, and the token refresh is the reason that answer is
// worth stating rather than assuming: `ccdad switch` may reach Anthropic's
// OAuth endpoint to freshen a stored login before installing it. One known
// endpoint belonging to the same service the credential is for is not the open
// world the hint is about; a tool that searched the web would be.
//
// Both POINTER fields are set explicitly, for the reason read.go's readOnly()
// gives: a nil pointer here is not false, it is the protocol's default of true.
func credentialMutator() *mcp.ToolAnnotations {
	yes, no := true, false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		IdempotentHint:  true,
		DestructiveHint: &yes,
		OpenWorldHint:   &no,
	}
}

// allowedWithoutAsking answers whether the person at the keyboard has ALREADY
// allowed this server to switch on a client that cannot carry a question.
//
// It is a package var so a test can answer it without a config file on disk;
// production never reassigns it.
var allowedWithoutAsking = readSwitchPermission

// readSwitchPermission reads that permission from the two places a MODEL cannot
// write through this server.
//
// That is the whole design, and it is the second one: the first put the
// permission in the tool call itself, refused a `confirm: true` argument as an
// accident guard, and fell back to requiring a literal string -- which the
// refusal message printed back, so one refused call handed the caller
// everything it needed to make the next one succeed. A permission that lives in
// a file on disk or in the server process's own environment has no such path:
// the model reaches this server through tool arguments and through nothing
// else.
//
// It is not a claim that a model can never obtain it. A model holding a shell
// on this machine can write the file -- and can also run `ccdad switch` at that
// shell and never come near this server, which is why the shell is not the
// boundary this is drawn against.
//
// The config file is read here rather than through the executor on purpose.
// `ccdad config get` would answer the same question, but its answer arrives as
// a JSON document, and this package deliberately keeps no Go struct mirroring
// any ccdad payload -- exec.go's ReadOut says why. config.Load is the SAME
// authority the command tree reads, one call earlier, rather than a second one.
func readSwitchPermission() bool {
	if raw, set := os.LookupEnv(EnvSwitchWithoutElicitation); set {
		// A value ParseBool cannot read is not a grant. It does not vanish
		// either: the refusal below names the variable, so a typo surfaces as a
		// refusal pointing at it rather than as a setting that does nothing.
		allowed, err := strconv.ParseBool(raw)
		return err == nil && allowed
	}
	cfg, err := config.Load()
	if err != nil {
		// A config file that exists and cannot be read is not a grant. ccdad's
		// own loader refuses to substitute the defaults for one, for the
		// weaker stake of an engine tuning value; here the stake is the login.
		//
		// Measured: this branch cannot currently be told apart from its own
		// absence, because Load answers every one of its error paths with the
		// ZERO Config and the field is false in it either way. It is written
		// anyway, because what makes it redundant is a fact about another
		// package's error returns rather than anything this one controls.
		return false
	}
	return cfg.MCPSwitchWithoutElicitation
}

// canAskForAForm reports whether this client can put a question in front of the
// person at the keyboard.
//
// It is asked BEFORE an input request is returned, and that ordering is the
// point. Two clients answer no for different reasons and the failure is the
// same for both: one that declared no elicitation capability at all, and one
// that declared only the url mode, for which the SDK refuses a form. Either
// way the refusal arrives as a JSON-RPC protocol error, which the model never
// sees and cannot correct itself from. Asking first turns both into ccdad's own
// sentence.
//
// Neither mode named is read as form support, because that is what the SDK does
// with it -- a client written before the modes existed declared the capability
// and nothing else.
func canAskForAForm(ss *mcp.ServerSession) bool {
	if ss == nil {
		return false
	}
	params := ss.InitializeParams()
	if params == nil || params.Capabilities == nil || params.Capabilities.Elicitation == nil {
		return false
	}
	caps := params.Capabilities.Elicitation
	return caps.Form != nil || caps.URL == nil
}

// confirmDecision reads the answer out of a call that is being retried.
//
// asked is false ONLY for a call carrying no answers at all, which is the first
// run. Everything else counts as answered, and every answer that is not exactly
// an accepted elicitation under this server's own id counts as a refusal. The
// asymmetry is deliberate: one file overwrite is downstream of this function,
// and "go ahead" is the reading a malformed response must never reach.
func confirmDecision(req *mcp.CallToolRequest) (asked, accepted bool) {
	if len(req.Params.InputResponses) == 0 {
		return false, false
	}
	res, ok := req.Params.InputResponses[confirmID].(*mcp.ElicitResult)
	if !ok {
		return true, false
	}
	// "accept" is the protocol's word for the person confirming. "decline" and
	// "cancel" are the two ways of not confirming, and they are not enumerated
	// here: anything that is not the one word is a refusal, so a revision that
	// adds a third way of saying no needs no edit to be safe.
	return true, res.Action == "accept"
}

// confirmParams is the question itself.
//
// The account is rendered QUOTED, and that is not formatting. It arrives as a
// model-supplied string and its destination is a dialog a human reads and
// clicks through; quoting escapes the newlines and control characters that
// would otherwise let the argument write its own sentence underneath ccdad's.
//
// There is no requested schema. The question is yes or no, and the protocol's
// action field already answers exactly that -- a form with a field in it would
// add a second thing that could be reported as accepted.
func confirmParams(account string, p provider.ID) *mcp.ElicitParams {
	// Two acts, two questions. A codex repoint does not rewrite any credential
	// on this machine -- codex holds no token here at all -- and it takes effect
	// on the next NEW thread rather than on the next request, because the proxy
	// keeps a thread with the account that produced its earlier turns. Asking
	// the Claude question about it would have a person decline something
	// harmless; asking the codex question about a Claude switch would have them
	// approve a login swap they thought was a pointer.
	if p == provider.Codex {
		return &mcp.ElicitParams{
			Message: fmt.Sprintf(
				"ccdad is about to make %q the account its local codex proxy serves for new threads. "+
					"Every codex session launched through ccdad on this machine is billed to it from "+
					"its next new thread; Claude Code's login is untouched. Allow it?",
				account),
		}
	}
	return &mcp.ElicitParams{
		Message: fmt.Sprintf(
			"ccdad is about to make the account %q the live Claude Code login on this machine.\n\n"+
				"Every Claude Code session on this login is billed to that account from its next "+
				"request, with no restart -- including the conversation asking for this. Allow it?",
			account),
	}
}

// providerOfRef answers which provider a model-supplied account reference names.
//
// It resolves IN PROCESS, against accounts.toml, because the consent prompt
// needs the provider before the command is allowed to run. Re-entering the CLI
// through the Exec seam would be a second parse and a second policy surface for
// an answer the store already owns.
//
// Every failure answers Claude: an unreadable store, an unresolvable reference,
// an ambiguous one. That is the conservative direction, because the Claude
// consent is the stronger of the two -- and the switch that follows refuses on
// the same reference a moment later anyway, in its own words.
func providerOfRef(ref string) provider.ID {
	root, err := ccpath.StoreHome()
	if err != nil {
		return provider.Claude
	}
	accounts, err := store.AccountsAt(root)
	if err != nil {
		return provider.Claude
	}
	a, err := store.Resolve(accounts, ref)
	if err != nil {
		return provider.Claude
	}
	if a.Provider == provider.Codex {
		return provider.Codex
	}
	return provider.Claude
}

// refusedWithoutAConfirm is what the model is told when nobody can be asked.
//
// It names both ways the person at the keyboard can grant the permission, and
// that is a deliberate reversal of the earlier design's mistake rather than a
// repeat of it: what this message names cannot be supplied in a tool call. The
// only way to act on it is at a shell or in an editor, and whoever is there can
// run `ccdad switch` directly instead.
// unchangedBy names what a refused switch left alone, in the words that
// provider's switch would have used. Saying "the live login is unchanged"
// after refusing a codex repoint is true but answers a question nobody asked.
func unchangedBy(p provider.ID) string {
	if p == provider.Codex {
		return "codex still serves the account it was serving"
	}
	return "the live login is unchanged"
}

func refusedWithoutAConfirm(p provider.ID) error {
	// The first clause names what would actually change, and the two providers
	// change different things: a Claude switch rewrites the live login, a codex
	// switch moves the pointer its proxy serves from and rewrites nothing. A
	// refusal that named the wrong one would describe a risk the caller does
	// not have, and this message exists to be acted on rather than skimmed.
	at := "rewrite the live Claude Code login"
	if p == provider.Codex {
		at = "repoint the account its local codex proxy serves"
	}
	return fmt.Errorf(
		"ccdad will not %s without asking the person at the keyboard, "+
			"and this client cannot carry the question: it declared no support for form elicitation when "+
			"it connected. Nothing was switched.\n\n"+
			"The person at the keyboard can allow it anyway, in either of two places this tool call cannot "+
			"reach: `ccdad config set %s true`, or %s=true in the environment of the process running "+
			"`ccdad mcp`. Otherwise `ccdad switch <account>` at a terminal does the same thing.",
		at, config.KeyMCPSwitchWithoutElicitation, EnvSwitchWithoutElicitation)
}

func addSwitchTool(srv *mcp.Server, e view.Exec) error {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "switch",
		Title:       "Make an account the live Claude Code login",
		Description: switchToolDescription + autoStarts,
		Annotations: credentialMutator(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in switchIn) (*mcp.CallToolResult, ActionOut, error) {
		// NOTHING above this line touches the world, and that is a requirement
		// rather than a habit. This handler runs TWICE for one confirmed
		// switch -- once to ask, once with the answer -- so any side effect
		// before the branch below happens twice, and the first of the two
		// happens before anybody has said yes.
		asked, accepted := confirmDecision(req)
		switch {
		case asked:
			if !accepted {
				return nil, ActionOut{}, fmt.Errorf(
					"the person at the keyboard did not confirm the switch to %q, so nothing was "+
						"switched and %s. This is their answer rather than "+
						"a failure: do not retry it.", in.Account, unchangedBy(providerOfRef(in.Account)))
			}
		case canAskForAForm(req.Session):
			// RequestState is deliberately empty, and the thing it could have
			// carried was considered: the account this confirm is ABOUT, so
			// that a retry naming a different one could be refused.
			//
			// It was not taken. RequestState is a token the CLIENT echoes
			// back, so a client willing to swap the account is equally willing
			// to echo a state that matches it -- the check would stop only an
			// honest client with a bug. Against that it would put every switch
			// behind a field some client may not echo at all, and this server's
			// client is a local process that launched it and could rewrite the
			// credentials file directly. Nothing verifiable is being given up.
			return &mcp.CallToolResult{
				InputRequests: mcp.InputRequestMap{
					confirmID: confirmParams(in.Account, providerOfRef(in.Account)),
				},
			}, ActionOut{}, nil
		case allowedWithoutAsking():
			// Granted out of band by the person at the keyboard, on a client
			// that has no way to put the question in front of them. Nothing to
			// do here: the switch below is the answer.
		default:
			return nil, ActionOut{}, refusedWithoutAConfirm(providerOfRef(in.Account))
		}

		out, isErr := actionResult(e, "switch", in.Account)
		return &mcp.CallToolResult{IsError: isErr}, out, nil
	})

	return nil
}

// switchToolDescription is the tool's own words, lifted out of the literal so a
// test can assert on it without constructing a server.
//
// The codex sentence is LAST because it is the narrower case: a caller reading
// the first two sentences has the answer for the account it is most likely to
// name, and the third tells it what changes when the account is a codex one.
const switchToolDescription = "Make one managed account the live Claude Code login on this machine. This is the " +
	"tool that rewrites the credentials file: from its next request, every Claude Code " +
	"session on this login is billed to the account named here, including the " +
	"conversation that called this, and nothing restarts. It asks the person at the " +
	"keyboard first and does nothing until they answer; on a client that cannot carry " +
	"that question it refuses, unless the person has allowed it out of band. Switching " +
	"to the account that is already live changes nothing and says so. Naming a codex " +
	"account instead repoints ccdad's local codex proxy: it takes effect on the next new " +
	"codex thread, threads already running keep the account that started them, and " +
	"Claude Code's own login is not touched."
