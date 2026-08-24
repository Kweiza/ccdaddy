package mcpsrv

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// storeToolNames is the class this file is about: the five verbs that write
// ccdad's own account file and nothing else.
var storeToolNames = []string{"enable", "disable", "alias", "move", "primary"}

// tools/list is the contract a client sees before it calls anything. All five
// have to be there, described, and shaped so that an invented argument is
// refused rather than ignored -- and every one of them needs an account, since
// a store mutator with no target has nothing to write about.
func TestToolsListOffersTheFiveStoreMutatorsWithTheirSchemas(t *testing.T) {
	tools := offeredTools(t)

	for _, name := range storeToolNames {
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
		if s.AdditionalProperties == nil || !bytesAreFalse(s.AdditionalProperties) {
			t.Errorf("%q accepts additional properties; an invented argument would be ignored instead of refused", name)
		}
		if !slices.Contains(s.Required, "account") {
			t.Errorf("%q does not require an account: required = %v", name, s.Required)
		}
	}
}

// The two arguments that are not strings are not strings in the SCHEMA either,
// which is where it counts: a position of "second" and a primary of "maybe" are
// refused before the handler runs, by a validator whose message names the
// argument. Taking them as strings and parsing them in the handler would move
// both refusals after the fact and give each a second spelling.
func TestThePositionAndThePrimaryFlagAreTypedRatherThanSpelledAsStrings(t *testing.T) {
	tools := offeredTools(t)

	if got := propertyType(t, schemaOf(t, tools["move"]), "position"); got != "integer" {
		t.Errorf("move.position schema type = %q, want integer", got)
	}
	if got := propertyType(t, schemaOf(t, tools["primary"]), "primary"); got != "boolean" {
		t.Errorf("primary.primary schema type = %q, want boolean", got)
	}
	if s := schemaOf(t, tools["move"]); !slices.Contains(s.Required, "position") {
		t.Errorf("move does not require a position: required = %v", s.Required)
	}
}

// primary is REQUIRED, and this is the assertion standing between the schema
// and a very quiet accident. An optional boolean defaults to false, `false` is
// `off`, and `off` is a real state this tool puts accounts into -- so a caller
// that named an account and forgot to say which way it meant would have the
// ceiling put back on rather than being told it said nothing.
func TestOmittingThePrimaryFlagIsRefusedRatherThanReadAsOff(t *testing.T) {
	if s := schemaOf(t, offeredTools(t)["primary"]); !slices.Contains(s.Required, "primary") {
		t.Errorf("primary does not require the flag it sets: required = %v", s.Required)
	}

	var ran bool
	res := callTool(t, recording(&ran), "primary", `{"account":"work@example.com"}`)
	if ran {
		t.Fatal("the handler ran with no flag; the account would have been set to off")
	}
	if !res.IsError {
		t.Fatal("a primary call that says nothing was accepted")
	}
}

// The typed arguments, exercised the way a model gets them wrong: a position
// that is not a whole number and a primary flag spelled as the word the command
// line takes. Both are the schema's refusal, and in both the handler never runs.
func TestAnArgumentOfTheWrongTypeIsRefusedAndTheHandlerNeverRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
		args string
	}{
		{"a fractional position", "move", `{"account":"work@example.com","position":1.5}`},
		{"a position spelled as a word", "move", `{"account":"work@example.com","position":"second"}`},
		{"primary spelled as the command line's word", "primary", `{"account":"work@example.com","primary":"on"}`},
		{"an account that is a number rather than a reference", "enable", `{"account":42}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran bool
			res := callTool(t, recording(&ran), tc.tool, tc.args)
			if ran {
				t.Fatal("the handler ran on an argument the schema rejects")
			}
			if !res.IsError {
				t.Fatal("a type mismatch was accepted")
			}
		})
	}
}

// The annotation trap again, pointed the other way from read.go's. These five
// are not reads, and each hint has to say so explicitly: a nil DestructiveHint
// is the protocol's TRUE, a nil OpenWorldHint is the protocol's TRUE, and an
// unset IdempotentHint is a false denial of the exit-3 contract.
func TestTheStoreToolsDeclareWritingRatherThanLettingAHintDefault(t *testing.T) {
	tools := offeredTools(t)
	for _, name := range storeToolNames {
		tool := tools[name]
		if tool == nil || tool.Annotations == nil {
			t.Errorf("%q carries no annotations at all, so every hint on it defaults", name)
			continue
		}
		if tool.Annotations.ReadOnlyHint {
			t.Errorf("%q is marked read-only; it writes ccdad's account file", name)
		}
		if !tool.Annotations.IdempotentHint {
			t.Errorf("%q is not marked idempotent, and calling it twice is exactly what exit 3 answers", name)
		}
		wire := annotationsOnTheWire(t, name, tool)
		if got, ok := wire["destructiveHint"]; !ok {
			t.Errorf("%q omits destructiveHint, and the protocol reads an omitted one as TRUE", name)
		} else if bytesAreFalse(got) {
			t.Errorf("%q declares destructiveHint false, which claims it only ADDS; it overwrites what it finds", name)
		}
		if got, ok := wire["openWorldHint"]; !ok {
			t.Errorf("%q omits openWorldHint, and the protocol reads an omitted one as TRUE", name)
		} else if !bytesAreFalse(got) {
			t.Errorf("%q has openWorldHint = %s, want false: it writes one local file", name, got)
		}
	}
}

// Each store tool runs exactly the command line a person would type, and no
// tool invents a flag or a spelling the command tree does not have.
func TestEachStoreToolRunsItsOwnVerbTheWayAPersonWouldTypeIt(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args string
		want []string
	}{
		{"enable", `{"account":"work@example.com"}`, []string{"enable", "work@example.com"}},
		{"disable", `{"account":"2"}`, []string{"disable", "2"}},
		{"alias", `{"account":"work@example.com","alias":"work"}`, []string{"alias", "work@example.com", "work"}},
		{"alias", `{"account":"work","clear":true}`, []string{"alias", "work", "--clear"}},
		{"move", `{"account":"work","position":1}`, []string{"move", "work", "1"}},
		{"move", `{"account":"work","position":12}`, []string{"move", "work", "12"}},
		{"primary", `{"account":"work","primary":true}`, []string{"primary", "work", "on"}},
		{"primary", `{"account":"work","primary":false}`, []string{"primary", "work", "off"}},
	} {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			var got []string
			e := func(argv []string) (int, string, string) {
				got = argv
				return 0, "", "work@example.com is now enabled.\n"
			}
			res := callTool(t, e, tc.tool, tc.args)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %v, want %v", got, tc.want)
			}
			if res.IsError {
				t.Fatalf("a clean run came back as an error: %s", contentText(res))
			}
			out := actionOut(t, res)
			if out.Command != "ccdad "+strings.Join(tc.want, " ") {
				t.Errorf("Command = %q, want the command line as the CLI would take it", out.Command)
			}
			if !out.Changed {
				t.Error("Changed is false on exit 0; the action was taken")
			}
		})
	}
}

// The two alias calls that are neither a set nor a clear reach the command tree
// UNCHANGED, because that is where the rule about them lives. An alias given
// together with clear, and a call that gives neither, are already usage errors
// in `ccdad alias` -- re-deciding them here would be the second copy that drifts.
//
// The empty alias is the case worth pinning: Go cannot tell an omitted string
// from an empty one, so it is passed as if omitted, and the refusal the command
// tree gives -- "alias needs an alias to set, or --clear" -- is the sentence
// that is true for both.
func TestTheAliasCallsThatDecideNothingAreLeftToTheCommandTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		want []string
	}{
		{"neither an alias nor clear", `{"account":"work"}`, []string{"alias", "work"}},
		{"an empty alias", `{"account":"work","alias":""}`, []string{"alias", "work"}},
		{"both an alias and clear", `{"account":"work","alias":"w","clear":true}`, []string{"alias", "work", "w", "--clear"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			e := func(argv []string) (int, string, string) {
				got = argv
				return 2, "", "alias needs an alias to set, or --clear to remove the one it has\n"
			}
			res := callTool(t, e, "alias", tc.args)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %v, want %v", got, tc.want)
			}
			if !res.IsError {
				t.Error("a usage error came back as a success")
			}
		})
	}
}

// Exit 3 is the contract this whole class exists to carry across the wire. A
// model that reads "ok" for both "changed" and "already as asked" tells the
// person it disabled an account that was already disabled.
func TestNothingToDoCrossesTheWireAsAnAnswerWithChangedFalse(t *testing.T) {
	e := func([]string) (int, string, string) {
		return 3, "", "work@example.com is already disabled.\n"
	}
	res := callTool(t, e, "disable", `{"account":"work@example.com"}`)
	if res.IsError {
		t.Fatalf("exit 3 crossed the wire as a tool error: %s", contentText(res))
	}
	out := actionOut(t, res)
	if out.Changed {
		t.Error("Changed is true on exit 3; nothing moved")
	}
	if !strings.Contains(out.Meaning, "already") {
		t.Errorf("Meaning = %q, want it to say the world was already as asked", out.Meaning)
	}
	if !strings.Contains(out.Notices, "already disabled") {
		t.Errorf("Notices = %q, want the command's own line", out.Notices)
	}
}

// The other end of the same contract: a refusal the command tree made is an
// ERROR here, and it still reports nothing changed. Reading exit 2 as success
// would have the model report a rename that never happened.
func TestARefusalFromTheCommandTreeIsAnErrorThatChangedNothing(t *testing.T) {
	e := func([]string) (int, string, string) {
		return 2, "", "invalid alias: \"9\" is all digits, which is reserved for the display index\n"
	}
	res := callTool(t, e, "alias", `{"account":"work","alias":"9"}`)
	if !res.IsError {
		t.Fatal("a usage error came back as a success")
	}
	out := actionOut(t, res)
	if out.Changed {
		t.Error("Changed is true on exit 2; nothing was written")
	}
	if !strings.Contains(out.Notices, "all digits") {
		t.Errorf("Notices = %q, want the command's own refusal", out.Notices)
	}
}

// Two sentences the descriptions have to keep, in both directions.
//
// Every store tool says it writes the account file rather than the login,
// because this same server holds a tool that DOES rewrite the login and the
// model is reading one paragraph per tool. And none of them may claim it starts
// the daemon: not one of these five is on ccdad's auto-start list, so the
// sentence would be false -- the same failure the sentence exists to prevent,
// pointed the other way.
func TestEveryStoreToolSaysWhichFileItWritesAndNoneClaimsToStartTheDaemon(t *testing.T) {
	tools := offeredTools(t)
	for _, name := range storeToolNames {
		if !strings.Contains(tools[name].Description, "live Claude Code login is untouched") {
			t.Errorf("%q does not say it leaves the live login alone: %q", name, tools[name].Description)
		}
		if strings.Contains(tools[name].Description, "background daemon") {
			t.Errorf("%q claims it may start the daemon; it is not on the auto-start list: %q", name, tools[name].Description)
		}
	}
}

// actionOut reads the envelope a mutating tool returned. The result carries it
// as text as well as structured content, and the text is what a pre-SEP-2106
// client reads, so that is the copy these assertions check.
func actionOut(t *testing.T, res *mcp.CallToolResult) ActionOut {
	t.Helper()
	var out ActionOut
	if err := json.Unmarshal([]byte(contentText(res)), &out); err != nil {
		t.Fatalf("the result is not the action envelope: %v (%s)", err, contentText(res))
	}
	return out
}

// propertyType is the declared type of one property, read off the wire rather
// than off the Go struct: what a client validates against is what travelled.
func propertyType(t *testing.T, s wireSchema, name string) string {
	t.Helper()
	raw, ok := s.Properties[name]
	if !ok {
		t.Fatalf("no %q property in the schema", name)
	}
	var p struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("reading the %q property back: %v", name, err)
	}
	return p.Type
}
