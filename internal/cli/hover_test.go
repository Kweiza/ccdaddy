package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
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
		Provider: provider.Claude,
		UUID:     uuid, Email: email, Kind: identity.KindCredit,
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

	if code, _, _, top := runRoot(t, "strategy", "hover"); code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	if !hoverValue(t) {
		t.Fatal("hover = false after `ccdad hover on`")
	}
	if code, _, _, top := runRoot(t, "strategy", "headroom"); code != ExitOK {
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

	if code, _, _, top := runRoot(t, "strategy", "hover"); code != ExitOK {
		t.Fatalf("first on: exit = %d (%s)", code, top)
	}
	code, _, stderr, _ := runRoot(t, "strategy", "hover")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d", code, ExitNothingToDo)
	}
	if !strings.Contains(stderr, "already hover") {
		t.Errorf("stderr = %q, want it to say hover is already selected", stderr)
	}
}

// Turning a fully automatic mode on must say what it means, and the money
// sentence is the half a user who typed it by mistake has to read while their
// ceiling is still where they left it.
func TestStrategyHoverClearsManualMode(t *testing.T) {
	isolate(t)
	runRoot(t, "strategy", "manual")
	runRoot(t, "strategy", "hover")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Hover || cfg.Manual {
		t.Fatalf("strategy hover left compatibility flags hover=%v manual=%v", cfg.Hover, cfg.Manual)
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
// One row per account, one cell per window, each cell "used/threshold". The
// pair is what this command exists to show: an omakase mode is only acceptable
// if you can see what it chose, and both halves of that are here. ELAPSED and
// SLACK moved to --json, which carries the whole derivation.
func TestHoverStatusShowsWhatEachWindowUsedAndWhatItIsHeldTo(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	writeConfig(t, "hover = true\n")
	seedHoverAccount(t, "u-1", "work@example.com")
	seedHoverAccount(t, "u-2", "spare@example.com")
	seedHoverWindows(t, "u-1", 0.80, 62, 0.43, 55)
	seedHoverWindows(t, "u-2", 0.80, 91, 0.43, 72)

	code, stdout, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	for _, want := range []string{
		// The unified table headers and the legend that maps them
		// back to the keys `ccdad config` takes a threshold on.
		"5H", "7D", "5H = five_hour", "7D = seven_day",
		// Two usable accounts, so a weekly window 43% elapsed is held to 93.
		"55%/93%", "72%/93%",
		// The five-hour window resets within the hour, so its pace target is
		// 80 + 50 = 130: no restraint at all.
		"62%/130%", "91%/130%",
		"quota cells show used/threshold",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not carry %q:\n%s", want, stdout)
		}
	}
	// One row per ACCOUNT. Two accounts carrying two windows each used to be
	// four rows; a reader of a real fleet had twelve.
	rows := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "@example.com") {
			rows++
		}
	}
	if rows != 2 {
		t.Errorf("the table has %d account rows, want 2 — one per account", rows)
	}
}

// ELAPSED and SLACK left the human table and must still be reachable, or the
// mode stopped being auditable.
func TestHoverStatusJSONStillCarriesTheWholeDerivation(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	writeConfig(t, "hover = true\n")
	seedHoverAccount(t, "u-1", "work@example.com")
	seedHoverWindows(t, "u-1", 0.80, 62, 0.43, 55)

	_, stdout, _, top := runRoot(t, "status", "--json")
	for _, want := range []string{"expectedPct", "slack", "threshold", "utilization"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--json does not carry %q (%s):\n%s", want, top, stdout)
		}
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

	code, stdout, _, top := runRoot(t, "status")
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
	// and 95 is the figure it is held to instead. It gets a CREDIT column of
	// its own, and the legend names the key it is filed under.
	if !strings.Contains(stdout, "/95%") {
		t.Errorf("the credit seat is not shown against the 95%% it is held to:\n%s", stdout)
	}
	if !strings.Contains(stdout, "CREDIT = extra_usage") {
		t.Errorf("the legend does not name the credit column's key:\n%s", stdout)
	}
	if !strings.Contains(stdout, "(primary)") || !strings.Contains(stdout, "credit:") {
		t.Errorf("the credit row does not identify its billing policy:\n%s", stdout)
	}

	_, stdout, _, _ = runRoot(t, "status", "--json")
	var payload struct {
		ActiveUUID string `json:"activeUuid"`
		Accounts   []struct {
			Email string `json:"email"`
			Usage struct {
				Windows map[string]map[string]any `json:"windows"`
			} `json:"usage"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if payload.ActiveUUID != "u-1" {
		t.Errorf("activeUuid = %q, want u-1", payload.ActiveUUID)
	}
	var credit map[string]any
	for _, account := range payload.Accounts {
		if account.Email == "seat@example.com" {
			credit = account.Usage.Windows["extra_usage"]
		}
	}
	if credit["thresholdPct"] != float64(strategy.HoverCreditThreshold) {
		t.Errorf("credit threshold = %v, want %v", credit["thresholdPct"], strategy.HoverCreditThreshold)
	}
}

// Unified status is a dashboard rather than a boolean probe. With hover not
// selected it reports the selected strategy and uses ordinary percentage cells.
func TestStatusWithHoverOffReportsTheSelectedStrategy(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	seedHoverAccount(t, "u-1", "work@example.com")
	seedHoverWindows(t, "u-1", 0.80, 62, 0.43, 55)

	code, stdout, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (%s)", code, ExitOK, top)
	}
	if top != "" {
		t.Errorf("ExecuteWith printed %q; a rendered answer is not a runtime failure", top)
	}
	if !strings.Contains(stdout, "Strategy: headroom") || strings.Contains(stdout, "used/threshold") {
		t.Errorf("stdout = %q, want ordinary headroom status", stdout)
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

	code, stdout, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	if strings.Contains(stdout, "warm-up at") || strings.Contains(stdout, "probe is queued") {
		t.Errorf("stdout promises a warm-up for a window no warm-up targets:\n%s", stdout)
	}
	if !strings.Contains(stdout, "80%") {
		t.Errorf("stdout does not carry the fallback threshold:\n%s", stdout)
	}
	// The cell carries the pair it always carries: 30% used against the
	// fallback threshold. ELAPSED left the human table for --json, which is
	// checked below -- and it is left OUT there rather than reported as zero,
	// because a 0 reads as a window that has only just rolled over.
	if !strings.Contains(stdout, "30%/80%") {
		t.Errorf("stdout does not carry the used/threshold pair:\n%s", stdout)
	}
}

// The row a warm-up DOES target: a five-hour window nothing has ever spent
// against. The note names an instant rather than a promise, and the JSON carries
// the state a caller can branch on without matching English.
func TestHoverStatusQueuesAWarmUpForAStoppedClock(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
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

	code, stdout, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	for _, want := range []string{"fresh@example.com", "0%/", "30%/"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status omitted %q from the stopped-clock account:\n%s", want, stdout)
		}
	}
}

func TestHoverStatusAnswersInJSON(t *testing.T) {
	isolate(t)
	freezeClock(t, hoverEpoch)
	writeConfig(t, "hover = true\n")
	seedHoverAccount(t, "u-1", "work@example.com")
	seedHoverWindows(t, "u-1", 0.80, 62, 0.43, 55)

	code, stdout, _, top := runRoot(t, "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	var payload struct {
		SchemaVersion int  `json:"schemaVersion"`
		Hover         bool `json:"hover"`
		Accounts      []struct {
			Email string `json:"email"`
			Usage struct {
				Windows map[string]map[string]any `json:"windows"`
				Pace    map[string]map[string]any `json:"pace"`
			} `json:"usage"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if payload.SchemaVersion != 1 || !payload.Hover || len(payload.Accounts) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	account := payload.Accounts[0]
	week := account.Usage.Windows["seven_day"]
	if account.Email != "work@example.com" || week["utilizationPct"] != float64(55) ||
		week["thresholdPct"] != float64(143) || week["slackPct"] != float64(88) {
		t.Errorf("account = %+v, seven_day = %+v", account, week)
	}
	if account.Usage.Pace["seven_day"]["expectedPct"] != float64(43) {
		t.Errorf("pace = %+v, want expectedPct 43", account.Usage.Pace["seven_day"])
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
	if !strings.Contains(stderr, "ccdad status") {
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

// seedLiveAs writes Claude Code's credentials file holding one stored account's
// login, which is what makes switcher.Evaluate able to name the live account.
func seedLiveAs(t *testing.T, uuid string) {
	t.Helper()
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"AT-`+uuid+`","refreshToken":"RT-`+uuid+`"}}`)
}
