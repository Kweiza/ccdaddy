package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
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

// The pace reading is suppressed for the first 24 hours after a reset: the
// elapsed time is tiny then, so almost any usage divides out as "far ahead" and
// the dashboard cries wolf every Monday.
func TestPaceIsSuppressedInTheFirstDayOfAWeeklyWindow(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	// The reset is six and a half days out, so only twelve hours have elapsed.
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-time.Minute),
		Snapshot: &usage.Snapshot{
			SevenDay: window(30, statusNow.Add(6*24*time.Hour+12*time.Hour)),
		},
	})

	_, out, _, _ := runRoot(t, "status", "--json")
	row := accountRow(t, statusJSON(t, out), "uuid-a")
	usageObj := row["usage"].(map[string]any)
	if pace, ok := usageObj["pace"]; ok {
		t.Errorf("pace was reported twelve hours into a weekly window: %v", pace)
	}

	_, human, _, _ := runRoot(t, "status")
	if strings.Contains(human, "ahead") {
		t.Errorf("the table calls a twelve-hour-old window ahead of pace:\n%s", human)
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
		[]byte(`{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-uuid-b"}}`), 0o600); err != nil {
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
