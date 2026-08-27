package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// The `--json` contract is one rule for the whole command tree: every read
// command emits a single machine-readable object on stdout, human notices go
// to stderr, every payload carries schemaVersion, and the contract is additive.
// Each command's own test file proves its own payload. This file proves the
// part that only exists BETWEEN commands — that nine `--json` surfaces answer
// to one shape — and it is a table so that the tenth costs one row rather than
// a decision.
//
// Four rules, and each is here because of a specific way a later command
// breaks the contract while its own tests stay green:
//
//  1. One object, with a schemaVersion, and nothing else on stdout. A command
//     that prints one friendly line beside its payload ends `ccdad … | jq`.
//  2. A negative answer still writes its payload AND still exits non-zero.
//     `which --json` does both (which.go), and so must every read command that
//     can answer no: one that instead wrote nothing on a negative answer would
//     break the contract silently, because a consumer only finds out on the
//     day the answer goes negative. The flag changes the representation, never
//     the answer — so the exit code is identical with and without it.
//  3. The document is INDENTED and `auto`'s stream is not. The contract's one
//     exception is NDJSON, and the two encoders must never converge: see the
//     comment on TestJSONContractOnlyAutoIsLineOriented for what that
//     asymmetry buys.
//  4. A stdout whose reader has gone away is exit 0, and any OTHER write
//     failure is exit 1. Both halves are needed to rule out both wrong
//     implementations — see TestJSONContractWriteFailures.
//  5. Every timestamp in a document is in ONE zone, and it is the machine's.
//     A payload builder that formats a moment itself is how the zone drifts:
//     the sites that call .In(jsonZone()) agree with each other and the ones
//     that forget publish whatever zone their input arrived carrying. See
//     TestJSONContractRendersEveryTimeInOneZone.
//
// The sixth test is the one that makes the other five keep working:
// TestJSONContractCoversEveryJSONCommand walks the tree and fails when a
// command offers --json and has no row here.

// jsonContractCase is one read command's answer, as the table sees it.
type jsonContractCase struct {
	// path is the command's path under ccdad, and doubles as the key the
	// coverage check matches against the tree. Positional arguments do not
	// belong here — they go in args.
	path string
	// name distinguishes two rows for one command. Empty means path is the name.
	name string
	// args are everything after the path, --json included.
	args []string
	// setup builds the machine this row describes, inside a world isolate()
	// has already sandboxed.
	setup func(*testing.T)
	// want is the exit code. It is asserted against the human form too: the
	// flag may not change the answer.
	want ExitCode
	// keys are the top-level payload keys this row must carry, over and above
	// schemaVersion. They are the contract a consumer writes against; adding
	// one is additive, removing one is what this pins.
	keys []string
	// stream marks the contract's one exception, `auto --json`, whose stdout is
	// NDJSON rather than a document.
	stream bool
}

func (c jsonContractCase) argv() []string {
	return append(strings.Fields(c.path), c.args...)
}

// humanArgv is the same invocation without the flag, for the exit-code parity
// rule.
func (c jsonContractCase) humanArgv() []string {
	argv := c.argv()
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if a == "--json" {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (c jsonContractCase) title() string {
	if c.name != "" {
		return c.name
	}
	return c.path
}

// jsonContractCases is the table. Every command in the tree that offers --json
// has at least one row, and every command that can answer NEGATIVELY has a row
// for each half: the negative rows are the ones rule 2 is about, and a table
// carrying only the happy answers would assert nothing about the asymmetry it
// exists to pin.
func jsonContractCases() []jsonContractCase {
	stopped := func(t *testing.T) {
		t.Helper()
		stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
	}
	return []jsonContractCase{{
		path:  "list",
		args:  []string{"--json"},
		setup: seedHealthyMachine,
		want:  ExitOK,
		keys:  []string{"accounts"},
	}, {
		path:  "which",
		name:  "which/attributed",
		args:  []string{"--json"},
		setup: seedHealthyMachine,
		want:  ExitOK,
		keys:  []string{"attributed", "via", "account"},
	}, {
		path: "which",
		name: "which/unattributed",
		args: []string{"--json"},
		setup: func(t *testing.T) {
			t.Helper()
			seedAccount(t, "u-1", "a@example.com")
		},
		want: ExitProbeNegative,
		keys: []string{"attributed", "via"},
	}, {
		path: "status",
		args: []string{"--json"},
		setup: func(t *testing.T) {
			t.Helper()
			seedHealthyMachine(t)
			stopped(t)
		},
		want: ExitOK,
		keys: []string{"daemon", "accounts"},
	}, {
		path: "daemon status",
		name: "daemon status/running",
		args: []string{"--json"},
		setup: func(t *testing.T) {
			t.Helper()
			stubDaemon(t, daemon.Report{State: daemon.DaemonRunning}, nil)
		},
		want: ExitOK,
		keys: []string{"daemon"},
	}, {
		path:  "daemon status",
		name:  "daemon status/not running",
		args:  []string{"--json"},
		setup: stopped,
		want:  ExitProbeNegative,
		keys:  []string{"daemon"},
	}, {
		path: "daemon status",
		name: "daemon status/cannot tell",
		args: []string{"--json"},
		// The third answer, and the one the exit contract split 5 off from 1
		// for: a lock that cannot be probed is not "no daemon". Nothing else in
		// the package reaches this command's --json branch for it, so without
		// this row the arm that keeps the answer silent AND non-zero is
		// unexecuted code.
		setup: func(t *testing.T) {
			t.Helper()
			stubDaemon(t, daemon.Report{State: daemon.DaemonUnknown}, daemon.ErrLocksUnsupported)
		},
		want: ExitFailure,
		keys: []string{"daemon"},
	}, {
		path: "runway",
		args: []string{"--json"},
		// A machine with no series behind it, which is the row that matters:
		// `forecast` is promised unconditionally, so it has to be here on the
		// document that has the least to say. A row seeded with a measured
		// fleet would pass while the key vanished on every cold machine.
		setup: seedHealthyMachine,
		want:  ExitOK,
		keys:  []string{"forecast"},
	}, {
		path:  "doctor",
		name:  "doctor/healthy",
		args:  []string{"--json"},
		setup: seedHealthyMachine,
		want:  ExitOK,
		keys:  []string{"ok", "checks"},
	}, {
		path: "doctor",
		name: "doctor/failing",
		args: []string{"--json"},
		setup: func(t *testing.T) {
			t.Helper()
			seedHealthyMachine(t)
			// The loudest form of the drift that doctor exists to catch, and
			// doctor's own suite's fixture for it: a credentials file ccdad
			// cannot parse is a levelFail, so this row exits 1 with a full
			// report on stdout.
			writeLiveFile(t, "this is not json")
		},
		want: ExitFailure,
		keys: []string{"ok", "checks"},
	}, {
		path: "config get",
		name: "config get/set",
		args: []string{"threshold", "--json"},
		setup: func(t *testing.T) {
			t.Helper()
			if code, _, _, top := runRoot(t, "config", "set", "threshold", "70"); code != ExitOK {
				t.Fatalf("setup config set = %d (%s)", code, top)
			}
		},
		want: ExitOK,
		keys: []string{"key", "value", "set", "source"},
	}, {
		path: "config get",
		name: "config get/unset",
		args: []string{"threshold", "--json"},
		want: ExitProbeNegative,
		keys: []string{"key", "value", "set", "source"},
	}, {
		path: "config list",
		args: []string{"--json"},
		want: ExitOK,
		keys: []string{"path", "keys"},
	}, {
		path: "config path",
		args: []string{"--json"},
		want: ExitOK,
		keys: []string{"path", "home", "exists"},
	}, {
		path:   "auto",
		name:   "auto/switched",
		args:   []string{"--once", "--json"},
		setup:  twoAccountsOneBetter,
		want:   ExitOK,
		keys:   []string{"kind", "at"},
		stream: true,
	}, {
		path: "auto",
		name: "auto/nothing to do",
		args: []string{"--once", "--json"},
		// Already on the best account. Exit 3, and a rendered answer like any
		// other: the stream carries the evaluation that reached it.
		setup: func(t *testing.T) {
			t.Helper()
			seedAccount(t, "u-1", "a@example.com")
			seedAccount(t, "u-2", "b@example.com")
			if code, _, _, top := runRoot(t, "switch", "2"); code != ExitOK {
				t.Fatalf("setup switch = %d (%s)", code, top)
			}
			seedUsage(t, "u-1", 10)
			seedUsage(t, "u-2", 80)
			clearCooldown(t)
		},
		want:   ExitNothingToDo,
		keys:   []string{"kind", "at"},
		stream: true,
	}, {
		path: "auto",
		name: "auto/blocked",
		args: []string{"--once", "--json"},
		// Wanted to move and could not, for want of any reading to rank on.
		// Exit 4 is the code a supervisor alerts on, which is the one this
		// table can least afford to leave unpinned.
		setup: func(t *testing.T) {
			t.Helper()
			seedAccount(t, "u-1", "a@example.com")
			seedAccount(t, "u-2", "b@example.com")
		},
		want:   ExitBlocked,
		keys:   []string{"kind", "at"},
		stream: true,
	}, {
		path: "hover",
		name: "hover/on",
		args: []string{"status", "--json"},
		setup: func(t *testing.T) {
			t.Helper()
			seedHealthyMachine(t)
			writeConfig(t, "hover = true\n")
		},
		want: ExitOK,
		keys: []string{"hover", "usableAccounts", "windows"},
	}, {
		path: "hover",
		name: "hover/off",
		// The mode is off, which is a negative answer to a probe rather than a
		// failure: the payload is still written, and the exit code is still 5.
		setup: seedHealthyMachine,
		args:  []string{"status", "--json"},
		want:  ExitProbeNegative,
		keys:  []string{"hover", "usableAccounts", "windows"},
	}, {
		path: "update",
		args: []string{"--json"},
		// The dev-build refusal is the arm this table can reach with no origin
		// at all: it fires before the network, writes its payload, exits 4, and
		// returns an error that is already silent.
		setup: func(t *testing.T) {
			t.Helper()
			_ = stubReleaseKeys(t)
			stubVersion(t, "dev")
		},
		want: ExitBlocked,
		keys: []string{"currentVersion", "updated", "reason"},
	}}
}

// inContractWorld builds the machine one row describes and runs f in it.
func inContractWorld(t *testing.T, c jsonContractCase, f func(t *testing.T)) {
	t.Helper()
	isolate(t)
	if c.setup != nil {
		c.setup(t)
	}
	f(t)
}

// Rule 1. One object on stdout, carrying a schemaVersion and the keys a
// consumer was promised, with every human word on stderr.
//
// "Nothing else on stdout" is checked by decoding and then requiring EOF, which
// is the assertion a stray Fprintln fails: a payload followed by a friendly
// line still unmarshals if you only look at the first document.
func TestJSONContractOneObjectPerReadCommand(t *testing.T) {
	for _, c := range jsonContractCases() {
		t.Run(c.title(), func(t *testing.T) {
			// A row with no keys would satisfy this rule on schemaVersion
			// alone, while the coverage test — which matches on path — still
			// reported the tree fully covered. That is a row that looks like
			// coverage and asserts nothing about the payload.
			if len(c.keys) == 0 {
				t.Fatalf("this row promises no keys, so it pins nothing a consumer reads")
			}
			inContractWorld(t, c, func(t *testing.T) {
				code, stdout, _, top := runRoot(t, c.argv()...)
				if code != c.want {
					t.Fatalf("exit = %d, want %d (ExecuteWith said %q)", code, c.want, top)
				}
				if c.stream {
					for i, ev := range decodeContractStream(t, stdout) {
						requireContractKeys(t, ev, c.keys, fmt.Sprintf("event %d", i+1))
					}
					return
				}
				requireContractKeys(t, decodeContractDocument(t, stdout), c.keys, "payload")
			})
		})
	}
}

// Rule 2, the asymmetry. `which --json` on a negative answer writes its full
// payload to stdout AND exits 5, and every read command that can answer no has
// to make the same choice: a command that wrote nothing instead would look
// identical in its own tests and break the contract on the day the answer went
// negative.
//
// The silent half is the other party to it. A negative answer is not a runtime
// failure, so ExecuteWith must not print `ccdad: …` on top of a document that
// already says what happened — a caller reading stdout would otherwise get the
// answer twice, in two formats, on two streams.
func TestJSONContractNegativeAnswersStillCarryTheirPayload(t *testing.T) {
	negatives := map[string]bool{}
	for _, c := range jsonContractCases() {
		if c.want == ExitOK {
			continue
		}
		negatives[c.path] = true
		t.Run(c.title(), func(t *testing.T) {
			inContractWorld(t, c, func(t *testing.T) {
				code, stdout, _, top := runRoot(t, c.argv()...)
				if code != c.want {
					t.Fatalf("exit = %d, want %d (%s)", code, c.want, top)
				}
				if c.stream {
					// The stream's form of "the payload is still there": a
					// non-zero `auto` still reports the evaluation that
					// reached the answer, rather than going quiet on it.
					for i, ev := range decodeContractStream(t, stdout) {
						requireContractKeys(t, ev, c.keys, fmt.Sprintf("event %d", i+1))
					}
				} else {
					requireContractKeys(t, decodeContractDocument(t, stdout), c.keys, "payload")
				}
				if top != "" {
					t.Errorf("ExecuteWith also printed %q; a rendered answer is not a runtime failure, "+
						"so it must come back as a silent error carrying its code", top)
				}
			})
		})
	}
	// A COUNT here would pass vacuously in the one way that matters: the four
	// commands below answer non-zero for four different reasons, so a guard
	// that only counted rows would let `which` — the command this rule is
	// named after — be deleted and stay green on the strength of the other
	// three. Naming them is what makes the guard about coverage rather than
	// about arithmetic.
	for _, path := range []string{"which", "daemon status", "config get", "doctor", "auto", "hover"} {
		if !negatives[path] {
			t.Errorf("`ccdad %s` has a rendered non-zero answer and no negative row in the table", path)
		}
	}
}

// The flag changes the representation, never the answer. A supervisor doing
// `ccdad which --json >/dev/null || restart` has to get the same verdict as one
// running the human form, and a command that computed its exit code inside the
// --json branch could disagree with itself for a whole release.
//
// Each half runs in its own world because the `auto` rows have side effects:
// `auto --once` switches the live login, so a second run against the same
// machine answers 3 for reasons that have nothing to do with the flag. Every
// other row reads, and the writes in their setups are idempotent — but the
// worlds are separate for all of them, because a rule that held only for the
// rows someone remembered to classify is not a rule.
func TestJSONContractDoesNotChangeTheExitCode(t *testing.T) {
	for _, c := range jsonContractCases() {
		t.Run(c.title(), func(t *testing.T) {
			var machine, human ExitCode
			var machineTop, humanTop string
			t.Run("json", func(t *testing.T) {
				inContractWorld(t, c, func(t *testing.T) {
					machine, _, _, machineTop = runRoot(t, c.argv()...)
				})
			})
			t.Run("human", func(t *testing.T) {
				inContractWorld(t, c, func(t *testing.T) {
					human, _, _, humanTop = runRoot(t, c.humanArgv()...)
				})
			})
			if machine != human {
				t.Fatalf("exit = %d with --json (%s) and %d without it (%s)",
					machine, machineTop, human, humanTop)
			}
		})
	}
}

// Rule 3, first half: a document spans lines, on purpose.
//
// writeJSON calls SetIndent, so "a single object" is several lines of it. That
// is this repository's own choice — jq-friendly, and unusable by anything
// line-oriented — and it is worth an assertion rather than a comment for two
// reasons. It is what makes reusing writeJSON for `auto`'s stream fail LOUDLY
// instead of producing a stream nothing can read; and it is how this table
// notices a command that stopped going through writeJSON altogether, which
// would otherwise satisfy every other rule while emitting a shape no other
// command emits.
func TestJSONContractDocumentsAreIndented(t *testing.T) {
	for _, c := range jsonContractCases() {
		if c.stream {
			continue
		}
		t.Run(c.title(), func(t *testing.T) {
			inContractWorld(t, c, func(t *testing.T) {
				_, stdout, _, _ := runRoot(t, c.argv()...)
				if !strings.Contains(stdout, "\n  \"") {
					t.Errorf("the payload is not indented, so this command is not going through writeJSON:\n%s", stdout)
				}
			})
		})
	}
}

// Rule 3, second half: `auto` is the only line-oriented surface, and it must
// stay that way in both directions.
//
// A stream that went through writeJSON would arrive as `{`, `  "kind": …`,
// `}` — every line a fragment, `head -1` returning an opening brace. So the
// assertion is not "auto emits JSON" but "every LINE is a whole object",
// which is the property that fails the moment someone reuses the shared
// helper. decodeContractStream is deliberately strict about blank lines too:
// a blank line is not an NDJSON record, and a reader that skips them cannot
// tell a gap from a dropped event.
func TestJSONContractOnlyAutoIsLineOriented(t *testing.T) {
	streams := map[string]bool{}
	for _, c := range jsonContractCases() {
		if !c.stream {
			continue
		}
		streams[c.path] = true
		t.Run(c.title(), func(t *testing.T) {
			inContractWorld(t, c, func(t *testing.T) {
				_, stdout, _, _ := runRoot(t, c.argv()...)
				decodeContractStream(t, stdout)
				for i, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
					if strings.HasPrefix(line, " ") {
						t.Fatalf("line %d begins with a space, so the stream is indented — "+
							"NDJSON must not go through writeJSON: %q", i+1, line)
					}
				}
			})
		})
	}
	if len(streams) != 1 || !streams["auto"] {
		t.Fatalf("%v are marked as streams; the `--json` contract has exactly one "+
			"exception and it is `auto --json`", streams)
	}
}

// Rule 4. A stdout whose reader has gone away is exit 0; any other write
// failure is exit 1.
//
// Both halves earn their place, and so does the message assertion in the
// second one — together they leave a swallowed write nowhere to hide, and each
// on its own has rows where it cannot see one:
//
//   - A command that maps every write failure to an error fails the first half
//     (`ccdad list --json | head -1` exits 0).
//   - A command that SWALLOWS write failures — the bufio.Writer flushed in a
//     defer, which is how this arrives in practice — still exits with whatever
//     code its ANSWER earned. On a row whose answer is 0 that is
//     indistinguishable from a clean write, so the first half says nothing
//     there; the second half catches it, because 1 is not a code a swallowed
//     write produces. The one row where even that agrees is `doctor/failing`,
//     which exits 1 either way — and there it is the missing `ccdad: …` line
//     that tells the two apart. Verified by mutation, all three ways.
func TestJSONContractWriteFailures(t *testing.T) {
	other := errors.New("the destination went away for some other reason")
	for _, c := range jsonContractCases() {
		t.Run(c.title(), func(t *testing.T) {
			t.Run("broken pipe is not a failure", func(t *testing.T) {
				inContractWorld(t, c, func(t *testing.T) {
					code, _, top := runRootTo(t, failingWriter{errBrokenPipeForTest}, c.argv()...)
					if code != ExitOK {
						t.Fatalf("exit = %d, want 0: a reader that has gone away is not an error", code)
					}
					if top != "" {
						t.Errorf("ExecuteWith printed %q; there is nobody left to read it", top)
					}
				})
			})
			t.Run("any other write failure is one", func(t *testing.T) {
				inContractWorld(t, c, func(t *testing.T) {
					code, _, top := runRootTo(t, failingWriter{other}, c.argv()...)
					if code != ExitFailure {
						t.Fatalf("exit = %d, want 1: reporting an answer nobody received claims an output "+
							"that was never written", code)
					}
					if !strings.Contains(top, other.Error()) {
						t.Errorf("ExecuteWith said %q, want the write failure named", top)
					}
				})
			})
		})
	}
}

// The coverage rule, and the reason this file is a table at all: a read command
// added later inherits every rule above by adding one row, and fails this test
// until it does.
//
// Both directions are checked. A row for a command that no longer offers --json
// is just as wrong as a command with no row: it passes silently while asserting
// nothing about the tree as it now stands.
// Rule 5. One zone per document, and it is the machine's.
//
// This is the rule that only exists BETWEEN commands. Each payload builder is
// correct on its own: engineJSON puts a time.Time in the map and lets the
// encoder render it, warmupJSON formats its own string, and both are readable
// in isolation. What they cannot see is that the moments reaching them come
// from different places — time.Now() carries the machine's offset, a window's
// resets_at is parsed off a wire string ending in Z and carries UTC — so a
// document assembled from several of them carries several zones. A live store
// held five poll times at +09:00 beside one at Z, and the Z row read as nine
// hours overdue while being four minutes in the future.
//
// The zone is pinned rather than read, for the reason pinJSONZone gives:
// nothing sets TZ in CI, so time.Local is UTC there and every row the bug
// leaves behind already ends in Z.
func TestJSONContractRendersEveryTimeInOneZone(t *testing.T) {
	seen := 0
	for _, c := range jsonContractCases() {
		t.Run(c.title(), func(t *testing.T) {
			inContractWorld(t, c, func(t *testing.T) {
				pinJSONZone(t, time.FixedZone("KST", 9*60*60))
				_, stdout, _, _ := runRoot(t, c.argv()...)
				for _, got := range jsonStamps(stdout) {
					seen++
					if !strings.HasSuffix(got, "+09:00") {
						t.Errorf("%s is not in the document's zone:\n%s", got, stdout)
					}
				}
			})
		})
	}
	// Without this the rule passes on a table whose fixtures stopped producing
	// moments at all, which is a green test asserting nothing.
	if seen == 0 {
		t.Error("no row in this table emitted a single timestamp, so this rule decided nothing")
	}
}

func TestJSONContractCoversEveryJSONCommand(t *testing.T) {
	inTree := map[string]bool{}
	for _, path := range jsonCommandPaths(NewRootCmd()) {
		inTree[path] = true
	}
	if len(inTree) == 0 {
		t.Fatal("no command in the tree offers --json; the walk is looking in the wrong place")
	}

	covered := map[string]bool{}
	for _, c := range jsonContractCases() {
		covered[c.path] = true
	}
	for path := range inTree {
		if !covered[path] {
			t.Errorf("`ccdad %s` offers --json and has no row in the `--json` contract table; "+
				"add one to jsonContractCases", path)
		}
	}
	for path := range covered {
		if !inTree[path] {
			t.Errorf("the table has a row for `ccdad %s`, which no longer offers --json", path)
		}
	}
}

// jsonCommandPaths lists every command in the tree that ACCEPTS --json, by path
// with the binary name stripped.
//
// Accepts, not declares, and through LocalFlags/InheritedFlags rather than
// Flags(): Cobra's Flags() is only the set built by Flags().XxxVar until
// mergePersistentFlags has run, so a command that declared --json on its own
// PersistentFlags() would be invisible to a walk over Flags() — a false
// NEGATIVE, and the one direction that matters here, because it would let a
// whole command escape the table without failing anything. LocalFlags and
// InheritedFlags both run that merge, and between them they see a --json the
// command declared either way and one it inherits from a group above it.
//
// Runnable is the second half: a group like `config` cannot emit a payload, so
// a persistent --json declared there belongs to its children, which is exactly
// where InheritedFlags puts it.
func jsonCommandPaths(cmd *cobra.Command) []string {
	var out []string
	if cmd.Runnable() && (cmd.LocalFlags().Lookup("json") != nil || cmd.InheritedFlags().Lookup("json") != nil) {
		out = append(out, strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "))
	}
	for _, sub := range cmd.Commands() {
		out = append(out, jsonCommandPaths(sub)...)
	}
	return out
}

// decodeContractDocument is the contract's "a single object", as an assertion:
// one JSON OBJECT, carrying schemaVersion as a number, with nothing after it.
func decodeContractDocument(t *testing.T, stdout string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("stdout is not one JSON object (%v):\n%s", err, stdout)
	}
	// Everything after the document, which is where a stray human line lands.
	// Decoding the first value alone would not see it.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout carries more than the one document the `--json` contract "+
			"promises (%q after it, %v):\n%s", extra, err, stdout)
	}
	if payload["schemaVersion"] != float64(1) {
		t.Fatalf("schemaVersion = %v (%T), want the number 1", payload["schemaVersion"], payload["schemaVersion"])
	}
	return payload
}

// decodeContractStream is the same assertion for the contract's one exception:
// one complete object per line, each carrying its own schemaVersion, and no
// blank lines standing in for records.
func decodeContractStream(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	if strings.HasPrefix(strings.TrimSpace(stdout), "[") {
		t.Fatalf("the stream is wrapped in an array; NDJSON is one object per line:\n%s", stdout)
	}
	// Said before the split, because splitting "" yields one empty line and the
	// loop below would then blame a blank record for a stream that was never
	// written at all — a muted command reported as a formatting bug.
	if strings.TrimRight(stdout, "\n") == "" {
		t.Fatal("the stream is empty; a rendered answer that emits no event has nothing for a consumer to read")
	}
	var events []map[string]any
	for i, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if line == "" {
			t.Fatalf("line %d is empty; a blank line is not an NDJSON record", i+1)
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d does not parse on its own (%v): %q", i+1, err, line)
		}
		if ev["schemaVersion"] != float64(1) {
			t.Fatalf("line %d: schemaVersion = %v, want the number 1: %q", i+1, ev["schemaVersion"], line)
		}
		events = append(events, ev)
	}
	return events
}

func requireContractKeys(t *testing.T, payload map[string]any, keys []string, where string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := payload[k]; !ok {
			t.Errorf("%s carries no %q; the contract is additive, so a key a consumer was promised cannot leave",
				where, k)
		}
	}
}

// failingWriter is a stdout that cannot be written to. The error is the whole
// fixture: which one it is decides whether the answer is exit 0 or exit 1.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// runRootTo is runRoot with a caller-supplied stdout, which is the only way to
// hand a command a writer that fails. It returns stderr and what ExecuteWith
// itself printed — the second is what tells a silent error from a reported one.
func runRootTo(t *testing.T, stdout io.Writer, args ...string) (code ExitCode, stderr, top string) {
	t.Helper()
	root := NewRootCmd()
	var errOut, topBuf strings.Builder
	root.SetOut(stdout)
	root.SetErr(&errOut)
	root.SetArgs(explicitArgs(args))
	code = ExecuteWith(root, &topBuf)
	return code, errOut.String(), topBuf.String()
}

// pipeRoleEnv puts a re-executed copy of this test binary into the role of the
// ccdad binary, with the argument line to run. internal/daemon's TestMain does
// the same thing for the same reason: some guarantees are properties of a real
// process and cannot be asserted by constructing a value.
const pipeRoleEnv = "CCDAD_TEST_PIPE_ARGV"

// mcpRoleEnv turns a re-executed copy of this binary into the ccdad binary
// running `ccdad mcp`, so one test can drive the real server over a real pipe
// through the protocol library's own command transport. Some guarantees are
// properties of a real process -- that nothing but protocol reaches stdout,
// that the handshake completes against a client that did not build the server
// -- and cannot be asserted by constructing a value.
//
// It is a role of its own rather than a reuse of the pipe role above: that one
// splits its argument line on whitespace to pin a broken-pipe behaviour, and
// the two fixtures answer different questions. A change to either would
// otherwise silently alter the other.
const mcpRoleEnv = "CCDAD_TEST_MCP_SERVER"

// argvRoleEnv turns a re-executed copy of this binary into a program that
// records the argument vector it was actually given, into the file the
// variable names. cmdshim_windows_test.go uses it as a stand-in for node: what
// an argument looks like AFTER Windows has parsed the command line is not
// something any in-process assertion can reach.
//
// It writes to a file rather than to stdout because runChild hands the child
// the real os.Stdout, and swapping that out under a running test to read one
// line back is a larger intrusion than an extra environment variable.
const argvRoleEnv = "CCDAD_TEST_ARGV_ECHO"

// updateAssetRoleEnv turns a re-executed copy of this binary into the release
// asset `ccdad update` has just staged. json_contract_test.go owns the roles
// for this package, so the branch lives in its TestMain.
const updateAssetRoleEnv = "CCDAD_TEST_UPDATE_ASSET"

// updateAssetRoleFail is the value that makes the role exit non-zero without
// printing, which is a staged binary that runs and is not a ccdad.
const updateAssetRoleFail = "fail"

// updateAssetRoleEnvFile, when set, makes the staged-asset role record how it
// was invoked into the file this names.
//
// It writes to a file for the reason argvRoleEnv does: the child's stdout
// belongs to whoever ran it — here it is what the smoke run reads the version
// line out of — and swapping that out under a running test is a larger
// intrusion than an extra variable.
const updateAssetRoleEnvFile = "CCDAD_TEST_UPDATE_ASSET_ENVFILE"

// updateAssetRecord is what that file holds. Both halves exist only inside the
// child: the smoke run is a real exec, so neither the environment it was handed
// nor the argument vector it was given can be observed in this process.
type updateAssetRecord struct {
	ChildEnv string   `json:"childEnv"`
	Argv     []string `json:"argv"`
}

func TestMain(m *testing.M) {
	// The staged release asset. `ccdad update` EXECUTES what it downloaded
	// before it installs it, and the only file that is both a real executable
	// on all three operating systems and able to answer --version is a copy of
	// this test binary. The value is what it prints; a role that must fail
	// instead prints nothing and exits non-zero.
	if v := os.Getenv(updateAssetRoleEnv); v != "" {
		// Before the fail check, so the record is written on both arms: what
		// was asked of the child is worth pinning whether or not the child is
		// pretending to be a binary that cannot answer.
		if out := os.Getenv(updateAssetRoleEnvFile); out != "" {
			enc, err := json.Marshal(updateAssetRecord{
				ChildEnv: os.Getenv(daemon.ChildEnvVar),
				Argv:     os.Args[1:],
			})
			if err != nil {
				os.Exit(70)
			}
			if err := os.WriteFile(out, enc, 0o600); err != nil {
				os.Exit(71)
			}
		}
		if v == updateAssetRoleFail {
			os.Exit(9)
		}
		fmt.Println(v)
		os.Exit(0)
	}
	if out := os.Getenv(argvRoleEnv); out != "" {
		enc, err := json.Marshal(os.Args[1:])
		if err != nil {
			os.Exit(70)
		}
		if err := os.WriteFile(out, enc, 0o600); err != nil {
			os.Exit(71)
		}
		os.Exit(0)
	}
	if os.Getenv(mcpRoleEnv) != "" {
		os.Args = []string{os.Args[0], "mcp"}
		os.Exit(int(Execute()))
	}
	if argv := os.Getenv(pipeRoleEnv); argv != "" {
		// Execute() itself, not a re-implementation of it. ignoreSIGPIPE is
		// called from inside Execute and nowhere else, so a role that called it
		// by hand would leave the one line this fixture exists to pin
		// unexercised — and deleting it from Execute would still pass.
		os.Args = append([]string{os.Args[0]}, strings.Fields(argv)...)
		os.Exit(int(Execute()))
	}
	os.Exit(m.Run())
}

// A closed reader is not an error, end to end: `ccdad list --json | head -1`
// exits 0.
//
// Every other test in this file injects the write error, which asserts the
// MAPPING in ExecuteWith and nothing about how a real closed pipe arrives. On
// Unix it arrives as SIGPIPE, whose default disposition kills the process with
// status 141 before any write ever returns an error — so without
// ignoreSIGPIPE the EPIPE arm is unreachable dead code and every injected test
// in this file still passes. Only a real process writing into a real pipe with
// no reader tells the two apart.
//
// The reader is closed BEFORE the child starts, rather than racing it: `head
// -1` that has already exited is the case that matters, and a handshake would
// be the only other way to make the write land after the close.
func TestJSONContractAClosedReaderExitsZero(t *testing.T) {
	for _, argv := range []string{"list --json", "auto --once --json"} {
		t.Run(argv, func(t *testing.T) {
			// The child gets this test's sandbox through the environment,
			// which t.Setenv has already put on the real process: the store,
			// both Claude Code homes, and — because
			// CLAUDE_SECURESTORAGE_CONFIG_DIR is DEFINED — auto-start's third
			// rule, so no daemon is spawned into a directory the framework is
			// about to delete.
			isolate(t)
			twoAccountsOneBetter(t)

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}

			var stderr strings.Builder
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(),
				pipeRoleEnv+"="+argv,
				// The recursion fuse, belt to the environment's braces: this is
				// the one axis auto-start checks before anything else.
				daemon.ChildEnvVar+"=1",
			)
			cmd.Stdout = w
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// The parent's copy of the write end, closed so the child holds the
			// only one. Nothing here reads, which is the point.
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			if err := cmd.Wait(); err != nil {
				t.Fatalf("`ccdad %s` into a pipe with no reader exited %v, want 0 — "+
					"a closed reader is not an error.\nstderr: %s", argv, err, stderr.String())
			}
		})
	}
}
