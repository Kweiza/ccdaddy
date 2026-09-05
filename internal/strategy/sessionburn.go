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
// without this function having to be told which account that is -- which matters
// because HoverThresholds does not know either, and threading an activeUUID into
// the pass to recover a number the pass already holds would be two sources for
// one fact.
//
// It reads across accounts on purpose, and that is the one objection worth
// stating: hover.go's own note refuses to read a FLEET-WIDE burn rate on the
// ground that a threshold a user cannot follow in `ccdad status` is a threshold
// they cannot argue with. This is a weaker version of the same thing and it
// survives the objection only because the figure is published -- `ccdad status`
// prints it beside the projection it moves, so the arithmetic stays closeable by
// hand.
//
// A rate goes stale by at most one poll interval. The poller recomputes it on
// every reading, and an account that has stopped being spent measures zero at
// its next poll, so a rate left behind by a session that has since moved lives
// until that account is next read. The error is in the safe direction: a rate
// that is too high switches EARLIER, which is the whole point of the mechanism.
func SessionBurnPerMin(pool []Candidate) (float64, bool) {
	out, found := 0.0, false
	for _, c := range pool {
		if !c.HasBurn {
			continue
		}
		if !found || c.BurnPerMin > out {
			out, found = c.BurnPerMin, true
		}
	}
	return out, found
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
