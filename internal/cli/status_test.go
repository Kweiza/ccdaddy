package cli

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
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
	if err := s.Add(store.Account{Provider: provider.Claude, UUID: uuid, Email: email, AddedAt: at}, credsFor("RT-"+uuid)); err != nil {
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
	// Both windows, both rollovers, the age, and the legend mapping each header
	// back to the wire key `ccdad config` takes a threshold on.
	for _, want := range []string{
		"work@example.com", "1m",
		"20%", "30m", "5H = five_hour",
		"62%", "2h14m", "7D = seven_day",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the dashboard does not mention %q:\n%s", want, stdout)
		}
	}
	// The derived columns are gone. A dashboard that still printed one would be
	// answering "which window binds" beside the windows themselves, which is
	// the question the old split listing answered differently.
	for _, unwanted := range []string{"USED", "WINDOW", "LEFT", "PACE"} {
		if strings.Contains(stdout, unwanted) {
			t.Errorf("the dashboard still carries the derived column %q:\n%s", unwanted, stdout)
		}
	}
}

// statusHeadingSeparator is the gap between two cells of a rendered table: at
// least two spaces, because columns() pads every cell but the last out to its
// column's width and then adds two more. A heading's OWN words are separated by
// one space, which is what makes splitting on this exact rather than
// approximate -- "7D OPUS IN" comes back as one field instead of three.
var statusHeadingSeparator = regexp.MustCompile(` {2,}`)

// The heading over `ccdad status`'s account table, word for word.
//
// Every other assertion in this package that touches that row is tolerant by
// construction: strings.Contains and strings.Index both match a heading with a
// suffix glued onto it, and the colour test matches a derived quota heading by
// SHAPE before its exact-text map is ever consulted. Renaming a column in
// internal/view therefore reached the terminal with nothing in internal/cli
// objecting, and the only thing that went red was the terminal dashboard's
// golden ladder -- which is the wrong package to hear it from, because all it
// can say is that the DASHBOARD moved.
//
// The row is pinned twice over, and both halves are the point. The printed
// heading is compared field for field against the words themselves, so a rename
// anywhere between view.ListColumns and stdout reddens this; and each fixed word
// is then compared against the constant internal/view holds it in, so a
// status.go that stopped reading the shared list and spelled the same strings
// itself reddens on the next rename instead of drifting quietly apart. The
// window and rollover headings have no constant to pin to -- they are built out
// of the fleet's own window names -- so they are pinned as words only.
//
// It belongs here rather than in internal/view because this row is the CLI's
// published output: a script reading a field out of `ccdad status` by position
// breaks when a column is renamed, reordered or inserted, and internal/view has
// never heard of that script.
func TestStatusHeadingRowIsExactlyTheSharedColumnNames(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	// Three windows rather than one, so the order INSIDE the quota block is
	// pinned too, along with the rollover columns that trail it: a fixture with
	// a single window would pass a renderer that emitted the block in any order
	// at all.
	seedAccount(t, "uuid-a", "work@example.com")
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow,
		Snapshot: &usage.Snapshot{
			FiveHour:     window(60, statusNow.Add(2*time.Hour)),
			SevenDay:     window(30, statusNow.Add(48*time.Hour)),
			SevenDayOpus: window(10, statusNow.Add(72*time.Hour)),
		},
	})

	code, stdout, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 (%s)\n%s", code, top, stdout)
	}
	// The heading is the line under the blank separator the report prints
	// between its labelled block and the table. Found by that structure rather
	// than by looking for a column name, because a test that locates the row by
	// its text cannot then be surprised by its text.
	lines := strings.Split(stdout, "\n")
	head := ""
	// The table opens on a SECTION heading now, and the column names are the
	// line under it: each provider's half draws its own quota block, so each
	// carries its own names over it.
	for i, l := range lines {
		if strings.TrimSpace(l) == "" && i+2 < len(lines) {
			if strings.TrimSpace(lines[i+1]) != view.ClaudeSection {
				t.Fatalf("the table does not open on the %s heading: %q", view.ClaudeSection, lines[i+1])
			}
			head = lines[i+2]
			break
		}
	}
	if strings.TrimSpace(head) == "" {
		t.Fatalf("no table heading under the blank separator:\n%s", stdout)
	}
	// Exactly two spaces in front. The IDX cell is the marker and the index
	// together, so the heading over it is shifted past the marker, and that
	// shift is this surface's own -- internal/view knows nothing about the
	// glyph sharing that cell.
	if !strings.HasPrefix(head, "  ") || strings.HasPrefix(head, "   ") {
		t.Errorf("the heading does not open shifted two spaces past the marker: %q", head)
	}

	got := statusHeadingSeparator.Split(strings.TrimLeft(head, " "), -1)
	want := []string{
		"IDX", "ACCOUNT", "TYPE", "TIER",
		"5H", "7D", "7D OPUS", "5H IN", "7D IN", "7D OPUS IN",
		"STATE", "AGE",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("heading row = %q, want %q\nfrom:\n%s", got, want, stdout)
	}
	// And the fixed words above are internal/view's, not this file's second
	// spelling of them.
	for _, c := range []struct {
		at     int
		shared string
	}{
		{0, view.IdxHeader},
		{1, view.AccountHeader},
		{2, view.TypeHeader},
		{3, view.TierHeader},
		{10, view.StateHeader},
		{11, view.AgeHeader},
	} {
		if want[c.at] != c.shared {
			t.Errorf("column %d is pinned here as %q, but internal/view now spells it %q: "+
				"either `ccdad status` stopped reading the shared list or the rename is "+
				"unfinished", c.at, want[c.at], c.shared)
		}
	}
}

// The table is SECTIONED by provider, and both headings draw whatever the fleet
// holds -- including over a section with no rows in it.
//
// The empty section is the whole point on this surface. A machine with Claude
// accounts and no codex one is the common case, and a table that drew only the
// heading it had rows for would render exactly as a build that has never heard
// of Codex -- which is precisely what the reader is trying to find out.
//
// Both are read at the ACCOUNT column's own offset rather than by Contains,
// because that is what makes them table ROWS: a line printed above the table
// would satisfy any substring check and would sit flush left, out of line with
// the addresses it is a heading for.
func TestStatusSectionsTheTableAndDrawsBothHeadings(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")

	code, stdout, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 (%s)\n%s", code, top, stdout)
	}

	lines := strings.Split(stdout, "\n")
	head := -1
	for i, l := range lines {
		if strings.Contains(l, view.AccountHeader) && strings.Contains(l, view.IdxHeader) {
			head = i
			break
		}
	}
	if head < 0 {
		t.Fatalf("no column heading row:\n%s", stdout)
	}
	at := strings.Index(lines[head], view.AccountHeader)

	// Claude first, then Codex, in the order internal/view groups them, and
	// each one alone on its row.
	// From the top of the page: the first heading is one row ABOVE the first
	// column-name row now, so a search that started under it would miss it.
	want := []string{view.ClaudeSection, view.CodexSection}
	var got []string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 1 || (fields[0] != view.ClaudeSection && fields[0] != view.CodexSection) {
			continue
		}
		got = append(got, fields[0])
		if i := strings.Index(line, fields[0]); i != at {
			t.Errorf("%s starts at column %d and ACCOUNT at %d; the heading is not in the account column",
				fields[0], i, at)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the table's section headings are %q, want %q\nfrom:\n%s", got, want, stdout)
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

// The projection rule, as it now stands.
//
// usage.Pace's single-reading extrapolation is still refused in front of a
// person: it draws a straight line from a window's opening through the one
// point that has been read, and real usage is bursty enough that the line is
// too rough to state as fact in a table. The measured runway line is allowed,
// because it is measured — several readings taken over hours, with the span
// they cover printed beside the answer.
//
// The bare substring "exhaust" is the oldest assertion here and it stays, with
// two narrower ones added beside it. A second projection in the tree does not
// blunt it, measured: nothing the approved line prints contains the substring —
// the shared wording spells "dry", "holds", "cannot tell yet" and "basis" — and
// this table's columns are IDX through AGE, none of which is a projected
// moment. So it still separates the refused projection from the approved one,
// and it is the only assertion here that would catch a leak spelled as a
// relative span, which is how every moment on this table is rendered.
//
// The two beside it rule out what a substring cannot. The key names catch a
// payload field copied verbatim onto stdout, and the rendered timestamp catches
// a column that printed the extrapolated moment under some other heading.
func TestTheSingleReadingProjectionIsStillJSONOnly(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	// Both projections at once. These accounts are four days into a seven-day
	// window — past the suppression and spending, so each carries a pace
	// projection — and they have a series behind them, so the measured line is
	// printed too. A fixture with only the pace projection would go green on a
	// dashboard that printed neither, which is the wrong implementation this
	// test most needs to rule out.
	seedBurningFleet(t)

	_, human, _, _ := runRoot(t, "status")
	if !strings.Contains(human, "Runway:") {
		t.Fatalf("no measured line on stdout, so nothing below separates the approved projection from the refused one:\n%s", human)
	}
	for _, forbidden := range []string{"exhaust", "projectedExhaustionAt", "willLastToReset"} {
		if strings.Contains(human, forbidden) {
			t.Errorf("the human dashboard mentions %q, which is kept to --json:\n%s", forbidden, human)
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
	at, _ := week["projectedExhaustionAt"].(string)
	if at == "" {
		t.Errorf("--json does not carry projectedExhaustionAt: %v", week)
	}
	if _, ok := week["willLastToReset"]; !ok {
		t.Errorf("--json does not carry willLastToReset: %v", week)
	}
	if _, ok := week["expectedPct"]; !ok {
		t.Errorf("--json does not carry expectedPct: %v", week)
	}
	// The two identifiers above are JSON key names, and a table that grew an
	// extrapolated column would print none of them. The MOMENT is what such a
	// column would carry, so it is asserted absent in the shape every absolute
	// time on a human surface here is rendered in.
	if at != "" {
		if rendered := view.Timestamp(mustTime(at), time.Local); strings.Contains(human, rendered) {
			t.Errorf("the dashboard prints %q, the moment the single-reading projection extrapolated to:\n%s", rendered, human)
		}
	}
}

// The measured line appears when there is a measurement behind it, and not
// otherwise — and it appears above the table.
//
// Both halves matter. A line printed with no series behind it would be a
// promise resting on no reading, which is the one output this measurement
// exists to refuse; and view.RunwayLine's empty string is what the dashboard
// also reads as "print nothing", so a status that decided for itself would be
// the first of two surfaces to disagree.
//
// The placement is not cosmetic. columns() writes the table when renderStatus
// calls it, so a line written to the same stream after that call arrives after
// the table and a line written before it arrives above.
func TestTheRunwayLineAppearsOnlyWithAHistoryBehindIt(t *testing.T) {
	t.Run("with a series", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		seedBurningFleet(t)

		_, human, _, _ := runRoot(t, "status")
		lines := strings.Split(human, "\n")
		mode := -1
		for i, l := range lines {
			if strings.HasPrefix(l, "Current:") {
				mode = i
			}
		}
		// The line UNDER Current:, not merely somewhere above the table. What
		// decides which side of the blank separator it lands on is where in
		// renderStatus it is written, and now that is also what decides the
		// byte order: columns() writes when it is called, so this line is
		// above the table because it is written above it. It belongs in the
		// block of labels its nine-character label is padded to line up with,
		// not in the block of rows.
		if mode < 0 || mode+1 >= len(lines) || !strings.HasPrefix(lines[mode+1], "Runway:  ") {
			t.Fatalf("the runway line is not the line under Current::\n%s", human)
		}
		runway := lines[mode+1]
		// The span the rates were measured over rides on the line: a verdict
		// from two hours of evidence and one from four support different
		// claims, and only the reader can weigh that.
		if !strings.Contains(runway, "basis 2h00m") {
			t.Errorf("the line states no basis: %q", runway)
		}
		// No percentage. Four tests in this file forbid one belonging to a
		// window the table is not reporting, and this line reports a fleet
		// rather than any single window.
		if strings.Contains(runway, "%") {
			t.Errorf("the runway line carries a percentage: %q", runway)
		}

		_, out, _, _ := runRoot(t, "status", "--json")
		f, ok := statusJSON(t, out)["forecast"].(map[string]any)
		if !ok {
			t.Fatalf("no forecast object:\n%s", out)
		}
		if _, ok := f["basis"]; !ok {
			t.Errorf("forecast = %v, which names no basis", f)
		}
		if _, ok := f["axes"]; !ok {
			t.Errorf("forecast = %v, which carries no measured axis", f)
		}
	})

	t.Run("without one", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		// One reading and nothing older than it: the machine that has been
		// recording for ten minutes.
		seedAccountAddedAt(t, "uuid-a", "a@example.com", runwayAddedAt)
		seedUsageEntry(t, "uuid-a", usage.Entry{
			FetchedAt: statusNow,
			Snapshot:  &usage.Snapshot{SevenDay: window(48, runwayWeeklyReset)},
		})

		_, human, _, _ := runRoot(t, "status")
		if strings.Contains(human, "Runway:") {
			t.Errorf("the dashboard states a runway with no reading behind it:\n%s", human)
		}

		_, out, _, _ := runRoot(t, "status", "--json")
		if f, ok := statusJSON(t, out)["forecast"]; ok {
			t.Errorf("forecast = %v was published on a machine with nothing measured; absent and zero are different answers", f)
		}
	})
}

// A series that cannot be read costs the rates and nothing else, and the reason
// is said out loud. A line that simply vanishes reads as a fleet with nothing
// to report rather than as a file nobody could open, and those want different
// things from a reader: one is a quiet week, the other is a store to go and
// look at.
func TestTheDashboardSaysSoWhenTheSeriesCannotBeRead(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)
	// Truncated JSON rather than an unreadable mode: a parse failure is the one
	// a real store reaches after a crash mid-write, and it is the case where
	// every level is still perfectly readable from the other file.
	if err := os.WriteFile(mustPath(history.Path()), []byte("{\"accounts\":"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, human, errOut, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d (%s); a dashboard renders whatever else is wrong\n%s", code, top, human)
	}
	if !strings.Contains(errOut, history.FileName) {
		t.Errorf("stderr names no file that could not be read:\n%s", errOut)
	}
	if strings.Contains(human, "Runway:") {
		t.Errorf("a runway was stated from a series that could not be read:\n%s", human)
	}
	if !strings.Contains(human, "a@example.com") {
		t.Errorf("the rows the usage cache still answers for were dropped with it:\n%s", human)
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

	// PACE left the human table with the derived window it was read off. The
	// reading itself did not go anywhere: --json carries one per window,
	// including the projection that must never reach a human view, and
	// `ccdad runway` is the human answer to "how fast".
	fh, ok := pace["five_hour"].(map[string]any)
	if !ok {
		t.Fatalf("five_hour pace is not an object: %v", pace["five_hour"])
	}
	if ahead, _ := fh["aheadOfPace"].(bool); !ahead {
		t.Errorf("--json does not call a five-hour window 90%% spent at 80%% elapsed ahead of pace: %v", fh)
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

// A per-model or per-surface weekly cap out of limits[] is a window like any
// other and gets a column of its own. A build that only knows the fixed five
// would leave it out of the table entirely — and publish a bindingWindow name
// that resolves to nothing.
func TestStatusRendersAScopedWindowAsItsOwnColumn(t *testing.T) {
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
		t.Errorf("the scoped cap has no cell:\n%s", human)
	}
	// BESIDE five_hour and not instead of it. The old table chose one window
	// per row and a reader could not tell which; both are here now.
	if !strings.Contains(human, "20%") {
		t.Errorf("five_hour lost its cell to the scoped cap:\n%s", human)
	}
	if !strings.Contains(human, "FABLE = weekly_scoped:model:Fable") {
		t.Errorf("the legend does not map the scoped column back to its wire key:\n%s", human)
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

// The 0.2.0 guard, restated for a table that derives nothing. A tripped WEEKLY
// cap is what a user has to be told about — it will not come back for days —
// and the old table could only tell them by NOT naming the five-hour window
// beside it, which is how the choice between the two became invisible. Both are
// on the row now, so neither can hide the other and the guard costs nothing.
func TestNeitherAWeeklyCapNorATighterWindowHidesTheOther(t *testing.T) {
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
	// The weekly, its rollover, and the legend naming it.
	for _, want := range []string{"85%", "1d16h", "7D = seven_day"} {
		if !strings.Contains(human, want) {
			t.Errorf("the dashboard does not report the tripped weekly cap (%q):\n%s", want, human)
		}
	}
	// And the five-hour window, which the old table suppressed to make room for
	// the weekly. Ten minutes is a true thing about this account too.
	for _, want := range []string{"95%", "10m", "5H = five_hour"} {
		if !strings.Contains(human, want) {
			t.Errorf("the dashboard does not report the tighter window (%q):\n%s", want, human)
		}
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
func TestStatusReportsTheThresholdsHoverRankedOn(t *testing.T) {
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

	_, out, _, _ := runRoot(t, "status", "--json")
	u, ok := accountRow(t, statusJSON(t, out), "uuid-a")["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage object in status --json:\n%s", out)
	}
	if got := u["windowThreshold"]; got != 93.0 {
		t.Errorf("windowThreshold = %v, want the 93 hover derived rather than the configured 80", got)
	}
	if got := u["slack"]; got != -2.0 {
		t.Errorf("slack = %v, want -2, which is 93 against 95%% used", got)
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

func TestTheUnifiedStatusRunsOneEngineEvaluationWithHoverOff(t *testing.T) {
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

	if _, out, _, _ := runRoot(t, "status", "--json"); out == "" {
		t.Fatal("list --json emitted nothing")
	}
	if !asked {
		t.Error("status did not evaluate the engine for its Current line")
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
	if strings.Contains(stdout, "Current:") {
		t.Errorf("the dashboard named a current mode it could not compute:\n%s", stdout)
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
	// 150 is the answer hover gives for an account its pass never saw: the
	// assumed elapsed share of a window it cannot date, plus a pool share, and
	// with no pool the share is the whole 100. The DISCRIMINATOR is that it is
	// not 60 -- hover ignores `threshold`, so the configured number is one
	// nothing would have used.
	if got := u["windowThreshold"]; got != 150.0 {
		t.Errorf("windowThreshold = %v, want 150 -- hover ignores `threshold`, so the configured 60 is a number nothing would have used", got)
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
	if !strings.Contains(stdout, "Current:  recovery") {
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
	if !strings.Contains(stdout, "Current:  headroom") {
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
	if strings.Contains(stdout, "Current:") {
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
	if !strings.Contains(stdout, "Current:  consume-first") {
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
	if strings.Contains(off, "Strategy: hover") {
		t.Errorf("the dashboard reports hover with hover off:\n%s", off)
	}

	if code, _, errOut, _ := runRoot(t, "config", "set", "hover", "true"); code != 0 {
		t.Fatalf("config set hover true exited %v: %s", code, errOut)
	}
	code, on, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, on)
	}
	if !strings.Contains(on, "Strategy: hover") {
		t.Errorf("the dashboard does not say hover is on:\n%s", on)
	}
	if !strings.Contains(on, "used/threshold") {
		t.Errorf("the dashboard names hover without explaining its quota cells:\n%s", on)
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

// stubOutWidth describes a terminal of cols display columns for whatever the
// command renders into. A real one is not something a test can arrange, and
// the fold is the whole of what this seam decides.
func stubOutWidth(t *testing.T, cols int) {
	t.Helper()
	saved := outWidth
	t.Cleanup(func() { outWidth = saved })
	outWidth = func(io.Writer) int { return cols }
}

// runwayBlockOf is the summary as it lands on the screen: the labelled line and
// every line hung under it. The continuation indent is nine columns, which is
// the label's width and not a number this test picked -- the table's own rows
// are indented two, so nothing else on the page can be mistaken for one.
func runwayBlockOf(t *testing.T, out string) []string {
	t.Helper()
	var block []string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "Runway:"):
			block = append(block, line)
		case len(block) > 0 && strings.HasPrefix(line, "         ") && strings.TrimSpace(line) != "":
			block = append(block, line)
		case len(block) > 0:
			return block
		}
	}
	if block == nil {
		t.Fatalf("no runway line in:\n%s", out)
	}
	return block
}

// runwayClausesOf recovers what a block says, with the layout taken back off:
// the label, the hanging indent, and the separator in both the forms it takes.
// Two renderings of one fleet must carry the same clauses in the same order or
// the fold dropped something.
func runwayClausesOf(t *testing.T, block []string) string {
	t.Helper()
	var clauses []string
	for i, l := range block {
		if i == 0 {
			l = strings.TrimPrefix(l, "Runway:  ")
		} else {
			l = strings.TrimPrefix(l, "         ")
		}
		clauses = append(clauses, strings.Split(strings.TrimSuffix(l, "  \u00b7"), "  \u00b7  ")...)
	}
	return strings.Join(clauses, "|")
}

// A 139-column line on an 80-column terminal is the measurement this item was
// filed on, and the terminal folds it wherever its own right edge falls. The
// dashboard has cut this line from the right since it shipped; `ccdad status`
// printed it raw, so the only reader that could not fold it was the one the
// line was written for.
//
// What is asserted is that nothing is lost and every line fits -- not a golden
// block. Where the packing puts each clause is allowed to change; that a clause
// survives the fold is not.
func TestTheRunwayLineIsFoldedToTheTerminalItIsPrintedOn(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)

	const width = 60

	// The default seam reports no width for the buffer these tests render
	// into, which is the unfolded rendering every other assertion in this
	// package is written against.
	_, unfolded, _, _ := runRoot(t, "status")
	one := runwayBlockOf(t, unfolded)
	if len(one) != 1 {
		t.Fatalf("a writer with no width folded anyway:\n%s", strings.Join(one, "\n"))
	}
	if w := ansi.StringWidth(one[0]); w <= width {
		t.Fatalf("the fixture's line is %d columns, inside the %d this test narrows to, so it rules nothing out: %q", w, width, one[0])
	}

	stubOutWidth(t, width)
	_, folded, _, _ := runRoot(t, "status")
	block := runwayBlockOf(t, folded)
	if len(block) < 2 {
		t.Fatalf("the line did not fold at %d columns:\n%s", width, strings.Join(block, "\n"))
	}
	for i, l := range block {
		// A line carrying one clause and nothing else is allowed to overflow:
		// the clauses end in an absolute moment and a span, and a cut through
		// either reads as a shorter date rather than as a line that did not fit.
		if w := ansi.StringWidth(l); w > width && strings.Contains(l, "\u00b7") {
			t.Errorf("line %d is %d columns on a %d-column terminal:\n%s", i, w, width, strings.Join(block, "\n"))
		}
	}
	if got, want := runwayClausesOf(t, block), runwayClausesOf(t, one); got != want {
		t.Errorf("the fold changed what the line says:\ngot  %s\nwant %s", got, want)
	}
}

// The runway line was the one that got filed, and Current's recovery
// explanation also exceeds 80 columns. Every labelled line uses the same
// hanging-wrap rule so the reader can identify continuation lines.
//
// The table below the block is deliberately NOT covered here. It is a
// tabwriter, its rows measured 84 columns on that same run, and a table cannot
// be word-wrapped -- narrowing one means dropping columns, which is a different
// decision with a different owner.
func TestEveryLabelledLineFitsTheTerminalItIsPrintedOn(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)
	// Hover makes Strategy's explanatory form visible instead of testing only
	// the short automatic strategy names.
	if code, _, errOut, _ := runRoot(t, "strategy", "hover"); code != ExitOK {
		t.Fatalf("strategy hover = %d (%s)", code, errOut)
	}
	// And a release the daemon has seen, for the same reason and after the same
	// mistake: Update: was added to this block and not to this fixture, so it
	// was the one site of the six whose wrap could be deleted with the whole
	// package still green.
	//
	// STOPPED with a status, which is not a contradiction and not a
	// convenience: the lock says nobody is running and the status file the last
	// daemon left is still readable. Keeping the state stopped is what keeps
	// Daemon: at the 59 columns the width below was chosen against -- a running
	// daemon's line is shorter than this test's own terminal.
	stubVersion(t, "0.6.1")
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonStopped,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion:   daemon.StatusSchemaVersion,
			UpdateCheckedAt: statusNow.Add(-2 * time.Hour),
			UpdateLatest:    "0.7.0",
		},
	}, nil)

	// Thirty-six rather than the eighty the defect was filed at, because at
	// eighty this fixture's Daemon: (59 display columns), Active: (37) and
	// Current: (72) all fit. At thirty-six every call site is
	// load bearing.
	const width = 36

	_, wide, _, _ := runRoot(t, "status")
	block := labelBlockOf(t, wide)
	// Each label named explicitly. A block that stopped printing one of these
	// would otherwise quietly shrink what this test covers.
	for _, label := range []string{"Daemon:", "Update:", "Active (Claude):", "Strategy:", "Current:", "Runway:"} {
		if !strings.HasPrefix(strings.Join(block, "\n"), label) && !strings.Contains(strings.Join(block, "\n"), "\n"+label) {
			t.Fatalf("no %s line in the block, so this test no longer covers it:\n%s", label, wide)
		}
	}
	over := 0
	for _, l := range block {
		if ansi.StringWidth(l) > width {
			over++
		}
	}
	// All six, and the count is named rather than "at least one": a fixture
	// that drifted narrow would otherwise leave this test passing over lines it
	// no longer exercises, which is how it read before it was measured.
	if over != 6 {
		t.Fatalf("%d of this fixture's %d block lines are over %d columns, want all 6, so the assertion below is weaker than it reads:\n%s", over, len(block), width, wide)
	}

	stubOutWidth(t, width)
	_, narrow, _, _ := runRoot(t, "status")
	narrowBlock := labelBlockOf(t, narrow)
	for i, l := range proseOf(narrowBlock) {
		// A single word wider than the terminal is allowed through: cutting a
		// path or an account label produces a shorter one that reads as real.
		if w := ansi.StringWidth(l); w > width && len(strings.Fields(l)) > 1 {
			t.Errorf("line %d is %d columns on a %d-column terminal:\n%s", i, w, width, strings.Join(narrowBlock, "\n"))
		}
	}
	// Wrapping is not rewriting. The block says the same words in the same
	// order at both widths.
	if got, want := strings.Join(strings.Fields(strings.Join(narrowBlock, " ")), " "),
		strings.Join(strings.Fields(strings.Join(block, " ")), " "); got != want {
		t.Errorf("the wrap changed what the block says:\ngot  %q\nwant %q", got, want)
	}
}

// labelBlockOf is the labelled block at the top of `ccdad status`: everything
// above the blank line that separates it from the table. The blank line is the
// separator the renderer actually writes, so this follows the rendering rather
// than a list of label names that would go stale the next time one is added.
func labelBlockOf(t *testing.T, out string) []string {
	t.Helper()
	var block []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" {
			if len(block) == 0 {
				continue
			}
			return block
		}
		block = append(block, l)
	}
	if block == nil {
		t.Fatalf("no labelled block in:\n%s", out)
	}
	return block
}

// proseOf is the block without its runway line and that line's continuations.
// The runway line is in this block and is wrapped by a different function under
// a different rule -- its clauses are atomic and one of them alone can be wider
// than a narrow terminal, which is a deliberate overflow rather than a line
// that did not fit. TestTheRunwayLineIsFoldedToTheTerminalItIsPrintedOn is
// where that one is measured.
func proseOf(block []string) []string {
	for i, l := range block {
		if strings.HasPrefix(l, "Runway:") {
			return block[:i]
		}
	}
	return block
}

// status.go states, in the paragraph above its first Fprintln, that every line
// of the labelled block folds through view.WrapLabeled -- or view.RunwayWrap,
// for the one line whose clauses are atomic -- at the width of
// cmd.OutOrStdout(), and it counts the sites so a reader can check the count.
// Nothing checked the count, and nothing checked the rule.
//
// TestEveryLabelledLineFitsTheTerminalItIsPrintedOn asks the stronger question
// where it reaches: it renders the block narrow and proves the fold HAPPENS,
// rather than that it was called for. What it cannot reach is a site its
// fixture never emits. Update: shipped through a hole of exactly that shape --
// added to the block, not added to that fixture -- and dropping its wrap left
// every test in this package green. Measured: of the six sites, that one and
// only that one.
//
// So this asks the structural question, where no fixture gets a vote: every
// Fprintln into out in this function folds, and there are six of them. A
// seventh line turns this red on purpose. That is the whole mechanism -- the
// paragraph in status.go cannot make anybody read it, and this can.
//
// Six and not the eleven this counted before there were loops here. Two loops
// replaced five sites: view.TrailerLines prints the block under the table, and
// Snapshot.SummaryLines prints Active, Strategy and Current -- which is now one
// line PER PROVIDER, so that block grew a line while losing two sites. That is
// what the count is for. It cannot be moved without being argued, and the fold
// it guards is still on every line, because each loop folds what it prints.
//
// The width EXPRESSION is pinned too, by source rather than by value.
// TestTheFoldMeasuresTheFileAndNotTheWriterItPaintsThrough holds that half at
// runtime but only at the sites its fixture executes, and neither Hover: nor
// Update: is one of them. outWidth(out) compiles, is 0 on every terminal there
// is, and changes no byte of any fixture.
func TestEveryLineOfTheStatusBlockFoldsAtTheFilesWidth(t *testing.T) {
	const (
		file      = "status.go"
		fn        = "renderStatus"
		wantSites = 7
		wantWidth = "outWidth(cmd.OutOrStdout())"
	)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == fn {
			body = fd.Body
		}
	}
	if body == nil {
		t.Fatalf("no func %s in %s, so this test now guards nothing", fn, file)
	}

	// RunwayWrap is here rather than in a rule of its own because the question
	// is which lines FOLD, not which helper folds them. The two differ in what
	// they treat as unbreakable, and both take the width as their last
	// argument, which is the half this asserts about either.
	folds := map[string]bool{"view.WrapLabeled": true, "view.RunwayWrap": true}

	sites := 0
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		// Two arguments: the bare Fprintln(out) below the block is the blank
		// separator and has nothing to fold, and the no-accounts line goes to
		// stderr rather than out.
		if !ok || types.ExprString(call.Fun) != "fmt.Fprintln" || len(call.Args) != 2 {
			return true
		}
		if types.ExprString(call.Args[0]) != "out" {
			return true
		}
		sites++
		where := fset.Position(call.Pos())
		inner, ok := call.Args[1].(*ast.CallExpr)
		if !ok || !folds[types.ExprString(inner.Fun)] {
			t.Errorf("%s: %s goes to out unfolded. Every line of this block wraps at the "+
				"terminal's width -- this one goes out raw, at whatever it measures, on the "+
				"80-column window it was written for",
				where, types.ExprString(call.Args[1]))
			return true
		}
		if got := types.ExprString(inner.Args[len(inner.Args)-1]); got != wantWidth {
			t.Errorf("%s: folds at %s, want %s. outWidth answers by asserting *os.File and "+
				"out is renderTarget's wrapper around one, so the assertion fails, the width "+
				"is 0 on every terminal there is, and the fold stops without a word",
				where, got, wantWidth)
		}
		return true
	})

	// Named rather than "at least one". A restructure that moved these lines
	// behind a helper would otherwise leave this test passing over a function
	// with nothing in it left to check.
	if sites != wantSites {
		t.Fatalf("%d lines of %s print to out, want %d. If a line was added, it needs the "+
			"fold and both counts -- this one and the paragraph in %s that states the rule -- "+
			"moved with it; if one was removed, the same",
			sites, fn, wantSites, file)
	}
}

func TestStatusSaysWhenTheDaemonHasSeenANewerRelease(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubVersion(t, "0.6.1")
	seedAccount(t, "uuid-a", "work@example.com")
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion:   daemon.StatusSchemaVersion,
			PID:             4242,
			StartedAt:       statusNow.Add(-3 * time.Hour),
			UpdateCheckedAt: statusNow.Add(-2 * time.Hour),
			UpdateLatest:    "0.7.0",
		},
	}, nil)

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "0.7.0 is out") {
		t.Errorf("the dashboard does not say a release is out:\n%s", stdout)
	}
	if !strings.Contains(stdout, "0.6.1") {
		t.Errorf("the dashboard does not say what this binary is:\n%s", stdout)
	}
}

// The line is an exception on a dashboard people read every day, so it appears
// only when there is something to say. A daemon publishing the version already
// running is the ordinary state of every up-to-date machine.
func TestStatusSaysNothingAboutAReleaseItIsAlreadyOn(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubVersion(t, "0.7.0")
	seedAccount(t, "uuid-a", "work@example.com")
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion:   daemon.StatusSchemaVersion,
			PID:             4242,
			UpdateCheckedAt: statusNow.Add(-2 * time.Hour),
			UpdateLatest:    "0.7.0",
		},
	}, nil)

	_, stdout, _, _ := runRoot(t, "status")
	if strings.Contains(stdout, "is out") {
		t.Errorf("the dashboard announced a release this binary is already on:\n%s", stdout)
	}
}

// jsonStampPattern finds every timestamp in a document the way a reader finds
// them — by their shape on the wire rather than by a list of key names this
// test would have to be told to grow. It matches a pre-formatted string exactly
// as it matches a marshalled time.Time, which is what makes it able to see the
// payloads that spell their own layout.
var jsonStampPattern = regexp.MustCompile(`"(\d{4}-\d\d-\d\dT[\d:.]+(?:Z|[+-]\d\d:\d\d))"`)

// jsonStamps is every timestamp in a --json document, in order.
func jsonStamps(doc string) []string {
	var out []string
	for _, m := range jsonStampPattern.FindAllStringSubmatch(doc, -1) {
		out = append(out, m[1])
	}
	return out
}

// pinReaderZone fixes the zone --json documents are rendered in, for a test that
// would otherwise be blind.
//
// Nothing sets TZ in CI, so time.Local is UTC there, and a test that asserted
// against it would accept the very rows the bug leaves behind — every one of
// them already ends in Z. Pinning a zone that is nobody's default makes the
// assertion decide the same thing on every machine.
func pinReaderZone(t *testing.T, loc *time.Location) {
	t.Helper()
	saved := readerZone
	t.Cleanup(func() { readerZone = saved })
	readerZone = func() *time.Location { return loc }
}

// One document, one zone, and it is the machine's.
//
// `ccdad status --json` is the widest of these documents: it carries the
// daemon's own stamps, the engine's poll schedule per account, the cache's
// fetch times, the windows' rollovers as the endpoint reported them, and two
// kinds of projection. Those arrive from four different places in four
// different zones — resets_at is parsed off a wire string ending in Z, a poll
// time pulled to a rollover inherits that Z, and everything computed from
// time.Now() carries the machine's offset — and until they are rendered
// together nothing makes them agree.
//
// The fixture carries the mixture on purpose. Seeding only machine-zone
// moments would let this pass on CI, where local is UTC and the bug's own
// output is indistinguishable from the fix's.
func TestStatusJSONRendersEveryTimeInOneZone(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	pinReaderZone(t, time.FixedZone("KST", 9*60*60))
	// Two accounts with history, pace projections and window rollovers, so the
	// document carries every kind of moment it can carry rather than only the
	// engine's two.
	seedBurningFleet(t)
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion: daemon.StatusSchemaVersion,
			PID:           4242,
			// Set, because a published document always carries it -- the writer
			// stamps it on every write. Left unset the payload would carry
			// generatedAt as year 1, which is not in any zone and would make
			// this assertion about the zero-time guard rather than about the
			// rule it exists to pin.
			GeneratedAt: statusNow.Add(-30 * time.Second),
			// An OLD daemon's document, still on disk after an upgrade: it was
			// written before the writer rendered one zone, so it carries both.
			// The reader has to be right about it too, or the fix only reaches
			// the machines that restarted the daemon.
			StartedAt: statusNow.Add(-3 * time.Hour),
			Accounts: []daemon.AccountStatus{
				{UUID: "uuid-a", State: daemon.StateActive,
					NextPollAt: statusNow.Add(10 * time.Minute).In(time.FixedZone("XYZ", -7*60*60)),
					LastPollAt: statusNow.Add(-2 * time.Minute)},
				{UUID: "uuid-b", State: daemon.StateCandidate,
					NextPollAt: statusNow.Add(12 * time.Minute).UTC(),
					LastPollAt: statusNow.Add(-1 * time.Minute).UTC()},
			},
		},
	}, nil)

	_, stdout, _, _ := runRoot(t, "status", "--json")
	stamps := jsonStamps(stdout)
	// The count is the guard against a fixture that stopped producing
	// timestamps: a document with none would satisfy every assertion below.
	if len(stamps) < 6 {
		t.Fatalf("the document carries %d timestamps, too few for this to be deciding anything:\n%s",
			len(stamps), stdout)
	}
	for _, got := range stamps {
		if !strings.HasSuffix(got, "+09:00") {
			t.Errorf("%s is not in the document's zone; one document carries one zone:\n%s", got, stdout)
		}
	}
}

// The zone is the machine's, because the person reading the document is on the
// machine. It is a var only so a test can pin it — see pinReaderZone.
func TestJSONZoneIsTheMachineZone(t *testing.T) {
	if got := readerZone(); got != time.Local {
		t.Errorf("--json documents render in %v, want the machine's zone %v", got, time.Local)
	}
}

func TestStatusWarnsWhenTheCodexProxyIsNotOnThePortCodexWasTold(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion:      daemon.StatusSchemaVersion,
			PID:                1,
			StartedAt:          statusNow.Add(-time.Hour),
			CodexProxyPort:     24242,
			CodexProxyFellBack: true,
		},
	}, nil)

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "must be relaunched") {
		t.Errorf("the dashboard never says the old sessions have to be relaunched:\n%s", stdout)
	}
	if !strings.Contains(stdout, "24242") {
		t.Errorf("the dashboard does not name the port the proxy is actually on:\n%s", stdout)
	}
}

func TestStatusSaysNothingAboutAProxyOnThePortItResolved(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion:  daemon.StatusSchemaVersion,
			PID:            1,
			StartedAt:      statusNow.Add(-time.Hour),
			CodexProxyPort: 24242,
		},
	}, nil)

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, stdout)
	}
	if strings.Contains(stdout, "must be relaunched") {
		t.Errorf("the dashboard warns about a proxy that is on the port it resolved:\n%s", stdout)
	}
}

// The two fixtures below differ from the first one in exactly ONE field, the
// daemon state, which is what makes each of them an assertion about that field
// rather than about the block as a whole.
//
// A stopped daemon leaves its last status document behind, readable, with
// codexProxyFellBack still true inside it. The lock says nobody is running and
// the file is still there; that pairing is not a contradiction and this package
// already exercises it. Reading the fallback flag out of such a document and
// printing it would name a loopback port nothing is bound to and tell the user
// to relaunch sessions when what they actually need is a daemon -- and `ccdad
// doctor` answers the same machine the opposite way, returning "no daemon is
// running, so nothing is serving codex" before it ever looks at the flag.
func TestStatusSaysNothingAboutTheProxyWhenTheDaemonIsStopped(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonStopped,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion:      daemon.StatusSchemaVersion,
			PID:                1,
			StartedAt:          statusNow.Add(-time.Hour),
			CodexProxyPort:     24242,
			CodexProxyFellBack: true,
		},
	}, nil)

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, stdout)
	}
	// The positive half, and it is load bearing rather than decoration. This
	// fixture seeds no accounts, so renderStatus prints the daemon line and
	// returns two lines later: an assertion that only looked for the ABSENCE of
	// the sentence would stay green with the whole block deleted, which is the
	// blind shape this pair exists to avoid. Proving the daemon line rendered
	// is what turns the silence below into a decision the code made.
	if !strings.Contains(stdout, "Daemon:  not running") {
		t.Fatalf("the daemon line never rendered, so the silence below proves nothing:\n%s", stdout)
	}
	if strings.Contains(stdout, "must be relaunched") {
		t.Errorf("the dashboard tells the user to relaunch codex sessions against a port whose daemon is gone:\n%s", stdout)
	}
}

// "Cannot tell" is never folded into "no", which is the rule this repository
// states wherever a daemon state is read: DaemonUnknown means the lock could
// not be probed, and on a filesystem where locks do not work the daemon may
// well be running with a proxy that fell back. That reader is exactly the one
// the sentence is written for, so the guard excludes the definite no and
// nothing else.
//
// Without this test the guard could be narrowed from `!= daemon.DaemonStopped`
// to `== daemon.DaemonRunning` with every other test in the package still
// green, and the narrowing would read like a tightening rather than the
// silencing it is.
//
// The probe error alongside a readable document is the real shape and not a
// contrived one: daemon.Observe reads the status file BEFORE it touches the
// singleton, so a lock it cannot probe costs the liveness verdict and not the
// contents. The error itself lands on stderr as a notice, which is why this
// asserts on stdout and why the exit code is still 0.
func TestStatusWarnsWhenTheCodexProxyFellBackAndTheDaemonStateIsUnknown(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonUnknown,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion:      daemon.StatusSchemaVersion,
			PID:                1,
			StartedAt:          statusNow.Add(-time.Hour),
			CodexProxyPort:     24242,
			CodexProxyFellBack: true,
		},
	}, daemon.ErrLocksUnsupported)

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "must be relaunched") {
		t.Errorf("a daemon whose lock could not be probed silenced the fallback warning, and a probe that cannot answer is not a daemon that is stopped:\n%s", stdout)
	}
}
