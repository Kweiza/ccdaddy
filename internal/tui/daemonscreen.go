package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// daemonScreen is the only place three facts can be told: whether a daemon
// exited cleanly or crashed, what the poll schedule actually is, and what the
// ranking's slack, threshold and binding window say.
//
// Everything it draws was read by somebody else. The log arrives as lines
// rather than as a path, and the credential home arrives as a string rather
// than as a resolution, because nothing in this package may reach the
// filesystem or the environment on its own.
type daemonScreen struct {
	Report daemon.Report
	Rows   []view.Row
	Log    []string
	Now    time.Time
	LogErr error
	// CredentialHome is THIS process's own resolution of the Claude Code
	// credential home, for comparison against the one the daemon published.
	// A daemon started from a shell that resolved a different one manages that
	// directory for the rest of its life, and every other file on the machine
	// looks normal — comparing the two is the only way anyone finds out.
	CredentialHome string
	// SamePath is how the two are compared. See driftsFrom: this screen may
	// not make that comparison itself, and making it as strings is a bug
	// rather than an approximation.
	SamePath func(a, b string) bool
}

// Keys is the base map with the start key disabled when the lock could not be
// probed.
//
// A lock that cannot be read is not an invitation. The auto-start path already
// encodes exactly this, and disabling the binding rather than ignoring the
// keypress is what also takes it out of the help bar — an advertised key that
// does nothing is worse than one that is not advertised.
func (d daemonScreen) Keys(base KeyMap) KeyMap {
	if d.Report.State == daemon.DaemonUnknown {
		base.Start.SetEnabled(false)
	}
	return base
}

// Body is the screen, in five blocks, cut to the size it was given.
func (d daemonScreen) Body(width, height int) string {
	var lines []string
	lines = append(lines, d.liveness()...)
	lines = append(lines, "")
	lines = append(lines, d.lastDecision()...)
	lines = append(lines, "")
	lines = append(lines, d.pollSchedule()...)
	lines = append(lines, "")
	lines = append(lines, d.cadence()...)
	lines = append(lines, "")
	lines = append(lines, d.logTail()...)

	// Every line here is prose or a value rather than a drawing, so a cut one
	// carries the same two-character cue the keybar does: a silently halved
	// path or error reads as a whole one.
	for i := range lines {
		lines[i] = truncateCue(lines[i], width)
	}
	// What does not fit is counted rather than dropped in silence: a screen
	// that stops at the bottom of the terminal is one a reader takes for the
	// whole of it.
	if height > 0 && len(lines) > height {
		lines = append(lines[:height-1], truncate(fmt.Sprintf("+%d more rows than fit", len(lines)-height+1), width))
	}
	return strings.Join(lines, "\n")
}

// liveness is what the lock says, plus everything the published document says
// about the process that wrote it.
//
// The generated-at stamp is labelled "last change" and NOTHING here colours it
// by age. The status writer skips writes whose content is unchanged, with the
// stamp zeroed for the comparison, so an idle healthy daemon's stamp goes
// stale on purpose — a screen that reddened it past some number of seconds
// would call a working daemon dead. Liveness comes from Report.State alone,
// which comes from the lock, which is the only thing that knows, because the
// kernel releases it when the process dies.
func (d daemonScreen) liveness() []string {
	out := []string{"Daemon"}

	switch d.Report.State {
	case daemon.DaemonRunning:
		out = append(out, "  "+view.DescribeRunning(d.Report, d.Now))
	case daemon.DaemonStopped:
		out = append(out, "  not running")
	default:
		out = append(out, "  unknown  (the lock could not be probed)")
	}

	if d.Report.StatusErr != nil {
		out = append(out, "  status.json could not be read: "+d.Report.StatusErr.Error())
	}
	if !d.Report.HasStatus {
		return append(out, "  no daemon has ever published on this machine")
	}

	s := d.Report.Status
	if s.SchemaVersion != daemon.StatusSchemaVersion {
		out = append(out, fmt.Sprintf("  published by a different ccdad (schema %d)", s.SchemaVersion))
	}
	out = append(out, "  last change "+d.ago(s.GeneratedAt))

	// The clean-shutdown flag only answers a question about a daemon that is
	// no longer running. Asking it of a live one prints an answer about a
	// previous life.
	if d.Report.State != daemon.DaemonRunning {
		if s.Stopped {
			out = append(out, "  last shutdown was clean")
		} else {
			out = append(out, "  no clean-shutdown record, so the last one was a crash or a kill")
		}
	}

	if s.CredentialHome != "" {
		out = append(out, "  credential home "+s.CredentialHome)
		if d.driftsFrom(s.CredentialHome) {
			out = append(out, "  this ccdad resolves "+d.CredentialHome+" instead, so the two manage different logins")
		}
	}
	return out
}

// driftsFrom reports whether this process resolves a DIFFERENT credential home
// from the one the daemon published.
//
// It is SamePath and never ==, and that is the whole reason this predicate has
// a name. ccdad manufactures the two spellings itself -- daemon.ChildEnv pins
// an absolute, symlink-resolved path into every daemon it spawns, while a
// shell's own spelling comes back untouched -- so a trailing slash or a symlink
// is enough to make two names for one directory. `doctor` asks this same
// question and learned it the same way: a string compare there would print the
// warning on every run forever, telling the user to restart a daemon that is
// driving exactly the right directory. This screen is where a reader goes to
// find out whether their daemon is healthy, so a false warning here costs more
// than it does in a report of thirty rows.
//
// A caller that supplied neither the resolution nor the comparison cannot
// answer, and no warning is the honest answer for a caller that cannot. It is
// deliberately NOT the other way round: guessing "they differ" from a missing
// input is how the false warning gets shipped by a different door.
func (d daemonScreen) driftsFrom(published string) bool {
	if d.CredentialHome == "" || d.SamePath == nil {
		return false
	}
	return !d.SamePath(d.CredentialHome, published)
}

// lastDecision is the engine's own record of what it did last, resolved
// against the accounts already loaded rather than against a second read.
func (d daemonScreen) lastDecision() []string {
	out := []string{"Last decision"}
	if !d.Report.HasStatus {
		return append(out, "  nothing published")
	}
	s := d.Report.Status
	if s.ActiveUUID == "" {
		out = append(out, "  the engine has observed no active account")
	} else {
		out = append(out, "  active "+d.label(s.ActiveUUID))
	}
	if s.LastSwitchAt.IsZero() {
		return append(out, "  no switch recorded")
	}
	return append(out, "  last switch "+d.ago(s.LastSwitchAt)+", to "+d.label(s.LastSwitchTo))
}

// pollSchedule is one line per account the daemon published, reusing the same
// state cell the main table draws so the two can never disagree about what a
// state is called.
//
// It is the only place slack, threshold and the binding window appear, and
// they appear together or not at all. All three come from ONE Headroom and
// therefore cannot describe different windows — which is exactly why they are
// here rather than in the main table, where the used, window and reset columns
// all describe the REPORTED window while slack is measured on the binding one.
func (d daemonScreen) pollSchedule() []string {
	out := []string{"Poll schedule  (a next-poll time is scheduled, not exact: cadences carry +/- 10%)"}
	accounts := d.Report.Status.Accounts
	if !d.Report.HasStatus || len(accounts) == 0 {
		return append(out, "  nothing published")
	}

	// The four fields every account has are padded to a common width so they
	// line up down the block. What comes after them is variable by nature --
	// an account with no reading has no slack clause at all, and one that has
	// never been refused has no badge -- and padding those would be inventing
	// columns for fields that are sometimes absent.
	cols := make([][4]string, len(accounts))
	var wide [4]int
	for i, a := range accounts {
		glyph, text, _ := stateCell(a.State)
		state := text
		if glyph != "" {
			state = glyph + " " + text
		}
		poll := "last poll -"
		if !a.LastPollAt.IsZero() {
			poll = "last poll " + d.ago(a.LastPollAt)
		}
		next := "next -"
		switch {
		case a.NextPollAt.IsZero():
		case !a.NextPollAt.After(d.Now):
			next = "next due"
		default:
			next = "next " + view.HumanDuration(a.NextPollAt.Sub(d.Now))
		}
		cols[i] = [4]string{state, d.label(a.UUID), poll, next}
		for j, c := range cols[i] {
			// Display columns rather than bytes: an internationalised address
			// is legal in the store, and padding one by its byte length would
			// misalign every row under it.
			wide[j] = max(wide[j], ansi.StringWidth(c))
		}
	}

	for i, a := range accounts {
		parts := make([]string, 0, 6)
		for j, c := range cols[i] {
			parts = append(parts, c+spaces(wide[j]-ansi.StringWidth(c)))
		}

		row, ok := d.row(a.UUID)
		if ok && row.Headroom.Known {
			parts = append(parts, fmt.Sprintf("slack %.1f vs threshold %.0f on %s",
				row.Headroom.Slack, row.Headroom.Threshold, row.Headroom.Binding))
		}
		// The zero time means NEVER, which is not the same as "an hour ago",
		// so the badge is absent rather than showing an age measured from it.
		if ok && row.HasEntry && !row.Entry.Poll.LastRateLimited.IsZero() {
			parts = append(parts, "429 "+d.ago(row.Entry.Poll.LastRateLimited))
		}
		out = append(out, "  "+strings.TrimRight(strings.Join(parts, "  "), " "))

		// An account whose poll failed is UNKNOWN and not empty, so the reason
		// gets its own line at full length instead of being squeezed into the
		// row above and cut there.
		if a.LastPollError != "" {
			out = append(out, "      "+a.LastPollError)
		}
	}
	return out
}

// cadence is the poll policy's own constants, read from the package that
// enforces them so this prose cannot go stale while the numbers move.
//
// The jitter is named because it is genuinely in force: the engine wires a
// random source, so a cadence is a range rather than a deadline, and every
// next-poll time on this screen is a scheduled one.
//
// The rate-limit band is worded rather than numbered. It is the same band the
// backoff uses, and spelling the status code here would put the string "429"
// on a screen that is also the only place a per-account 429 badge appears —
// where a reader, and a test, could no longer tell the two apart.
func (d daemonScreen) cadence() []string {
	span := func(v time.Duration) string { return view.HumanDuration(v) }
	return []string{
		"Cadence in force  (+/- 10% jitter on every interval)",
		fmt.Sprintf("  floor %s   urgent %s   active up to %s   candidate up to %s   exhausted %s",
			span(pollpolicy.MinInterval), span(pollpolicy.UrgentInterval),
			span(pollpolicy.ActiveMaxInterval), span(pollpolicy.CandidateMaxInterval),
			span(pollpolicy.ExhaustedInterval)),
		fmt.Sprintf("  after a rate-limit refusal: %s up to %s",
			span(pollpolicy.Post429MinInterval), span(pollpolicy.Post429MaxInterval)),
		fmt.Sprintf("  cache served under %s; above %.0f%% of a window the live account polls at %s",
			span(pollpolicy.ServeTTL), pollpolicy.DangerBandPct, span(pollpolicy.DangerInterval)),
		fmt.Sprintf("  with %d accounts on one identity, effectively %s apiece",
			len(d.Rows), span(pollpolicy.PerIdentity(pollpolicy.MinInterval, len(d.Rows)))),
	}
}

// logTail is the last few lines the daemon wrote. A missing file is "no log
// yet" rather than an error: a machine where no daemon has ever run is the
// ordinary state of a fresh install.
func (d daemonScreen) logTail() []string {
	out := []string{"Log"}
	if d.LogErr != nil {
		return append(out, "  the log could not be read: "+d.LogErr.Error())
	}
	if len(d.Log) == 0 {
		return append(out, "  no log yet")
	}
	for _, line := range d.Log {
		out = append(out, "  "+line)
	}
	return out
}

// label resolves a uuid to the name the table uses. An account the daemon
// published and the store no longer holds keeps its uuid: it is the only name
// left, and inventing a friendlier one would hide that they disagree.
func (d daemonScreen) label(uuid string) string {
	if r, ok := d.row(uuid); ok {
		return r.ListLabel()
	}
	if uuid == "" {
		return "-"
	}
	return uuid
}

func (d daemonScreen) row(uuid string) (view.Row, bool) {
	for _, r := range d.Rows {
		if r.Account.UUID == uuid {
			return r, true
		}
	}
	return view.Row{}, false
}

// ago is a stamp as an age. A zero stamp is "never", which is a different
// answer from a very old one.
func (d daemonScreen) ago(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return view.HumanDuration(d.Now.Sub(at)) + " ago"
}
