package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/forecast"
	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// seedHistory writes a series straight into history.json, mirroring
// seedUsageEntry. No existing helper can manufacture one, and every measured
// answer in this file needs several readings that the poller would have taken
// hours apart.
//
// It goes through WithHistory and Put rather than through Record because Record
// prunes against a wall clock, and every test here runs on a frozen one set
// days in the past — the samples it means to seed are exactly the ones
// retention would drop.
func seedHistory(t *testing.T, uuid string, samples ...history.Sample) {
	t.Helper()
	if err := history.WithHistory(5*time.Second, func(h *history.History) error {
		for _, s := range samples {
			h.Put(uuid, s)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// seedAccountWithTier stores an account carrying the rate_limit_tier the
// profile endpoint reported. It is a separate seeder because the tri-state that
// field carries is the whole subject of one test below: "" is a real state, and
// it is a DIFFERENT field from Account.Tier, which is organization_type.
func seedAccountWithTier(t *testing.T, uuid, email, rateLimitTier string, addedAt time.Time) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{
		UUID: uuid, Email: email, RateLimitTier: rateLimitTier, AddedAt: addedAt,
	}, credsFor("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
}

var (
	// The moments both fixture windows below roll over. The weekly one is three
	// days out, which is longer than the fleet takes to empty at the seeded
	// rate, so the answer is not an artefact of a reset landing inside the run.
	runwayWeeklyReset = statusNow.Add(72 * time.Hour)
	runwayFiveReset   = statusNow.Add(2 * time.Hour)

	// runwayAddedAt is a day before the oldest seeded sample. It has to be
	// stated: the read side drops every sample older than its account's
	// AddedAt, and store.Add stamps a NEW account with the wall clock, which on
	// a suite frozen days in the past discards the whole series.
	runwayAddedAt = statusNow.Add(-24 * time.Hour)

	runwayTier = "default_claude_max_20x"
)

// seedBurningAccount is one account that has been polled every half hour for
// the last two and has been spending the whole time.
//
// Two hours rather than four, deliberately: the span a rate is measured over is
// four hours, so a fixture observed for exactly that long makes the measured
// span and the measuring window the same number, and a renderer that printed
// the wrong one of the two would look right.
//
// Both axes rise by two points an hour, from levels chosen so the two answers
// differ: the five-hour window rolls over every five hours and holds at that
// rate, and the weekly one does not come back before the fleet empties.
func seedBurningAccount(t *testing.T, uuid, email, rateLimitTier string, addedAt time.Time) {
	t.Helper()
	seedAccountWithTier(t, uuid, email, rateLimitTier, addedAt)
	seedUsageEntry(t, uuid, usage.Entry{
		FetchedAt: statusNow,
		Snapshot: &usage.Snapshot{
			FiveHour: window(18, runwayFiveReset),
			SevenDay: window(48, runwayWeeklyReset),
		},
	})
	var samples []history.Sample
	for i := 4; i >= 0; i-- {
		at := statusNow.Add(-time.Duration(i) * 30 * time.Minute)
		five, weekly := 18-float64(i), 48-float64(i)
		samples = append(samples, history.Sample{At: at, Windows: map[usage.WindowName]history.Reading{
			usage.WindowFiveHour: {Pct: five, Reset: runwayFiveReset},
			usage.WindowSevenDay: {Pct: weekly, Reset: runwayWeeklyReset},
		}})
	}
	seedHistory(t, uuid, samples...)
}

// seedBurningFleet is the two-account machine every measured answer below reads.
func seedBurningFleet(t *testing.T) {
	t.Helper()
	seedBurningAccount(t, "uuid-a", "a@example.com", runwayTier, runwayAddedAt)
	seedBurningAccount(t, "uuid-b", "b@example.com", runwayTier, runwayAddedAt)
}

// Not enough history is the true state of a store that has been recording for
// ten minutes. It is not a failure to answer and not a negative answer; exit 3
// and exit 5 both say more than is known.
func TestRunwayOnAColdMachineExitsZeroAndStillCarriesItsPayload(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedHealthyMachine(t)

	code, out, errOut, top := runRoot(t, "runway", "--json")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one document: %v\n%s", err, out)
	}
	f, ok := doc["forecast"].(map[string]any)
	if !ok {
		t.Fatal("forecast is absent; the contract row promises it and a promise that vanishes on a cold machine cannot be pinned")
	}
	basis, ok := f["basis"].(map[string]any)
	if !ok {
		t.Fatal("no basis object")
	}
	if basis["readings"] != float64(0) {
		t.Errorf("basis.readings = %v, want 0 on a machine that has recorded nothing", basis["readings"])
	}
	// A measurement that never happened has no span. Publishing four hours here
	// would describe evidence the store does not hold.
	if _, ok := basis["observedSeconds"]; ok {
		t.Errorf("basis carries observedSeconds on a machine with no readings: %v", basis)
	}
	if _, ok := f["axes"]; ok {
		t.Errorf("the payload carries axes measured from nothing: %v", f)
	}
	if errOut == "" {
		t.Error("nothing on stderr; the human half must say why there is no answer")
	}

	// The human half of the same machine. It prints what it knows -- how much
	// evidence there is -- and stops: a table of question marks under a
	// "VERDICT" heading looks like an answer, and there is none to give.
	code, human, humanErr, top := runRoot(t, "runway")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	if !strings.Contains(human, "Basis:   nothing measured yet") {
		t.Errorf("stdout does not say the basis is empty:\n%s", human)
	}
	if strings.Contains(human, "VERDICT") {
		t.Errorf("a machine with no readings drew the verdict table anyway:\n%s", human)
	}
	if humanErr == "" {
		t.Error("nothing on stderr for the human form either")
	}
}

// `holds` is absent, not false, when the two runs of the measured band
// disagreed. A wide band straddling the boundary decides nothing, and `false`
// is a decision -- a consumer gating on it would treat "we cannot tell" as "it
// runs dry", which is the fail-open reading in the other direction.
func TestAnUndecidedAxisPublishesNoVerdictAtAll(t *testing.T) {
	undecided := axisJSON(forecast.Axis{
		Burn: forecast.Band{Low: 4, High: 9, Known: true}, Replenish: 3,
	})
	if _, ok := undecided["holds"]; ok {
		t.Errorf("holds = %v on an axis whose two runs disagreed; the key must be absent", undecided["holds"])
	}
	if _, ok := undecided["dryAt"]; ok {
		t.Errorf("dryAt = %v on an axis that decided nothing", undecided["dryAt"])
	}
	// The rate is still published: it was measured, and the band is what says
	// why no verdict came out of it.
	if undecided["burnPpPerHour"] != 4.0 || undecided["burnPpPerHourHigh"] != 9.0 {
		t.Errorf("axis = %v, want the measured band published beside the missing verdict", undecided)
	}

	held := axisJSON(forecast.Axis{Burn: forecast.Band{Low: 1, High: 2, Known: true}, Verdict: forecast.VerdictHolds})
	if held["holds"] != true {
		t.Errorf("holds = %v on an axis both runs held", held["holds"])
	}
	// An unmeasured axis publishes no rate at all, rather than a rate of zero.
	// A fleet nobody has enough readings for is not a fleet burning nothing.
	unmeasured := axisJSON(forecast.Axis{Replenish: 3})
	if _, ok := unmeasured["burnPpPerHour"]; ok {
		t.Errorf("axis = %v, want no rate published for a band that cleared no gate", unmeasured)
	}
}

// The human table, whole. Every figure in it is checkable against the fixture:
// two accounts each spending two points an hour on both axes for four hours is
// four points an hour across the fleet, and each has 52 of its 100 weekly
// points left.
func TestRunwayRendersTheMeasuredFleet(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)

	code, out, _, top := runRoot(t, "runway")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	// The two header lines are this file's own formatting and are asserted
	// byte for byte. The table rows below go through a tabwriter, whose column
	// widths are arithmetic over the whole block, so they are compared with
	// runs of spaces collapsed: what is being pinned is which cell carries
	// which figure, not how wide the widest email address was.
	for _, want := range []string{
		"Basis:   the last 2h00m  (2 accounts, 10 readings, 0 unreadable)",
		"Fleet:   104 of 200 points left on the weekly axis",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout carries no %q:\n%s", want, out)
		}
	}
	rows := squeezedLines(out)
	for _, want := range []string{
		"AXIS BURN REPLENISHES VERDICT",
		// The five-hour axis rolls over inside the run and comes back faster
		// than the fleet spends it; the weekly one does not.
		"5-hour 4.0 pp/h 40.0 pp/h holds",
		// Three glyphs, three meanings, on one row: credits could not be read
		// here, and they have no replenishment to read at all.
		"Credits ? - ?",
		"IDX ACCOUNT WINDOW LEFT BURN EMPTY",
		// LEFT is bare, and BURN is this account's OWN rate: the two rows sum
		// to the 4.0 pp/h on the 7-day row above them, which is the column's
		// whole point. The label, not the uuid — the forecast knows nothing but
		// uuids, so a renderer that skipped the lookup would print one.
		"1 a@example.com seven_day 52 2.0 pp/h ",
		"2 b@example.com seven_day 52 2.0 pp/h ",
	} {
		if !hasRow(rows, want) {
			t.Errorf("no row %q in:\n%s", want, out)
		}
	}
	if !hasRow(rows, "7-day 4.0 pp/h 1.2 pp/h runs dry ") {
		t.Errorf("the weekly axis does not run dry, at four points an hour against 1.2 back:\n%s", out)
	}
	// `ccdad status`'s human output is asserted to carry no percentage
	// belonging to a window its table is not reporting, and this column reports
	// a different window from that one.
	if strings.Contains(out, "52%") {
		t.Errorf("the LEFT column carries a percent sign:\n%s", out)
	}
}

// squeezedLines is stdout with every run of whitespace inside a line collapsed
// to one space, so a tabwriter's padding does not have to be recomputed by hand
// in every assertion above.
func squeezedLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}
	return lines
}

// hasRow matches a whole squeezed row, or its beginning when the wanted text
// ends in a space — which is how a row whose last cell is a timestamp is pinned
// without pinning the machine's zone.
func hasRow(lines []string, want string) bool {
	for _, line := range lines {
		if line == want || (strings.HasSuffix(want, " ") && strings.HasPrefix(line+" ", want)) {
			return true
		}
	}
	return false
}

// The payload a consumer writes against. There is no top-level burn figure: one
// percentage point of a five-hour window and one of a weekly window are
// different quantities, and a consumer that added them would get a number with
// no unit.
func TestRunwayJSONCarriesTheAxesItMeasured(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)

	code, out, _, top := runRoot(t, "runway", "--json")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	var doc struct {
		Forecast struct {
			Basis map[string]any `json:"basis"`
			Axes  map[string]struct {
				Burn      *float64 `json:"burnPpPerHour"`
				High      *float64 `json:"burnPpPerHourHigh"`
				Replenish *float64 `json:"replenishPpPerHour"`
				Holds     *bool    `json:"holds"`
				DryAt     string   `json:"dryAt"`
			} `json:"axes"`
			Fleet  map[string]any `json:"fleet"`
			Credit map[string]any `json:"credit"`
		} `json:"forecast"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one document: %v\n%s", err, out)
	}
	f := doc.Forecast
	// Two figures, not one: observedSeconds is what these rates were divided
	// by and windowSeconds is how far back the measurement was willing to
	// look. A fleet polled for two of the four hours has two hours of evidence,
	// and a reader weighing the answer needs to be told which is which.
	if f.Basis["observedSeconds"] != float64(2*3600) || f.Basis["readings"] != float64(10) {
		t.Errorf("basis = %v, want two hours of ten readings", f.Basis)
	}
	if f.Basis["windowSeconds"] != float64(4*3600) {
		t.Errorf("basis.windowSeconds = %v, want the span a rate is measured over", f.Basis["windowSeconds"])
	}
	weekly, ok := f.Axes["weekly"]
	if !ok {
		t.Fatalf("no weekly axis in %v", f.Axes)
	}
	if weekly.Burn == nil || *weekly.Burn != 4 {
		t.Errorf("weekly.burnPpPerHour = %v, want 4", weekly.Burn)
	}
	// The upper bound is published beside the figure, not instead of it: it
	// carries one quantisation step per contributing account, and it is what
	// the "holds" claim on the five-hour axis had to survive.
	if weekly.High == nil || *weekly.High != 5 {
		t.Errorf("weekly.burnPpPerHourHigh = %v, want 5", weekly.High)
	}
	if weekly.Holds == nil || *weekly.Holds {
		t.Errorf("weekly.holds = %v, want false: the fleet spends faster than the week gives back", weekly.Holds)
	}
	if weekly.DryAt == "" {
		t.Error("weekly runs dry and the payload names no moment")
	}
	five, ok := f.Axes["five_hour"]
	if !ok {
		t.Fatalf("no five_hour axis in %v", f.Axes)
	}
	if five.Holds == nil || !*five.Holds {
		t.Errorf("five_hour.holds = %v, want true", five.Holds)
	}
	if five.DryAt != "" {
		t.Errorf("five_hour holds and still carries dryAt = %q", five.DryAt)
	}
	if f.Fleet["pointsLeft"] != float64(104) || f.Fleet["pointsTotal"] != float64(200) {
		t.Errorf("fleet = %v, want 104 of 200", f.Fleet)
	}
	// Money fails closed: no account here reported paid usage, so there is no
	// credit object rather than one full of zeros.
	if f.Credit != nil {
		t.Errorf("credit = %v on a fleet with no readable paid usage", f.Credit)
	}
}

// A series belongs to the account that was in the store when it was taken. An
// account removed and added again at the same uuid is a fresh login, and
// handing it its predecessor's slope would report spending it never did.
func TestRunwayDoesNotInheritAPreviousLoginsSlope(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	// The store's account was added a minute from now; every seeded sample was
	// taken before that, so all of them belong to the login this uuid used to
	// carry.
	seedBurningAccount(t, "uuid-a", "a@example.com", runwayTier, statusNow.Add(time.Minute))

	code, out, _, top := runRoot(t, "runway", "--json")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one document: %v\n%s", err, out)
	}
	f := doc["forecast"].(map[string]any)
	if _, ok := f["axes"]; ok {
		t.Errorf("the fresh login was measured on the removed account's readings: %v", f)
	}
	basis := f["basis"].(map[string]any)
	if basis["readings"] != float64(0) {
		t.Errorf("basis.readings = %v, want 0: every sample predates this login", basis["readings"])
	}
}

// Summing percentage points across accounts assumes their quotas are the same
// size, and rate_limit_tier is the only evidence of that this build has. It is
// NOT Account.Tier, which is organization_type and is the same string for two
// seats on very different plans.
func TestRunwayNamesTheMixedTiersItSummedAcross(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningAccount(t, "uuid-a", "a@example.com", runwayTier, runwayAddedAt)
	seedBurningAccount(t, "uuid-b", "b@example.com", "default_claude_max_5x", runwayAddedAt)

	code, _, errOut, top := runRoot(t, "runway")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	for _, want := range []string{"default_claude_max_5x", "default_claude_max_20x"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr names no %q, so a reader cannot check the mix:\n%s", want, errOut)
		}
	}
}

// An empty store is an answer, not a failure — the same call `ccdad list` and
// `ccdad status` make. --json still writes its document, because the contract
// is that the flag changes the representation and never the answer.
func TestRunwayWithNoAccountsSaysSoAndStillAnswers(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)

	code, out, errOut, top := runRoot(t, "runway")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing: there is no fleet to report on", out)
	}
	if !strings.Contains(errOut, "ccdad add") {
		t.Errorf("stderr = %q, which does not say what to do about it", errOut)
	}
}

// seedPaidAccount is one account with paid usage switched on: a hundred-dollar
// monthly cap, fifty of it already spent, and a series in which the spend rose
// a dollar every half hour for the last two.
//
// It spells both money representations on purpose. The wire reports MINOR units
// and usage.ExtraUsage converts them; history.json stores what that conversion
// returned, in major ones. A fixture that used one figure for both would pass
// against an arithmetic that divided by a hundred once too often or not at all.
func seedPaidAccount(t *testing.T, uuid, email string) {
	t.Helper()
	seedAccountWithTier(t, uuid, email, runwayTier, runwayAddedAt)
	limit, used := 10000.0, 5000.0 // cents: a $100 cap, $50 spent
	seedUsageEntry(t, uuid, usage.Entry{
		FetchedAt: statusNow,
		Snapshot: &usage.Snapshot{
			FiveHour: window(18, runwayFiveReset),
			SevenDay: window(48, runwayWeeklyReset),
			ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
				State: usage.ExtraUsageEnabled, Currency: "USD",
				MonthlyLimit: &limit, UsedCredits: &used,
			}),
		},
	})
	major := 100.0
	var samples []history.Sample
	for i := 4; i >= 0; i-- {
		at := statusNow.Add(-time.Duration(i) * 30 * time.Minute)
		spent := 14 - float64(i)
		samples = append(samples, history.Sample{
			At: at,
			Windows: map[usage.WindowName]history.Reading{
				usage.WindowFiveHour: {Pct: 18 - float64(i), Reset: runwayFiveReset},
				usage.WindowSevenDay: {Pct: 48 - float64(i), Reset: runwayWeeklyReset},
			},
			Credit: &history.Credit{Used: spent, Limit: &major, Currency: "USD"},
		})
	}
	seedHistory(t, uuid, samples...)
}

// The credit row, measured end to end: the balance off the current snapshot,
// the spend rate off the series, and a date that is the first divided by the
// second. Nothing else on this page is money, and money is the one quantity
// this repository fails closed on — every unit test around it works on
// hand-built halves, and only a rendered row can show the two were joined.
func TestRunwayRendersTheCreditsItMeasured(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedPaidAccount(t, "uuid-a", "a@example.com")

	code, out, _, top := runRoot(t, "runway")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	// Four dollars over the two hours observed is two an hour, and the fifty
	// dollars left of the cap last twenty-five of them. The middle cell stays
	// "-": credits have no renewal boundary to read, which is a different
	// answer from one that could not be read.
	if !hasRow(squeezedLines(out), "Credits 2.00 USD/h - runs dry ") {
		t.Errorf("no measured credit row in:\n%s", out)
	}

	// The same measurement through the other half of this command, because the
	// two halves are published by different code and only one of them was
	// pinned. Elsewhere in this file the credit object is asserted ABSENT on a
	// fleet with no readable paid usage, which is the fail-closed side of the
	// rule; a payload that never published the object at all would satisfy that
	// assertion perfectly. This is the side that says the key exists when the
	// money was read, and `ccdad status --json` and `ccdad list --json` carry
	// this same object under this same key.
	code, doc, _, top := runRoot(t, "runway", "--json")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	var parsed struct {
		Forecast struct {
			Credit *struct {
				Currency     string  `json:"currency"`
				SpendPerHour float64 `json:"spendPerHour"`
				DryAt        string  `json:"dryAt"`
			} `json:"credit"`
		} `json:"forecast"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("stdout is not one document: %v\n%s", err, doc)
	}
	credit := parsed.Forecast.Credit
	if credit == nil {
		t.Fatalf("no credit object on a fleet whose paid usage was read:\n%s", doc)
	}
	// The currency is carried rather than assumed: a figure of 2.00 means
	// nothing without it, and nothing in this payload names a default one.
	if credit.Currency != "USD" {
		t.Errorf("credit.currency = %q, want the currency the account reported", credit.Currency)
	}
	if credit.SpendPerHour != 2 {
		t.Errorf("credit.spendPerHour = %v, want the two an hour the row above renders", credit.SpendPerHour)
	}
	if credit.DryAt == "" {
		t.Error("credit.dryAt is empty; the balance runs out at a moment and the row above prints it")
	}
}

// An account the rotation cannot switch to is not the fleet's runway.
//
// A disabled seat is polled on cadence, has a cache entry and has a recorded
// series, so nothing about the evidence marks it: only the store's own flag
// does. Counting it doubles this fixture's points and pushes the moment the
// fleet empties most of a day out — an over-promise on the headline figure of
// this command, made of quota no switch can reach.
func TestRunwayLeavesAnAccountOutOfRotationOutOfTheFleet(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)
	if code, _, errOut, _ := runRoot(t, "disable", "2"); code != ExitOK {
		t.Fatalf("disable 2 = %d (%s)", code, errOut)
	}

	code, out, _, top := runRoot(t, "runway")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	// Half the points of the two-account fixture, and the basis says where the
	// other account went: it is in the store and out of the rotation, so no
	// figure on this page covers it.
	for _, want := range []string{
		"Fleet:   52 of 100 points left on the weekly axis",
		"Basis:   the last 2h00m  (2 accounts, 5 readings, 0 unreadable, 1 not in rotation)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout carries no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "b@example.com") {
		t.Errorf("the disabled account has a row of its own:\n%s", out)
	}
	// And the axis figures are one account's, not two.
	if !hasRow(squeezedLines(out), "7-day 2.0 pp/h 0.6 pp/h runs dry ") {
		t.Errorf("the weekly axis still carries both accounts' burn:\n%s", out)
	}
}

// --out writes the same bytes --json puts on stdout, at 0600, and stdout stays
// empty. The mode is the reason the flag exists in this repository at all: a
// shell redirect creates the file at the umask, typically 0644, in whatever
// directory the shell happens to be in. `ccdad export --out` in
// internal/cli/export.go made the same call first, and this is that flag rather
// than a second one that means something slightly different.
func TestOutWritesTheDocumentAtModeSixHundred(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)

	path := filepath.Join(t.TempDir(), "runway.json")
	code, stdout, errOut, top := runRoot(t, "runway", "--json", "--out", path)
	if code != ExitOK {
		t.Fatalf("runway --json --out = %d (%s), want 0", code, top)
	}
	// Nothing on stdout: the point of naming a file is that the document went
	// there, and a command that wrote it to both would hand a pipeline the
	// payload it was told to keep out of one.
	if stdout != "" {
		t.Errorf("the document went to the file AND to stdout:\n%s", stdout)
	}
	if !strings.Contains(errOut, path) {
		t.Errorf("stderr = %q, want it to name the file it wrote", errOut)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX mode bits to assert on, and the atomic writer cannot
	// give it any; every other target is held to 0600.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("%s is %04o, want 0600 — a redirect at the umask is what --out exists to avoid",
			path, info.Mode().Perm())
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The file is the document, checked before it is compared: two empty
	// strings are equal, and an assertion that only compared them would pass on
	// a command that wrote nothing anywhere.
	doc := decodeContractDocument(t, string(written))
	if _, ok := doc["forecast"]; !ok {
		t.Fatalf("the file carries no forecast:\n%s", written)
	}
	_, onStdout, _, _ := runRoot(t, "runway", "--json")
	if string(written) != onStdout {
		t.Errorf("--out and --json wrote different bytes\nfile:\n%s\nstdout:\n%s", written, onStdout)
	}
}

// --out without --json is a usage error, not a silent choice of one of the two
// representations. `ccdad export` makes the same call for --include-mcp: a flag
// alone is a usage error rather than a silent upgrade, because a flag the user
// did not pass is not something to infer an output from.
func TestOutWithoutJSONIsAUsageError(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)

	path := filepath.Join(t.TempDir(), "f.json")
	code, stdout, errOut, top := runRoot(t, "runway", "--out", path)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (%s)", code, ExitUsage, top)
	}
	// The refusal names both halves of the pair, so the fix is in the message
	// rather than in the help text.
	for _, want := range []string{"--out", "--json"} {
		if !strings.Contains(errOut+top, want) {
			t.Errorf("the refusal does not name %s:\n%s\n%s", want, errOut, top)
		}
	}
	if stdout != "" {
		t.Errorf("the refusal rendered an answer anyway:\n%s", stdout)
	}
	// And it wrote no file. A usage error that had already created the target
	// would leave a caller holding a file it was told it could not have.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want the file never to have been created", path, err)
	}
}

// The --json contract is untouched: its table row in json_contract_test.go
// passes no --out, so stdout still carries exactly one indented object and
// stderr carries no word about a file.
func TestOutDoesNotChangeThePlainJSONAnswer(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)

	code, stdout, errOut, top := runRoot(t, "runway", "--json")
	if code != ExitOK {
		t.Fatalf("runway --json = %d (%s), want 0", code, top)
	}
	doc := decodeContractDocument(t, stdout)
	if _, ok := doc["forecast"]; !ok {
		t.Fatalf("stdout carries no forecast:\n%s", stdout)
	}
	if !strings.Contains(stdout, "\n  \"") {
		t.Errorf("the payload is not indented, so this command is not going through writeJSON:\n%s", stdout)
	}
	if strings.Contains(errOut, "0600") {
		t.Errorf("a run that named no file reported writing one:\n%s", errOut)
	}
}

// seedAccountClimbingAt is one account polled every half hour for the last two
// hours, whose two axes climb at the two rates named.
//
// It exists beside seedBurningAccount because that fixture's two axes climb
// TOGETHER, and which axis a fleet needs its next account for is decided by the
// ratio between them: an account gives the five-hour axis 20 points an hour back
// and the weekly axis 100/168, so the two axes ask for the same number of
// accounts exactly when the five-hour rate is 33.6 times the weekly one. A
// fixture that could not move that ratio could not put two fleets on opposite
// sides of it.
//
// The five-hour window starts empty and the weekly one at 40, so that a
// five-hour rate steep enough to bind does not walk the account to 100 inside
// the observed two hours. That would measure a fleet that had lost a seat rather
// than one that is spending.
func seedAccountClimbingAt(t *testing.T, uuid, email string, fivePerHour, weeklyPerHour float64) {
	t.Helper()
	const fiveFrom, weeklyFrom = 0.0, 40.0
	seedAccountWithTier(t, uuid, email, runwayTier, runwayAddedAt)
	var samples []history.Sample
	for i := 4; i >= 0; i-- {
		at := statusNow.Add(-time.Duration(i) * 30 * time.Minute)
		// Five readings thirty minutes apart span two hours, so the rate this
		// produces is exact: Σ max(0, Δ) over that span is the figure asked for.
		elapsed := 2 - float64(i)/2
		samples = append(samples, history.Sample{At: at, Windows: map[usage.WindowName]history.Reading{
			usage.WindowFiveHour: {Pct: fiveFrom + elapsed*fivePerHour, Reset: runwayFiveReset},
			usage.WindowSevenDay: {Pct: weeklyFrom + elapsed*weeklyPerHour, Reset: runwayWeeklyReset},
		}})
	}
	seedHistory(t, uuid, samples...)
	// The cache and the newest sample agree because they are the same reading.
	// The cache is authoritative for every level and the series for nothing but
	// slopes, so a fixture whose two disagreed would be a machine that cannot
	// exist.
	seedUsageEntry(t, uuid, usage.Entry{
		FetchedAt: statusNow,
		Snapshot: &usage.Snapshot{
			FiveHour: window(fiveFrom+2*fivePerHour, runwayFiveReset),
			SevenDay: window(weeklyFrom+2*weeklyPerHour, runwayWeeklyReset),
		},
	})
}

// runwayFleetObject is the `fleet` object of one `ccdad runway --json` run.
func runwayFleetObject(t *testing.T) map[string]any {
	t.Helper()
	code, out, _, top := runRoot(t, "runway", "--json")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one document: %v\n%s", err, out)
	}
	f, ok := doc["forecast"].(map[string]any)
	if !ok {
		t.Fatalf("no forecast object:\n%s", out)
	}
	fleet, ok := f["fleet"].(map[string]any)
	if !ok {
		t.Fatalf("no fleet object:\n%s", out)
	}
	return fleet
}

// The seat count, rendered under the axis block and above the per-account
// table.
//
// It answers that block from the other end: those rows say when the fleet runs
// out, and this says how many accounts it would take for it not to. Both come
// out of the same runs, which is why they can be read against each other -- a
// count from a second mechanism would be free to say "you have enough accounts"
// under a row that says "runs dry".
//
// Position is asserted rather than presence alone. Everything above the rows
// goes to the same stream and the rows go through a tabwriter that holds them
// until Flush, so a line written anywhere before that call still lands above the
// table; what the position in the function decides is which side of the axis
// block's prose it falls on.
func TestRunwayRendersTheAccountsLineUnderTheAxisBlock(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)

	code, out, _, top := runRoot(t, "runway")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	// Two accounts, and the weekly axis is the one that cannot hold: the fleet
	// spends 5 points an hour at the upper bound of the band and an account
	// gives back 100/168 = 0.595, so nine seats supply 5.36 and eight supply
	// 4.76.
	const want = "Accounts:  2 usable, 9 needed to hold at this rate  (7 more)"
	if !strings.Contains(out, want) {
		t.Errorf("stdout carries no %q:\n%s", want, out)
	}

	lines := strings.Split(out, "\n")
	accounts, prose, table := -1, -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "Accounts:"):
			accounts = i
		case strings.Contains(l, "Credits do not reset"):
			prose = i
		case strings.Contains(l, "IDX"):
			table = i
		}
	}
	if accounts < 0 || prose < 0 || table < 0 {
		t.Fatalf("accounts = %d, prose = %d, table = %d; one of the three blocks is missing:\n%s",
			accounts, prose, table, out)
	}
	if accounts < prose || accounts > table {
		t.Errorf("the accounts line is not between the axis block and the per-account table:\n%s", out)
	}
}

// The exact form of a fleet that is already the smallest one that holds: the
// count and nothing after it. The parenthetical is the actionable half, and
// there is nothing here to act on.
func TestAFleetThatIsExactlyBigEnoughGetsNoParenthetical(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	// One account whose two axes did not move. Its measured burn is zero and
	// its upper bound is one quantisation step over two hours -- 0.5 points an
	// hour -- which both axes give back faster than, so the fleet holds and the
	// search downward stops at the one seat it has.
	seedAccountClimbingAt(t, "uuid-a", "a@example.com", 0, 0)

	code, out, _, top := runRoot(t, "runway")
	if code != ExitOK {
		t.Fatalf("code = %v, want ExitOK (%s)", code, top)
	}
	const want = "Accounts:  1 usable, 1 needed to hold at this rate"
	if !strings.Contains(out, want) {
		t.Errorf("stdout carries no %q:\n%s", want, out)
	}
	if strings.Contains(out, "to spare") || strings.Contains(out, "more)") {
		t.Errorf("a fleet sitting on the smallest count that holds was given a parenthetical:\n%s", out)
	}
}

// accountsNeeded is omitted when there is no basis, never zeroed: a consumer
// cannot tell a fleet that needs no more accounts from a fleet nobody measured,
// and one of those two is a reason to go and buy nothing.
//
// accountsUsable is published either way, because it is a count that was always
// readable -- the run had that many accounts to work with whether or not it had
// any history to measure them over.
func TestTheFleetObjectCarriesTheAccountCountsOnlyWhenMeasured(t *testing.T) {
	t.Run("measured", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		seedBurningFleet(t)

		fleet := runwayFleetObject(t)
		if fleet["accountsUsable"] != float64(2) {
			t.Errorf("accountsUsable = %v, want the two accounts the run worked with", fleet["accountsUsable"])
		}
		needed, ok := fleet["accountsNeeded"].(float64)
		if !ok {
			t.Fatalf("fleet = %v, which carries no accountsNeeded for a fleet that was measured", fleet)
		}
		if needed <= 2 {
			t.Errorf("accountsNeeded = %v on a fleet whose weekly axis runs dry with two", needed)
		}
		if fleet["accountsNeededBy"] != "weekly" {
			t.Errorf("accountsNeededBy = %v, want the axis that asked for the seats", fleet["accountsNeededBy"])
		}
	})

	t.Run("nothing measured", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		// One reading and nothing older than it: the machine that has been
		// recording for ten minutes. The account is perfectly readable, so the
		// count of seats exists; the slope does not.
		seedAccountAddedAt(t, "uuid-a", "a@example.com", runwayAddedAt)
		seedUsageEntry(t, "uuid-a", usage.Entry{
			FetchedAt: statusNow,
			Snapshot: &usage.Snapshot{
				FiveHour: window(18, runwayFiveReset),
				SevenDay: window(48, runwayWeeklyReset),
			},
		})

		fleet := runwayFleetObject(t)
		if fleet["accountsUsable"] != float64(1) {
			t.Errorf("accountsUsable = %v, want the one readable account", fleet["accountsUsable"])
		}
		for _, key := range []string{"accountsNeeded", "accountsNeededBy"} {
			if v, ok := fleet[key]; ok {
				t.Errorf("%s = %v on a fleet with no basis; absent and zero are different answers", key, v)
			}
		}
	})
}

// accountsNeededBy names the axis that asked for the seats, and which axis that
// is is MEASURED. An account gives the five-hour axis 20 points an hour back and
// the weekly axis 100/168, so the two axes want the same number of accounts
// exactly when the five-hour rate is 168/5 = 33.6 times the weekly one. These
// two fleets sit on either side of that ratio.
//
// The value is spelled the way the sibling `axes` object spells its keys, so it
// is a key into that object rather than a second vocabulary for one axis.
func TestTheBindingAxisIsReportedAndCanBeEither(t *testing.T) {
	t.Run("weekly", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		// Both axes at 2 points an hour per account: a ratio of 1, far under
		// 33.6, so the weekly axis runs out of accounts first.
		seedBurningFleet(t)

		if got := runwayFleetObject(t)["accountsNeededBy"]; got != "weekly" {
			t.Errorf("accountsNeededBy = %v, want weekly: the five-hour axis holds on the seats the fleet has", got)
		}
	})

	t.Run("five_hour", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		// 30 points an hour on the five-hour axis against a weekly axis that
		// did not move: a ratio far over 33.6. One account gives back 20 an
		// hour, so the five-hour axis asks for a second seat while the weekly
		// axis holds on one.
		seedAccountClimbingAt(t, "uuid-a", "a@example.com", 30, 0)

		fleet := runwayFleetObject(t)
		if got := fleet["accountsNeededBy"]; got != "five_hour" {
			t.Errorf("accountsNeededBy = %v, want five_hour on a fleet burning 30 points an hour against 20 back", got)
		}
		if got := fleet["accountsNeeded"]; got != float64(2) {
			t.Errorf("accountsNeeded = %v, want 2: the band's upper bound is 30.5 an hour and two seats give back 40", got)
		}
	})
}

// A search that reached its bound publishes the bound AND says it is one. The
// count on its own would be a number a consumer could act on, and acting on it
// buys that many accounts for a fleet the run never found a holding size for.
func TestASearchThatReachedItsBoundIsPublishedAsABound(t *testing.T) {
	capped, ok := forecastJSON(forecast.Fleet{
		Basis:          forecast.Basis{Known: true},
		AccountsUsable: 3, AccountsNeeded: 256, HasNeeded: true, NeededCapped: true,
	})["fleet"].(map[string]any)
	if !ok {
		t.Fatal("no fleet object")
	}
	if capped["accountsNeeded"] != 256 {
		t.Errorf("accountsNeeded = %v, want the bound the search stopped at", capped["accountsNeeded"])
	}
	if capped["accountsNeededCapped"] != true {
		t.Errorf("fleet = %v, which publishes a bound as though it were a count", capped)
	}

	found, ok := forecastJSON(forecast.Fleet{
		Basis:          forecast.Basis{Known: true},
		AccountsUsable: 3, AccountsNeeded: 9, HasNeeded: true,
	})["fleet"].(map[string]any)
	if !ok {
		t.Fatal("no fleet object")
	}
	if v, ok := found["accountsNeededCapped"]; ok {
		t.Errorf("accountsNeededCapped = %v on a search that found an answer; the flag is the exception, not a field", v)
	}
}

// The one-line summary `ccdad status` and `ccdad list` share carries the seat
// count only when the fleet is SHORT. A fleet that holds already has its answer
// in the word "holds", and a line read at a glance beside Daemon: and Active:
// cannot spend a clause on good news.
//
// Both surfaces render through view.RunwayLine, so this pins that they picked
// the clause up rather than that they each grew one.
func TestTheSummaryLineNamesTheNeedOnlyForAFleetThatCannotHold(t *testing.T) {
	t.Run("short", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		seedBurningFleet(t)

		for _, cmd := range []string{"status", "list"} {
			_, out, _, top := runRoot(t, cmd)
			line := runwaySummaryLine(t, out)
			if !strings.Contains(line, "need 9 (7 more)") {
				t.Errorf("%s (%s): the line names no seat count: %q", cmd, top, line)
			}
		}
	})

	t.Run("holding", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		seedAccountClimbingAt(t, "uuid-a", "a@example.com", 0, 0)

		for _, cmd := range []string{"status", "list"} {
			_, out, _, top := runRoot(t, cmd)
			line := runwaySummaryLine(t, out)
			if !strings.Contains(line, "holds on both axes") {
				t.Fatalf("%s (%s): the fixture no longer holds, so the absence below is not the one under test: %q", cmd, top, line)
			}
			if strings.Contains(line, "need") {
				t.Errorf("%s: a fleet that holds was told how many accounts it needs: %q", cmd, line)
			}
		}
	})
}

// runwaySummaryLine is the one line beginning "Runway:" in a rendered page.
func runwaySummaryLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "Runway:") {
			return l
		}
	}
	t.Fatalf("no runway line at all:\n%s", out)
	return ""
}
