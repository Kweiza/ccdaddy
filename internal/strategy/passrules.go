package strategy

import "github.com/Kweiza/ccdaddy/internal/usage"

// Hover and the configured strategies measure an account on the same levels
// (HeadroomFor) and project the same runway (runwayOf). They differ in what
// they file the account at beyond its levels: whether a last point is a limit,
// whether a credit seat's second meter is, whether a departure mark holds the
// account out, and what Closing means. Each mode's answer is one type here,
// chosen once from the pass (Options.rules), so measure, which both modes
// share, asks no question about which mode it is in.

// passLimits is what a pass's rules file one account at beyond its levels:
// the fields of Ranked of the same names, and the live account's limit.
type passLimits struct {
	lastPoint        usage.WindowName
	secondMeterEmpty usage.WindowName
	heldOut          usage.WindowName
	closing          usage.WindowName
	live             liveLimit
}

// passRules is one mode's rules. h is what the account is ranked on and plan
// its plan windows, the same reading for every account but a primary credit
// seat (measure).
type passRules interface {
	limitsOf(o Options, c Candidate, h, plan Headroom, rw runway) passLimits
}

// rules is the pass's rules: hover's when the pass installed hover's table
// (withHover), the configured strategies' otherwise. The installed table is
// the one fact that says which mode a pass is in, so the choice reads it
// rather than keeping a second record that could disagree with it.
func (o Options) rules() passRules {
	if o.hover != nil {
		return hoverRules{}
	}
	return configuredRules{}
}

// configuredRules are the configured strategies'. A threshold a user typed is
// where they said to stop, and it is not these rules' to move: no last point
// is a limit, a credit seat's emptiness is read as it always has been (the
// empty-live waiver in their margins included,
// TestTheConfiguredMarginsDoNotReadAPoolSeatsBalance), and no mark holds an
// account out.
//
// Closing is the pre-emptive projection over the account's own horizon, its
// blind interval plus preempt_lead (closingWindow), on every account, and ""
// whenever pre-emption is off.
type configuredRules struct{}

func (configuredRules) limitsOf(o Options, c Candidate, h, _ Headroom, _ runway) passLimits {
	return passLimits{closing: o.closingWindow(c, h)}
}

// hoverRules are hover's: the last point (lastPointOf), the second meter
// (Options.secondMeterEmptyOf) and the departure marks (departure.go) are
// limits in its order.
//
// Closing is set on the LIVE account only, and means the work would stop it
// within its window's reserve after its next reading (liveLimitOf): an account
// the session is not on is filed by its levels and its mark alone, and whether
// it could take the work is Carries.
type hoverRules struct{}

func (hoverRules) limitsOf(o Options, c Candidate, h, plan Headroom, rw runway) passLimits {
	out := passLimits{
		lastPoint:        lastPointOf(h, plan),
		secondMeterEmpty: o.secondMeterEmptyOf(c, plan),
		heldOut:          o.departures.holding(c, o.Now),
	}
	if !isActive(c.UUID, o.live) {
		return out
	}
	out.live, out.closing = o.liveLimitOf(c, rw)
	return out
}
