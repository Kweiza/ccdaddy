package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// twoAccountsOneBetter is the ordinary engine setup: u-1 is live and nearly
// spent, u-2 has room.
func twoAccountsOneBetter(t *testing.T) {
	t.Helper()
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 10)
	seedUsage(t, "u-2", 80)
	clearCooldown(t)
}

// decodeNDJSON parses a stream and fails on anything that is not one complete
// object per line.
func decodeNDJSON(t *testing.T, out string) []map[string]any {
	t.Helper()
	if strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Fatalf("the stream is wrapped in an array; NDJSON is one object per line:\n%s", out)
	}
	var events []map[string]any
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			t.Fatalf("line %d is empty; a blank line is not an NDJSON record", i+1)
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d does not parse on its own (%v): %q", i+1, err, line)
		}
		if ev["schemaVersion"] == nil {
			t.Fatalf("line %d carries no schemaVersion: %q", i+1, line)
		}
		if ev["kind"] == nil {
			t.Fatalf("line %d carries no kind: %q", i+1, line)
		}
		events = append(events, ev)
	}
	return events
}

func kindsOf(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e["kind"].(string))
	}
	return out
}

func TestAutoOnceSwitchesToTheAccountWithRoom(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)

	code, out, errOut, top := runRoot(t, "auto", "--once")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty without --json", out)
	}
	if got := liveUUIDOf(t); got != "u-2" {
		t.Fatalf("live account = %q, want u-2", got)
	}
}

// §9.3's whole point: a no-op is 3, and it is NOT the code a typo produces.
func TestAutoOnceOnTheBestAccountIsExitThree(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "2"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 10)
	seedUsage(t, "u-2", 80)
	clearCooldown(t)

	code, _, errOut, _ := runRoot(t, "auto", "--once")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitNothingToDo)
	}
}

// Wanted to move, nothing to move to: 4, because the operator has to do
// something about it. Alert on 4, ignore 3.
func TestAutoOnceWithNoViableTargetIsExitFour(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.Quarantine("u-1", time.Now(), time.Hour, "dead refresh token")
		st.Quarantine("u-2", time.Now(), time.Hour, "dead refresh token")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, _ := runRoot(t, "auto", "--once")
	if code != ExitBlocked {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitBlocked)
	}
}

func TestAutoOnceWithNoReadingsIsExitFour(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")

	code, _, errOut, _ := runRoot(t, "auto", "--once")
	if code != ExitBlocked {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitBlocked)
	}
	if !strings.Contains(errOut, "no usage readings") {
		t.Errorf("stderr = %q, want the missing readings named", errOut)
	}
	assertNoLiveCredentials(t)
}

// The defect §9.3 names by name: cswap answers 2 for both a typo and a normal
// no-op, so `cswap auto --once --thresold 80` looks like a healthy cron line
// forever. Here 2 is usage-only.
func TestAMistypedAutoFlagIsExitTwoAndNotThree(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)

	code, _, _, top := runRoot(t, "auto", "--once", "--thresold", "80")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s), want %d — a typo must not read as a no-op", code, top, ExitUsage)
	}
	if got := liveUUIDOf(t); got != "u-1" {
		t.Fatalf("live account = %q, want u-1 — a rejected command must not switch", got)
	}
}

// NDJSON, not JSON: one object per line, no enclosing array, and every line
// parses on its own. writeJSON indents, which would make each object span
// several lines and break every line-oriented consumer.
func TestAutoJSONIsOneObjectPerLine(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)

	code, out, errOut, _ := runRoot(t, "auto", "--once", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, errOut)
	}
	events := decodeNDJSON(t, out)
	if len(events) < 2 {
		t.Fatalf("events = %v, want at least the evaluation and the switch", kindsOf(events))
	}
	kinds := kindsOf(events)
	if kinds[0] != "evaluated" {
		t.Fatalf("first event = %q, want evaluated", kinds[0])
	}
	if kinds[len(kinds)-1] != "switched" {
		t.Fatalf("last event = %q, want switched", kinds[len(kinds)-1])
	}
}

// The flag changes the representation, never the answer.
func TestAutoJSONKeepsTheSameExitCode(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "2"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 10)
	seedUsage(t, "u-2", 80)
	clearCooldown(t)

	code, out, _, _ := runRoot(t, "auto", "--once", "--json")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d", code, ExitNothingToDo)
	}
	events := decodeNDJSON(t, out)
	if got := events[0]["action"]; got != "stay" {
		t.Fatalf("action = %v, want stay", got)
	}
}

// §9.4: human notices go to stderr. In NDJSON mode stdout must carry the stream
// and nothing else, or `ccdad auto --json | jq` dies on the first notice.
func TestAutoJSONKeepsNoticesOffTheStream(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")

	code, out, errOut, _ := runRoot(t, "auto", "--once", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	decodeNDJSON(t, out)
	if !strings.Contains(errOut, "no usage readings") {
		t.Fatalf("stderr = %q, want the human notice", errOut)
	}
}

// EPIPE is not an error: `ccdad auto --json | head -1` exits 0.
func TestAutoJSONExitsZeroWhenTheReaderGoesAway(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)

	root := NewRootCmd()
	root.SetArgs([]string{"auto", "--once", "--json"})
	root.SetOut(brokenPipe{})
	var errOut, top bytes.Buffer
	root.SetErr(&errOut)

	if code := ExecuteWith(root, &top); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0 on a closed reader", code, errOut.String(), top.String())
	}
}

// brokenPipe is a stdout whose reader has gone away.
type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, errBrokenPipeForTest }

// Two processes both executing switches fight the cooldown and the anti-flap
// state, which live on disk. The continuous form refuses rather than joining
// in, and 3 rather than 1: a daemon already doing this is the world being as
// the caller asked, not a failure.
func TestAutoRefusesWhileADaemonHoldsTheSingleton(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)
	single, err := daemon.AcquireSingleton()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = single.Release() })

	code, _, errOut, _ := runRoot(t, "auto")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitNothingToDo)
	}
	if !strings.Contains(errOut, "daemon") {
		t.Fatalf("stderr = %q, want it to name the daemon", errOut)
	}
}

// --once does NOT take the singleton, and that is safe because the executor
// re-decides under the credential locks: a cron line and a daemon can overlap
// without either of them writing over the other's switch.
func TestAutoOnceRunsAlongsideADaemon(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)
	single, err := daemon.AcquireSingleton()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = single.Release() })

	if code, _, errOut, top := runRoot(t, "auto", "--once"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
}

// With CLAUDE_CODE_OAUTH_TOKEN set the swap changes nothing about what Claude
// Code authenticates as, so an unattended engine would switch into the void on
// every evaluation. That is exit 4: wanted, and the operator has to act.
func TestAutoRefusesToSwitchIntoTheVoid(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-SOMETHING")

	code, _, errOut, _ := runRoot(t, "auto", "--once")
	if code != ExitBlocked {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitBlocked)
	}
	if !strings.Contains(errOut, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("stderr = %q, want the variable named", errOut)
	}
	if got := liveUUIDOf(t); got != "u-1" {
		t.Fatalf("live account = %q, want u-1 — nothing should have been written", got)
	}
}

// A degraded input is reported on its own event rather than folded into the
// evaluation, so a consumer can alert on it without parsing prose.
func TestAutoJSONReportsADegradedConfigAsItsOwnEvent(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)
	writeConfig(t, "threshold = \"not a number\"\n")

	code, out, _, _ := runRoot(t, "auto", "--once", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 — a mistyped threshold must not stop the engine", code)
	}
	events := decodeNDJSON(t, out)
	var found bool
	for _, e := range events {
		if e["kind"] == "notice" {
			found = true
		}
	}
	if !found {
		t.Fatalf("events = %v, want a notice for the unusable config", kindsOf(events))
	}
}

// The continuous form is the daemon's loop with nothing detached: it keeps
// evaluating, it acts on what it decides, and an interrupt is 130 rather than a
// process killed mid-swap. That last part is why SIGINT is trapped here at all
// — a tick killed between the credential locks and the write abandons three
// lock directories whose stale windows are 60 s, 60 s and 15 s.
func TestTheContinuousFormKeepsEvaluatingAndReportsAnInterrupt(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)

	root := NewRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}

	// Two seconds against a one-millisecond interval, for an assertion that
	// wants two evaluations. Measured on Linux under -race the old
	// 200 ms budget yielded 83-109 of them, which reads as ample and is not:
	// the margin that matters is the one on the slowest runner in the matrix
	// under load, and nothing about this test needs the budget to be tight.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = runAutoLoop(ctx, newAutoEmitter(root, true), s, time.Millisecond)

	if code := CodeFor(err); code != ExitInterrupted {
		t.Fatalf("exit = %d (%v), want %d", code, err, ExitInterrupted)
	}
	if got := liveUUIDOf(t); got != "u-2" {
		t.Fatalf("live account = %q, want u-2 — the loop never acted", got)
	}
	events := decodeNDJSON(t, out.String())
	var evaluated int
	for _, e := range events {
		if e["kind"] == "evaluated" {
			evaluated++
		}
	}
	if evaluated < 2 {
		t.Fatalf("evaluated events = %d, want more than one — it did not loop", evaluated)
	}
}

// The human form does not repeat itself. Evaluating once a second and printing
// the same sentence each time is a terminal nobody watches; the STREAM is not
// deduplicated, because a consumer wants every tick and a dropped one would
// make a gap ambiguous.
func TestTheContinuousFormDoesNotRepeatTheSameHumanLine(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "2"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 10)
	seedUsage(t, "u-2", 80)
	clearCooldown(t)

	root := NewRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = runAutoLoop(ctx, newAutoEmitter(root, true), s, time.Millisecond)

	if n := strings.Count(errOut.String(), "Staying put"); n != 1 {
		t.Fatalf("printed %d identical lines:\n%s", n, errOut.String())
	}
	var evaluated int
	for _, e := range decodeNDJSON(t, out.String()) {
		if e["kind"] == "evaluated" {
			evaluated++
		}
	}
	if evaluated < 2 {
		t.Fatalf("evaluated events = %d, want the stream to carry every tick", evaluated)
	}
}

// The EPIPE test above only means something if a write failure reaches the exit
// code at all. This is its pair: a stdout that fails for any OTHER reason is a
// runtime failure, and it must not be swallowed into the pass's own code.
//
// Without it, `ccdad auto --json > /full/disk` would report whatever the
// evaluation decided and lose the fact that nothing was written.
func TestAutoJSONReportsAStreamThatCouldNotBeWritten(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)

	root := NewRootCmd()
	root.SetArgs([]string{"auto", "--once", "--json"})
	root.SetOut(deadWriter{})
	var errOut, top bytes.Buffer
	root.SetErr(&errOut)

	if code := ExecuteWith(root, &top); code != ExitFailure {
		t.Fatalf("exit = %d (%s), want %d on an unwritable stream", code, top.String(), ExitFailure)
	}
}

// deadWriter is a stdout that fails for a reason that is not a closed reader.
type deadWriter struct{}

func (deadWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// A broken stream stops the continuous form rather than leaving it writing into
// a pipe nobody is reading, once a second, forever.
func TestTheContinuousFormStopsWhenTheStreamBreaks(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)

	root := NewRootCmd()
	root.SetOut(deadWriter{})
	var errOut bytes.Buffer
	root.SetErr(&errOut)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runAutoLoop(ctx, newAutoEmitter(root, true), s, time.Millisecond) }()

	select {
	case err := <-done:
		if CodeFor(err) != ExitFailure {
			t.Fatalf("exit = %d (%v), want %d", CodeFor(err), err, ExitFailure)
		}
	case <-ctx.Done():
		t.Fatal("the loop kept running with a dead stream")
	}
}

// With no readings the ranking never ran, so there is no plan to describe. A
// zero-value strategy.Plan stringifies to real-looking values — Reason(0) is
// "a better target cleared every margin" and Mode(0) is "headroom" — so an
// event built from one does not look empty, it looks WRONG, and a consumer
// charting why the engine stayed put would be charting a decision nobody made.
func TestTheNoReadingsEventDoesNotDescribeAPlanThatNeverRan(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")

	_, out, _, _ := runRoot(t, "auto", "--once", "--json")
	ev := decodeNDJSON(t, out)[0]
	if ev["action"] != "blocked" || ev["reason"] != "no usage readings yet" {
		t.Fatalf("event = %v, want blocked with the readings named", ev)
	}
	for _, key := range []string{"explanation", "mode", "order", "target"} {
		if _, ok := ev[key]; ok {
			t.Errorf("event carries %q = %v, but no ranking ever ran", key, ev[key])
		}
	}
}
