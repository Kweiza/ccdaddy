package view

import (
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// This file is one account's facts about ONE window, and it exists because the
// tables stopped deriving a single window per row.
//
// Every quota column used to be an answer to "which window binds", asked once
// per row and then rendered as though it were the account's whole story. The
// two tables asked it differently -- `list` off the ordering window, `status`
// off the reported one -- so they named different windows, neither said which,
// and a reader could not tell a fleet with a fifth of its week left from one
// with nothing. The row now carries a cell per window and concludes nothing;
// what is left of the conclusions lives in the engine, where it always did.

// WindowState is why a cell says what it says. The three are kept apart because
// they are three different facts and one of them is a bug if it is guessed.
type WindowState int

const (
	// WindowAbsent: the reading does not carry this window at all. Rendered
	// "-". It is a statement about the ACCOUNT -- a Codex seat has no
	// five-hour window and never will -- and it is only ever said about a
	// reading somebody actually took.
	WindowAbsent WindowState = iota
	// WindowUnreadable: nothing was read, or the window was carried and
	// reported no utilization. Rendered Unreadable.
	//
	// Never "-" and never "0%". An account nobody could read is not an account
	// with a fresh window, and cswap's engine parked itself permanently on
	// exactly that confusion. Present-with-null is a real shape on the wire --
	// a freshly reset window reports {"utilization":null,"resets_at":null} and
	// still HAS the window -- so the two absences are not the same absence.
	WindowUnreadable
	// WindowRead: a utilization came back.
	WindowRead
)

// CarriedWindows is every window this row's reading actually carries, in wire
// order, plus the credit meter when it reported a percentage.
//
// The Present filter is load-bearing rather than tidiness. Snapshot's fixed
// Claude keys are listed unconditionally, so an unfiltered walk hands every
// Claude fleet five columns with three of them null on every row forever.
func (r Row) CarriedWindows() []usage.NamedWindow {
	if !r.HasEntry || r.Entry.Snapshot == nil {
		return nil
	}
	all := r.Entry.Snapshot.AllWindows()
	out := make([]usage.NamedWindow, 0, len(all)+1)
	for _, w := range all {
		if w.Present {
			out = append(out, w)
		}
	}
	if pct, ok := r.Entry.Snapshot.ExtraUsage.Percent(); ok {
		out = append(out, usage.NamedWindow{
			Name:   strategy.CreditWindow,
			Window: usage.NewWindow(&pct, nil),
		})
	}
	return out
}

// window finds one window on this row's reading, credit meter included.
func (r Row) window(n usage.WindowName) (usage.Window, bool) {
	for _, w := range r.CarriedWindows() {
		if w.Name == n {
			return w.Window, true
		}
	}
	return usage.Window{}, false
}

// WindowPct is this row's utilization of one window, and why there is no
// number when there is none.
func (r Row) WindowPct(n usage.WindowName) (float64, WindowState) {
	if !r.HasEntry || r.Entry.Snapshot == nil {
		// Nothing about this account was read, so nothing is known about any
		// of its windows -- including whether it has one.
		return 0, WindowUnreadable
	}
	w, carried := r.window(n)
	if !carried {
		return 0, WindowAbsent
	}
	pct, ok := w.Percent()
	if !ok {
		return 0, WindowUnreadable
	}
	return pct, WindowRead
}

// WindowCell is one quota cell: the percentage USED.
//
// Used and not left, so that 100% means the same thing in every cell of the
// table and the eye can scan a row for the number that stops it. The polarity
// is stated once, here, and the header row says nothing about it.
func (r Row) WindowCell(n usage.WindowName) string {
	pct, state := r.WindowPct(n)
	switch state {
	case WindowAbsent:
		return "-"
	case WindowUnreadable:
		return Unreadable
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// WindowThreshold returns the threshold for the exact displayed axis. Credit
// has its own fail-closed threshold and must not fall through to the ordinary
// window default merely because it shares the table with quota windows.
func (r Row) WindowThreshold(n usage.WindowName) float64 {
	if n == strategy.CreditWindow {
		return r.Thresholds.CreditThreshold()
	}
	return r.Thresholds.For(n)
}

// WindowReset is when one window rolls over.
func (r Row) WindowReset(n usage.WindowName) (time.Time, bool) {
	w, carried := r.window(n)
	if !carried {
		return time.Time{}, false
	}
	return w.Reset()
}

// CreditLine is the money sentence for a seat metered in credits, which no
// percentage column can carry.
//
// A balance beats a percentage here: "795.23/2000.00 used, 1204.77 left (USD)"
// is what a person deciding whether to top up needs, and 40% is not.
func (r Row) CreditLine() (string, bool) {
	return r.creditLeftLabel()
}
