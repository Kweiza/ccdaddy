package pollpolicy

import (
	"math"
	"testing"
	"time"
)

// The Codex table is deliberately TIMID. The quota endpoint advertises no
// budget at all — no Retry-After on a good response, no x-ratelimit-* headers,
// and upstream's own client never polls it on a timer — so a background poller
// is a traffic shape no official client emits. Fifteen minutes is a floor
// chosen to be tightened only against measurement, never loosened by a policy
// that wants a fresher number.
func TestTheCodexTableIsTheMeasuredTimidOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"MinInterval", Codex.MinInterval, 15 * time.Minute},
		{"ActiveMaxInterval", Codex.ActiveMaxInterval, 30 * time.Minute},
		{"CandidateMaxInterval", Codex.CandidateMaxInterval, 60 * time.Minute},
		{"ExhaustedInterval", Codex.ExhaustedInterval, 60 * time.Minute},
		{"Post429MinInterval", Codex.Post429MinInterval, 30 * time.Minute},
		{"Post429MaxInterval", Codex.Post429MaxInterval, 4 * time.Hour},
		{"Recent429Window", Codex.Recent429Window, time.Hour},
		{"ServeTTL", Codex.ServeTTL, 180 * time.Second},
		{"Post429BackoffMult", Codex.Post429BackoffMult, 1.5},
		{"MovementDeltaPct", Codex.MovementDeltaPct, 1.0},
		{"JitterFrac", Codex.JitterFrac, 0.1},
		{"Urgent", Codex.Urgent, false},
		{"Danger", Codex.Danger, false},
	} {
		if tc.got != tc.want {
			t.Errorf("Codex.%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// The Claude table IS today's constants. If it is not, every one of the 23
// tests written against the package functions is now testing something else.
func TestTheClaudeTableIsTodaysConstants(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"ServeTTL", Claude.ServeTTL, ServeTTL},
		{"MinInterval", Claude.MinInterval, MinInterval},
		{"UrgentInterval", Claude.UrgentInterval, UrgentInterval},
		{"ActiveMaxInterval", Claude.ActiveMaxInterval, ActiveMaxInterval},
		{"CandidateMaxInterval", Claude.CandidateMaxInterval, CandidateMaxInterval},
		{"ExhaustedInterval", Claude.ExhaustedInterval, ExhaustedInterval},
		{"Post429MinInterval", Claude.Post429MinInterval, Post429MinInterval},
		{"Post429MaxInterval", Claude.Post429MaxInterval, Post429MaxInterval},
		{"Recent429Window", Claude.Recent429Window, Recent429Window},
		{"Post429BackoffMult", Claude.Post429BackoffMult, Post429BackoffMult},
		{"MovementDeltaPct", Claude.MovementDeltaPct, MovementDeltaPct},
		{"JitterFrac", Claude.JitterFrac, JitterFrac},
		{"Urgent", Claude.Urgent, true},
		{"Danger", Claude.Danger, true},
	} {
		if tc.got != tc.want {
			t.Errorf("Claude.%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	// DangerInterval is MinInterval by definition, and base() spells the
	// branch as the table's floor. Pinned here so the two cannot drift apart
	// silently.
	if DangerInterval != MinInterval {
		t.Errorf("DangerInterval = %s, want MinInterval — base() returns the table's floor for the band", DangerInterval)
	}
}

// The package functions are the Claude table's methods and nothing else. Every
// existing test in this package goes through them, so this is what makes those
// 23 tests still measure the rules they were written for.
func TestThePackageFunctionsAreTheClaudeTable(t *testing.T) {
	in := Input{Now: epoch, Active: true, Reading: sample(70), Threshold: 80}
	s := seen(60)

	gotAt, gotState := Next(s, in, 0.5)
	wantAt, wantState := Claude.Next(s, in, 0.5)
	if !gotAt.Equal(wantAt) || gotState != wantState {
		t.Errorf("Next() = %s/%+v, Claude.Next() = %s/%+v", gotAt, gotState, wantAt, wantState)
	}
	if InDangerBand(in) != Claude.InDangerBand(in) {
		t.Error("InDangerBand() and Claude.InDangerBand() disagree")
	}
	if Share(time.Minute, 3, in) != Claude.Share(time.Minute, 3, in) {
		t.Error("Share() and Claude.Share() disagree")
	}
	if ServeTTLFor(in) != Claude.ServeTTLFor(in) {
		t.Error("ServeTTLFor() and Claude.ServeTTLFor() disagree")
	}
	if !StandDownUntil(epoch, 0.5).Equal(Claude.StandDownUntil(epoch, 0.5)) {
		t.Error("StandDownUntil() and Claude.StandDownUntil() disagree")
	}
	if RateLimited(s, epoch, time.Minute, true) != Claude.RateLimited(s, epoch, time.Minute, true) {
		t.Error("RateLimited() and Claude.RateLimited() disagree")
	}
	limited := Claude.RateLimited(s, epoch, 0, false)
	gotUntil, gotOK := RateLimitedUntil(limited, epoch)
	wantUntil, wantOK := Claude.RateLimitedUntil(limited, epoch)
	if !gotUntil.Equal(wantUntil) || gotOK != wantOK {
		t.Error("RateLimitedUntil() and Claude.RateLimitedUntil() disagree")
	}
}

// Fifteen minutes is the floor and nothing argues past it. rnd=0.5 is the
// middle of the jitter band, so the deadline is the interval exactly.
func TestTheCodexFloorHoldsForEveryShapeOfAccount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state State
		in    Input
		want  time.Duration
	}{
		{"an idle candidate", seen(10), Input{Now: epoch, Reading: sample(10), Threshold: 80}, 60 * time.Minute},
		{"a moving candidate", seen(10), Input{Now: epoch, Reading: sample(30), Threshold: 80}, 15 * time.Minute},
		{"the serving account, idle", seen(10), Input{Now: epoch, Active: true, Reading: sample(10), Threshold: 80}, 30 * time.Minute},
		{"the serving account, moving", seen(10), Input{Now: epoch, Active: true, Reading: sample(30), Threshold: 80}, 15 * time.Minute},
		{"exhausted", seen(99), Input{Now: epoch, Active: true, Reading: Reading{BindingPct: 99, Known: true, Exhausted: true}, Threshold: 80}, 60 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, _ := Codex.Next(tc.state, tc.in, 0.5)
			if got := at.Sub(epoch); got != tc.want {
				t.Errorf("Codex.Next() = %s after now, want %s", got, tc.want)
			}
		})
	}
}

// No urgent band. The Claude table drops to sixty seconds for a live account
// that is moving AND near its threshold; the Codex table has no such cadence,
// and if it took the branch anyway it would return a zero interval — a
// deadline of now, polled as fast as the loop runs.
func TestTheCodexTableHasNoUrgentBand(t *testing.T) {
	in := Input{Now: epoch, Active: true, Reading: sample(85), Threshold: 80}
	s := seen(70) // moving, and inside the 15 pp band

	at, _ := Codex.Next(s, in, 0.5)
	if got := at.Sub(epoch); got != 15*time.Minute {
		t.Errorf("Codex.Next() = %s after now, want the 15m floor — the Codex table has no urgent cadence", got)
	}

	// The Claude table, on the same input, DOES take it.
	claudeAt, _ := Claude.Next(s, in, 0.5)
	if got := claudeAt.Sub(epoch); got != UrgentInterval {
		t.Errorf("Claude.Next() = %s after now, want %s — the fixture must be inside the urgent band for this test to mean anything", got, UrgentInterval)
	}
}

// No danger band either, so nothing is exempt from the identity share.
func TestTheCodexTableHasNoDangerBand(t *testing.T) {
	in := Input{Now: epoch, Active: true, Reading: sample(97), Threshold: 80}
	if Codex.InDangerBand(in) {
		t.Error("Codex.InDangerBand() = true; the Codex table has no band")
	}
	if !Claude.InDangerBand(in) {
		t.Error("Claude.InDangerBand() = false; the fixture must be inside the band for this test to mean anything")
	}
	if got, want := Codex.Share(time.Hour, 3, in), 3*time.Hour; got != want {
		t.Errorf("Codex.Share() = %s, want %s — with no band there is no exemption from the division", got, want)
	}
	if got := Claude.Share(time.Hour, 3, in); got != time.Hour {
		t.Errorf("Claude.Share() = %s, want the undivided hour inside the band", got)
	}
}

// AIMD climbs to the Codex ceiling and stops there, four hours out rather than
// the Claude table's thirty minutes.
func TestTheCodexBackoffClampsAtItsOwnCeiling(t *testing.T) {
	var s State
	for i := 0; i < 32; i++ {
		s = Codex.RateLimited(s, epoch, 0, false)
	}
	if s.Interval != 4*time.Hour {
		t.Errorf("Interval = %s after repeated 429s, want the 4h ceiling", s.Interval)
	}
	at, ok := Codex.RateLimitedUntil(s, epoch)
	if !ok {
		t.Fatal("RateLimitedUntil() reported no hold after a 429")
	}
	if got := at.Sub(epoch); got != 4*time.Hour {
		t.Errorf("RateLimitedUntil() = %s out, want 4h", got)
	}
}

// The jitter is the table's own fraction, applied to the interval. The widest
// gap the Codex table can leave is what history retention is sized against.
func TestTheCodexJitterIsAFractionOfItsOwnInterval(t *testing.T) {
	in := Input{Now: epoch, Reading: sample(10), Threshold: 80}
	s := seen(10)

	low, _ := Codex.Next(s, in, 0)
	high, _ := Codex.Next(s, in, math.Nextafter(1, 0))
	base := 60 * time.Minute
	if got, want := low.Sub(epoch), time.Duration(float64(base)*(1-Codex.JitterFrac)); got != want {
		t.Errorf("rnd=0 gave %s, want %s", got, want)
	}
	if spread, cap := high.Sub(low), time.Duration(2*Codex.JitterFrac*float64(base)); spread > cap {
		t.Errorf("spread = %s, want at most %s", spread, cap)
	}
}
