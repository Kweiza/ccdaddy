package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

func burned(uuid string, rate float64) Candidate {
	c := sub(uuid, snap(win(10, time.Hour), win(10, 72*time.Hour)))
	c.BurnPerMin, c.HasBurn = rate, true
	return c
}

// Only one account is live at a time, so only one account's window is moving and
// every other measured rate is a zero taken while nothing was spent there. The
// maximum is therefore the live account's rate, without this having to be told
// which account is live.
func TestTheSessionsRateIsThePoolsMaximum(t *testing.T) {
	pool := []Candidate{burned("idle-1", 0), burned("live", 5.4), burned("idle-2", 0)}
	got := measuredSessionBurn(pool)
	if !got.has {
		t.Fatal("no rate from a pool that carries three")
	}
	if got.perMin != 5.4 {
		t.Errorf("rate = %v, want 5.4", got.perMin)
	}
}

// A fleet nobody has measured twice yet reports nothing, and every rule
// downstream then behaves exactly as it did before the rate existed.
func TestAnUnmeasuredFleetReportsNoRate(t *testing.T) {
	pool := []Candidate{sub("a", nil), sub("b", nil)}
	if got := measuredSessionBurn(pool); got.has {
		t.Error("reported a rate for a fleet with no measurement in it")
	}
}

// A measured zero is a rate. It is what an idle fleet says, and it is not the
// same answer as "nobody has measured".
func TestAnIdleFleetReportsAMeasuredZero(t *testing.T) {
	got := measuredSessionBurn([]Candidate{burned("a", 0), burned("b", 0)})
	if !got.has {
		t.Fatal("an idle fleet has a rate and it is zero")
	}
	if got.perMin != 0 {
		t.Errorf("rate = %v, want 0", got.perMin)
	}
}

// room is an account with minPct points left on its all-model week, and nothing
// on any other window, so the carry test has exactly one window to read.
func room(minPct float64) (*usage.Snapshot, Headroom) {
	return &usage.Snapshot{SevenDay: win(100-minPct, 72*time.Hour)},
		Headroom{Known: true, MinPct: minPct, MinAnyModelPct: minPct, MinAnyModelWindow: usage.WindowSevenDay}
}

// weekRate is the work running at perMin points of a week a minute.
func weekRate(perMin float64) sessionBurn {
	return sessionBurn{perMin: perMin, has: true, window: usage.WindowSevenDay}
}

// The question the engine has to ask of an account the work is not on: not "will
// this account run out on its own" -- it will not, nobody is spending it -- but
// "would the work running now run it out inside its horizon".
func TestAnAccountCarriesTheSessionForItsRoomOverTheRate(t *testing.T) {
	// 27 points at 5.4 a minute is five minutes.
	s, h := room(27)
	if got := weekRate(5.4).runsOut(s, h, now, 4*time.Minute); got != "" {
		t.Errorf("27 points at 5.4/min ran out on %s inside four minutes", got)
	}
	if got := weekRate(5.4).runsOut(s, h, now, 6*time.Minute); got != usage.WindowSevenDay {
		t.Errorf("27 points at 5.4/min carried six minutes (%q)", got)
	}
}

// With no measurement, or a rate of nothing, every account carries: a missing
// measurement is never evidence, and a zero rate empties nothing.
func TestWithoutARateEveryAccountCarries(t *testing.T) {
	s, h := room(1)
	if got := (sessionBurn{}).runsOut(s, h, now, time.Hour); got != "" {
		t.Errorf("refused an account on a fleet with no measured rate (%q)", got)
	}
	if got := weekRate(0).runsOut(s, h, now, 10*time.Hour); got != "" {
		t.Errorf("a zero rate ran an account out (%q)", got)
	}
}

// A rate is in points of its own window's length, so a five-hour session's rate
// says nothing about how long a week lasts.
func TestTheCarryTestReadsARateOnlyOnAWindowOfItsLength(t *testing.T) {
	s, h := room(1)
	five := sessionBurn{perMin: 5.4, has: true, window: usage.WindowFiveHour}
	if got := five.runsOut(s, h, now, time.Hour); got != "" {
		t.Errorf("a five-hour rate ran a week out (%q)", got)
	}
}

// RoomOf is OutOfQuota's own reading as a number. The two have to agree, or an
// account is filed as having room and as out of quota at once -- and the carry
// test reads the same room.
func TestRoomAndEmptinessReadTheSameWindow(t *testing.T) {
	h := Headroom{
		Known:             true,
		MinPct:            40,
		MinAnyModelPct:    0,
		MinAnyModelWindow: "seven_day",
	}
	if got := RoomOf(h); got != 0 {
		t.Errorf("RoomOf = %v, want 0: the model choice cannot dodge seven_day", got)
	}
	empty, known := OutOfQuota(h)
	if !known || !empty {
		t.Fatalf("OutOfQuota = %v/%v, want empty: the fixture is not exercising the agreement", empty, known)
	}
	s := &usage.Snapshot{SevenDay: win(100, 72*time.Hour)}
	if got := weekRate(5.4).runsOut(s, h, now, time.Minute); got != usage.WindowSevenDay {
		t.Errorf("an account with no room carried a minute (%q)", got)
	}
}
