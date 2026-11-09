package strategy

import (
	"testing"
	"time"
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
	got, ok := SessionBurnPerMin(pool)
	if !ok {
		t.Fatal("no rate from a pool that carries three")
	}
	if got != 5.4 {
		t.Errorf("rate = %v, want 5.4", got)
	}
}

// A fleet nobody has measured twice yet reports nothing, and every rule
// downstream then behaves exactly as it did before the rate existed.
func TestAnUnmeasuredFleetReportsNoRate(t *testing.T) {
	pool := []Candidate{sub("a", nil), sub("b", nil)}
	if _, ok := SessionBurnPerMin(pool); ok {
		t.Error("reported a rate for a fleet with no measurement in it")
	}
}

// A measured zero is a rate. It is what an idle fleet says, and it is not the
// same answer as "nobody has measured".
func TestAnIdleFleetReportsAMeasuredZero(t *testing.T) {
	got, ok := SessionBurnPerMin([]Candidate{burned("a", 0), burned("b", 0)})
	if !ok {
		t.Fatal("an idle fleet has a rate and it is zero")
	}
	if got != 0 {
		t.Errorf("rate = %v, want 0", got)
	}
}

func room(minPct float64) Headroom {
	return Headroom{Known: true, MinPct: minPct}
}

// The question the engine has to ask of a switch target: not "will this account
// run out on its own" -- it will not, nobody is spending it -- but "how long
// would it carry the work running now".
func TestAnAccountCarriesTheSessionForItsRoomOverTheRate(t *testing.T) {
	// 27 points at 5.4 a minute is five minutes.
	if carries, known := CarriesFor(room(27), 5.4, true, 4*time.Minute); !known || !carries {
		t.Errorf("27 points at 5.4/min did not carry four minutes (known=%v)", known)
	}
	if carries, known := CarriesFor(room(27), 5.4, true, 6*time.Minute); !known || carries {
		t.Errorf("27 points at 5.4/min carried six minutes (known=%v)", known)
	}
}

// With no measurement every account carries, and the answer is marked unknown so
// a caller can tell "yes" from "cannot say" and decline to act on the second.
func TestWithoutARateEveryAccountCarriesAndSaysItCannotSay(t *testing.T) {
	carries, known := CarriesFor(room(1), 0, false, time.Hour)
	if !carries {
		t.Error("refused an account on a fleet with no measured rate")
	}
	if known {
		t.Error("reported an unmeasured answer as known")
	}
}

// Nothing is being spent, so nothing runs out.
func TestAtAZeroRateEveryAccountCarries(t *testing.T) {
	carries, known := CarriesFor(room(1), 0, true, 10*time.Hour)
	if !carries || !known {
		t.Errorf("carries=%v known=%v, want true/true: a zero rate empties nothing", carries, known)
	}
}

// RoomOf is OutOfQuota's own reading as a number. The two have to agree, or an
// account is filed as having room and as out of quota at once.
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
	if carries, known := CarriesFor(h, 5.4, true, time.Minute); !known || carries {
		t.Errorf("an account with no room carried a minute (carries=%v known=%v)", carries, known)
	}
}
