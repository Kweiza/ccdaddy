package mcpsrv

import (
	"encoding/json"
	"strings"
	"testing"
)

// daemonToolNames is the class this file is about: the four controls over the
// process that outlives the session.
var daemonToolNames = []string{"daemon_start", "daemon_stop", "daemon_restart", "daemon_status"}

// All four are offered, described, and take no arguments -- so a caller that
// invents one is refused rather than having it ignored.
func TestToolsListOffersTheFourDaemonControlsAndNoneOfThemTakesAnArgument(t *testing.T) {
	tools := offeredTools(t)

	for _, name := range daemonToolNames {
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
		// No PROPERTIES, not merely nothing required. An optional argument is
		// the shape this would otherwise miss: it would be accepted, silently
		// ignored by a handler that never reads it, and reported to the model
		// as a thing it may pass.
		if len(s.Properties) != 0 {
			t.Errorf("%q declares %v; none of the daemon controls takes an argument", name, propertyNames(s))
		}
		if s.AdditionalProperties == nil || !bytesAreFalse(s.AdditionalProperties) {
			t.Errorf("%q accepts additional properties; an invented argument would be ignored instead of refused", name)
		}
	}
}

// Each control runs the ccdad command line a person would type. None spawns,
// signals or polls anything itself: the scoped-session refusal, the claimed
// credential home and the singleton wait all live in the command tree's
// pre-run, and a second implementation here is one that can disagree with it.
func TestEachDaemonToolRunsItsOwnCommandLineAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		tool string
		want []string
	}{
		{"daemon_start", []string{"daemon", "start"}},
		{"daemon_stop", []string{"daemon", "stop"}},
		{"daemon_restart", []string{"daemon", "restart"}},
		{"daemon_status", []string{"daemon", "status", "--json"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			var got []string
			e := func(argv []string) (int, string, string) {
				got = argv
				return 0, "{\"schemaVersion\":1,\"daemon\":{\"state\":\"running\"}}\n", ""
			}
			res := callTool(t, e, tc.tool, `{}`)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %v, want %v", got, tc.want)
			}
			if res.IsError {
				t.Fatalf("a clean run came back as an error: %s", contentText(res))
			}
		})
	}
}

// daemon_status is the only one of the four that carries a DOCUMENT, and it is
// the reason it is a read rather than an action: `ccdad daemon status --json`
// writes its payload and then exits non-zero for both of its other answers, so
// an envelope that dropped the document would throw away the whole answer.
func TestDaemonStatusReturnsTheDocumentAndTheOtherThreeReturnAChangedFlag(t *testing.T) {
	doc := "{\"schemaVersion\":1,\"daemon\":{\"state\":\"running\",\"pid\":4242}}\n"
	e := func([]string) (int, string, string) { return 0, doc, "" }

	res := callTool(t, e, "daemon_status", `{}`)
	var read ReadOut
	if err := json.Unmarshal([]byte(contentText(res)), &read); err != nil {
		t.Fatalf("daemon_status did not return a read envelope: %v (%s)", err, contentText(res))
	}
	if read.Document != doc {
		t.Errorf("Document = %q, want the exact bytes the command wrote", read.Document)
	}

	started := func([]string) (int, string, string) { return 0, "", "Started the ccdad daemon (pid 4242).\n" }
	out := actionOut(t, callTool(t, started, "daemon_start", `{}`))
	if !out.Changed {
		t.Error("Changed is false after a daemon was started")
	}
	if out.Command != "ccdad daemon start" {
		t.Errorf("Command = %q, want the command line as the CLI would take it", out.Command)
	}
}

// The exit taxonomy this group added two codes for, across the wire.
//
// 5 on a PROBE is a complete negative answer. 5 on `stop` is the Windows case
// where a daemon holds the lock and is listening for nothing -- also not a
// failure, and the reason daemon_stop's description says out loud that such a
// daemon is still running. 3 is "already as you asked" on both of the verbs
// that can be asked twice, and 1 is the answer that must never read as either.
func TestTheDaemonExitCodesCrossTheWireAsThemselves(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tool    string
		code    int
		isError bool
		says    string
	}{
		{"no daemon is running", "daemon_status", 5, false, "negative"},
		{"ccdad cannot tell whether one is", "daemon_status", 1, true, "failed"},
		{"one is already running", "daemon_start", 3, false, "already"},
		{"another store's engine holds the credential home", "daemon_start", 4, true, "blocked"},
		{"there was none to stop", "daemon_stop", 3, false, "already"},
		{"it holds the lock and listens for nothing", "daemon_stop", 5, false, "negative"},
		{"a restart that did not end with one running", "daemon_restart", 1, true, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := func([]string) (int, string, string) {
				return tc.code, "{\"schemaVersion\":1}\n", "a line for the operator\n"
			}
			res := callTool(t, e, tc.tool, `{}`)
			if res.IsError != tc.isError {
				t.Errorf("IsError = %v for exit %d, want %v: %s", res.IsError, tc.code, tc.isError, contentText(res))
			}
			if !strings.Contains(contentText(res), tc.says) {
				t.Errorf("the envelope does not say %q for exit %d: %s", tc.says, tc.code, contentText(res))
			}
		})
	}
}

// The annotations, and the two places the four of them genuinely disagree.
//
// restart is the one that is NOT idempotent: it answers 0 every time and leaves
// a different process behind each time, so a client that retried it on the
// strength of an idempotent hint would be abandoning a live tick per attempt.
// start is the one that is NOT destructive: it only ever adds a process.
func TestTheDaemonControlsDeclareWhichOfThemEndsAProcessAndWhichMayBeRetried(t *testing.T) {
	tools := offeredTools(t)
	for _, tc := range []struct {
		name        string
		readOnly    bool
		destructive bool
		idempotent  bool
	}{
		{"daemon_start", false, false, true},
		{"daemon_stop", false, true, true},
		{"daemon_restart", false, true, false},
		{"daemon_status", true, false, true},
	} {
		tool := tools[tc.name]
		if tool == nil || tool.Annotations == nil {
			t.Errorf("%q carries no annotations at all, so every hint on it defaults", tc.name)
			continue
		}
		if tool.Annotations.ReadOnlyHint != tc.readOnly {
			t.Errorf("%q readOnlyHint = %v, want %v", tc.name, tool.Annotations.ReadOnlyHint, tc.readOnly)
		}
		if tool.Annotations.IdempotentHint != tc.idempotent {
			t.Errorf("%q idempotentHint = %v, want %v", tc.name, tool.Annotations.IdempotentHint, tc.idempotent)
		}
		wire := annotationsOnTheWire(t, tc.name, tool)
		got, ok := wire["destructiveHint"]
		if !ok {
			t.Errorf("%q omits destructiveHint, and the protocol reads an omitted one as TRUE", tc.name)
		} else if bytesAreFalse(got) == tc.destructive {
			t.Errorf("%q destructiveHint = %s, want %v", tc.name, got, tc.destructive)
		}
		if got, ok := wire["openWorldHint"]; !ok {
			t.Errorf("%q omits openWorldHint, and the protocol reads an omitted one as TRUE", tc.name)
		} else if !bytesAreFalse(got) {
			t.Errorf("%q has openWorldHint = %s, want false: what it touches is a lock file on this machine", tc.name, got)
		}
	}
}

// The one fact about this group a model cannot get from the verb: the process
// keeps running, and keeps switching the live login, after the session ends.
// daemon_status is deliberately without it -- it starts nothing, so on that one
// the sentence would be describing something the tool did not do.
func TestTheDaemonToolsThatLeaveOneRunningSayItOutlivesTheSession(t *testing.T) {
	tools := offeredTools(t)
	const says = "after this session has ended"
	for _, name := range []string{"daemon_start", "daemon_restart"} {
		if !strings.Contains(tools[name].Description, says) {
			t.Errorf("%q does not say the daemon outlives the session: %q", name, tools[name].Description)
		}
	}
	for _, name := range []string{"daemon_stop", "daemon_status"} {
		if strings.Contains(tools[name].Description, says) {
			t.Errorf("%q claims to leave a process outliving the session; it leaves none: %q", name, tools[name].Description)
		}
	}
}

// The gate's map and the register have to agree about the spelling, and the
// underscore is where they would disagree: the command line spells these as two
// words, `ccdad daemon start`, and a tool named "daemon start" would be classed
// by nothing and refused by the gate before it ran.
func TestTheDaemonToolsAreNamedTheWayTheGateClassifiesThem(t *testing.T) {
	tools := offeredTools(t)
	for _, name := range daemonToolNames {
		if _, ok := tools[name]; !ok {
			t.Errorf("no tool is registered as %q", name)
		}
		if _, ok := classOf(name); !ok {
			t.Errorf("%q has no class verdict, so the gate would refuse it by name", name)
		}
	}
	if _, ok := tools["daemon"]; ok {
		t.Error("a bare `daemon` tool is registered; the group is four named controls, not a verb taking a subcommand")
	}
}
