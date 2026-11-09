package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// A primary credit seat that also carries a weekly cap.
//
// This is not a contrived shape. identity.Classify files an account KindCredit
// off Snapshot.HasSubscriptionWindows, which iterates RateLimitWindows -- the
// fixed five and the two codex keys, and nothing else -- so a weekly cap that
// arrived in limits[] is invisible to that test. An account with no fixed
// windows, extra_usage enabled and a real scoped weekly is BOTH a credit seat
// and an account carrying quota.
//
// The cap is under a scope this build cannot name, so it binds only where the
// user has set a threshold on its name. That is what makes it the sharpest case:
// consent lives in the CONFIGURED table, and hover used to hand such a seat a
// table with no PerWindow at all.
const seatCap usage.WindowName = "weekly_scoped:region:eu"

func seatWithCap(uuid string, seatPct, capPct float64, capResetsIn time.Duration) Candidate {
	c := primarySeat(uuid, seatPct)
	c.Usage.Limits = []usage.Limit{unknownScoped("region", "eu", capPct, capResetsIn)}
	return c
}

// seatOpts is a hover pass in which the user has opted the seat's cap in.
func seatOpts() Options {
	o := perWindow(map[usage.WindowName]float64{seatCap: 50})
	o.Hover = true
	return o
}

// Hover derives a table for a primary credit seat like it does for every other
// account, and the set of windows that table admits is the set the CONFIGURED
// table admits.
//
// It used to return early for such a seat, on the ground that a seat metered in
// credits carries no plan windows. The cost was a Thresholds with no PerWindow,
// and admittedWindows reads exactly that map to decide whether an unknown scope
// was opted into -- so every opt-in the user typed was dropped for this one
// account.
//
// The assertion is SET EQUALITY rather than one threshold, because the failure
// it guards is a window going MISSING. It also catches a future derived value
// that is not strictly positive, since admittedWindows reads a non-positive entry
// as "not opted in" and would drop the window on that route instead.
func TestHoverDerivesATableForAPrimaryCreditSeatToo(t *testing.T) {
	week := 7 * 24 * time.Hour
	seat := seatWithCap("c-seat", 10, 60, remaining(week, 0.75))
	pool := []Candidate{seat, sub("a-sub", snap(win(10, time.Hour), win(20, week/2)))}

	o := seatOpts()
	p := HoverThresholds(pool, o)

	names := func(t *testing.T, tbl Thresholds) map[usage.WindowName]bool {
		t.Helper()
		out := map[usage.WindowName]bool{}
		for _, w := range admittedWindows(seat.Usage, "", tbl) {
			out[w.Name] = true
		}
		return out
	}
	configured, derived := names(t, o.Thresholds()), names(t, p.For("c-seat"))
	if len(configured) != len(derived) {
		t.Fatalf("derived admits %v, configured admits %v", derived, configured)
	}
	for n := range configured {
		if !derived[n] {
			t.Errorf("the derived table drops %s; hover may not revoke an opt-in the user gave", n)
		}
	}
	if !derived[seatCap] {
		t.Fatalf("neither table admits %s; the fixture is not testing what it says", seatCap)
	}

	// The seat's own meter is still on its own fixed figure: it is not a plan
	// window.
	if got := p.For("c-seat").CreditThreshold(); got != HoverCreditThreshold {
		t.Errorf("credit threshold = %v, want %v", got, HoverCreditThreshold)
	}
}

// Pre-emption may not run to a seat whose own opted-in cap is all but gone.
//
// Under hover the seat is at a limit on that cap: its last point reads the plan
// meter as well as the credit one, so the seat sits on the limit rung and the
// walk turns it away. That premise is asserted rather than assumed, because it is
// the opt-in reaching the level -- a hover table that dropped the consent would
// leave the seat roomy on its own meter, with nothing at a limit for the order to
// see. It is NOT the seat's own projection any longer: the seat is not live, and
// its own rate says nothing about what the session would do to it.
//
// The third account is in the fixture on purpose: without it the assertion would
// be that pre-emption does not fire, which a dozen unrelated changes could
// satisfy. With it the assertion is about WHERE the session goes; and without it
// the session must still not go to the seat.
func TestPreemptionWillNotRunToASeatWhoseOptedInCapRunsOutFirst(t *testing.T) {
	// 88% of a five-hour window three hours in: its limit is close enough that
	// the blind interval reaches it.
	live := polled(burning("a-live", 88), 1800*time.Second)
	// The seat's own meter is roomy and its opted-in weekly is all but gone,
	// with an hour to run on a seven-day length.
	seat := seatWithCap("b-seat", 10, 99.9, time.Hour)
	seat.FetchedAt, seat.NextPollAt = now.Add(-time.Minute), now.Add(time.Minute)
	healthy := polled(burning("c-healthy", 20), 1800*time.Second)

	p := Decide([]Candidate{live, seat, healthy}, seatOpts(), Config{}, NewState(), "a-live")
	if r, ok := find(p.Result, "b-seat"); !ok || r.LastPoint != seatCap || r.Rung() != RungAtLimit {
		t.Fatalf("the seat is at rung %d (last point on %q): the opted-in cap is what its level has to see", r.Rung(), r.LastPoint)
	}
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "c-healthy")

	if p := Decide([]Candidate{live, seat}, seatOpts(), Config{}, NewState(), "a-live"); p.Action == ActionSwitch {
		t.Errorf("switched to %s (%s): the seat's cap is at its last point", p.Target.UUID, p.Reason)
	}
}

// The fix may not close the defect by revoking the user's consent.
//
// This is the test that forbids the OTHER fix -- making preempt read the hover
// table without first giving the seat a table worth reading. That change alone
// silently drops the opt-in for a live seat, so the cap the user asked to be
// watched stops being projected and the engine sits on the seat while it runs
// out.
//
// The control is the same pool with no opt-in. Hover's order still moves the
// session -- the five-hour account has room and the seat is weekly-only -- but
// as a return, not as a hand-off at a limit, and the reason and the mark are
// what tell the two apart: the limit is the opt-in's doing. The cap is at its
// last point, so the limit the seat is left at is that level, on that cap.
func TestAnOptedInCapStillPreemptsFromAPrimaryCreditSeat(t *testing.T) {
	seat := seatWithCap("a-seat", 10, 99.9, time.Hour)
	seat.FetchedAt, seat.NextPollAt = now.Add(-time.Minute), now.Add(time.Minute)
	room := polled(burning("b-room", 23), 1800*time.Second)
	pool := []Candidate{seat, room}

	p := Decide(pool, seatOpts(), Config{}, NewState(), "a-seat")
	want(t, p, ActionSwitch, ReasonLastPoint, "b-room")
	if p.Departure.Closing != seatCap {
		t.Errorf("Departure = %+v, want the seat left on %s", p.Departure, seatCap)
	}

	off := opts()
	off.Hover = true
	if p := Decide(pool, off, Config{}, NewState(), "a-seat"); p.Reason == ReasonLastPoint || p.Departure.Closing != "" {
		t.Errorf("left at a limit (%s, %+v) with no opt-in; there is no limit without the consent", p.Reason, p.Departure)
	}
}

// A seat handed off at its plan meter's last point is at a limit on THAT cap, and
// the mark it leaves names it: the cap's rollover is what lets the seat back, and
// a credit balance does not roll over. Its credit meter is roomy, so a name read
// off the meter it is ranked on would name a window that is not at any limit.
//
// No poll stamps, so nothing projects and the level alone moves the session.
func TestAPrimaryCreditSeatAtItsPlanLastPointIsAtALimitOnThatCap(t *testing.T) {
	seat := seatWithCap("a-seat", 10, 99.5, 2*24*time.Hour)
	room := sub("b-room", snap(win(20, 3*time.Hour), win(30, 3*24*time.Hour)))

	p := Decide([]Candidate{seat, room}, seatOpts(), Config{}, NewState(), "a-seat")
	if r, _ := find(p.Result, "a-seat"); r.LastPoint != seatCap || r.AtLimit() != seatCap {
		t.Fatalf("last point on %q, at a limit on %q, want both on %s", r.LastPoint, r.AtLimit(), seatCap)
	}
	want(t, p, ActionSwitch, ReasonLastPoint, "b-room")
	if p.Departure != (Departure{From: "a-seat", Closing: seatCap}) {
		t.Errorf("Departure = %+v, want a-seat left on %s", p.Departure, seatCap)
	}
}
