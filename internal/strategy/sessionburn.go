package strategy

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The session's burn rate, and the question it answers that no per-account rate
// can.
//
// Every rate this package could read before it belonged to an ACCOUNT. That is
// the wrong owner. An account nobody is spending measures zero, and the moment a
// session moves onto it, it burns at whatever rate that session burns -- so a
// rule that asked "will this candidate run out" of a candidate's own numbers got
// "no" from every account in the fleet, including the ones a session would empty
// in nine minutes. The rate belongs to the work, not to the seat it is done in.

// sessionBurn is the work's rate together with the window it was measured on.
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

// workBurnOf is the work's measured rate for one pass: the LIVE account's, because
// the work is running there, and the pool's maximum only when no live account is
// named.
//
// The live account's and not the pool's maximum, and the difference is the rule
// closingWindow is built on. A rate goes stale by up to one poll interval: an
// account the session has just left keeps the rate it had until it is next read,
// up to ten idle minutes later. Taken as the work's rate, that stale figure
// would judge the very account it belongs to -- its own stale burn condemning it
// by the back door -- and judge every other candidate on a session that is no
// longer running. A live account whose reading cannot name a rate yet says
// "cannot say", which carries every candidate: a missing measurement is never
// evidence.
//
// It reads one account on purpose, and it moves no threshold for doing so. The
// objection to a fleet-wide rate is that a threshold a user cannot follow in
// `ccdad status` is one they cannot argue with; this rate sets none. It only asks
// of an account the work is not on whether that work would run it out, and it is
// two consecutive readings of one window, a figure the same table shows.
func workBurnOf(cands []Candidate, live string) sessionBurn {
	for _, c := range cands {
		if !isActive(c.UUID, live) {
			continue
		}
		if !c.HasBurn {
			return sessionBurn{}
		}
		return sessionBurn{perMin: c.BurnPerMin, has: true, window: c.BurnWindow}
	}
	return measuredSessionBurn(eligibleOf(cands))
}

// measuredSessionBurn is how fast the work running right now is spending quota,
// in points of a binding window a minute, with the window it was measured on --
// for a pass that cannot name the live account.
//
// It is the MAXIMUM over the pool, and that is a measurement rather than a
// heuristic: only one account is live at a time, so only one account's binding
// window is moving, and every other measured rate is a zero taken while nothing
// was being spent there. The maximum is therefore the live account's rate, up
// to the staleness workBurnOf describes.
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

// runsOut names the window the work running now would run out on this account
// inside horizon, or "" when it would not or nobody can say.
//
// It is the carry test: the work's rate applied to the account's own room --
// RoomOf's window, the one OutOfQuota and the last point read -- and asked
// whether that room outlasts the horizon or the window resets first (runsOutBy).
//
// The "cannot say" arms are what keep it from narrowing the engine on a fleet it
// has not measured yet: with no rate, or a rate measured on a window of another
// length, the account carries, and with a rate of nothing nothing runs out. That
// is the same direction Candidate.HasBurn documents and the same one unreadable
// takes everywhere else in this package -- a missing measurement is never
// evidence.
func (b sessionBurn) runsOut(s *usage.Snapshot, h Headroom, now time.Time, horizon time.Duration) usage.WindowName {
	name := roomWindow(h)
	perMin, applies := b.onWindow(name)
	if !applies {
		return ""
	}
	at, spending := atRate(now, RoomOf(h), perMin)
	if !spending {
		return ""
	}
	if !runsOutBy(windowNamed(s, name), at, now.Add(horizon)) {
		return ""
	}
	return name
}

// windowNamed is the named window of this reading, or the absent window when it
// carries none -- which has no reset, so runsOutBy reads it as never resetting.
func windowNamed(s *usage.Snapshot, n usage.WindowName) usage.Window {
	for _, w := range s.AllWindows() {
		if w.Name == n {
			return w.Window
		}
	}
	return usage.Window{}
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
