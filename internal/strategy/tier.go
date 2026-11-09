package strategy

import "github.com/Kweiza/ccdaddy/internal/usage"

// The hover order: a rung, then the soonest weekly reset, then the uuid.
//
// The rung says WHICH KIND of account should carry the session and the reset
// says which of that kind. Nothing else is compared -- not slack, not pace, not
// how much room an account holds -- because none of the owner's rules ask for
// it: a five-hour account is used until it reaches a limit whatever another one
// holds, and among weekly-only seats the one whose week ends first is the one
// whose quota is about to be lost.

// Rung is where hover's order files an account, best first. It is exported
// because the order is reported -- `ccdad status` prints one line per account
// and `ccdad auto --json` one field -- and a reporter that re-derived it from
// the fields would be a second implementation of the order, free to drift
// from the one that moves the session.
type Rung uint8

const (
	// RungShortWindow is an account that carries a window shorter than a week
	// -- a five-hour window, on today's plans -- with room on everything that
	// can stop a session. These are spent first: their quota comes back every
	// five hours, so a point of it not spent now is a point thrown away.
	RungShortWindow Rung = iota
	// RungWeeklyOnly is an account with room whose every window is a week. A
	// verified Ultra seat is one; so is a primary credit seat, which carries no
	// window at all and so, having no weekly reset either, sorts last here.
	RungWeeklyOnly
	// RungUnknown is an account nobody could read. A maybe beats a no, which
	// is the place headroomTier gives it too: behind every account known to
	// have room, ahead of every account known not to.
	RungUnknown
	// RungHeldOut is an account the engine left at a limit on a window that
	// has not rolled since; departure.go says why that has to be remembered.
	// Above the limit rungs, so it is the first choice once nothing roomier is
	// left.
	RungHeldOut
	// RungAtLimit is an account down to its last point, or projected to run
	// out before its next reading lands.
	RungAtLimit
	// RungEmpty is an account with nothing left on a window no model choice
	// can dodge.
	RungEmpty
)

func (r Rung) String() string {
	switch r {
	case RungShortWindow:
		return "short-window"
	case RungWeeklyOnly:
		return "weekly-only"
	case RungUnknown:
		return "unreadable"
	case RungHeldOut:
		return "held-out"
	case RungAtLimit:
		return "at-limit"
	case RungEmpty:
		return "empty"
	}
	return "unknown"
}

// shortWindowOf names the first window this reading carries that is shorter
// than a week, or "" when it carries none.
//
// It reads PRESENCE and not utilization, and that is the one subtle line in the
// tier. A five-hour window nothing has spent against since it rolled reads
// {"utilization":null,"resets_at":null}: the key is there and both fields are
// null. That account holds the most five-hour room it ever will, and reading it
// as weekly-only would move the session off a Max account at exactly the moment
// its window is freshest. A verified Ultra seat sends "five_hour": null, which
// usage.Window reads as absent, and that is the only signal the wire gives for
// the tier.
//
// A known length is required as well as presence. A Codex window carries its
// own and a Claude one is named; a window nothing can state the length of is
// not known to be shorter than a week, so it is no evidence of the short tier.
func shortWindowOf(s *usage.Snapshot) usage.WindowName {
	if s == nil {
		return ""
	}
	for _, w := range s.RateLimitWindows() {
		if !w.Present {
			continue
		}
		if _, known := usage.WindowLengthOf(w.Name, w.Window); known && !usage.IsWeeklyOf(w.Name, w.Window) {
			return w.Name
		}
	}
	return ""
}

// AtLimit names the window that has this account at a limit, or "" when it is
// not at one.
//
// Three ways, each one that precedes the 429 rather than following it: nothing
// left (OutOfQuota), the last point before the endpoint reports nothing left
// (lastPoint, hover's alone), or a projection that the next reading lands past
// the limit (Closing). The levels name the window their room was read off; the
// projection names the window it saw run out.
func (r Ranked) AtLimit() usage.WindowName {
	if empty, known := OutOfQuota(r.Headroom); (known && empty) || r.LastPoint {
		return roomWindow(r.Headroom)
	}
	return r.Closing
}

// Rung files this account on the hover order.
//
// The limits are asked first and the hold after them, so an account that is
// both held out and at a limit is at the limit: the hold says when it may be
// taken back, and an account at a limit is not somewhere to take a session
// whatever the hold says.
func (r Ranked) Rung() Rung {
	if empty, known := OutOfQuota(r.Headroom); known && empty {
		return RungEmpty
	}
	if r.AtLimit() != "" {
		return RungAtLimit
	}
	if r.HeldOut != "" {
		return RungHeldOut
	}
	if !r.Headroom.Known {
		return RungUnknown
	}
	if r.ShortWindow != "" {
		return RungShortWindow
	}
	return RungWeeklyOnly
}

// lessHover is the hover order: the rung, then lessConsumeFirst, which is the
// one comparator for "the soonest weekly reset" and gains a second caller
// rather than a copy.
func lessHover(a, b Ranked) bool {
	if ra, rb := a.Rung(), b.Rung(); ra != rb {
		return ra < rb
	}
	return lessConsumeFirst(a, b)
}
