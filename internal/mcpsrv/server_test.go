package mcpsrv

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Every tool this server has runs a ccdad command, and there is nothing to run
// one with unless the caller passed an executor. Refusing at construction is
// the difference between a server that cannot start and one that starts and
// then fails every call it is given.
func TestAServerWithNoExecutorIsRefusedRatherThanBuilt(t *testing.T) {
	_, err := New(Options{Version: "0.0.0-test", Logger: quietLogger()})
	if err == nil {
		t.Fatal("a server with no executor was built; every tool it has would have nothing to run")
	}
	if !strings.Contains(err.Error(), "executor") {
		t.Errorf("error = %q, want it to name the missing executor", err)
	}
}

// The logger is required rather than defaulted, and the reason is specific: the
// registration error for an invalid tool name has nowhere to go but the logger,
// so a server built without one loses a tool silently.
func TestAServerWithNoLoggerIsRefusedBecauseAnInvalidToolNameWouldRegisterSilently(t *testing.T) {
	_, err := New(Options{Version: "0.0.0-test", Exec: func([]string) (int, string, string) { return 0, "", "" }})
	if err == nil {
		t.Fatal("a server with no logger was built; an invalid tool name would register silently")
	}
	if !strings.Contains(err.Error(), "silently") {
		t.Errorf("error = %q, want it to say what goes wrong without a logger", err)
	}
}

// The instructions are the one thing a client is told before it calls anything,
// and this server is holding the keys to somebody's account. Three facts have
// to survive an edit of that prose: what it can rewrite, that switching asks
// first, and that a daemon outlives the session that started it.
func TestInitializeSaysWhatThisServerIsHoldingBeforeAnyToolIsCalled(t *testing.T) {
	cs := connectToServer(t, newTestServer(t, func([]string) (int, string, string) { return 0, "", "" }))
	got := cs.InitializeResult()

	if got.ServerInfo.Name != ServerName {
		t.Errorf("ServerInfo.Name = %q, want %q", got.ServerInfo.Name, ServerName)
	}
	for _, want := range []string{"refresh token", "credentials", "asks the person at the keyboard", "outlives"} {
		if !strings.Contains(got.Instructions, want) {
			t.Errorf("instructions do not say %q:\n%s", want, got.Instructions)
		}
	}
}

// Left to itself the server advertises a logging capability it does not
// implement. Passing an explicit empty set suppresses it, and the tools
// capability still arrives because the server derives that one from the tools
// it actually has.
func TestTheServerAdvertisesNoCapabilityItDoesNotImplement(t *testing.T) {
	cs := connectToServer(t, newTestServer(t, func([]string) (int, string, string) { return 0, "", "" }))
	caps := cs.InitializeResult().Capabilities

	if caps.Logging != nil {
		t.Error("the server advertises a logging capability; it implements none, and logging is deprecated in the protocol")
	}
	if caps.Prompts != nil || caps.Resources != nil {
		t.Errorf("the server advertises prompts or resources it has none of: %+v", caps)
	}
	if caps.Tools == nil {
		t.Error("the server advertises no tools capability, and it has fifteen tools to offer")
	}
}

// New installs the gate, and whether it did is visible from the client rather
// than by inspection. Measured on this server with the gate left out, one of
// the eight verbs that are deliberately not tools comes back as a PROTOCOL
// error -- `calling "tools/call": unknown tool "add"` -- which the model never
// sees and cannot correct itself from. With the gate, the same call is a tool
// result carrying ccdad's own sentence.
func TestTheServerRefusesAnUnclassifiedNameWhereTheModelCanReadIt(t *testing.T) {
	cs := connectToServer(t, newTestServer(t, func([]string) (int, string, string) { return 0, "", "" }))

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "add", Arguments: json.RawMessage("{}")})
	if err != nil {
		t.Fatalf("the refusal came back as a protocol error (%v); the gate is not installed", err)
	}
	if !res.IsError {
		t.Fatal("a verb that is deliberately not a tool was allowed through")
	}
	if !strings.Contains(contentText(res), "no gate verdict") {
		t.Errorf("refusal = %q, want ccdad's own refusal rather than the library's", contentText(res))
	}
}

// newTestServer builds the real thing -- tools, gate and all -- around a
// recording executor, with a version that a release cannot move underneath an
// assertion.
func newTestServer(t *testing.T, e func([]string) (int, string, string)) *mcp.Server {
	t.Helper()
	srv, err := New(Options{Exec: e, Version: "0.0.0-test", Logger: quietLogger()})
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	return srv
}

// connectToServer connects a client to a server over an in-memory pair and
// hands back the client's session.
//
// THE SERVER CONNECTS FIRST, and this is the only place in the package's tests
// that decides that. The pair is a synchronous pipe with no buffer, so a client
// that connects first blocks writing its handshake to nobody, and the failure
// is a hang rather than an assertion -- the kind of thing that is worth having
// exactly one copy of.
func connectToServer(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	return connectClient(t, srv, nil)
}

// connectClient is connectToServer with the client's own options, which is what
// a test needs when the property under test is something the CLIENT declares --
// whether it can put a question in front of the person at the keyboard, above
// all. Options and no options share this body so that the ordering rule above
// keeps having exactly one copy.
func connectClient(t *testing.T, srv *mcp.Server, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ctx := t.Context()

	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("connecting the server: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "ccdad-test-client", Version: "0.0.0-test"}, opts).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connecting the client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// quietLogger is what the tests hand the server for a logger.
//
// It discards rather than writing through t.Log on purpose: the session runs on
// its own goroutines and can still be logging while the test is being torn
// down, and t.Log after the test has returned panics. The one property the
// logger is required FOR -- that an invalid tool name is not lost -- is
// asserted directly above rather than by reading log output.
func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
