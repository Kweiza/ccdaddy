package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// hoverEpoch is the clock every case below runs against, so "43% of a week
// elapsed" is a fact about the fixture rather than about the day it is run.
var hoverEpoch = mustTime("2026-08-22T12:00:00Z")

// hoverValue is what the config file now says, read back the way the engine
// would read it rather than by grepping the document.
func hoverValue(t *testing.T) bool {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("the config the engine would load is unusable: %v", err)
	}
	return cfg.Hover
}

// seedHoverAccount stores an account whose AddedAt predates the fixture clock.
//
// It is seedAccountAddedAt rather than seedAccount because switcher.Evaluate
// prunes every cache entry dated before its account's AddedAt -- a reading
// older than the login belonged to a previous account at that uuid -- and
// store.Add stamps AddedAt with the wall clock when the caller leaves it zero,
// which is after any fixed date a test can name.
func seedHoverAccount(t *testing.T, uuid, email string) {
	t.Helper()
	seedAccountAddedAt(t, uuid, email, hoverEpoch.Add(-time.Hour))
}

// seedHoverWindows puts one reading in the cache carrying a five-hour window
// fiveShare of the way through it and a seven-day window weekShare of the way
// through its own, at the given utilizations.
func seedHoverWindows(t *testing.T, uuid string, fiveShare, fivePct, weekShare, weekPct float64) {
	t.Helper()
	five, week := 5*time.Hour, 7*24*time.Hour
	seedUsageEntry(t, uuid, usage.Entry{
		FetchedAt: hoverEpoch,
		Snapshot: &usage.Snapshot{
			FiveHour: window(fivePct, hoverEpoch.Add(five-time.Duration(fiveShare*float64(five)))),
			SevenDay: window(weekPct, hoverEpoch.Add(week-time.Duration(weekShare*float64(week)))),
		},
	})
}

// seedHoverCreditAccount stores a seat billed only in credits, already marked
// primary, carrying the one figure that meter reports.
//
// It is not seedPrimaryCreditAccount for the same reason seedHoverAccount is not
// seedAccount: that one lets store.Add stamp AddedAt with the wall clock, and a
// reading dated at the fixture epoch is then pruned as one that belonged to a
// previous login at the same uuid.
func seedHoverCreditAccount(t *testing.T, uuid, email string, pct float64) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a := store.Account{
		UUID: uuid, Email: email, Kind: identity.KindCredit,
		Primary: true, AddedAt: hoverEpoch.Add(-time.Hour),
	}
	if err := s.Add(a, credsFor("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
	seedUsageEntry(t, uuid, usage.Entry{
		FetchedAt: hoverEpoch,
		Snapshot: &usage.Snapshot{ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
			State: usage.ExtraUsageEnabled, Currency: "USD", Utilization: &pct,
		})},
	})
}

// hoverRow is one printed window's cells, found by the window's own name rather
// than at a fixed offset: the active marker shares the first cell with IDX, so
// counting from the left would read a different column depending on whether the
// row happens to be the live one.
func hoverRow(t *testing.T, stdout, window string) []string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == window {
				return fields[i:]
			}
		}
	}
	t.Fatalf("no %s row in:\n%s", window, stdout)
	return nil
}

func TestHoverOnAndOffWriteTheKey(t *testing.T) {
	isolate(t)

	if code, _, _, top := runRoot(t, "hover", "on"); code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	if !hoverValue(t) {
		t.Fatal("hover = false after `ccdad hover on`")
	}
	if code, _, _, top := runRoot(t, "hover", "off"); code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	if hoverValue(t) {
		t.Fatal("hover = true after `ccdad hover off`")
	}
}

// The world already being how the caller asked for it is 3, never 0 and never a
// second identical write: the daemon re-reads this file every second and detects
// change on the BYTES, so an idempotent rewrite would look like an edit.
func TestHoverOnTwiceIsNothingToDo(t *testing.T) {
	isolate(t)

	if code, _, _, top := runRoot(t, "hover", "on"); code != ExitOK {
		t.Fatalf("first on: exit = %d (%s)", code, top)
	}
	code, _, stderr, _ := runRoot(t, "hover", "on")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d", code, ExitNothingToDo)
	}
	if !strings.Contains(stderr, "already on") {
		t.Errorf("stderr = %q, want it to say the mode was already on", stderr)
	}
}

// Turning a fully automatic mode on must say what it means, and the money
// sentence is the half a user who typed it by mistake has to read while their
// ceiling is still where they left it.
func TestHoverOnNamesWhatItStopsReadingAndWhatItDoesNot(t *testing.T) {
	isolate(t)

	_, _, stderr, _ := runRoot(t, "hover", "on")
	for _, want := range []string{"threshold", "hysteresis_pct", "credit.max_auto_spend"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
}

// A verb ccdad does not have is a usage error, not a failure. An accepted typo
// here would leave the mode in whatever state it was already in and report
// success for it.
func TestHoverRefusesAVerbItDoesNotHave(t *testing.T) {
	isolate(t)

	for _, args := range [][]string{
		{"hover"},
		{"hover", "onn"},
		{"hover", "on", "off"},
	} {
		if code, _, _, _ := runRoot(t, args...); code != ExitUsage {
			t.Errorf("%v exit = %d, want %d", args, code, ExitUsage)
		}
	}
	if hoverValue(t) {
		t.Error("a refused verb wrote the key anyway")
	}
}

// --json belongs to one of the three verbs. A flag silently ignored by the other
// two is a caller who piped `ccdad hover on --json` into jq and got nothing.
func TestHoverRefusesJSONOnTheWritingVerbs(t *testing.T) {
	isolate(t)

	for _, verb := range []string{"on", "off"} {
		code, stdout, _, _ := runRoot(t, "hover", verb, "--json")
		if code != ExitUsage {
			t.Errorf("`hover %s --json` exit = %d, want %d", verb, code, ExitUsage)
		}
		if stdout != "" {
			t.Errorf("`hover %s --json` wrote %q to stdout", verb, stdout)
		}
	}
}

// The whole point of an omakase mode being acceptable: every number chosen on
// the user's behalf is printed, together with the two figures it was derived
// from.
func TestHoverStatusShowsTheThresholdTheUtilizationAndTheSlack(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	writeConfig(t, "hover = true\n")
	seedHoverAccount(t, "u-1", "work@example.com")
	seedHoverAccount(t, "u-2", "spare@example.com")
	seedHoverWindows(t, "u-1", 0.80, 62, 0.43, 55)
	seedHoverWindows(t, "u-2", 0.80, 91, 0.43, 72)

	code, stdout, _, top := runRoot(t, "hover", "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	for _, want := range []string{
		"THRESHOLD", "SLACK",
		// Two usable accounts, so a weekly window 43% elapsed is held to 93.
		"seven_day", "93%",
		// The five-hour window resets within the hour, so it caps out.
		"five_hour", "99%",
		"2 usable accounts",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not carry %q:\n%s", want, stdout)
		}
	}
	// ELAPSED is the input the threshold was derived FROM, and it is what lets a
	// reader check 43 + 50 = 93 rather than take it on trust.
	if row := hoverRow(t, stdout, "seven_day"); row[1] != "43%" {
		t.Errorf("ELAPSED = %q, want the share of the week that has passed", row[1])
	}
}

// The row the engine is measuring the LIVE account on is marked, and a seat
// billed only in credits says why it has no elapsed share instead of looking
// like a window that failed to report one.
func TestHoverStatusMarksTheLiveAccountAndTheCreditSeat(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	writeConfig(t, "hover = true\n")
	seedHoverAccount(t, "u-1", "work@example.com")
	seedHoverWindows(t, "u-1", 0.80, 62, 0.43, 55)
	seedHoverCreditAccount(t, "u-2", "seat@example.com", 41)
	writeLiveFile(t, liveLoginJSON("RT-u-1", ""))

	code, stdout, _, top := runRoot(t, "hover", "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	var marked bool
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "*") {
			marked = true
			if !strings.Contains(line, "work@example.com") {
				t.Errorf("the marked row is not the live account's: %q", line)
			}
		}
	}
	if !marked {
		t.Errorf("no row is marked as the live account's:\n%s", stdout)
	}
	// A credit seat has no window and no reset, so pace says nothing about it
	// and 95 is the figure it is held to instead.
	credit := hoverRow(t, stdout, "extra_usage")
	if credit[1] != "-" || credit[3] != "95%" {
		t.Errorf("the credit row = %v, want no elapsed share and a threshold of 95%%", credit)
	}
	if !strings.Contains(stdout, "(primary, metered in credits)") {
		t.Errorf("the credit row does not say what it is metered on:\n%s", stdout)
	}

	_, stdout, _, _ = runRoot(t, "hover", "status", "--json")
	var payload struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	var sawActive, sawCredit bool
	for _, row := range payload.Windows {
		if row["window"] == "extra_usage" {
			sawCredit = row["credit"] == true
			if _, ok := row["active"]; ok {
				t.Errorf("the credit seat is marked active and the live login is elsewhere: %+v", row)
			}
			continue
		}
		if row["active"] == true {
			sawActive = true
		}
	}
	if !sawActive {
		t.Errorf("no window carries active for the live account: %+v", payload.Windows)
	}
	if !sawCredit {
		t.Errorf("the credit row is not marked credit: %+v", payload.Windows)
	}
}

// A report on a mode that is off is a negative answer to a probe, which the exit
// contract puts at 5 -- and the table is printed either way, because the numbers
// hover WOULD choose are exactly what somebody deciding whether to turn it on
// has to see.
func TestHoverStatusWithTheModeOffStillShowsTheNumbers(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	seedHoverAccount(t, "u-1", "work@example.com")
	seedHoverWindows(t, "u-1", 0.80, 62, 0.43, 55)

	code, stdout, stderr, top := runRoot(t, "hover", "status")
	if code != ExitProbeNegative {
		t.Fatalf("exit = %d, want %d (%s)", code, ExitProbeNegative, top)
	}
	if top != "" {
		t.Errorf("ExecuteWith printed %q; a rendered answer is not a runtime failure", top)
	}
	if !strings.Contains(stdout, "Hover:   off") || !strings.Contains(stdout, "THRESHOLD") {
		t.Errorf("stdout = %q, want the preview table", stdout)
	}
	if !strings.Contains(stderr, "hover on") {
		t.Errorf("stderr = %q, want it to say how to put these numbers in force", stderr)
	}
}

// A window with quota already spent against it and STILL no reset time is the
// one shape a warm-up cannot answer: the reset is one this build could not read,
// and another turn buys the same unreadable field back. strategy.ColdWindow has
// never targeted it — it requires 0% for the never-spent arm — so the table must
// not promise a warm-up here.
//
// This test used to assert the opposite. It asserted the literal string "a probe
// is queued" on exactly this row, which is how the defect survived: the note was
// rendered from ProbeWanted alone, ProbeWanted is "named no reset" and nothing
// more, and the daemon's own gate had a condition the note could not see.
func TestHoverStatusPromisesNoWarmUpForAWindowThatIsAlreadyInUse(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	writeConfig(t, "hover = true\n")
	seedHoverAccount(t, "u-1", "fresh@example.com")
	pct := 30.0
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: hoverEpoch,
		Snapshot:  &usage.Snapshot{SevenDay: usage.NewWindow(&pct, nil)},
	})

	code, stdout, _, top := runRoot(t, "hover", "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	if strings.Contains(stdout, "warm-up at") || strings.Contains(stdout, "probe is queued") {
		t.Errorf("stdout promises a warm-up for a window no warm-up targets:\n%s", stdout)
	}
	if !strings.Contains(stdout, "a warm-up cannot fix that") {
		t.Errorf("stdout does not say why this row cannot be fixed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "80%") {
		t.Errorf("stdout does not carry the fallback threshold:\n%s", stdout)
	}
	// The ELAPSED cell is the unknown rather than a zero. A 0% there reads as a
	// window that has only just rolled over, which is the most generous answer
	// there is, and "30%" already contains "0%" -- so this is read as a cell.
	if row := hoverRow(t, stdout, "seven_day"); row[1] != "-" {
		t.Errorf("ELAPSED = %q for a window with no reset, want %q", row[1], "-")
	}

	// And the payload leaves the share OUT rather than reporting it as zero,
	// which would read as a window that has only just rolled over.
	_, stdout, _, _ = runRoot(t, "hover", "status", "--json")
	var payload struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if len(payload.Windows) != 1 {
		t.Fatalf("windows = %+v, want one row", payload.Windows)
	}
	if _, ok := payload.Windows[0]["expectedPct"]; ok {
		t.Errorf("expectedPct is present for a window with no reset: %+v", payload.Windows[0])
	}
	// probeWanted keeps its own meaning — "this window named no reset" — and its
	// key, because that is a true and useful thing to say. What must not appear
	// is the warmup object, which is the one that answers "will anything happen".
	if payload.Windows[0]["probeWanted"] != true {
		t.Errorf("probeWanted is not set on a row that named no reset: %+v", payload.Windows[0])
	}
	if _, ok := payload.Windows[0]["warmup"]; ok {
		t.Errorf("warmup is present on a row no warm-up targets: %+v", payload.Windows[0])
	}
}

// The row a warm-up DOES target: a five-hour window nothing has ever spent
// against. The note names an instant rather than a promise, and the JSON carries
// the state a caller can branch on without matching English.
func TestHoverStatusQueuesAWarmUpForAStoppedClock(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	stubProbeAvailable(t, nil)
	writeConfig(t, "hover = true\n")
	seedHoverAccount(t, "u-1", "fresh@example.com")
	// A second account holding the live login. Without one, which account is
	// live cannot be worked out, and the daemon refuses every warm-up in that
	// state — so the table would honestly say "held" and this test would be
	// asserting the wrong sentence.
	seedHoverAccount(t, "u-2", "live@example.com")
	seedLiveAs(t, "u-2")
	seedUsageEntry(t, "u-2", usage.Entry{
		FetchedAt: hoverEpoch,
		Snapshot:  &usage.Snapshot{FiveHour: window(4, hoverEpoch.Add(4*time.Hour))},
	})
	idle, spent := 0.0, 30.0
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt:  hoverEpoch,
		NextPollAt: hoverEpoch.Add(7 * time.Minute),
		Snapshot: &usage.Snapshot{
			FiveHour: usage.NewWindow(&idle, nil),
			SevenDay: usage.NewWindow(&spent, nil),
		},
	})

	code, stdout, _, top := runRoot(t, "hover", "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	// The scheduler's own next look, not the instant the gate opens: a warm-up
	// runs on a tick where the account is poll-due as well as eligible, and the
	// gate here is open already.
	if want := "warm-up at " + hoverEpoch.Add(7*time.Minute).Local().Format("15:04"); !strings.Contains(stdout, want) {
		t.Errorf("stdout does not name when the warm-up will run (%q):\n%s", want, stdout)
	}
	// One note per account, on the row ColdWindow targets. The seven-day window
	// named no reset too, and saying it is behind the five-hour one is not the
	// same as promising it a turn of its own.
	if !strings.Contains(stdout, "aims at five_hour first") {
		t.Errorf("the second stopped clock does not say what it is behind:\n%s", stdout)
	}

	_, stdout, _, _ = runRoot(t, "hover", "status", "--json")
	var payload struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	warmups := 0
	for _, w := range payload.Windows {
		obj, ok := w["warmup"].(map[string]any)
		if !ok {
			continue
		}
		warmups++
		if w["window"] != string(usage.WindowFiveHour) {
			t.Errorf("the warmup object is on %v rather than the five-hour window", w["window"])
		}
		if obj["state"] != "queued" {
			t.Errorf("state = %v, want queued: %+v", obj["state"], obj)
		}
		if _, ok := obj["at"]; !ok {
			t.Errorf("a queued warm-up names no instant: %+v", obj)
		}
	}
	if warmups != 1 {
		t.Errorf("warmup objects = %d, want exactly one — a warm-up is one turn aimed at one window", warmups)
	}
}

func TestHoverStatusAnswersInJSON(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	writeConfig(t, "hover = true\n")
	seedHoverAccount(t, "u-1", "work@example.com")
	seedHoverWindows(t, "u-1", 0.80, 62, 0.43, 55)

	code, stdout, _, top := runRoot(t, "hover", "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	var payload struct {
		SchemaVersion  int  `json:"schemaVersion"`
		Hover          bool `json:"hover"`
		UsableAccounts int  `json:"usableAccounts"`
		Windows        []struct {
			Account     map[string]any `json:"account"`
			Window      string         `json:"window"`
			ExpectedPct float64        `json:"expectedPct"`
			Utilization float64        `json:"utilization"`
			Threshold   float64        `json:"threshold"`
			Slack       float64        `json:"slack"`
		} `json:"windows"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if payload.SchemaVersion != 1 || !payload.Hover || payload.UsableAccounts != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	var week bool
	for _, w := range payload.Windows {
		if w.Window != "seven_day" {
			continue
		}
		week = true
		// One usable account, so the share is the whole 100 points and the cap
		// is what decides.
		if w.Threshold != 99 || w.Utilization != 55 || w.Slack != 44 {
			t.Errorf("seven_day = %+v, want threshold 99 against 55%% used", w)
		}
		if w.ExpectedPct != 43 {
			t.Errorf("expectedPct = %v, want 43", w.ExpectedPct)
		}
		if w.Account["email"] != "work@example.com" {
			t.Errorf("account = %+v, want the row named", w.Account)
		}
	}
	if !week {
		t.Fatalf("no seven_day row in %+v", payload.Windows)
	}
}

// A user who tuned a number and then turned hover on has a file full of values
// that no longer do anything. Hiding them would be worse than printing them
// unmarked: the listing has to say which ones stopped mattering, or it is
// reporting settings as in force that the engine is not reading.
func TestListMarksTheKeysHoverIsOverriding(t *testing.T) {
	isolate(t)
	writeConfig(t, "hover = true\nthreshold = 55\n")

	code, stdout, stderr, _ := runRoot(t, "config", "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "HOVER") {
		t.Fatalf("the listing has no hover column:\n%s", stdout)
	}
	// Still printed, with its file value, and marked.
	if !strings.Contains(stdout, "55") {
		t.Errorf("the overridden key's value was hidden rather than marked:\n%s", stdout)
	}
	if !strings.Contains(stdout, "overriding") {
		t.Errorf("no key is marked as overridden:\n%s", stdout)
	}
	// The one key hover does not touch says so in the same column.
	if !strings.Contains(stdout, "honoured") {
		t.Errorf("no key is marked as still honoured:\n%s", stdout)
	}
	if !strings.Contains(stderr, "hover status") {
		t.Errorf("stderr = %q, want it to point at where the derived numbers are", stderr)
	}
}

// With hover off the column would be the same word on every row. A column that
// says nothing costs width and invites a reader to hunt for a meaning it does
// not have.
func TestListHasNoHoverColumnWhenHoverIsOff(t *testing.T) {
	isolate(t)
	writeConfig(t, "threshold = 55\n")

	code, stdout, _, _ := runRoot(t, "config", "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stdout, "HOVER") {
		t.Errorf("the hover column is present with hover off:\n%s", stdout)
	}
}

func TestListReportsTheOverriddenKeysInJSON(t *testing.T) {
	isolate(t)
	writeConfig(t, "hover = true\nthreshold = 55\n")

	code, stdout, _, _ := runRoot(t, "config", "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var payload struct {
		Hover bool `json:"hover"`
		Keys  []struct {
			Key        string `json:"key"`
			Value      string `json:"value"`
			Overridden bool   `json:"overriddenByHover"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if !payload.Hover {
		t.Error("hover = false in a payload built from a file that sets it")
	}
	marked := map[string]bool{}
	for _, k := range payload.Keys {
		marked[k.Key] = k.Overridden
	}
	if !marked["threshold"] {
		t.Error("threshold is not marked as overridden")
	}
	if marked["credit.max_auto_spend"] {
		t.Error("credit.max_auto_spend is marked as overridden; hover must never supply that opt-in")
	}
	if marked["hover"] {
		t.Error("hover marked itself as overridden")
	}
}

// stubProbeAvailable says whether this machine has a Claude Code to warm with,
// rather than letting the answer depend on the PATH of the host running the
// suite.
func stubProbeAvailable(t *testing.T, err error) {
	t.Helper()
	saved := probeAvailable
	t.Cleanup(func() { probeAvailable = saved })
	probeAvailable = func() error { return err }
}

// seedLiveAs writes Claude Code's credentials file holding one stored account's
// login, which is what makes switcher.Evaluate able to name the live account.
func seedLiveAs(t *testing.T, uuid string) {
	t.Helper()
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"AT-`+uuid+`","refreshToken":"RT-`+uuid+`"}}`)
}

// Nothing on this machine can warm anything, and the table must say so once
// rather than promising a turn per row. The daemon deliberately records NOTHING
// in this state — a machine with no Claude Code has made no attempt and must not
// consume a rung — so there is no stamp to read and the command has to look.
func TestHoverStatusSaysWhenNothingCanBeWarmed(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	stubProbeAvailable(t, errors.New("`claude` is not on this PATH"))
	writeConfig(t, "hover = true\n")
	seedHoverAccount(t, "u-1", "fresh@example.com")
	seedHoverAccount(t, "u-2", "live@example.com")
	seedLiveAs(t, "u-2")
	idle := 0.0
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: hoverEpoch,
		Snapshot:  &usage.Snapshot{FiveHour: usage.NewWindow(&idle, nil)},
	})
	seedUsageEntry(t, "u-2", usage.Entry{
		FetchedAt: hoverEpoch,
		Snapshot:  &usage.Snapshot{FiveHour: window(4, hoverEpoch.Add(4*time.Hour))},
	})

	code, stdout, _, top := runRoot(t, "hover", "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	if strings.Contains(stdout, "warm-up at") {
		t.Errorf("stdout promises a warm-up on a machine that cannot run one:\n%s", stdout)
	}
	if !strings.Contains(stdout, "not on this PATH") {
		t.Errorf("stdout does not say why nothing can be warmed:\n%s", stdout)
	}
}
