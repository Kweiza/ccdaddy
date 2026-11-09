package strategy

import "time"

// clockTurns is every instant at which the clock alone can change what the
// hand-off makes of the LIVE account's runway, with no reading landing and no
// switch: outLead and carrierLead counted back from the runway's end, and the
// instant it turns closing, each window's reserve counted back from that
// window's own stop (Ranked.closesAt). Its runway runs from its reading, so
// those instants are fixed between readings, and its next look can be overdue
// -- no poll is made sooner than ServeTTL after the last -- so each then runs
// from the clock.
//
// That is the contract, and it is the live account's alone. The runway of an
// account the session is not on runs from now, so its end moves with the clock
// and these instants never come. It carries no closing instant (Ranked.closesAt
// is zero): a five-hour one would sit at the runway's end, and a short runway's
// end, always a moment away, had the simulation ask every second of it for
// nothing -- three times as long, the same figures. Its verdicts can still
// change by the clock --
// the carry test while its prompt poll is ahead of now, a bridge as the live
// account nears its stop -- and clockTurns does not report them. The weekly
// simulation, its one reader, asks every second while the live account is at a
// limit, which is when a bridge or a last resort can matter, and its figures
// were measured equal to asking every second throughout.
//
// It lives beside its reader rather than in the engine, which never asks it:
// the daemon is asked every second and needs no schedule of when to ask.
func (r Ranked) clockTurns() []time.Time {
	if !r.HasRunway || r.RunsOutAt.IsZero() {
		return nil
	}
	return []time.Time{
		r.RunsOutAt.Add(-outLead),
		r.closesAt,
		r.RunsOutAt.Add(-carrierLead),
	}
}
