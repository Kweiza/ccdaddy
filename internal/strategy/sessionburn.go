package strategy

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The session's burn rate, and the two questions it answers that no per-account
// rate can.
//
// Every rate this package could read before it belonged to an ACCOUNT. That is
// the wrong owner. An account nobody is spending measures zero, and the moment a
// session moves onto it, it burns at whatever rate that session burns -- so a
// rule that asked "will this candidate run out" of a candidate's own numbers got
// "no" from every account in the fleet, including the ones a session would empty
// in nine minutes. The rate belongs to the work, not to the seat it is done in.

// SessionBurnPerMin is how fast the work running right now is spending quota, in
// points of a binding window a minute, and whether it could be measured.
//
// It is the MAXIMUM over the pool, and that is a measurement rather than a
// heuristic: only one account is live at a time, so only one account's binding
// window is moving, and every other measured rate is a zero taken while nothing
// was being spent there. The maximum is therefore the live account's rate,
// without this function having to be told which account that is -- threading an
// activeUUID into the walk to recover a number the pool already holds would be
// two sources for one fact.
//
// It reads across accounts on purpose, and it moves no threshold for doing so.
// The objection to a fleet-wide rate is that a threshold a user cannot follow
// in `ccdad status` is one they cannot argue with; this rate sets none. It only
// refuses a pre-emptive target that would run out inside the horizon the live
// account is being left over, and it is two consecutive readings of one window,
// a figure the same table shows.
//
// A rate goes stale by at most one poll interval. The poller recomputes it on
// every reading, and an account that has stopped being spent measures zero at
// its next poll, so a rate left behind by a session that has since moved lives
// until that account is next read. The error is in the safe direction: a rate
// that is too high switches EARLIER, which is the whole point of the mechanism.
func SessionBurnPerMin(pool []Candidate) (float64, bool) {
	burn := measuredSessionBurn(pool)
	return burn.perMin, burn.has
}

// sessionBurn is the session's rate together with the window it was measured on.
//
// A rate is in points of ONE window, and the same work is a different number of
// points on a window of a different length. That stopped being academic when
// verified Ultra seats lost their five-hour window: a fleet can now hold a Max
// seat whose binding window is five hours beside Ultra seats that only have a
// week, and five points a minute of the first is not five points a minute of the
// second. Read across, it told a seat holding twenty points of a week -- hours of
// work -- that it could not last five minutes.
type sessionBurn struct {
	perMin float64
	has    bool
	window usage.WindowName
}

// measuredSessionBurn is SessionBurnPerMin's measurement, keeping the window the
// maximum came from.
func measuredSessionBurn(pool []Candidate) sessionBurn {
	var out sessionBurn
	for _, c := range pool {
		if !c.HasBurn {
			continue
		}
		if !out.has || c.BurnPerMin > out.perMin {
			out = sessionBurn{perMin: c.BurnPerMin, has: true, window: c.BurnWindow}
		}
	}
	return out
}

// onWindow is the rate as it applies to the named window, and whether it applies
// at all. A rate whose window is unnamed applies everywhere, because that is how
// a reading cached before the name existed has always been read. A window of a
// different length, or of a length nobody can state, is "cannot say": there is
// no conversion between the two windows' points that is not a guess about the
// plan.
//
// The window is named by a Headroom, which carries no reading to ask for a
// length, so the length is the name's.
func (b sessionBurn) onWindow(n usage.WindowName) (float64, bool) {
	if !b.has {
		return 0, false
	}
	if b.window == "" {
		return b.perMin, true
	}
	from, ok := usage.WindowLength(b.window)
	length, known := usage.WindowLength(n)
	if !ok || !known || from != length {
		return 0, false
	}
	return b.perMin, true
}

// RoomOf is the raw room the session will actually be stopped by: the least room
// among the windows a model choice cannot dodge, falling back to the least room
// anywhere when no such window was readable.
//
// It is OutOfQuota's own reading, as a number rather than as a yes/no. The two
// have to agree: a rule that measured room one way and emptiness another would
// file an account as having four points left and as out of quota at once.
func RoomOf(h Headroom) float64 {
	if h.MinAnyModelWindow == "" {
		return h.MinPct
	}
	return h.MinAnyModelPct
}

// roomWindow names the window RoomOf read its figure off, by the same fallback,
// so a rule that acts on the room can say which window it was.
func roomWindow(h Headroom) usage.WindowName {
	if h.MinAnyModelWindow == "" {
		return h.MinWindow
	}
	return h.MinAnyModelWindow
}

// CarriesFor reports whether an account's room would carry the session for d at
// the measured rate, and whether that could be answered at all.
//
// The "cannot say" arm is what keeps this from narrowing the engine on a fleet
// it has not measured yet: with no rate, every account carries, and every rule
// downstream behaves exactly as it did before this file existed. That is the
// same direction Candidate.HasBurn documents and the same one unreadable takes
// everywhere else in this package -- a missing measurement is never evidence.
func CarriesFor(h Headroom, perMin float64, hasBurn bool, d time.Duration) (bool, bool) {
	if !hasBurn {
		return true, false
	}
	left, ok := usage.MinutesAtRate(RoomOf(h), perMin)
	if !ok {
		// Nothing is being spent, so nothing runs out. Every account carries.
		return true, true
	}
	return left >= d.Minutes(), true
}
