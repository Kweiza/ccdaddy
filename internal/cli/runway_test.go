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
