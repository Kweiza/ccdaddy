package usage

import "time"

// The measured burn rate: how fast the account a session is running on is
// actually spending its binding window, in points of that window per minute.
//
// It exists because the only rate this package published before it was PaceOf's,
// which extrapolates the AVERAGE SINCE THE WINDOW OPENED -- `actual / elapsed`.
// That is the right number for a dashboard and the wrong one for a decision. A
// session that starts three hours into a quiet five-hour window is measured
// against three hours of doing nothing, so the average understates what is
// happening now by whatever the idle stretch was, and the projection built on it
// lands after the limit rather than before it.
//
// Measured on a live fleet on 2026-09-05: a five-hour window went 0% to 100% in
// 18m28s twice, and 10% to 100% in 25m27s once, which are 5.4, 5.3 and 3.5
// points a minute. The only points-per-time figure in the tree before this was
// 100 x HoverCooldown / length -- 0.333 points a minute for a five-hour window --
// and that is not a burn rate at all: it is how much of the window the CLOCK
// gets through in two minutes. Sizing a guard against it under-reads what a
// session does by a factor of sixteen.
//
// Two points and not a longer series, and that is a deliberate limit rather than
// a first draft. A series belongs to internal/forecast, which the engine may not
// import -- TestTheEngineDoesNotImportTheForecast is that line -- and the reason
// behind that rule applies here too: a threshold a user cannot follow in
// `ccdad status` is a threshold they cannot argue with. Two consecutive readings
// are a figure the same table can show, from the same two numbers a reader can
// see for themselves.

// BindingSample is one reading of an account's binding window: how much of it
// was used, when that was read, and when the window rolls over.
//
// The reset is carried for one purpose, and it is not decoration: it is what
// tells a burn from a ROLLOVER. Between two readings either side of one, the
// utilization falls, and a rate taken across it is meaningless in both
// directions -- negative where the new window is untouched, and small-positive
// where it is not, which is the dangerous one because it reads as a slow burn
// on an account that has just had a whole window handed back.
type BindingSample struct {
	// Window names which window this reading is OF. Two samples of different
	// windows are not a rate, and the binding window changes on its own -- a
	// five-hour window that rolls over stops being the tightest one, and the
	// next reading is then of the weekly window instead. Subtracting across
	// that pair reads a fresh window's low number against a spent one's high
	// number, or the reverse, and neither is a burn.
	Window WindowName
	Pct    float64
	At     time.Time
	Reset  time.Time
}

// BurnPerMin is the rate between two readings of the same window, in points per
// minute, and whether one could be taken at all.
//
// It refuses in five cases, and each refusal is "cannot say" rather than "zero":
// no baseline, a clock that did not advance, a window that is not the same
// window, a reset that moved, and a reading that fell. The last two are the same
// event seen from two sides -- a window that rolled over -- and the fall is
// checked as well as the reset because a window that has never named a reset can
// still roll.
//
// An unchanged reading IS a rate, and it is zero. That distinction carries the
// whole meaning of this figure for a candidate: an account nobody is spending
// measures zero, which is the honest statement that no session is running there
// and precisely the statement the projection must not confuse with "this account
// will never run out".
func BurnPerMin(prev, cur BindingSample) (float64, bool) {
	if prev.At.IsZero() || !cur.At.After(prev.At) {
		return 0, false
	}
	if prev.Window != cur.Window {
		return 0, false
	}
	if !prev.Reset.IsZero() && !cur.Reset.IsZero() && !prev.Reset.Equal(cur.Reset) {
		return 0, false
	}
	delta := cur.Pct - prev.Pct
	if delta < 0 {
		return 0, false
	}
	return delta / cur.At.Sub(prev.At).Minutes(), true
}

// MinutesAtRate is how long `room` points last at `perMin` points a minute.
//
// It is the question the engine has to ask of a switch target, and it is not the
// question PaceOf answers. PaceOf asks "when does this account run out at the
// rate IT has been going", and for an account nobody is using the answer is
// never -- which is true right up to the moment a session is moved onto it. This
// asks the other one: how long would this account carry the work that is running
// now.
//
// A rate at or below zero has no answer. Returning a very large number instead
// would be read by a caller comparing durations as "lasts longer than anything",
// which is the same mistake as reading an unreadable window as empty, made in
// the generous direction.
//
// Room at or below zero answers zero rather than refusing: an account with
// nothing left lasts no time, and that is a measurement.
func MinutesAtRate(room, perMin float64) (float64, bool) {
	if perMin <= 0 {
		return 0, false
	}
	if room <= 0 {
		return 0, true
	}
	return room / perMin, true
}
