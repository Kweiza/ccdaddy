// Package view is the dashboard's model: one account's row, and every cell a
// human table renders it into.
//
// It exists so that a renderer outside package cli can exist at all. Package
// cli exports nine identifiers and none of them is a row, so a second renderer
// would have had to spell every percentage, every reset span and every "could
// not be read" a second time — and the two spellings would agree until the day
// one of them changed. That `ccdad status`, `ccdad list` and the terminal
// dashboard can never disagree is a property of there being ONE of each of
// these, not of three of them being written carefully.
//
// Nothing here reads the clock, the filesystem or the environment. A Row is
// built from documents somebody else read.
package view

import (
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Unreadable is what a value that could not be read renders as. Never "0%" --
// unknown is never read as zero, and cswap's version of that bug parked its
// engine on the account that reset last, because one expired token made every
// account look empty.
const Unreadable = "?"

// NoQuantity is what a cell renders when the quantity does not exist here, as
// against Unreadable's "somebody tried to read it and could not". The two are
// different verdicts and both appear in one rendered row: an account with no
// cached reading has an unknown percentage and no window to name, so its USED
// cell is "?" and its WINDOW cell is "-". Collapsing them would tell a reader
// that a quantity which cannot exist merely went unread, and send them looking
// for the reading.
const NoQuantity = "-"

// Row is one account with everything the dashboard knows about it, from each
// field's one authoritative source.
type Row struct {
	Account  store.Account
	Active   bool
	Entry    usage.Entry
	HasEntry bool
	Headroom strategy.Headroom
	Pace     map[usage.WindowName]usage.Pace
	Engine   daemon.AccountStatus
}

// Rows pairs every account with its cached reading.
//
// `list` builds its rows through this too, and that is what "`ccdad list` and
// `ccdad status --json` can never disagree" actually rests on: one cache, read
// one way, into one shape. Two commands each deriving headroom for themselves
// would agree until the day one of them was changed.
//
// Engine state is deliberately NOT filled in here. It comes from status.json,
// which is the daemon's own document and no part of what `list` reports.
func Rows(accounts []store.Account, cache *usage.Cache, active store.Account,
	hasActive bool, now time.Time, thresholds func(uuid string) strategy.Thresholds) []Row {

	rows := make([]Row, 0, len(accounts))
	for _, a := range accounts {
		row := Row{Account: a, Active: hasActive && a.UUID == active.UUID}
		if entry, ok := cache.Get(a.UUID); ok && entry.Snapshot != nil {
			row.Entry, row.HasEntry = entry, true
			row.Headroom = strategy.HeadroomOf(entry.Snapshot, thresholds(a.UUID))
			row.Pace = entry.Snapshot.Pace(now)
		}
		rows = append(rows, row)
	}
	return rows
}

// ReportedName is the window this account is REPORTED against, which is not
// always the one it is ordered on.
//
// A tripped WEEKLY cap wins, because it is the one that will not come back for
// days: an account whose five-hour window rolls over in eight minutes is still
// unusable until Friday, and naming the five-hour window would tell the user to
// wait eight minutes for it. Ordering still runs on Headroom.Slack, which is the
// tightest window whichever family it belongs to, so this rule moves no account
// in the ordinary order. It is not inert in RECOVERY order: what an account has
// to wait out is the weekly floor, so the same field decides whether it counts
// as returning inside the horizon.
func (r Row) ReportedName() usage.WindowName {
	if r.Headroom.HasFloor {
		return r.Headroom.Floor
	}
	return r.Headroom.Binding
}

// Reported resolves ReportedName to the window itself, together with when it
// rolls over. It is what the USED, WINDOW, RESETS IN and PACE columns all read,
// so they always describe the same window.
//
// The Known check is redundant TODAY and is kept deliberately: with no window
// reporting a utilization, strategy leaves both names as the empty WindowName
// and the loop below matches nothing anyway. A mutation removing it survives for
// exactly that reason. It stays because the alternative is for this function's
// correctness to rest on an invariant of another package's zero value.
func (r Row) Reported() (usage.NamedWindow, bool) {
	if !r.HasEntry || !r.Headroom.Known {
		return usage.NamedWindow{}, false
	}
	// AllWindows, not RateLimitWindows: the reported window can be a per-model or
	// per-surface weekly one out of limits[], and looking it up in the fixed five
	// alone would leave both columns blank for an account whose headroom is
	// perfectly well known.
	want := r.ReportedName()
	for _, w := range r.Entry.Snapshot.AllWindows() {
		if w.Name == want {
			return w, true
		}
	}
	return usage.NamedWindow{}, false
}

// Marker is the active-row marker: the character `status` and `list` both
// already print in front of the IDX column.
func (r Row) Marker() string {
	if r.Active {
		return "*"
	}
	return " "
}

// UsedLabel is how much of the reported window is SPENT, which is the column
// `ccdad status` carries. Never "0%" for an account that could not be read:
// there are two absences here and both render Unreadable. Reported() is false
// with no cache entry, unknown headroom, or a name AllWindows does not carry;
// Percent() is false when the window is present and reported no utilization.
func (r Row) UsedLabel() string {
	bw, ok := r.Reported()
	if !ok {
		return Unreadable
	}
	pct, ok := bw.Percent()
	if !ok {
		return Unreadable
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// WindowLabel names the reported window, or "-" when there is no reading. It
// is deliberately separate from UsedLabel even though both come out of one
// Reported() call: the old inline form left windowName set and used unset when
// Percent() was false, and that combination is real -- a window that is present
// and reported no number.
func (r Row) WindowLabel() string {
	bw, ok := r.Reported()
	if !ok {
		return NoQuantity
	}
	return string(bw.Name)
}

// AgeLabel is how old this account's reading is.
func (r Row) AgeLabel(now time.Time) string {
	if !r.HasEntry {
		return Unreadable
	}
	d, ok := r.Entry.Age(now)
	if !ok {
		return Unreadable
	}
	return HumanDuration(d)
}

// StatusFlags is the suffix `ccdad status` prints after the age column. It is
// NOT `ccdad list`'s: that one joins primary and disabled, because a listing is
// where an account is chosen and an account can carry both flags at once.
func (r Row) StatusFlags() string {
	if r.Account.Disabled {
		return "  (disabled)"
	}
	// An account another machine drives is never chosen here either, and the
	// dashboard is where a reader asks why nothing is rotating to it.
	if r.Account.Elsewhere {
		return "  (another machine)"
	}
	return ""
}

func (r Row) StatusLabel() string { return r.Account.Label() }

// ListLabel is both the address and the handle, which is NOT Account.Label():
// that one returns the alias alone, and a listing is where a user learns which
// alias belongs to which address.
func (r Row) ListLabel() string {
	if r.Account.Alias == "" {
		return r.Account.Email
	}
	return fmt.Sprintf("%s (%s)", r.Account.Email, r.Account.Alias)
}

// TypeLabel has no caller inside package cli today, and never will: both
// `status`'s renderStatus and `list` pass r.Account.Kind straight to a `%s`
// verb and let Stringer run, since that produces the identical bytes with one
// less method call in the diff. It exists for a renderer that cannot lean on
// Stringer -- a terminal UI measuring a column's width needs an actual string
// to measure, not a value it can only format and then measure the result of.
func (r Row) TypeLabel() string { return r.Account.Kind.String() }

func (r Row) TierLabel() string {
	if r.Account.Tier == "" {
		return NoQuantity
	}
	return r.Account.Tier
}

// LeftLabel is how much of the binding window is LEFT, which is the column
// `ccdad list` carries.
//
// It is the complement of status's USED column and deliberately so: `list` is
// where an account is chosen, and headroom is the quantity that choice is made
// on — it is what the engine itself ranks by. The two columns are labelled, so
// a reader is never asked to guess which way round a bare percentage runs.
//
// It stays on the ORDERING window while RESETS IN beside it names the reported
// one, so on an account with a tripped weekly cap the two describe different
// windows. That is the intended trade: LEFT has to keep meaning "how much of the
// tightest window is left" or it stops being the figure the ranking used, and
// RESETS IN has to name the cap that actually holds the account back or it tells
// a user to wait ten minutes for an account that is gone until Friday.
//
// Never "0%" for an account that could not be read.
func (r Row) LeftLabel() string {
	if r.Headroom.Known {
		return fmt.Sprintf("%.0f%%", r.Headroom.Pct)
	}
	// Headroom is computed from the five subscription windows alone (see
	// strategy.HeadroomFor) and is never Known for a seat with none, which is
	// every enterprise/pay-as-you-go account KindCredit names — not just an
	// account that failed to poll. Reading the credit axis instead of printing
	// "?" for the whole class is safe because it is read-only display: it is
	// the SAME extra_usage the credit gate prices a switch against
	// (internal/strategy/credit.go), never a second source for the number.
	if label, ok := r.creditLeftLabel(); ok {
		return label
	}
	return Unreadable
}

// creditLeftLabel is LeftLabel's credit-axis fallback: the remaining amount
// and, when the account reports both figures, the used/limit pair beside it —
// the two things a LEFT column showing nothing but "?" was hiding entirely.
func (r Row) creditLeftLabel() (string, bool) {
	if !r.HasEntry {
		return "", false
	}
	e := r.Entry.Snapshot.ExtraUsage
	if !e.Present {
		return "", false
	}
	used, hasUsed := e.UsedCredits()
	limit, hasLimit := e.MonthlyLimit()
	switch {
	case hasUsed && hasLimit:
		return fmt.Sprintf("%s/%s used, %s left (%s)",
			e.AmountString(used), e.AmountString(limit), e.AmountString(limit-used), e.CurrencyCode()), true
	case hasUsed:
		// Limit is nil, which means the ACCOUNT sets no cap of its own (the
		// credit gate then falls back to the configured ceiling) — there is no
		// account-side number to show a remainder against, so this says used
		// and stops rather than inventing a denominator.
		return fmt.Sprintf("%s used, no account limit (%s)", e.AmountString(used), e.CurrencyCode()), true
	}
	// Neither money figure was on the wire; extra_usage.utilization was the
	// only readable axis. Still worth a real number over "?".
	if pct, ok := e.Percent(); ok {
		return fmt.Sprintf("%.0f%% left", 100-pct), true
	}
	return "", false
}

// ResetsLabel is when the reported window rolls over, as a span. Both tables
// render it from here so the two can never describe one reset two ways.
func (r Row) ResetsLabel(now time.Time) string {
	bw, ok := r.Reported()
	if !ok {
		return NoQuantity
	}
	reset, ok := bw.Reset()
	if !ok {
		return NoQuantity
	}
	return HumanDuration(reset.Sub(now))
}

// PaceLabel is the pace reading's human half: how the reported window's
// consumption compares with the time elapsed in it.
//
// It reports the REPORTED window's pace and no other, so the column describes
// the same window the two columns beside it do. Every window's pace is in
// --json.
//
// The projection is deliberately absent: projectedExhaustionAt and
// willLastToReset stay out of every human view, because a straight line through
// bursty real usage is too rough to present as fact — and the way that sticks is
// that nothing here can reach them: they are behind usage.Pace.Projection.
func (r Row) PaceLabel() string {
	bw, ok := r.Reported()
	if !ok {
		return NoQuantity
	}
	p, ok := r.Pace[bw.Name]
	if !ok {
		// Either a window with no length to measure against, or less than a
		// seventh of the window since its reset — in which case elapsed time is
		// tiny and almost any usage divides out as "far ahead". Saying nothing
		// is the deliberate answer.
		return NoQuantity
	}
	if p.AheadOfPace {
		return "ahead"
	}
	return "on pace"
}
