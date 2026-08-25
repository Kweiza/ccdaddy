package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// statusNow is the clock this screen is drawn against. It is the same instant
// the page fixtures use, so a row's poll stamps and its quota stamps cannot
// disagree about what "now" is.
var statusNow = fixtureNow

// generatedAt is a CHANGE stamp and not a heartbeat: the status writer skips
// writes whose content is unchanged, with the timestamp zeroed for the compare,
// so an idle healthy daemon's stamp goes stale on purpose. A screen that reads
// it as liveness calls a working daemon dead.
func TestTheChangeStampIsLabelledAsAChangeAndNeverAsALastSeen(t *testing.T) {
	body := runningScreen(t).Body(100, 40)
	if !strings.Contains(body, "last change") {
		t.Error("the generated-at stamp is not labelled as a change")
	}
	if strings.Contains(body, "last seen") || strings.Contains(body, "heartbeat") {
		t.Error("the change stamp is presented as liveness, which it is not")
	}
}

// A lock that cannot be probed is not an invitation to start a daemon. The
// binding is disabled, which also takes it out of the help bar.
func TestTheStartKeyIsDisabledWhenTheLockCouldNotBeProbed(t *testing.T) {
	k := daemonScreen{Report: daemon.Report{State: daemon.DaemonUnknown}}.Keys(DefaultKeys())
	if k.Start.Enabled() {
		t.Fatal("the start key is live under DaemonUnknown, which is a keypress-driven respawn loop on a filesystem where locks do not work")
	}
	live := daemonScreen{Report: daemon.Report{State: daemon.DaemonStopped}}.Keys(DefaultKeys())
	if !live.Start.Enabled() {
		t.Fatal("the start key is dead on a daemon that is definitely stopped, which is the one case it exists for")
	}
}

// The only thing separating a crash from a clean exit. Both leave a valid
// document behind and a free lock, and only the flag says which happened.
func TestACleanShutdownIsDistinguishedFromACrash(t *testing.T) {
	clean := stoppedScreen(t, true).Body(100, 40)
	if !strings.Contains(clean, "last shutdown was clean") {
		t.Fatalf("a clean shutdown was not reported as one:\n%s", clean)
	}
	crashed := stoppedScreen(t, false).Body(100, 40)
	if strings.Contains(crashed, "last shutdown was clean") {
		t.Fatalf("a crash was reported as a clean shutdown:\n%s", crashed)
	}
	if !strings.Contains(crashed, "no clean-shutdown record") {
		t.Fatalf("a crash was not distinguished from a clean exit at all:\n%s", crashed)
	}
}

// A live daemon is not asked about its last shutdown: the flag on a running
// daemon's document describes a previous life.
func TestALiveDaemonIsNotAskedAboutItsLastShutdown(t *testing.T) {
	if body := runningScreen(t).Body(100, 40); strings.Contains(body, "shutdown") {
		t.Fatalf("a running daemon reported a shutdown state:\n%s", body)
	}
}

// An account whose poll failed is UNKNOWN, not empty, so the error is shown in
// full on its own line rather than truncated into the row.
func TestAFailedPollShowsItsWholeErrorOnItsOwnLine(t *testing.T) {
	const boom = "401 unauthorized: the refresh token was rejected"
	d := runningScreen(t)
	d.Report.Status.Accounts[0].LastPollError = boom

	var found bool
	for _, line := range strings.Split(d.Body(120, 40), "\n") {
		if !strings.Contains(line, boom) {
			continue
		}
		found = true
		if strings.Contains(line, d.Rows[0].ListLabel()) {
			t.Fatalf("the error was folded into the account's own row: %q", line)
		}
	}
	if !found {
		t.Fatal("a failed poll's reason never reached the screen")
	}
}

// The three figures come from ONE Headroom, so they cannot describe different
// windows -- which is exactly why slack is on this screen and not in the main
// table, where used, window and resets all describe the reported window and
// slack is measured on the binding one.
func TestSlackThresholdAndBindingWindowAreNamedTogetherOrNotAtAll(t *testing.T) {
	body := runningScreen(t).Body(160, 40)
	var seen bool
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "slack ") {
			continue
		}
		seen = true
		if !strings.Contains(line, "threshold ") || !strings.Contains(line, "five_hour") && !strings.Contains(line, "seven_day") {
			t.Fatalf("slack appears without the threshold and the window it was measured on: %q", line)
		}
	}
	if !seen {
		t.Fatal("slack never appears, and this screen is the only place it can")
	}

	// The row that could not be read has no Headroom at all, so none of the
	// three is invented for it.
	unread := runningScreen(t)
	label := unread.Rows[3].ListLabel()
	for _, line := range strings.Split(unread.Body(160, 40), "\n") {
		if strings.Contains(line, label) && strings.Contains(line, "slack") {
			t.Fatalf("an account with no reading was given a slack figure: %q", line)
		}
	}
}

// The engine wires a random source, so poll cadences are genuinely jittered
// and this screen must say so rather than presenting a scheduled time as an
// exact one.
func TestTheCadenceBlockAdvertisesTheJitterThatIsActuallyInForce(t *testing.T) {
	body := runningScreen(t).Body(100, 40)
	if !strings.Contains(body, "10%") {
		t.Fatal("the cadence block does not name the jitter spread that is actually in force")
	}
	if !strings.Contains(body, "scheduled, not exact") {
		t.Fatal("a next-poll time is presented as an exact arithmetic figure")
	}
}

// The cadence numbers come from the package that enforces them, so the prose
// cannot go stale while the constants move.
func TestTheCadenceBlockReadsTheNumbersRatherThanRestatingThem(t *testing.T) {
	body := runningScreen(t).Body(100, 40)
	for _, want := range []string{"floor 3m", "urgent 1m", "active up to 5m", "candidate up to 10m"} {
		if !strings.Contains(body, want) {
			t.Errorf("the cadence block does not carry %q:\n%s", want, body)
		}
	}
	// Four accounts on one identity, so the per-account cadence is four times
	// the floor. This is the figure a user needs to explain why an account
	// they are watching has not been polled.
	if !strings.Contains(body, "12m apiece") {
		t.Errorf("the per-identity cadence is not derived from the account count:\n%s", body)
	}
}

// The zero time means NEVER, not "an hour ago" -- the same rule that applies
// to an unread window applies to an account that has never been rate-limited.
func TestA429BadgeIsAbsentWhenLastRateLimitedIsZero(t *testing.T) {
	body := screenWithPoll(t, usage.PollState{}).Body(160, 40)
	if strings.Contains(body, "429") {
		t.Fatalf("a zero LastRateLimited rendered a 429 badge; zero means NEVER:\n%s", body)
	}
}

// A real 429 renders the badge with its age, on the same line as the
// account's poll state.
func TestA429BadgeShowsItsAgeWhenLastRateLimitedIsSet(t *testing.T) {
	d := screenWithPoll(t, usage.PollState{LastRateLimited: statusNow.Add(-90 * time.Second)})
	body := d.Body(160, 40)
	if !strings.Contains(body, "429") {
		t.Fatalf("a non-zero LastRateLimited did not render a 429 badge:\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "429") && !strings.Contains(line, d.Rows[0].ListLabel()) {
			t.Fatalf("the badge is not on the account's own line: %q", line)
		}
	}
	if !strings.Contains(body, "429 1m ago") {
		t.Fatalf("the badge carries no age:\n%s", body)
	}
}

// This screen reads the daemon's document and the log. Any utilization, window
// reset or credit figure here would be a number read from a second file, and
// two files publishing one figure is the failure the one-authority rule exists
// to prevent.
func TestTheDaemonScreenPrintsNoQuotaFigure(t *testing.T) {
	body := runningScreen(t).Body(160, 40)
	gauge := "[" + strings.Repeat(string(UnicodeGlyphs.GaugeFull), 9)
	for _, quota := range []string{"87%", "52%", "100%", "1h22m", "4h11m", gauge} {
		if strings.Contains(body, quota) {
			t.Errorf("the daemon screen carries %q, which belongs to the usage cache", quota)
		}
	}
}

// A machine where no daemon has ever run says so, rather than printing a page
// of empty fields that reads as a daemon with nothing to report.
func TestAMachineWithNoPublishedDocumentSaysSo(t *testing.T) {
	d := daemonScreen{Report: daemon.Report{State: daemon.DaemonStopped}, Now: statusNow, Glyphs: UnicodeGlyphs}
	body := d.Body(100, 40)
	if !strings.Contains(body, "no daemon has ever published") {
		t.Fatalf("an unpublished machine rendered no explanation:\n%s", body)
	}
	if !strings.Contains(body, "no log yet") {
		t.Fatalf("a missing log rendered no explanation:\n%s", body)
	}
}

// A document that cannot be read costs the numbers, not the liveness answer:
// the lock still knows, and that is the part a dashboard cannot degrade
// without.
func TestADamagedDocumentStillReportsLiveness(t *testing.T) {
	d := daemonScreen{
		Report: daemon.Report{State: daemon.DaemonRunning, StatusErr: errors.New("status.json: unexpected end of JSON input")},
		Now:    statusNow,
		Glyphs: UnicodeGlyphs,
	}
	body := d.Body(100, 40)
	if !strings.Contains(body, "running") {
		t.Fatalf("a damaged document cost the liveness answer:\n%s", body)
	}
	if !strings.Contains(body, "unexpected end of JSON input") {
		t.Fatalf("the reason the document could not be read was swallowed:\n%s", body)
	}
}

// A daemon managing a different credential home is behaving correctly and
// every other file on the machine looks normal, so comparing the two
// resolutions is the only way anyone finds out.
func TestACredentialHomeThatDisagreesWithThisProcessIsNamed(t *testing.T) {
	d := runningScreen(t)
	d.CredentialHome = "/home/somebody/.config/claude"
	body := d.Body(120, 40)
	if !strings.Contains(body, "/home/somebody/.config/claude") {
		t.Fatalf("this process's own credential home is not shown beside the daemon's:\n%s", body)
	}
	if !strings.Contains(body, "different logins") {
		t.Fatalf("a disagreement was shown without saying what it means:\n%s", body)
	}
	same := runningScreen(t)
	if strings.Contains(same.Body(120, 40), "different logins") {
		t.Fatal("two resolutions that agree were reported as a disagreement")
	}
}

// The warning is off for a caller that cannot answer, and it is off in BOTH
// directions of "cannot": no resolution to compare, and no way to compare it.
//
// This is the failure the whole injection exists to prevent, reached from the
// inside. ccdad manufactures two spellings of this path itself, so a screen
// that treated a missing comparison as "they differ" would tell a user whose
// daemon is driving exactly the right directory to go and restart it -- on the
// one screen a reader opens to find out whether their daemon is healthy.
func TestTheCredentialHomeWarningIsOffForACallerThatCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		with func(daemonScreen) daemonScreen
	}{
		{"no resolution of its own", func(d daemonScreen) daemonScreen {
			d.CredentialHome = ""
			return d
		}},
		{"no way to compare two spellings", func(d daemonScreen) daemonScreen {
			d.CredentialHome = "/home/somebody/.config/claude"
			d.SamePath = nil
			return d
		}},
		{"two spellings of one directory", func(d daemonScreen) daemonScreen {
			d.CredentialHome = "/home/u/.claude/"
			d.SamePath = func(a, b string) bool { return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/") }
			return d
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.with(runningScreen(t)).Body(120, 40)
			if strings.Contains(body, "different logins") {
				t.Fatalf("the screen warned about a disagreement it was in no position to find:\n%s", body)
			}
			// The daemon's own published home is a fact it read from the
			// document, and it stays on the screen whether or not this process
			// can say anything about it.
			if !strings.Contains(body, "credential home /home/u/.claude") {
				t.Fatalf("the published credential home went missing with the warning:\n%s", body)
			}
		})
	}
}

// The screen obeys the size it was given, and says how much it could not show
// rather than stopping at the bottom of the terminal in silence.
func TestTheScreenFitsTheSizeItWasGivenAndSaysWhatItCutAway(t *testing.T) {
	d := runningScreen(t)
	for _, tc := range []struct{ w, h int }{{100, 40}, {80, 12}, {40, 6}, {35, 3}} {
		body := d.Body(tc.w, tc.h)
		lines := strings.Split(body, "\n")
		if len(lines) > tc.h {
			t.Errorf("at %dx%d the screen is %d rows", tc.w, tc.h, len(lines))
		}
		for i, line := range lines {
			if got := ansi.StringWidth(line); got > tc.w {
				t.Errorf("at %dx%d line %d is %d columns: %q", tc.w, tc.h, i, got, line)
			}
		}
		if len(lines) == tc.h && !strings.Contains(lines[len(lines)-1], "more rows than fit") {
			t.Errorf("at %dx%d the screen filled the terminal without saying it was cut", tc.w, tc.h)
		}
	}
}

// runningScreen is the ordinary case: a live daemon that has published, four
// accounts, and a couple of log lines.
func runningScreen(t *testing.T) daemonScreen {
	t.Helper()
	rows := fixtureRows()
	accounts := []daemon.AccountStatus{
		{UUID: rows[0].Account.UUID, State: daemon.StateActive,
			LastPollAt: statusNow.Add(-2 * time.Minute), NextPollAt: statusNow.Add(3 * time.Minute)},
		{UUID: rows[1].Account.UUID, State: daemon.StateExhausted,
			LastPollAt: statusNow.Add(-3 * time.Minute), NextPollAt: statusNow.Add(10 * time.Minute)},
		{UUID: rows[2].Account.UUID, State: daemon.StateCandidate,
			LastPollAt: statusNow.Add(-6 * time.Minute), NextPollAt: statusNow.Add(-1 * time.Minute)},
		{UUID: rows[3].Account.UUID, State: daemon.StateUnknown},
	}
	return daemonScreen{
		Report: daemon.Report{
			State:     daemon.DaemonRunning,
			HasStatus: true,
			Status: daemon.Status{
				SchemaVersion:  daemon.StatusSchemaVersion,
				GeneratedAt:    statusNow.Add(-4 * time.Minute),
				PID:            8123,
				StartedAt:      statusNow.Add(-2*time.Hour - 5*time.Minute),
				CredentialHome: "/home/u/.claude",
				ActiveUUID:     rows[0].Account.UUID,
				LastSwitchAt:   statusNow.Add(-12 * time.Minute),
				LastSwitchTo:   rows[0].Account.UUID,
				Accounts:       accounts,
			},
		},
		Rows:           rows,
		Log:            []string{"12:00:00 polled work@example.com", "12:00:01 nothing to do"},
		Now:            statusNow,
		CredentialHome: "/home/u/.claude",
		// A pure predicate, so this package's tests stay string comparisons.
		// The real one asks the filesystem, which is why it is injected at all;
		// that it is credhome.SamePath rather than == is pinned where the
		// injection happens, in internal/cli.
		SamePath: func(a, b string) bool { return a == b },
		Glyphs:   UnicodeGlyphs,
	}
}

// stoppedScreen is a daemon that is definitely gone, with or without the flag
// that says it left on purpose.
func stoppedScreen(t *testing.T, clean bool) daemonScreen {
	t.Helper()
	d := runningScreen(t)
	d.Report.State = daemon.DaemonStopped
	d.Report.Status.Stopped = clean
	return d
}

// screenWithPoll hangs a poll state off the first account's cache entry, which
// is where the rate-limit stamp lives.
func screenWithPoll(t *testing.T, p usage.PollState) daemonScreen {
	t.Helper()
	d := runningScreen(t)
	d.Rows[0].Entry.Poll = p
	return d
}
