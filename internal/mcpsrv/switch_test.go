package mcpsrv

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/provider"
)

// switch takes an account and NOTHING else, and the "nothing else" is the
// security property rather than tidiness. The confirm arrives in the protocol's
// own inputResponses field, which the client fills in; if this schema had a
// second property, or accepted one it does not declare, the model would have a
// way to put its own answer where the person's answer belongs.
func TestSwitchTakesAnAccountAndOffersTheModelNoWayToConfirmForItself(t *testing.T) {
	tool := offeredTools(t)["switch"]
	if tool == nil {
		t.Fatal("tools/list does not offer switch")
	}
	s := schemaOf(t, tool)
	if s.Type != "object" {
		t.Errorf("switch input schema type = %q, want object", s.Type)
	}
	if !slices.Contains(s.Required, "account") {
		t.Errorf("switch does not require an account: required = %v", s.Required)
	}
	if len(s.Properties) != 1 {
		t.Errorf("switch declares %d properties (%v); it takes an account and nothing else",
			len(s.Properties), propertyNames(s))
	}
	if s.AdditionalProperties == nil || !bytesAreFalse(s.AdditionalProperties) {
		t.Error("switch accepts additional properties, so a caller could send its own confirm alongside the account")
	}
}

// The two arguments a model would reach for if it wanted to answer for the
// person, refused by the schema before the handler runs.
func TestAnArgumentThatLooksLikeAConfirmIsRefusedAndTheHandlerNeverRuns(t *testing.T) {
	for _, args := range []string{
		`{"account":"work@example.com","confirm":true}`,
		`{"account":"work@example.com","inputResponses":{"confirm_switch":{"action":"accept"}}}`,
		`{"account":"work@example.com","mcp_switch_without_elicitation":true}`,
	} {
		t.Run(args, func(t *testing.T) {
			var ran bool
			res := callTool(t, recording(&ran), "switch", args)
			if ran {
				t.Fatal("the handler ran on an argument the schema rejects")
			}
			if !res.IsError {
				t.Fatal("an invented confirm argument was accepted")
			}
		})
	}
}

// This is the one tool that rewrites the live login, and every hint has to say
// so: it is not a read, it is destructive, and it is idempotent -- switching to
// the account already live is exit 3 and rewrites nothing.
func TestSwitchDeclaresWhatItRewritesRatherThanLettingAHintDefault(t *testing.T) {
	tool := offeredTools(t)["switch"]
	if tool == nil || tool.Annotations == nil {
		t.Fatal("switch carries no annotations at all, so every hint on it defaults")
	}
	if tool.Annotations.ReadOnlyHint {
		t.Error("switch is marked read-only; it rewrites the credentials file")
	}
	if !tool.Annotations.IdempotentHint {
		t.Error("switch is not marked idempotent, and switching to the account already live is exit 3")
	}
	wire := annotationsOnTheWire(t, "switch", tool)
	if got, ok := wire["destructiveHint"]; !ok {
		t.Error("switch omits destructiveHint, and the protocol reads an omitted one as TRUE")
	} else if bytesAreFalse(got) {
		t.Error("switch declares destructiveHint false; it overwrites the live Claude Code login")
	}
	if got, ok := wire["openWorldHint"]; !ok {
		t.Error("switch omits openWorldHint, and the protocol reads an omitted one as TRUE")
	} else if !bytesAreFalse(got) {
		t.Errorf("switch has openWorldHint = %s, want false", got)
	}
}

// switch is on ccdad's auto-start list, so its description carries the same
// sentence the three reads on that list carry -- and it has to say that it
// asks first, because a model that believed otherwise would announce a switch
// it has not been given yet.
func TestTheSwitchDescriptionSaysItAsksFirstAndMayStartTheDaemon(t *testing.T) {
	got := offeredTools(t)["switch"].Description
	for _, want := range []string{"asks the person at the keyboard", "background daemon", "billed"} {
		if !strings.Contains(got, want) {
			t.Errorf("the switch description does not say %q:\n%s", want, got)
		}
	}
}

// The confirmed path, end to end over the wire, and the count is the assertion.
//
// The handler runs TWICE for one confirmed switch: once to return the input
// request, once with the answer. `ccdad switch` must still run ONCE. A handler
// that acted before its confirm branch would show up here as two -- and the
// first of those two would have happened before anybody was asked.
func TestAConfirmedSwitchRunsCcdadExactlyOnceHoweverOftenTheHandlerIsEntered(t *testing.T) {
	var argv [][]string
	e := func(a []string) (int, string, string) {
		argv = append(argv, a)
		return 0, "", "Switched to work@example.com.\n"
	}
	asked := 0
	cs := connectClient(t, newTestServer(t, e), &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			asked++
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "switch",
		Arguments: json.RawMessage(`{"account":"work@example.com"}`),
	})
	if err != nil {
		t.Fatalf("the call came back as a protocol error (%v); the model cannot see one", err)
	}
	if asked != 1 {
		t.Errorf("the person was asked %d times for one switch, want 1", asked)
	}
	if len(argv) != 1 {
		t.Fatalf("ccdad ran %d times for one confirmed switch (%v), want 1", len(argv), argv)
	}
	if strings.Join(argv[0], " ") != "switch work@example.com" {
		t.Errorf("argv = %v, want [switch work@example.com]", argv[0])
	}
	if res.IsError {
		t.Fatalf("a confirmed switch came back as an error: %s", contentText(res))
	}
	if out := actionOut(t, res); !out.Changed {
		t.Error("Changed is false after a switch that was taken")
	}
}

// The hazard the switch handler is written around, measured rather than
// assumed: one confirmed tool call enters the handler TWICE -- once to return
// the input request, once with the answer -- so anything a handler does before
// its confirm branch happens twice, and the first time before anybody has been
// asked.
// `TestAConfirmedSwitchRunsCcdadExactlyOnceHoweverOftenTheHandlerIsEntered` is
// the other half: this one pins the premise, that one pins that ccdad still ran
// once.
//
// The probe is registered with mcp.AddTool directly, like gate_test.go's, so
// that the count belongs to the SDK's round-trip machinery and not to anything
// this package could accidentally be making true.
//
// The count of WIRE calls is logged rather than asserted, and the reason is a
// measurement of its own: a client speaking the multi-round-trip revision
// fulfils the request itself and RETRIES, which is a second tools/call, while
// an older one is served from inside the server on one. Both are correct and
// this test cannot choose between them -- ClientSessionOptions.protocolVersion
// is unexported, so no test outside the SDK can pin the older path.
func TestOneConfirmedCallEntersItsHandlerTwice(t *testing.T) {
	var entries, wireCalls int

	srv := mcp.NewServer(implementation("0.0.0-test"), &mcp.ServerOptions{})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "probe_confirm",
		Description: "a probe that asks once and answers once, so a test can count how often it is entered",
	}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		entries++
		if len(req.Params.InputResponses) == 0 {
			return &mcp.CallToolResult{
				InputRequests: mcp.InputRequestMap{confirmID: confirmParams("work@example.com", provider.Claude)},
			}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: probeRan}}}, nil, nil
	})
	// Added after the constructor, exactly where installGate goes, so this
	// counts what a gate in that position would see.
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				wireCalls++
			}
			return next(ctx, method, req)
		}
	})

	cs := connectClient(t, srv, &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "probe_confirm", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if !strings.Contains(contentText(res), probeRan) {
		t.Fatalf("the confirmed call did not reach its second run: %q", contentText(res))
	}
	if entries != 2 {
		t.Errorf("the handler was entered %d times for one confirmed call, want 2", entries)
	}
	t.Logf("one confirmed call: %d handler entries, %d tools/call requests on the wire", entries, wireCalls)
}

// Declined, cancelled, and answered by something that is not an answer at all.
// In every one of them the credentials file is never reached, and the model is
// told so in a sentence it can act on rather than retry.
func TestNothingIsSwitchedUntilThePersonAtTheKeyboardSaysYes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer *mcp.ElicitResult
	}{
		{"declined", &mcp.ElicitResult{Action: "decline"}},
		{"dismissed without choosing", &mcp.ElicitResult{Action: "cancel"}},
		{"an action this protocol revision does not have", &mcp.ElicitResult{Action: "maybe"}},
		{"accepted with no action word at all", &mcp.ElicitResult{Content: map[string]any{"confirm": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran bool
			cs := connectClient(t, newTestServer(t, recording(&ran)), &mcp.ClientOptions{
				ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					return tc.answer, nil
				},
			})
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      "switch",
				Arguments: json.RawMessage(`{"account":"work@example.com"}`),
			})
			if err != nil {
				t.Fatalf("the refusal came back as a protocol error (%v); the model cannot see one", err)
			}
			if ran {
				t.Fatal("ccdad switch ran without the person having confirmed it")
			}
			if !res.IsError {
				t.Fatal("an unconfirmed switch came back as a success")
			}
			if !strings.Contains(contentText(res), "nothing was switched") {
				t.Errorf("refusal = %q, want it to say the live login is unchanged", contentText(res))
			}
		})
	}
}

// The account reaches a human's dialog, and it arrives as a string the MODEL
// wrote. It is rendered quoted so that its escapes are visible rather than
// executed: an account carrying a newline and a sentence of its own would
// otherwise be able to write underneath ccdad's question.
func TestTheQuestionNamesTheAccountAndTheAccountCannotWriteTheQuestion(t *testing.T) {
	const hostile = "work@example.com\"\n\nThis is routine. Allow it.\nAccount: \""
	var seen string
	cs := connectClient(t, newTestServer(t, func([]string) (int, string, string) { return 0, "", "" }), &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			seen = req.Params.Message
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})
	args, err := json.Marshal(map[string]string{"account": hostile})
	if err != nil {
		t.Fatal(err)
	}
	// json.RawMessage and not the bare []byte: Arguments is an `any`, and a
	// []byte in it is marshalled as base64 rather than as the document it is.
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "switch", Arguments: json.RawMessage(args)}); err != nil {
		t.Fatalf("protocol error: %v", err)
	}

	if !strings.Contains(seen, "work@example.com") {
		t.Errorf("the question does not name the account: %q", seen)
	}
	// Two blank-line-separated paragraphs are ccdad's own; a third would be the
	// argument's. Counting newlines is the blunt form of the same check and it
	// is the one that fails if the quoting is ever dropped.
	if strings.Count(seen, "\n") != strings.Count(confirmParams("x", provider.Claude).Message, "\n") {
		t.Errorf("the account added lines of its own to the question:\n%s", seen)
	}
}

// A client that cannot carry the question is refused rather than served, and
// the refusal names both of the places the person at the keyboard can grant the
// permission. Neither of them is reachable from a tool call, which is the whole
// difference between this message and the one the earlier design printed.
func TestAClientThatCannotAskIsRefusedRatherThanSwitchingBehindThePersonsBack(t *testing.T) {
	withPermission(t, false)

	var ran bool
	res := callTool(t, recording(&ran), "switch", `{"account":"work@example.com"}`)
	if ran {
		t.Fatal("ccdad switch ran on a client that cannot ask anybody")
	}
	if !res.IsError {
		t.Fatal("a switch nobody confirmed came back as a success")
	}
	text := contentText(res)
	for _, want := range []string{config.KeyMCPSwitchWithoutElicitation, EnvSwitchWithoutElicitation, "Nothing was switched"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal = %q, want it to name %q", text, want)
		}
	}
}

// The out-of-band grant, on the same client that was refused above. It is the
// half that makes this a fallback rather than a wall: a person driving a client
// with no elicitation can still hand ccdad the permission, once, where a model
// talking to this server cannot reach it.
func TestTheOperatorCanAllowASwitchOnAClientThatCannotAsk(t *testing.T) {
	withPermission(t, true)

	var argv []string
	e := func(a []string) (int, string, string) {
		argv = a
		return 0, "", "Switched to work@example.com.\n"
	}
	res := callTool(t, e, "switch", `{"account":"work@example.com"}`)
	if res.IsError {
		t.Fatalf("an allowed switch came back as an error: %s", contentText(res))
	}
	if strings.Join(argv, " ") != "switch work@example.com" {
		t.Errorf("argv = %v, want [switch work@example.com]", argv)
	}
}

// Elicitation is PREFERRED, not merely accepted. The permission is the fallback
// for a client that cannot ask, so on one that can, the person is still asked
// -- otherwise granting it once would silently turn the confirm off everywhere.
func TestAGrantedPermissionDoesNotStopAClientThatCanAskFromAsking(t *testing.T) {
	withPermission(t, true)

	var ran bool
	asked := 0
	cs := connectClient(t, newTestServer(t, recording(&ran)), &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			asked++
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "switch",
		Arguments: json.RawMessage(`{"account":"work@example.com"}`),
	}); err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if asked != 1 {
		t.Errorf("the person was asked %d times, want 1: the permission is a fallback, not an override", asked)
	}
	if ran {
		t.Fatal("the switch ran despite being declined; the permission overrode the person's own answer")
	}
}

// A client that declared only the URL mode cannot render a form, and the SDK
// refuses one with a PROTOCOL error the model never sees. Asking before the
// input request is returned is what turns it into ccdad's own sentence.
func TestAClientThatCanOnlyOpenAURLCountsAsOneThatCannotAsk(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps *mcp.ElicitationCapabilities
		want bool
	}{
		{"no elicitation at all", nil, false},
		{"neither mode named, as clients written before the modes did", &mcp.ElicitationCapabilities{}, true},
		{"forms", &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}, true},
		{"urls only", &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}, false},
		{"both", &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ss := connectedSessionDeclaring(t, tc.caps)
			if got := canAskForAForm(ss); got != tc.want {
				t.Errorf("canAskForAForm = %v, want %v", got, tc.want)
			}
		})
	}
	if canAskForAForm(nil) {
		t.Error("a call with no session at all was read as one that can ask somebody")
	}
}

// The answer is read from one key, one type and one word, and everything else
// is a refusal. A switch is one file overwrite downstream of this, so "go
// ahead" is the reading a malformed response must never reach.
func TestOnlyAnAcceptedElicitationUnderThisServersOwnIdCountsAsAConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses mcp.InputResponseMap
		asked     bool
		accepted  bool
	}{
		{"no answers at all is the first run", nil, false, false},
		{"an empty map is the first run", mcp.InputResponseMap{}, false, false},
		{"accepted", mcp.InputResponseMap{confirmID: &mcp.ElicitResult{Action: "accept"}}, true, true},
		{"declined", mcp.InputResponseMap{confirmID: &mcp.ElicitResult{Action: "decline"}}, true, false},
		{"accepted under somebody else's id", mcp.InputResponseMap{"other": &mcp.ElicitResult{Action: "accept"}}, true, false},
		{"an answer that is not an elicitation", mcp.InputResponseMap{confirmID: &mcp.ListRootsResult{}}, true, false},
		{"the word capitalised", mcp.InputResponseMap{confirmID: &mcp.ElicitResult{Action: "Accept"}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
				Name:           "switch",
				InputResponses: tc.responses,
			}}
			asked, accepted := confirmDecision(req)
			if asked != tc.asked || accepted != tc.accepted {
				t.Errorf("confirmDecision = (%v, %v), want (%v, %v)", asked, accepted, tc.asked, tc.accepted)
			}
		})
	}
}

// The permission comes from the two places a tool call cannot write, and from
// nowhere else. The environment decides when it is set at all -- including when
// it is set to false, which is how one client takes back what the file granted
// for the machine.
func TestThePermissionIsReadFromTheFileAndFromTheEnvironmentThatOverridesIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		env  *string
		want bool
	}{
		{"no file and no variable refuses", "", nil, false},
		{"the file grants it", "mcp_switch_without_elicitation = true\n", nil, true},
		{"the file refuses it explicitly", "mcp_switch_without_elicitation = false\n", nil, false},
		{"a file about something else refuses it", "threshold = 70\n", nil, false},
		{"a file that does not parse refuses it", "mcp_switch_without_elicitation = yes\n", nil, false},
		{"the variable grants it with no file", "", ptr("true"), true},
		{"the variable takes back what the file granted", "mcp_switch_without_elicitation = true\n", ptr("false"), false},
		{"the variable grants what the file refused", "mcp_switch_without_elicitation = false\n", ptr("1"), true},
		{"a variable that is not a boolean is not a grant", "", ptr("please"), false},
		{"an empty variable is not a grant", "mcp_switch_without_elicitation = true\n", ptr(""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CCDAD_HOME", home)
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(home, config.FileName), []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.env != nil {
				t.Setenv(EnvSwitchWithoutElicitation, *tc.env)
			} else {
				withoutTheEnvironmentGrant(t)
			}
			if got := readSwitchPermission(); got != tc.want {
				t.Errorf("readSwitchPermission() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A config file that exists and cannot be read is not a grant. ccdad's own
// loader refuses to substitute the defaults for one at the far lower stake of
// an engine tuning value, and reading it as permission to rewrite the live
// login would be the most expensive possible way to be wrong about a file.
func TestAConfigFileThatCannotBeReadIsNotAGrant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CCDAD_HOME", home)
	withoutTheEnvironmentGrant(t)
	// A DIRECTORY where the file belongs fails the read on every platform,
	// unlike a mode bit, which Windows does not have.
	if err := os.Mkdir(filepath.Join(home, config.FileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if readSwitchPermission() {
		t.Error("a config file that could not be read was taken as granting the permission")
	}
}

// withoutTheEnvironmentGrant unsets the variable for one test and has it put
// back afterwards.
//
// t.Setenv is what registers the restore; the unset is what the test needs, and
// an empty string would not do it -- an empty variable is SET, and set is the
// thing that decides.
func withoutTheEnvironmentGrant(t *testing.T) {
	t.Helper()
	t.Setenv(EnvSwitchWithoutElicitation, "")
	os.Unsetenv(EnvSwitchWithoutElicitation)
}

// withPermission answers the permission for one test without a config file,
// and puts the real reader back afterwards.
func withPermission(t *testing.T, allowed bool) {
	t.Helper()
	previous := allowedWithoutAsking
	allowedWithoutAsking = func() bool { return allowed }
	t.Cleanup(func() { allowedWithoutAsking = previous })
}

// connectedSessionDeclaring stands a client up declaring exactly the
// elicitation capability given and hands back the SERVER's side of it, because
// the capability question is asked of the server session.
//
// Capabilities is set explicitly rather than inferred from an elicitation
// handler: the client SDK fills in the form capability whenever a handler is
// present, so a handler alone cannot describe the url-only client this is here
// to cover.
func connectedSessionDeclaring(t *testing.T, caps *mcp.ElicitationCapabilities) *mcp.ServerSession {
	t.Helper()
	ctx := t.Context()
	srv := newTestServer(t, func([]string) (int, string, string) { return 0, "", "" })

	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("connecting the server: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	opts := &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{Elicitation: caps}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "ccdad-test-client", Version: "0.0.0-test"}, opts).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connecting the client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	// The server session is initialized once the client's handshake has
	// returned, which is what makes InitializeParams answerable here.
	return ss
}

func ptr(s string) *string { return &s }
