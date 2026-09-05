package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// spentFleetOf20260905 is the fleet that produced eighteen switches in two and a
// half hours: every five-hour window at 100%, each account at a different point
// through its own cycle, and a healthy week underneath so five_hour is the only
// thing that is out.
//
// The elapsed shares are what made the ranking move. Hover's threshold is
// elapsed + share, so the account furthest through its window carries the
// highest threshold and therefore the least negative slack -- which is the whole
// of what "a better target" meant on every one of those eighteen lines.
func spentFleetOf20260905() []Candidate {
	shares := []float64{0.98, 0.95, 0.90, 0.85}
	names := []string{"1-official", "2-tlfyvhsdlek", "3-chan", "4-ejalrnrmf"}
	out := make([]Candidate, 0, len(shares))
	for i, share := range shares {
		out = append(out, sub(names[i], snap(
			elapsedWindow(5*time.Hour, share, 100),
			elapsedWindow(7*24*time.Hour, 0.30, 40),
		)))
	}
	return out
}

// Swapping an empty account for an empty account buys the session nothing.
//
// The waiver at the top of gate exists to get a session OFF an account with
// nothing in it, and it asks only whether the LIVE account is out of quota. On
// 2026-09-05 every account in the fleet was out of quota on five_hour, so the
// waiver fired on every tick, every margin below it was skipped, and the only
// guard left was HoverCooldown -- which is why the switches are spaced exactly
// two minutes and one tick apart.
func TestAFleetWithNothingLeftStaysPutInsteadOfSwappingEmptyForEmpty(t *testing.T) {
	pool := spentFleetOf20260905()
	o := hoverOpts()
	for _, live := range pool {
		p := Decide(pool, o, Config{}, NewState(), live.UUID)
		if p.Action == ActionSwitch {
			t.Errorf("live=%s switched to %s: both are out of quota, so the move costs a credential rotation and buys nothing",
				live.UUID, p.Target.UUID)
		}
	}
}

// And it says so, rather than reporting a margin it never evaluated.
//
// RetryAt is the soonest rollover among the pool, because that is the instant
// the answer can change, and a refusal with no retry reads as a permanent state.
func TestAFleetWithNothingLeftNamesTheStateAndWhenItLifts(t *testing.T) {
	pool := spentFleetOf20260905()
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "4-ejalrnrmf")

	if p.Reason != ReasonNothingCanServe {
		t.Errorf("Reason = %v (%s), want ReasonNothingCanServe", p.Reason, p.Reason)
	}
	// 0.98 of the way through a five-hour window is the soonest rollover in the
	// fixture: six minutes out.
	want := now.Add(remaining(5*time.Hour, 0.98))
	if !p.RetryAt.Equal(want) {
		t.Errorf("RetryAt = %v, want %v (the soonest rollover in the pool)", p.RetryAt, want)
	}
}

// The narrowing must not close the door it was opened for: with ONE account
// still holding quota, the waiver still fires and the session still gets off the
// empty one. TestHoverSwitchesOffTheEmptyAccount covers the ordinary fleet; this
// covers the fleet that is otherwise entirely spent, which is the case the
// narrowing is most likely to break.
func TestOneAccountWithRoomStillPullsTheSessionOffAnEmptyOne(t *testing.T) {
	pool := append(spentFleetOf20260905(), sub("5-fresh", snap(
		elapsedWindow(5*time.Hour, 0.50, 10),
		elapsedWindow(7*24*time.Hour, 0.30, 40),
	)))
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "1-official")
	if p.Action != ActionSwitch {
		t.Fatalf("action = %s (%s), want switch: one account still has room", p.Action, p.Reason)
	}
	if p.Target.UUID != "5-fresh" {
		t.Errorf("switched to %s, want 5-fresh", p.Target.UUID)
	}
}

// An unreadable target is NOT an empty one, and the narrowing must not fold the
// two together. OutOfQuota is three-valued for exactly this reason: a candidate
// nobody could read is a maybe worth trying, and refusing it here would strand a
// session on a spent account every time a poll failed.
func TestAnUnreadableTargetIsStillWorthTryingFromAnEmptyAccount(t *testing.T) {
	pool := []Candidate{
		sub("1-official", snap(
			elapsedWindow(5*time.Hour, 0.98, 100),
			elapsedWindow(7*24*time.Hour, 0.30, 40),
		)),
		sub("2-unreadable", snap(unread(), unread())),
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "1-official")
	if p.Action != ActionSwitch {
		t.Fatalf("action = %s (%s), want switch: an unreadable account is a maybe, not a no",
			p.Action, p.Reason)
	}
	if p.Target.UUID != "2-unreadable" {
		t.Errorf("switched to %s, want 2-unreadable", p.Target.UUID)
	}
}

// The state is reported the same way whether or not hover is on: it is a fact
// about the fleet, not about the pacing policy.
//
// The live account is the one the ranking puts LAST, so the gate is actually
// reached. With the ranked-first account live the answer is ReasonAlreadyBest
// or ReasonAllExhausted -- both already correct, and neither exercises this.
func TestTheNothingCanServeStateIsNotHoverSpecific(t *testing.T) {
	p := Decide(spentFleetOf20260905(), opts(), Config{}, NewState(), "4-ejalrnrmf")
	if p.Action == ActionSwitch {
		t.Errorf("switched to %s under the stock strategy: both are out of quota", p.Target.UUID)
	}
	if p.Reason != ReasonNothingCanServe {
		t.Errorf("Reason = %v (%s), want ReasonNothingCanServe", p.Reason, p.Reason)
	}
}

// The OTHER way a fleet with nothing left is reported -- the live account is
// already the ranked-first one, so the gate is never reached and the arm at the
// bottom of Decide answers instead. It said when nothing could serve but never
// when that would lift, which is the half a reader needs.
func TestEveryAccountSpentAlsoSaysWhenTheFleetComesBack(t *testing.T) {
	p := Decide(spentFleetOf20260905(), opts(), Config{}, NewState(), "1-official")
	if p.Reason != ReasonAllExhausted {
		t.Fatalf("Reason = %v (%s), want ReasonAllExhausted: this fixture reaches the bottom arm", p.Reason, p.Reason)
	}
	want := now.Add(remaining(5*time.Hour, 0.98))
	if !p.HasRetryAt || !p.RetryAt.Equal(want) {
		t.Errorf("RetryAt = %v (has=%v), want %v", p.RetryAt, p.HasRetryAt, want)
	}
}

// It is ActionBlocked and not ActionStay, because the engine wanted to move and
// could not -- which is exit 4, the one worth alerting on.
func TestNothingCanServeBlocksRatherThanReportingAQuietStay(t *testing.T) {
	p := Decide(spentFleetOf20260905(), hoverOpts(), Config{}, NewState(), "4-ejalrnrmf")
	if p.Action != ActionBlocked {
		t.Errorf("action = %s, want blocked: the session is about to stop and nothing can carry it", p.Action)
	}
}

// A window that reports no reset leaves RetryAt zero rather than inventing one.
func TestNothingCanServeWithNoRolloverLeavesTheRetryUnset(t *testing.T) {
	pool := []Candidate{
		sub("a", &usage.Snapshot{FiveHour: usage.NewWindow(pct(100), nil)}),
		sub("b", &usage.Snapshot{FiveHour: usage.NewWindow(pct(100), nil)}),
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "a")
	if p.Action == ActionSwitch {
		t.Fatalf("switched to %s: both are out of quota", p.Target.UUID)
	}
	if !p.RetryAt.IsZero() {
		t.Errorf("RetryAt = %v, want the zero time: no window in the pool named a rollover", p.RetryAt)
	}
}

func pct(v float64) *float64 { return &v }
