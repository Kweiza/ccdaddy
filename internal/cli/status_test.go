package cli

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// statusNow is the clock every test in this file runs against.
var statusNow = mustTime("2026-08-22T12:00:00Z")

func freezeClock(t *testing.T, at time.Time) {
	t.Helper()
	saved := timeNow
	t.Cleanup(func() { timeNow = saved })
	timeNow = func() time.Time { return at }
}

// stubDaemon replaces the liveness probe. status must render a dashboard
// whatever the answer is, including "cannot determine", and a real broken lock
// is not something a test can arrange.
func stubDaemon(t *testing.T, r daemon.Report, err error) {
	t.Helper()
	saved := observeDaemon
	t.Cleanup(func() { observeDaemon = saved })
	observeDaemon = func() (daemon.Report, error) { return r, err }
}

func window(pct float64, reset time.Time) usage.Window {
	return usage.NewWindow(&pct, &reset)
}

// seedAccountAddedAt is seedAccount with the stamp under the test's control. It
// matters wherever a fixture reaches the ENGINE rather than only the cache: a
// reading older than its account's AddedAt is pruned as one that belonged to a
// previous login at the same uuid, and a frozen clock in the past makes every
// reading look exactly like that.
func seedAccountAddedAt(t *testing.T, uuid, email string, at time.Time) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{UUID: uuid, Email: email, AddedAt: at}, credsFor("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
}

func seedUsageEntry(t *testing.T, uuid string, e usage.Entry) {
	t.Helper()
	if err := usage.WithCache(5*time.Second, func(c *usage.Cache) error {
		c.Put(uuid, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func statusJSON(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("status --json did not emit one object: %v\n%s", err, stdout)
	}
	return out
}

func accountRow(t *testing.T, payload map[string]any, uuid string) map[string]any {
	t.Helper()
	rows, ok := payload["accounts"].([]any)
	if !ok {
		t.Fatalf("no accounts array in %v", payload)
	}
	for _, r := range rows {
		row := r.(map[string]any)
		if row["uuid"] == uuid {
			return row
		}
	}
	t.Fatalf("no row for %s in %v", uuid, rows)
	return nil
}

func TestStatusRendersTheDashboard(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	// The binding window is deliberately NOT the first one in schema order.
	// five_hour has more left, so seven_day binds — and a renderer that just
	// took RateLimitWindows()[0] would pass a fixture where the two agreed.
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-90 * time.Second),
		Snapshot: &usage.Snapshot{
			FiveHour: window(20, statusNow.Add(30*time.Minute)),
			SevenDay: window(62, statusNow.Add(2*time.Hour+14*time.Minute)),
		},
	})

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, stdout)
	}
	for _, want := range []string{"work@example.com", "62%", "seven_day", "2h14m", "1m"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the dashboard does not mention %q:\n%s", want, stdout)
		}
	}
	// The window with room to spare is not the one the row describes.
	for _, unwanted := range []string{"20%", "five_hour", "30m"} {
		if strings.Contains(stdout, unwanted) {
			t.Errorf("the dashboard shows %q, which is not the binding window:\n%s", unwanted, stdout)
		}
	}
}

// Unknown is never zero. A row whose usage cannot be read is not an empty
// account, and cswap's version of this bug parked its engine on the account
// that reset last.
func TestStatusRendersAnUnreadableAccountAsAQuestionMark(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "unread@example.com")

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "?") {
		t.Errorf("an account with no reading is not rendered as ?:\n%s", stdout)
	}
	if strings.Contains(stdout, "0%") {
		t.Errorf("an account with no reading was rendered as 0%%:\n%s", stdout)
	}
}

func TestStatusJSONOmitsUsageEntirelyWhenThereIsNoReading(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "unread@example.com")

	code, stdout, _, _ := runRoot(t, "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	row := accountRow(t, statusJSON(t, stdout), "uuid-a")
	if _, ok := row["usage"]; ok {
		t.Errorf("an account with no reading carries a usage object: %v", row)
	}
}

// A freshly reset account reports {"utilization":null,"resets_at":null}: the
// window is PRESENT and says nothing. That is a reading, so there is a usage
// object — but there is no number in it, and the row must say so rather than
// inventing a headroom of 100% out of a utilization nobody reported.
func TestAPresentWindowThatReportedNothingIsStillUnreadable(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "fresh@example.com")
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{FiveHour: usage.NewWindow(nil, nil)},
	})

	_, human, _, _ := runRoot(t, "status")
	if !strings.Contains(human, "?") {
		t.Errorf("a window that reported nothing is not rendered as ?:\n%s", human)
	}
	for _, unwanted := range []string{"0%", "100%"} {
		if strings.Contains(human, unwanted) {
			t.Errorf("a window that reported nothing was rendered as %s:\n%s", unwanted, human)
		}
	}

	_, out, _, _ := runRoot(t, "status", "--json")
	row := accountRow(t, statusJSON(t, out), "uuid-a")
	usageObj, ok := row["usage"].(map[string]any)
	if !ok {
		t.Fatalf("there IS a reading, so there must be a usage object: %v", row)
	}
	if v, ok := usageObj["headroomPct"]; ok {
		t.Errorf("headroomPct = %v was published although no window reported a utilization", v)
	}
	if v, ok := usageObj["bindingWindow"]; ok {
		t.Errorf("bindingWindow = %v was published although nothing binds", v)
	}
}

// The usage endpoint's budget is roughly 28-30 requests per identity per
// rolling HOUR, on a sliding window, so one burst saturates the identity for a
// full hour. A dashboard a user hammers must never be a source of those
// requests.
func TestStatusNeverFetches(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	// Far older than ServeTTL, so an implementation that refreshes stale rows
	// would refresh this one.
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-6 * time.Hour),
		Snapshot:  &usage.Snapshot{FiveHour: window(62, statusNow.Add(time.Hour))},
	})
	before, err := os.ReadFile(mustPath(usage.CachePath()))
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(mustPath(usage.CachePath()))
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	after, err := os.ReadFile(mustPath(usage.CachePath()))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("status rewrote the usage cache with different content, so it fetched")
	}
	// Content equality alone is not enough: a fetch that happened to return the
	// same numbers would still rewrite the file. Every write here is a rename,
	// so the modification time moves whether the bytes did or not.
	afterInfo, err := os.Stat(mustPath(usage.CachePath()))
	if err != nil {
		t.Fatal(err)
	}
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Error("status wrote the usage cache at all; a dashboard has no business touching it")
	}
	// And it rendered the stale reading rather than dropping it: a stale number
	// is still a number, and the age is what tells the user it is old.
	if !strings.Contains(stdout, "62%") {
		t.Errorf("the cached reading was not rendered:\n%s", stdout)
	}
}

// status is a dashboard, not a probe. Exit 5 belongs to `daemon status`.
func TestStatusWithNoDaemonExitsZero(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 — status is a dashboard, not a probe", code)
	}
	if !strings.Contains(strings.ToLower(stdout), "not running") {
		t.Errorf("the dashboard does not say the daemon is down:\n%s", stdout)
	}
}

func TestStatusReportsARunningDaemon(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion: daemon.StatusSchemaVersion,
			PID:           4242,
			StartedAt:     statusNow.Add(-3 * time.Hour),
			ActiveUUID:    "uuid-a",
		},
	}, nil)

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "running") || !strings.Contains(stdout, "4242") {
		t.Errorf("the dashboard does not report the running daemon:\n%s", stdout)
	}
	if !strings.Contains(stdout, "3h00m") {
		t.Errorf("the dashboard does not report how long it has been up:\n%s", stdout)
	}
}

// "Cannot tell" is not "no", and it is not a reason to refuse a dashboard
// either. A status that failed here would be unusable on exactly the NFS mount
// where the user most needs to see what is going on.
func TestStatusReportsAnUnprobeableLockAsUnknown(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	stubDaemon(t, daemon.Report{State: daemon.DaemonUnknown}, daemon.ErrLocksUnsupported)

	code, stdout, stderr, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "unknown") {
		t.Errorf("the dashboard does not report the daemon state as unknown:\n%s", stdout)
	}
	if !strings.Contains(stderr, "lock") {
		t.Errorf("the reason is not on stderr:\n%s", stderr)
	}
}

// A linear projection through bursty real usage is too rough to state as fact
// in a table a human reads, so it is computed and kept to --json.
func TestTheProjectionIsJSONOnly(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	// Four days into a seven-day window: past the 24-hour suppression, and
	// spending fast enough to have a projection.
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot: &usage.Snapshot{
			SevenDay: window(80, statusNow.Add(72*time.Hour)),
		},
	})

	_, human, _, _ := runRoot(t, "status")
	for _, forbidden := range []string{"projectedExhaustion", "willLastToReset", "exhaust"} {
		if strings.Contains(human, forbidden) {
			t.Errorf("the human table mentions %q, which is kept to --json:\n%s", forbidden, human)
		}
	}

	_, out, _, _ := runRoot(t, "status", "--json")
	row := accountRow(t, statusJSON(t, out), "uuid-a")
	usageObj, ok := row["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage object: %v", row)
	}
	pace, ok := usageObj["pace"].(map[string]any)
	if !ok {
		t.Fatalf("no pace object: %v", usageObj)
	}
	week, ok := pace["seven_day"].(map[string]any)
	if !ok {
		t.Fatalf("no seven_day pace: %v", pace)
	}
	if _, ok := week["projectedExhaustionAt"]; !ok {
		t.Errorf("--json does not carry projectedExhaustionAt: %v", week)
	}
	if _, ok := week["willLastToReset"]; !ok {
		t.Errorf("--json does not carry willLastToReset: %v", week)
	}
	if _, ok := week["expectedPct"]; !ok {
		t.Errorf("--json does not carry expectedPct: %v", week)
	}
}

// The pace reading is suppressed for the first seventh of a window: elapsed time
// is tiny then, so almost any usage divides out as "far ahead" and the dashboard
// cries wolf every Monday. For a seven-day window that seventh is 24 hours; for
// a five-hour window it is 43 minutes, which a fixed 24-hour rule could never
// have expressed.
func TestPaceIsSuppressedInTheFirstSeventhOfAWindow(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	// The seven-day reset is six and a half days out, so twelve hours have run.
	// The five-hour reset is four and three quarter hours out, so fifteen
	// minutes have. Both are inside their own seventh.
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot: &usage.Snapshot{
			FiveHour: window(30, statusNow.Add(4*time.Hour+45*time.Minute)),
			SevenDay: window(30, statusNow.Add(6*24*time.Hour+12*time.Hour)),
		},
	})

	_, out, _, _ := runRoot(t, "status", "--json")
	row := accountRow(t, statusJSON(t, out), "uuid-a")
	usageObj := row["usage"].(map[string]any)
	if pace, ok := usageObj["pace"]; ok {
		t.Errorf("pace was reported inside the first seventh of both windows: %v", pace)
	}

	_, human, _, _ := runRoot(t, "status")
	if strings.Contains(human, "ahead") {
		t.Errorf("the table calls a window inside its own suppression ahead of pace:\n%s", human)
	}
}

// A five-hour binding window carries a pace reading. This is the dashboard the
// README shows at the top of the file, and until the suppression became a share
// of the window rather than a fixed 24 hours the code could not produce it: a
// five-hour window is never 24 hours old.
func TestTheFiveHourWindowCarriesAPaceReading(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	// Four hours into a five-hour window: 90% spent against 80% elapsed.
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{FiveHour: window(90, statusNow.Add(time.Hour))},
	})

	_, out, _, _ := runRoot(t, "status", "--json")
	row := accountRow(t, statusJSON(t, out), "uuid-a")
	pace, ok := row["usage"].(map[string]any)["pace"].(map[string]any)
	if !ok {
		t.Fatalf("no pace object: %v", row["usage"])
	}
	if _, ok := pace["five_hour"]; !ok {
		t.Fatalf("--json carries no five_hour pace: %v", pace)
	}

	_, human, _, _ := runRoot(t, "status")
	if !strings.Contains(human, "ahead") {
		t.Errorf("the table does not call a five-hour window 90%% spent at 80%% elapsed ahead of pace:\n%s", human)
	}
}

func TestStatusJSONCarriesTheSchemaVersionAndTheDaemonState(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")

	code, stdout, _, _ := runRoot(t, "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	payload := statusJSON(t, stdout)
	if payload["schemaVersion"] != float64(1) {
		t.Errorf("schemaVersion = %v", payload["schemaVersion"])
	}
	d, ok := payload["daemon"].(map[string]any)
	if !ok {
		t.Fatalf("no daemon object: %v", payload)
	}
	if d["state"] != "stopped" {
		t.Errorf("daemon.state = %v, want stopped", d["state"])
	}
}

func TestStatusMarksTheActiveAccount(t *testing.T) {
	claude := isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "a@example.com")
	seedAccount(t, "uuid-b", "b@example.com")
	// The live credentials file carries uuid-b's refresh token, which is what
	// attribution anchors on — the same answer `which` and `list` give.
	if err := os.WriteFile(claude+"/.credentials.json",
		[]byte(liveLoginJSON("RT-uuid-b", "")), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stdout, _, _ := runRoot(t, "status", "--json")
	payload := statusJSON(t, stdout)
	if payload["activeUuid"] != "uuid-b" {
		t.Errorf("activeUuid = %v, want uuid-b", payload["activeUuid"])
	}
	if accountRow(t, payload, "uuid-b")["active"] != true {
		t.Error("the active row is not marked active")
	}
	if accountRow(t, payload, "uuid-a")["active"] != false {
		t.Error("an inactive row is marked active")
	}
}

func TestStatusWithNoAccountsSaysSoAndExitsZero(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)

	code, _, stderr, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stderr, "ccdad add") {
		t.Errorf("an empty store does not say what to do about it:\n%s", stderr)
	}
}

func TestStatusTakesNoArguments(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	code, _, _, _ := runRoot(t, "status", "extra")
	if code != ExitUsage {
		t.Errorf("exit %d, want 2", code)
	}
}

// The --json contract: one object on stdout, human notices on stderr, so a
// --json caller always receives exactly one document.
func TestStatusJSONPutsNothingButTheObjectOnStdout(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonUnknown}, daemon.ErrLocksUnsupported)

	_, stdout, _, _ := runRoot(t, "status", "--json")
	statusJSON(t, stdout)
}

func TestStatusReportsTheEngineStatePerAccount(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion: daemon.StatusSchemaVersion,
			PID:           4242,
			Accounts: []daemon.AccountStatus{{
				UUID:       "uuid-a",
				State:      daemon.StateQuarantined,
				NextPollAt: statusNow.Add(10 * time.Minute),
			}},
		},
	}, nil)

	_, stdout, _, _ := runRoot(t, "status", "--json")
	row := accountRow(t, statusJSON(t, stdout), "uuid-a")
	engine, ok := row["engine"].(map[string]any)
	if !ok {
		t.Fatalf("no engine object: %v", row)
	}
	if engine["state"] != "quarantined" {
		t.Errorf("engine.state = %v", engine["state"])
	}
	if engine["nextPollAt"] == nil {
		t.Errorf("engine.nextPollAt is missing: %v", engine)
	}
}

func TestHumanDurationReadsAtEveryScale(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Minute, "due"},
		{30 * time.Second, "30s"},
		{14 * time.Minute, "14m"},
		{2*time.Hour + 14*time.Minute, "2h14m"},
		{50 * time.Hour, "2d2h"},
	} {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// The binding window can be a per-model or per-surface weekly cap out of
// limits[]. Both columns read the binding window, and a lookup that only knows
// the fixed five leaves them blank for an account whose headroom is known — and
// publishes a bindingWindow name that resolves to nothing.
func TestStatusRendersAScopedBindingWindow(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	resets := statusNow.Add(30 * time.Hour)
	pct := 93.0
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot: &usage.Snapshot{
			FiveHour: window(20, statusNow.Add(time.Hour)),
			Limits: []usage.Limit{usage.LimitFor(usage.LimitInput{
				Kind: "weekly_scoped", Group: "model", Model: "Fable",
				Percent: &pct, ResetsAt: &resets,
			})},
		},
	})

	_, human, _, _ := runRoot(t, "status")
	if !strings.Contains(human, "93%") {
		t.Errorf("the scoped cap that binds is not in the USED column:\n%s", human)
	}
	if strings.Contains(human, "20%") {
		t.Errorf("five_hour is rendered although the scoped cap is the one that binds:\n%s", human)
	}

	_, out, _, _ := runRoot(t, "status", "--json")
	usageObj, ok := accountRow(t, statusJSON(t, out), "uuid-a")["usage"].(map[string]any)
	if !ok {
		t.Fatal("no usage object")
	}
	name, _ := usageObj["bindingWindow"].(string)
	if name != "weekly_scoped:model:Fable" {
		t.Fatalf("bindingWindow = %q, want the scoped cap", name)
	}
	if got := usageObj["headroomPct"]; got != 7.0 {
		t.Errorf("headroomPct = %v, want 7", got)
	}
	windows, ok := usageObj["windows"].(map[string]any)
	if !ok {
		t.Fatalf("no windows object: %v", usageObj)
	}
	if _, ok := windows[name]; !ok {
		t.Errorf("bindingWindow names %q, which is not one of the published windows %v", name, windows)
	}
}

// A tripped WEEKLY cap is what a user has to be told about: it is the one that
// will not come back for days, and naming the five-hour window instead tells
// them to wait ten minutes for an account that is unusable until Friday. The
// engine still ORDERS on the tightest window, which here is the five-hour one.
func TestStatusReportsATrippedWeeklyCapRatherThanTheTightestWindow(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot: &usage.Snapshot{
			// 95% used and back in ten minutes: the least slack, so the ranking
			// orders on it.
			FiveHour: window(95, statusNow.Add(10*time.Minute)),
			// 85% used and not back for forty hours: over threshold, so it is a
			// floor, and it is what the row reports.
			SevenDay: window(85, statusNow.Add(40*time.Hour)),
		},
	})

	_, human, _, _ := runRoot(t, "status")
	for _, want := range []string{"85%", "seven_day", "1d16h"} {
		if !strings.Contains(human, want) {
			t.Errorf("the dashboard does not report the tripped weekly cap (%q):\n%s", want, human)
		}
	}
	if strings.Contains(human, "five_hour") {
		t.Errorf("the dashboard names five_hour, which comes back in ten minutes:\n%s", human)
	}

	_, out, _, _ := runRoot(t, "status", "--json")
	usageObj, ok := accountRow(t, statusJSON(t, out), "uuid-a")["usage"].(map[string]any)
	if !ok {
		t.Fatal("no usage object")
	}
	if got := usageObj["bindingWindow"]; got != "seven_day" {
		t.Errorf("bindingWindow = %v, want seven_day", got)
	}
	// And the axis is still the five-hour window: 80 - 95.
	if got := usageObj["slack"]; got != -15.0 {
		t.Errorf("slack = %v, want -15 from the five-hour window", got)
	}
	if got := usageObj["windowThreshold"]; got != 80.0 {
		t.Errorf("windowThreshold = %v, want the configured 80", got)
	}
	if got := usageObj["headroomPct"]; got != 5.0 {
		t.Errorf("headroomPct = %v, want 5", got)
	}
}

// Under hover the engine ranks on thresholds it derived from each window's own
// elapsed share and the size of the pool, and the dashboard has to report those.
// A row measured against the number still sitting in the config file has a slack
// that is arithmetic the engine never did, and it can name a different binding
// window as well, because the binding window is the one with the least slack and
// slack moves with the threshold. Hover's whole claim is that an automatic mode
// a user cannot audit is one they have to take on trust.
func TestStatusAndListReportTheThresholdsHoverRankedOn(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	// 43% of a seven-day window gone, with two accounts to divide what is left
	// between: 43 + 100/2 = 93, where the configured default is 80.
	reset := statusNow.Add(time.Duration(0.57 * float64(7*24*time.Hour)))
	// Added BEFORE the frozen clock, because the engine prunes a reading older
	// than its account's AddedAt as one that belonged to a previous login at the
	// same uuid -- and seedAccount stamps that from the real clock.
	for uuid, email := range map[string]string{"uuid-a": "work@example.com", "uuid-b": "alt@example.com"} {
		seedAccountAddedAt(t, uuid, email, statusNow.Add(-24*time.Hour))
	}
	for uuid, pct := range map[string]float64{"uuid-a": 95, "uuid-b": 41} {
		seedUsageEntry(t, uuid, usage.Entry{
			FetchedAt: statusNow.Add(-time.Minute),
			Snapshot:  &usage.Snapshot{SevenDay: window(pct, reset)},
		})
	}
	if code, _, errOut, _ := runRoot(t, "config", "set", "hover", "true"); code != 0 {
		t.Fatalf("config set hover true exited %v: %s", code, errOut)
	}

	// Both commands, because they share one builder and the property worth
	// pinning is that they cannot disagree.
	for _, name := range []string{"status", "list"} {
		t.Run(name, func(t *testing.T) {
			_, out, _, _ := runRoot(t, name, "--json")
			u, ok := accountRow(t, statusJSON(t, out), "uuid-a")["usage"].(map[string]any)
			if !ok {
				t.Fatalf("no usage object in %s --json:\n%s", name, out)
			}
			if got := u["windowThreshold"]; got != 93.0 {
				t.Errorf("windowThreshold = %v, want the 93 hover derived rather than the configured 80", got)
			}
			if got := u["slack"]; got != -2.0 {
				t.Errorf("slack = %v, want -2, which is 93 against 95%% used", got)
			}
		})
	}
}

// stubEnginePlan replaces the engine seam. An evaluation that cannot be made is
// not something a test can arrange with a store and a cache.
func stubEnginePlan(t *testing.T, fn func(*store.Store, time.Time) (strategy.Plan, bool, error)) {
	t.Helper()
	saved := enginePlan
	t.Cleanup(func() { enginePlan = saved })
	enginePlan = fn
}

// The dashboard asks the engine EXACTLY ONCE, hover or no hover.
//
// It used to ask only under hover, on the ground that the rows never needed a
// ranking pass. Naming the mode is a second question, and it is one the cache
// cannot answer at all — so the pass now always runs. What this pins is the
// count, because the failure it replaces is worse than the cost: two passes, one
// for the thresholds and one for the mode, would give `status` a second source
// for a number it already had, and two sources are how `list` and `status`
// start disagreeing.
func TestTheDashboardAsksTheEngineExactlyOnce(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccountAddedAt(t, "uuid-a", "work@example.com", statusNow.Add(-24*time.Hour))
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{SevenDay: window(95, statusNow.Add(40*time.Hour))},
	})

	for _, hover := range []string{"false", "true"} {
		t.Run("hover="+hover, func(t *testing.T) {
			if code, _, errOut, _ := runRoot(t, "config", "set", "hover", hover); code != 0 {
				t.Fatalf("config set hover %s exited %v: %s", hover, code, errOut)
			}
			asked := 0
			stubEnginePlan(t, func(*store.Store, time.Time) (strategy.Plan, bool, error) {
				asked++
				return strategy.Plan{}, true, nil
			})

			if _, out, _, _ := runRoot(t, "status", "--json"); out == "" {
				t.Fatal("status --json emitted nothing")
			}
			if asked != 1 {
				t.Errorf("the dashboard made %d engine evaluations, want exactly 1", asked)
			}
		})
	}
}

// `ccdad list` keeps the older contract, and that is the whole reason the two
// commands do not share a call site: nothing in the listing needs a ranking
// pass, and it has no mode line to put a second question behind.
func TestTheListingDoesNotRunTheEngineWithHoverOff(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccountAddedAt(t, "uuid-a", "work@example.com", statusNow.Add(-24*time.Hour))
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{SevenDay: window(95, statusNow.Add(40*time.Hour))},
	})

	asked := false
	stubEnginePlan(t, func(*store.Store, time.Time) (strategy.Plan, bool, error) {
		asked = true
		return strategy.Plan{}, true, nil
	})

	if _, out, _, _ := runRoot(t, "list", "--json"); out == "" {
		t.Fatal("list --json emitted nothing")
	}
	if asked {
		t.Error("the listing ran an engine evaluation with hover off; the configured bundle needs none")
	}
}

// An engine that could not be asked leaves the Mode line off, and SAYS so. A
// line that simply disappears reads as "the engine has nothing to say" rather
// than "it could not be asked", and those want different things from a reader.
func TestTheDashboardSaysSoWhenTheModeCannotBeRead(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
	seedAccountAddedAt(t, "uuid-a", "a@example.com", statusNow.Add(-time.Hour))
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{FiveHour: window(92, statusNow.Add(40*time.Minute))},
	})
	stubEnginePlan(t, func(*store.Store, time.Time) (strategy.Plan, bool, error) {
		return strategy.Plan{}, false, errors.New("the engine state could not be read")
	})

	code, stdout, errOut, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d; a dashboard renders whatever else is wrong\n%s", code, stdout)
	}
	if strings.Contains(stdout, "Mode:") {
		t.Errorf("the dashboard named a mode it could not compute:\n%s", stdout)
	}
	if !strings.Contains(errOut, "the engine state could not be read") {
		t.Errorf("nothing on stderr says why the mode is missing:\n%s", errOut)
	}
}

// An engine that cannot be asked is a notice, never a blank dashboard. The rows
// fall back to the configured bundle because that is the last table anyone can
// name, and the note is what stops the number being read as hover's.
func TestTheDashboardSaysSoWhenHoversThresholdsCannotBeRead(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccountAddedAt(t, "uuid-a", "work@example.com", statusNow.Add(-24*time.Hour))
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{SevenDay: window(95, statusNow.Add(40*time.Hour))},
	})
	if code, _, errOut, _ := runRoot(t, "config", "set", "hover", "true"); code != 0 {
		t.Fatalf("config set hover true exited %v: %s", code, errOut)
	}
	stubEnginePlan(t, func(*store.Store, time.Time) (strategy.Plan, bool, error) {
		return strategy.Plan{}, false, errors.New("the account store could not be read")
	})

	code, out, errOut, _ := runRoot(t, "status", "--json")
	if code != 0 {
		t.Fatalf("status exited %v; a dashboard renders whatever else is wrong", code)
	}
	// The HOVER sentence specifically, not merely the error text. `status` also
	// reports that a failed evaluation cost it the Mode line, and that notice
	// carries the same error -- so asserting the error alone stopped
	// distinguishing "the rows are not hover's numbers" from "there is no mode
	// line", which are different things to tell a user.
	if !strings.Contains(errOut, "hover is on, but the thresholds it derived could not be read") {
		t.Errorf("nothing on stderr names why hover's own thresholds are missing:\n%s", errOut)
	}
	if !strings.Contains(errOut, "the account store could not be read") {
		t.Errorf("the notice does not carry the underlying error:\n%s", errOut)
	}
	u, ok := accountRow(t, statusJSON(t, out), "uuid-a")["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage object:\n%s", out)
	}
	if got := u["windowThreshold"]; got != 80.0 {
		t.Errorf("windowThreshold = %v, want the configured 80 the note points at", got)
	}
}

// A row the dashboard renders and the engine discarded. `status` reports every
// account from the cache; the engine prunes a reading older than its account's
// AddedAt, so with only such readings it makes no pass and there is no derived
// table at all. What the row must NOT be measured against is `threshold`, which
// is the first key hover stops reading -- so the configured 60 here is the wrong
// answer and hover's own 80 for an account it never saw is the right one.
func TestARowTheHoverPassNeverSawIsMeasuredAsHoverWouldMeasureIt(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	// Added AFTER the reading was taken, which is what makes the engine treat
	// it as a previous login's quota and prune it.
	seedAccountAddedAt(t, "uuid-a", "work@example.com", statusNow.Add(time.Hour))
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{SevenDay: window(95, statusNow.Add(40*time.Hour))},
	})
	for _, kv := range [][2]string{{"hover", "true"}, {"threshold", "60"}} {
		if code, _, errOut, _ := runRoot(t, "config", "set", kv[0], kv[1]); code != 0 {
			t.Fatalf("config set %s %s exited %v: %s", kv[0], kv[1], code, errOut)
		}
	}

	_, out, _, _ := runRoot(t, "status", "--json")
	u, ok := accountRow(t, statusJSON(t, out), "uuid-a")["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage object:\n%s", out)
	}
	if got := u["windowThreshold"]; got != 80.0 {
		t.Errorf("windowThreshold = %v, want 80 -- hover ignores `threshold`, so the configured 60 is a number nothing would have used", got)
	}
}

// Recovery mode reverses the sort: every account is known to be over its
// threshold, so the engine stops ranking by how much is left and starts ranking
// by which one comes back first. The table looks identical either way — same
// columns, same percentages — so a dashboard that does not name the mode gives a
// user staring at five accounts at 95% no way to tell an engine that is still
// working from an engine that has given up.
//
// The accounts are seeded BEFORE the reading, not with seedAccount: an entry
// older than its account's AddedAt is pruned as a previous login's quota, and
// seedAccount stamps AddedAt from the real clock while this test's clock is
// frozen in the past. Every fixture here reaches the ENGINE, not just the cache.
func TestStatusNamesTheModeWhenEveryAccountIsOverThreshold(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
	// 92% used against the default threshold of 80: both accounts are KNOWN to
	// be over, which is the only state that reaches recovery. One unreadable
	// account would be enough to hold the engine in headroom mode.
	for _, uuid := range []string{"uuid-a", "uuid-b"} {
		seedAccountAddedAt(t, uuid, uuid+"@example.com", statusNow.Add(-time.Hour))
		seedUsageEntry(t, uuid, usage.Entry{
			FetchedAt: statusNow.Add(-time.Minute),
			Snapshot:  &usage.Snapshot{FiveHour: window(92, statusNow.Add(40*time.Minute))},
		})
	}

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "Mode:    recovery") {
		t.Errorf("the dashboard does not name the recovery mode:\n%s", stdout)
	}
	if !strings.Contains(stdout, "every account is over its threshold") {
		t.Errorf("the dashboard names the mode without saying what put it there:\n%s", stdout)
	}
}

// The ordinary case still says which question is being asked, because "recovery"
// only means something to a reader who has seen the other answer.
func TestStatusNamesTheHeadroomModeToo(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
	seedAccountAddedAt(t, "uuid-a", "a@example.com", statusNow.Add(-time.Hour))
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{FiveHour: window(12, statusNow.Add(40*time.Minute))},
	})

	_, stdout, _, _ := runRoot(t, "status")
	if !strings.Contains(stdout, "Mode:    headroom") {
		t.Errorf("the dashboard does not name the headroom mode:\n%s", stdout)
	}
}

// A machine that has never been polled has no ranking to report. The line is
// left OFF rather than defaulted, because strategy.Mode's zero value stringifies
// to "headroom" — a plausible answer rather than an empty one, which is exactly
// the trap switcher.Evaluation.Decided exists to close.
func TestStatusOmitsTheModeWhenNothingHasEverBeenPolled(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
	seedAccountAddedAt(t, "uuid-a", "a@example.com", statusNow.Add(-time.Hour))

	_, stdout, _, _ := runRoot(t, "status")
	if strings.Contains(stdout, "Mode:") {
		t.Errorf("the dashboard claims a mode with no reading behind it:\n%s", stdout)
	}
}

// A script watching for the engine to give up reads this rather than parsing the
// table. The key is CONDITIONAL — absent when no ranking ran — for the same
// reason usageJSON returns nil for an account with no reading: an absent key
// cannot be mistaken for an answer, and "headroom" is a real answer. `ccdad auto
// --json` already publishes it under the same name and with the same guard.
func TestStatusJSONCarriesTheModeOnlyWhenARankingRan(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
	seedAccountAddedAt(t, "uuid-a", "a@example.com", statusNow.Add(-time.Hour))

	_, blind, _, _ := runRoot(t, "status", "--json")
	if _, ok := statusJSON(t, blind)["mode"]; ok {
		t.Error("status --json reports a mode with no reading behind it")
	}

	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{FiveHour: window(92, statusNow.Add(40*time.Minute))},
	})
	_, out, _, _ := runRoot(t, "status", "--json")
	if got := statusJSON(t, out)["mode"]; got != "recovery" {
		t.Errorf("mode = %v, want recovery", got)
	}
}

// The third mode is a strategy a user asked for rather than a situation the
// engine discovered, and it is answered BEFORE recovery: an account can be over
// every threshold and still be ranked on which weekly window expires soonest.
// Without this the dashboard would call that pass "recovery" and send a reader
// looking for a shortage that is not what the engine is acting on.
func TestStatusNamesTheConsumeFirstMode(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
	if code, _, errOut, _ := runRoot(t, "config", "set", "strategy", "consume-first"); code != 0 {
		t.Fatalf("config set strategy consume-first exited %v: %s", code, errOut)
	}
	seedAccountAddedAt(t, "uuid-a", "a@example.com", statusNow.Add(-time.Hour))
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{SevenDay: window(40, statusNow.Add(40*time.Hour))},
	})

	_, stdout, _, _ := runRoot(t, "status")
	if !strings.Contains(stdout, "Mode:    consume-first") {
		t.Errorf("the dashboard does not name the consume-first mode:\n%s", stdout)
	}
	if !strings.Contains(stdout, "perishable") {
		t.Errorf("the dashboard names the mode without saying what it is spending:\n%s", stdout)
	}
}

// `ccdad status` says when hover is on, and it is the one line that explains
// every other number on the page. Without it the dashboard is actively
// misleading under hover: the Mode line names headroom because hover FORCED
// headroom, so a reader who configured consume-first sees a mode they did not
// ask for and nothing anywhere saying why.
//
// The line is printed only when hover is ON. Absence is unambiguous here --
// hover off is the default and the configured numbers are the ones in force --
// which is what separates this from the Mode line, where a missing value would
// have been defaulted to a plausible answer nobody computed.
func TestStatusSaysWhenHoverIsOn(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
	seedAccountAddedAt(t, "uuid-a", "a@example.com", statusNow.Add(-time.Hour))
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot:  &usage.Snapshot{FiveHour: window(12, statusNow.Add(40*time.Minute))},
	})

	_, off, _, _ := runRoot(t, "status")
	if strings.Contains(off, "Hover:") {
		t.Errorf("the dashboard reports hover with hover off:\n%s", off)
	}

	if code, _, errOut, _ := runRoot(t, "config", "set", "hover", "true"); code != 0 {
		t.Fatalf("config set hover true exited %v: %s", code, errOut)
	}
	code, on, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, on)
	}
	if !strings.Contains(on, "Hover:   on") {
		t.Errorf("the dashboard does not say hover is on:\n%s", on)
	}
	if !strings.Contains(on, "ccdad hover status") {
		t.Errorf("the dashboard says hover is on without saying where its numbers can be read:\n%s", on)
	}
}

// A script that wants to know whether the thresholds on the wire were derived or
// configured reads this rather than parsing the table.
//
// The key is CONDITIONAL for the reason unnamableWeeklyCaps is: an ordinary
// payload does not carry a field that is always the boring default. The contract
// is additive, so schemaVersion stays 1 and a consumer that has never heard of
// the key is unaffected.
func TestStatusJSONCarriesHoverOnlyWhenItIsOn(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
	seedAccountAddedAt(t, "uuid-a", "a@example.com", statusNow.Add(-time.Hour))

	_, off, _, _ := runRoot(t, "status", "--json")
	if _, ok := statusJSON(t, off)["hover"]; ok {
		t.Error("status --json carries a hover key with hover off")
	}

	if code, _, errOut, _ := runRoot(t, "config", "set", "hover", "true"); code != 0 {
		t.Fatalf("config set hover true exited %v: %s", code, errOut)
	}
	_, on, _, _ := runRoot(t, "status", "--json")
	if got := statusJSON(t, on)["hover"]; got != true {
		t.Errorf("hover = %v, want true", got)
	}
}
