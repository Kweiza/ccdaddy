// Package view is the dashboard's model: one account's row, and every cell a
// human table renders it into.
//
// It exists so that a renderer outside package cli can exist at all. Package
// cli exports nine identifiers and none of them is a row, so a second renderer
// would have had to spell every percentage, every reset span and every "could
// not be read" a second time — and the two spellings would agree until the day
// one of them changed. That `ccdad status` and the terminal dashboard can never
// disagree is a property of there being ONE of each of these, not of two of
// them being written carefully.
//
// Nothing here reads the clock, the filesystem or the environment. A Row is
// built from documents somebody else read.
package view

import (
	"fmt"
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/provider"
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
	// Thresholds is the bundle this row was MEASURED against, kept so a cell can
	// be coloured against the same numbers.
	//
	// It is resolved once in Rows and handed to HeadroomOrCredit from this
	// field, so the bundle a cell reads and the bundle the row was ranked on
	// are one object by construction rather than two calls that agree until the
	// day one of them moves. Under hover that matters twice over: the table is
	// derived per account and per window, so a second resolution could hand a
	// cell a number no part of this row was measured with.
	Thresholds strategy.Thresholds
	Pace       map[usage.WindowName]usage.Pace
	Engine     daemon.AccountStatus
}

// Rows pairs every account with its cached reading.
//
// Both status forms and the dashboard build their rows through this: one cache,
// read one way, into one shape. Separate renderers deriving headroom for
// themselves would agree until the day one of them changed.
//
// Engine state is deliberately NOT filled in here. It comes from status.json,
// the daemon's own document, and loadSnapshot joins it after building rows.
func Rows(accounts []store.Account, cache *usage.Cache, active store.Account,
	hasActive bool, now time.Time, thresholds func(uuid string) strategy.Thresholds) []Row {

	rows := make([]Row, 0, len(accounts))
	for _, a := range accounts {
		row := Row{Account: a, Active: hasActive && a.UUID == active.UUID}
		if entry, ok := cache.Get(a.UUID); ok && entry.Snapshot != nil {
			row.Entry, row.HasEntry = entry, true
			// Resolved ONCE and stored, then handed on from the field. Two
			// calls would agree today and diverge the first time the resolver
			// grew a clock or a pool in it -- and under hover it has both.
			row.Thresholds = thresholds(a.UUID)
			// HeadroomOrCredit, not HeadroomOf: a seat metered only in money
			// carries no plan window, and the window-only axis reported it
			// unknown while the engine ranked it on its balance.
			row.Headroom = strategy.HeadroomOrCredit(entry.Snapshot, row.Thresholds)
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

func (r Row) Empty() (empty, known bool) { return strategy.OutOfQuota(r.Headroom) }

// Marker is the active-row marker printed in front of the IDX column.
func (r Row) Marker() string {
	if r.Active {
		return "*"
	}
	return " "
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

// StatusFlags is the suffix `ccdad status` prints after the age column. An
// account may carry more than one flag, so they are joined in one suffix.
func (r Row) StatusFlags() string {
	var flags []string
	if r.Account.Primary {
		flags = append(flags, "primary")
	}
	if r.Account.Disabled {
		flags = append(flags, "disabled")
	}
	// An account another machine drives is never chosen here either, and the
	// dashboard is where a reader asks why nothing is rotating to it.
	if r.Account.Elsewhere {
		flags = append(flags, "another machine")
	}
	if len(flags) == 0 {
		return ""
	}
	return "  (" + strings.Join(flags, ", ") + ")"
}

func (r Row) StatusLabel() string { return r.Account.Label() }

// ListLabel is both the address and the handle, which is NOT Account.Label():
// that one returns the alias alone, and the dashboard is where a user learns
// which alias belongs to which address.
func (r Row) ListLabel() string {
	if r.Account.Alias == "" {
		return r.Account.Email
	}
	return fmt.Sprintf("%s (%s)", r.Account.Email, r.Account.Alias)
}

// TypeLabel is what the TYPE column says about an account.
//
// It is shared by status and the dashboard because it cannot be derived from
// Kind alone. Every ChatGPT plan is a
// subscription, so a codex row rendered from Kind would say "subscription" --
// true, and useless: it would be the word on every codex row and on most of the
// Claude rows beside them, distinguishing nothing. What a reader needs in that
// column is which side of the machine the row belongs to, because that is what
// decides which command moves it.
//
// Status and the dashboard both call this rather than formatting Kind
// themselves. Two renderers formatting one value two ways is the exact failure
// package view exists to remove.
func (r Row) TypeLabel() string {
	if r.Account.Provider == provider.Codex {
		return "codex"
	}
	return r.Account.Kind.String()
}

func (r Row) TierLabel() string {
	if r.Account.Tier == "" {
		return NoQuantity
	}
	return r.Account.Tier
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
