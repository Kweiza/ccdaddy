package strategy

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The one fact the hover order has to remember: which accounts the engine left
// at a limit, and on which window.
//
// It cannot be read back off the readings, and that is the whole reason it is
// state. An account left because its five-hour window was CLOSING -- projected
// to run out before the next reading, at the rate the session was spending it --
// is not empty when it is left. It may hold fifteen points. The moment the
// session moves, the rate on it drops to zero, because the rate is a fact about
// the session and not about the account, and the projection that condemned it
// now says it will last. Read as it stands, it has room again; the order takes
// the session back; the rate returns; the projection fires; and the two
// alternate. That is the flap preemptTarget's own note measured at sixty-five
// switches in fifty-four minutes. The quantity that would stop it -- the
// session's burn in the left account's own window units -- is not observable
// after the switch: the session is now spending some other window, and
// sessionBurn refuses to convert a weekly rate into five-hour points.
//
// So the left account is remembered, and held out until the window that had it
// at a limit has rolled over. It is NOT a latch, in the sense antiflap.go
// refuses one. A latch remembers the TARGET and holds the engine on it; this
// remembers the account LEFT and yields to the one reading that can actually
// give it room again on that window -- one that shows the window started
// afresh. And it is a RUNG, never a filter: a held-out account stays in
// Result.Order, and it is the first choice the moment everything above it is
// at a limit or empty, so no account's last points are ever made unreachable
// by it.
//
// The marks are PER ACCOUNT. One remembered departure would reopen the flap on
// any fleet with two five-hour accounts: left A1 at a limit (marked), left A2
// (the mark now names A2), and A1 reads as roomy and unmarked with nobody
// spending it, so the session goes back to the account it was just driven off.
// TestTwoFiveHourAccountsLeftAtTheirLimitsAreBothHeldOut is that fleet.

// Departure is the half of a switch the ranking later reads: the account the
// engine left, and the window that had it at a limit. Closing is empty when the
// account left was not at one -- a return to a five-hour account, a move to a
// weekly-only seat whose week ends sooner, a user's own switch.
type Departure struct {
	From    string
	Closing usage.WindowName
}

// departureMark is one account's remembered departure.
//
// At is when the switch was recorded, not when it was decided: the window has
// to have started after the session actually left, and the stamp the cooldown
// is measured from is the one that says when that was.
type departureMark struct {
	Closing usage.WindowName `json:"closing"`
	At      time.Time        `json:"at"`
}

// departures is every remembered departure, keyed by account uuid -- never by
// idx, for the reason the quarantine map gives.
type departures map[string]departureMark

// holding names the window this candidate is held out on, or "" when it is not
// held out at all: no mark, or a mark whose window has rolled since.
func (d departures) holding(c Candidate, now time.Time) usage.WindowName {
	m, ok := d[c.UUID]
	if !ok || rolledSince(c.Usage, m.Closing, m.At, now) {
		return ""
	}
	return m.Closing
}

// rolledSince reports whether the named window of this reading has rolled over
// since at -- which is to say whether it can have room the departure did not
// already see.
//
// The test is when the window STARTED, and the reset alone cannot say that. A
// reading taken a minute before the departure has a reset in the future too;
// what tells a new window from the old one is that it began after the session
// left, and a window began one length before its reset.
//
// Every answer that cannot hold the account reads as rolled, because a hold
// with nothing behind it is a hold on a guess:
//
//   - no such window in the reading, including no reading at all;
//   - a window nothing has spent against, which is what a rolled window reads
//     as until its next turn -- the {null,null} shape, or none used and no reset;
//   - a reset at or before now: the window rolled and the refresh has not landed.
//
// A window that carries utilization and no reset, or a length nobody can state,
// cannot be placed in time, and it HOLDS. That is the conservative side for a
// hold that is only a rung: the account is still reachable, so the cost of a
// wrong hold is an order, while the cost of a wrong release is the flap.
func rolledSince(s *usage.Snapshot, name usage.WindowName, at, now time.Time) bool {
	w := usage.NamedWindow{Name: name, Window: windowNamed(s, name)}
	if _, used := w.Percent(); !used || neverSpent(w) {
		return true
	}
	reset, hasReset := w.Reset()
	if !hasReset {
		return false
	}
	if !reset.After(now) {
		return true
	}
	length, known := usage.WindowLengthOf(w.Name, w.Window)
	return known && reset.Add(-length).After(at)
}

// departureFrom is the departure a switch off the live account makes: who it
// leaves, and the window that has it at a limit if one does.
//
// It is read off the live account's own ranking rather than off the arm of
// Decide that made the move, because the mark is a fact about the account and
// not about the rule. A live account projected to run out can be left by the
// pre-emptive switch, or -- when that walk refuses every candidate -- by the
// ordinary one; it is at its limit either way, and the second would otherwise
// leave it unmarked for the order to take straight back.
func departureFrom(res Result, activeUUID string) Departure {
	active, ok := find(res, activeUUID)
	if !ok {
		return Departure{}
	}
	return Departure{From: active.UUID, Closing: active.AtLimit()}
}
