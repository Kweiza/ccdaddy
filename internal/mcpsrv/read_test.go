package mcpsrv

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tools/list is the contract a client sees before it calls anything. The five
// reads have to be there, described, and shaped so that an invented argument is
// refused rather than ignored.
func TestToolsListOffersTheFiveReadsWithTheirSchemas(t *testing.T) {
	tools := offeredTools(t)

	for _, name := range []string{"list", "status", "which", "doctor", "config_get"} {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("tools/list does not offer %q", name)
			continue
		}
		if tool.Description == "" {
			t.Errorf("%q has no description; a client shows the model this and nothing else", name)
		}
		s := schemaOf(t, tool)
		if s.Type != "object" {
			t.Errorf("%q input schema type = %q, want object", name, s.Type)
		}
		// A caller that invents an argument is refused rather than quietly
		// ignored, which is the difference between a typo and a wrong answer.
		if s.AdditionalProperties == nil || !bytesAreFalse(s.AdditionalProperties) {
			t.Errorf("%q accepts additional properties; an invented argument would be ignored instead of refused", name)
		}
	}

	// The two shapes that are not "no arguments", asserted by what the protocol
	// requires rather than by what the Go struct looks like.
	if s := schemaOf(t, tools["config_get"]); !slices.Contains(s.Required, "key") {
		t.Errorf("config_get does not require a key: required = %v", s.Required)
	}
	if s := schemaOf(t, tools["list"]); slices.Contains(s.Required, "all") {
		t.Errorf("list requires `all`; it is a filter and its absence is the ordinary call: required = %v", s.Required)
	} else if _, ok := s.Properties["all"]; !ok {
		t.Error("list offers no `all` property, so there is no way to ask for the accounts held out of rotation")
	}
}

// The annotation trap, and it is worth a test of its own. The annotations type
// mixes plain booleans with POINTERS whose protocol default is TRUE, so a nil
// pointer is not false: an author who fills in only the plain fields has
// declared a destructive, open-world tool. Marshalling the annotations and
// looking for the two keys is the only way to see it -- a nil pointer is
// omitted from the wire entirely, and what the client then reads is the
// protocol's default rather than the author's intent.
func TestEveryReadToolDeclaresBothPointerAnnotationsExplicitly(t *testing.T) {
	for name, tool := range offeredTools(t) {
		if tool.Annotations == nil {
			t.Errorf("%q carries no annotations at all, so every hint on it defaults", name)
			continue
		}
		raw, err := json.Marshal(tool.Annotations)
		if err != nil {
			t.Fatalf("marshalling %q annotations: %v", name, err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("reading %q annotations back: %v", name, err)
		}
		for _, key := range []string{"destructiveHint", "openWorldHint"} {
			got, ok := wire[key]
			if !ok {
				t.Errorf("%q omits %s, and the protocol reads an omitted one as TRUE", name, key)
				continue
			}
			if !bytesAreFalse(got) {
				t.Errorf("%q has %s = %s, want false: it is a read", name, key, got)
			}
		}
		if !tool.Annotations.ReadOnlyHint {
			t.Errorf("%q is not marked read-only", name)
		}
	}
}

// The whole reason the protocol library was taken rather than hand-rolled: a
// malformed argument is refused BEFORE the handler runs. A hand-rolled server
// was measured returning an empty account name for the same input.
func TestAMalformedArgumentIsRefusedAndTheHandlerNeverRuns(t *testing.T) {
	var ran bool
	// config_get takes one string key; 42 is not one.
	res := callTool(t, recording(&ran), "config_get", `{"key":42}`)
	if ran {
		t.Fatal("the handler ran on an argument the schema rejects")
	}
	if !res.IsError {
		t.Fatal("a type mismatch was accepted")
	}
}

// Each read tool runs exactly the command line the CLI would take, with --json,
// and returns its bytes untouched.
func TestEachReadToolRunsItsOwnVerbWithJSONAndReturnsTheBytes(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args string
		want []string
	}{
		{"list", `{}`, []string{"list", "--json"}},
		{"list", `{"all":true}`, []string{"list", "--all", "--json"}},
		{"status", `{}`, []string{"status", "--json"}},
		{"which", `{}`, []string{"which", "--json"}},
		{"doctor", `{}`, []string{"doctor", "--json"}},
		{"config_get", `{"key":"threshold"}`, []string{"config", "get", "threshold", "--json"}},
	} {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			var got []string
			doc := "{\"schemaVersion\":1}\n"
			e := func(argv []string) (int, string, string) {
				got = argv
				return 0, doc, ""
			}
			res := callTool(t, e, tc.tool, tc.args)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %v, want %v", got, tc.want)
			}
			if res.IsError {
				t.Fatalf("a clean run came back as an error: %s", contentText(res))
			}
			// The document reaches the client as the bytes the command wrote,
			// inside the envelope and not restated by it.
			var out ReadOut
			if err := json.Unmarshal([]byte(contentText(res)), &out); err != nil {
				t.Fatalf("the result is not the envelope: %v (%s)", err, contentText(res))
			}
			if out.Document != doc {
				t.Errorf("Document = %q, want the exact bytes the command wrote", out.Document)
			}
			if out.Command != "ccdad "+strings.Join(tc.want, " ") {
				t.Errorf("Command = %q, want the command line as the CLI would take it", out.Command)
			}
		})
	}
}

// A read that ends in a negative answer keeps its document and is not an error,
// over the wire and not merely in the envelope builder: which, config get and
// daemon status all write a complete payload and exit 5.
func TestANegativeAnswerCrossesTheWireAsAnAnswerAndNotAsAnError(t *testing.T) {
	e := func([]string) (int, string, string) {
		return 5, "{\"schemaVersion\":1,\"attributed\":false}\n", "not one ccdad manages\n"
	}
	res := callTool(t, e, "which", `{}`)
	if res.IsError {
		t.Fatalf("exit 5 crossed the wire as a tool error: %s", contentText(res))
	}
	if !strings.Contains(contentText(res), "attributed") {
		t.Errorf("the payload did not survive: %s", contentText(res))
	}
}

// The three reads that the command tree will auto-start a daemon for say so.
// A description that let a model believe a read-only tool has no side effects
// would be a lie the model repeats to the person running it. doctor and
// config_get are deliberately not on that list -- doctor must not create what
// it is checking for -- so the sentence would be false on them.
func TestTheReadsThatStartADaemonSayThatTheyDo(t *testing.T) {
	tools := offeredTools(t)
	const says = "background daemon"
	for _, name := range []string{"list", "status", "which"} {
		if !strings.Contains(tools[name].Description, says) {
			t.Errorf("%q does not warn that it may start the daemon: %q", name, tools[name].Description)
		}
	}
	for _, name := range []string{"doctor", "config_get"} {
		if strings.Contains(tools[name].Description, says) {
			t.Errorf("%q claims it may start the daemon; it is not on the auto-start list: %q", name, tools[name].Description)
		}
	}
}

// offeredTools is what a client sees in tools/list, keyed by name.
func offeredTools(t *testing.T) map[string]*mcp.Tool {
	t.Helper()
	cs := connectToServer(t, newTestServer(t, func([]string) (int, string, string) { return 0, "{}\n", "" }))
	out := map[string]*mcp.Tool{}
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		out[tool.Name] = tool
	}
	return out
}

// wireSchema is the part of an input schema these assertions read. It is
// deliberately not the library's schema type: what matters here is what
// travelled, and a field the wire omitted has to read as absent rather than as
// a zero value the decoder invented.
type wireSchema struct {
	Type                 string                     `json:"type"`
	Properties           map[string]json.RawMessage `json:"properties"`
	Required             []string                   `json:"required"`
	AdditionalProperties json.RawMessage            `json:"additionalProperties"`
}

func schemaOf(t *testing.T, tool *mcp.Tool) wireSchema {
	t.Helper()
	if tool == nil {
		t.Fatal("no such tool")
	}
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshalling %q input schema: %v", tool.Name, err)
	}
	var s wireSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("reading %q input schema back: %v", tool.Name, err)
	}
	return s
}

func bytesAreFalse(raw json.RawMessage) bool { return string(raw) == "false" }

// callTool calls one tool on a fully composed server -- tools, gate and all --
// over the real wire, and refuses to let a protocol error pass as a result.
func callTool(t *testing.T, e func([]string) (int, string, string), name, rawArgs string) *mcp.CallToolResult {
	t.Helper()
	res, err := connectToServer(t, newTestServer(t, e)).CallTool(t.Context(), &mcp.CallToolParams{
		Name:      name,
		Arguments: json.RawMessage(rawArgs),
	})
	if err != nil {
		t.Fatalf("calling %q came back as a protocol error (%v); the model cannot see one", name, err)
	}
	return res
}

// recording is an executor that only reports whether it was reached.
func recording(ran *bool) func([]string) (int, string, string) {
	return func([]string) (int, string, string) {
		*ran = true
		return 0, "{}\n", ""
	}
}
