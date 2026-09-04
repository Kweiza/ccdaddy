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

// WarnBand is how close to its threshold a cell has to be before it is amber.
//
// It is a DISPLAY constant and not an engine number, which is the whole of its
// justification: no anti-flap margin means "close", hysteresis_pct can legally
// be zero, and under hover the engine does not read a configured threshold at
// all. A named display constant claims only "close to the threshold", claims it
// in its own name, and no engine number can drift away from it.
const WarnBand = 10.0

// CellState is a cell's verdict about its own window.
type CellState int

const (
	CellUnknown CellState = iota
	CellAbsent
	CellOver
	CellWarn
	CellOK
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

// WindowSpent is whether one window has nothing left in it.
//
// It is spelled here rather than delegated because strategy carries no
// window-level predicate to delegate to: OutOfQuota is the ACCOUNT-level one,
// and the two stopped agreeing the moment a cap scoped to one model family
// could be gone while the account went on serving every other model.
func (r Row) WindowSpent(n usage.WindowName) (spent, known bool) {
	pct, state := r.WindowPct(n)
	if state != WindowRead {
		return false, false
	}
	return pct >= 100, true
}

// WindowReset is when one window rolls over.
func (r Row) WindowReset(n usage.WindowName) (time.Time, bool) {
	w, carried := r.window(n)
	if !carried {
		return time.Time{}, false
	}
	return w.Reset()
}

// CellState is the verdict that colours one cell.
//
// EMPTY IS CHECKED FIRST, ahead of the band, and that order is the whole of
// this function. Under hover a threshold is an unclamped PACE TARGET, so a
// window far enough through its own cycle with nothing left in it reports
// POSITIVE slack -- measured on a live fleet at +17, past WarnBand -- and a
// band consulted first paints a spent week the colour of a healthy one. Over is
// reserved for pct >= 100, which is the one verdict the cell's own text already
// states, so no arm here can contradict what the reader is looking at.
//
// The band reads THIS window's threshold and no other. Headroom.Slack is one
// window per account; a cell is one window per cell, and feeding it the
// account's number would colour a cell from a window it is not about.
func (r Row) CellState(n usage.WindowName) CellState {
	pct, state := r.WindowPct(n)
	switch state {
	case WindowAbsent:
		return CellAbsent
	case WindowUnreadable:
		return CellUnknown
	}
	if pct >= 100 {
		return CellOver
	}
	if r.Thresholds.For(n)-pct <= WarnBand {
		return CellWarn
	}
	return CellOK
}

// CreditLine is the money sentence for a seat metered in credits, which no
// percentage column can carry.
//
// A balance beats a percentage here: "795.23/2000.00 used, 1204.77 left (USD)"
// is what a person deciding whether to top up needs, and 40% is not.
func (r Row) CreditLine() (string, bool) {
	return r.creditLeftLabel()
}
