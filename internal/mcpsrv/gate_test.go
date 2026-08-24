package mcpsrv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The eight verbs that are NOT tools, and the reason each one is not.
//
// Absence is enforced rather than documented: there is no class entry for any
// of them, and the gate refuses by name anything it has no class for. A handler
// registered under one of these names would be refused before it ran.
func TestTheVerbsThatAreNotToolsHaveNoClassAtAll(t *testing.T) {
	for _, name := range []string{
		"add", "add-token", "run", "export", "import",
		"uninstall", "setup-path", "bootstrap",
	} {
		if _, ok := classOf(name); ok {
			t.Errorf("%q has a class; it must have none, because a tool with no class is refused by name", name)
		}
	}
}

// A tool nobody classified is refused, and the refusal is a TOOL error rather
// than a protocol error: a protocol error is invisible to the model, which can
// then neither see it nor correct itself from it.
func TestAToolWithNoClassIsRefusedAsAToolErrorAndNotAProtocolError(t *testing.T) {
	res, err := callThroughGate(t, "not-a-ccdad-tool", "{}")
	if err != nil {
		t.Fatalf("the refusal came back as a protocol error (%v); the model cannot see one", err)
	}
	if !res.IsError {
		t.Fatal("an unclassified tool was allowed through")
	}
	text := contentText(res)
	if !strings.Contains(text, "no gate verdict") {
		t.Errorf("refusal = %q, want it to say ccdad has no gate verdict for the tool", text)
	}
	if !strings.Contains(text, "not-a-ccdad-tool") {
		t.Errorf("refusal = %q, want it to name the tool it refused", text)
	}
}

// The gate reads the tool NAME and never the arguments. Middleware runs before
// the schema validation, so the arguments there are raw untrusted JSON: a gate
// that unmarshalled them into a typed struct and ignored the error would decide
// on a zero-valued account name.
func TestTheGateDecidesOnTheNameEvenWhenTheArgumentsAreNonsense(t *testing.T) {
	res, err := callThroughGate(t, "not-a-ccdad-tool", `{"account":42,"":null}`)
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if !res.IsError || !strings.Contains(contentText(res), "no gate verdict") {
		t.Errorf("garbage arguments changed the gate's answer: %+v", res)
	}
}

// A classified tool is not merely tolerated by the gate, it reaches its
// handler. Without this the three assertions above would all still hold for a
// middleware that refused everything.
func TestAClassifiedToolReachesItsHandler(t *testing.T) {
	res, err := callThroughGate(t, "list", "{}")
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("a classified tool was refused: %q", contentText(res))
	}
	if !strings.Contains(contentText(res), probeRan) {
		t.Errorf("result = %q, want the probe handler's own text", contentText(res))
	}
}

// probeRan is what the probe handler writes, so a test can tell "the gate let
// it through" from "the gate returned something that merely is not an error".
const probeRan = "the probe handler ran"

// callThroughGate stands a server up around ONE probe tool registered under the
// given name, gates it, and calls it over the real wire with the given raw
// arguments.
//
// The probe is registered with mcp.AddTool directly rather than through this
// package's own registration, because the point is to reach the middleware with
// a name of the test's choosing -- including names no ccdad tool will ever have.
func callThroughGate(t *testing.T, name, rawArgs string) (*mcp.CallToolResult, error) {
	t.Helper()

	srv := mcp.NewServer(implementation("0.0.0-test"), &mcp.ServerOptions{})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        name,
		Description: "a probe with no behaviour of its own, registered so that a call reaches the gate",
	}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: probeRan}}}, nil, nil
	})
	installGate(srv)

	// json.RawMessage rather than a map: the arguments have to reach the wire
	// exactly as written, including the shapes no typed value can express.
	return connectToServer(t, srv).CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(rawArgs)})
}

// contentText joins the text a result carries. A refusal is text content and
// nothing else, so anything this drops was not part of one.
func contentText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// The mirror of the command tree's own totality gate: every REGISTERED tool has
// a class. A tool added later gets NO verdict rather than a permissive one.
func TestEveryRegisteredToolHasAClassVerdict(t *testing.T) {
	srv := mcp.NewServer(implementation("0.0.0-test"), &mcp.ServerOptions{})
	if err := Register(srv, func([]string) (int, string, string) { return 0, "", "" }); err != nil {
		t.Fatal(err)
	}

	var missing []string
	for _, name := range registeredToolNames(t, srv) {
		if _, ok := classOf(name); !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("no class verdict for: %s\nAdd each to toolClass as read, store, credential or daemon.",
			strings.Join(missing, ", "))
	}
}

// registeredToolNames asks the server what it offers, over the wire, the way a
// client would. There is no way to read a server's tools without connecting to
// one, and asking through the protocol is the more honest question anyway: it
// is the list a client actually sees.
//
// The iterator rather than a single list call, because it follows the cursor.
// A page-sized list would pass today and silently stop covering the tail on the
// day this server has more tools than one page holds.
func registeredToolNames(t *testing.T, srv *mcp.Server) []string {
	t.Helper()
	cs := connectToServer(t, srv)
	var names []string
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		names = append(names, tool.Name)
	}
	return names
}
