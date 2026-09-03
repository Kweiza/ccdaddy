package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// codexPrimarySnap is a reading carrying ONLY a codex primary window, with the
// length the endpoint reported for it. The length is the whole point: the name
// codex_primary is in no weekly list and must not be put in one, because the
// same name runs thirty days on one plan and a week on another.
func codexPrimarySnap(pct float64, resetsIn, length time.Duration) *usage.Snapshot {
	at := now.Add(resetsIn)
	return &usage.Snapshot{CodexPrimary: usage.NewWindowWithLength(&pct, &at, length)}
}

// A thirty-day window with nothing left in it is the floor: it is what has to
// clear before the account is usable again, and it does not clear for weeks.
// Read off the name there is no floor at all, and the account then reads as
// recovered the moment some shorter window rolls over.
func TestABlownThirtyDayCodexWindowIsTheFloor(t *testing.T) {
	s := codexPrimarySnap(100, 20*24*time.Hour, 30*24*time.Hour)

	h := HeadroomOf(s, thr())
	if !h.HasFloor || h.Floor != usage.WindowCodexPrimary {
		t.Fatalf("Floor = (%q, %v), want (codex_primary, true) -- a thirty-day window with nothing "+
			"left in it is what has to clear", h.Floor, h.HasFloor)
	}
}

// The negative, and the reason this is a test about LENGTH rather than a rule
// about the two codex names: a five-hour secondary window is a rollover nobody
// can lose, so it is not a floor however blown it is. A fix that admitted both
// codex names by name would pass the test above and fail this one.
func TestABlownFiveHourCodexWindowIsNotAFloor(t *testing.T) {
	pct, at := 100.0, now.Add(2*time.Hour)
	s := &usage.Snapshot{CodexSecondary: usage.NewWindowWithLength(&pct, &at, 5*time.Hour)}

	h := HeadroomOf(s, thr())
	if h.HasFloor {
		t.Fatalf("Floor = %q; a five-hour window rolls over on its own and is not quota anyone can lose",
			h.Floor)
	}
}

// The second site. weeklyResetOf is what consume-first spends against, so a
// thirty-day window that is not weekly leaves the pass with no reset to aim at
// and the perishable quota goes unspent.
func TestWeeklyResetOfTakesTheThirtyDayCodexReset(t *testing.T) {
	s := codexPrimarySnap(40, 20*24*time.Hour, 30*24*time.Hour)

	got := weeklyResetOf(s, opts().Model, thr())
	if !got.ok {
		t.Fatal("weeklyResetOf() reported no reset; the thirty-day codex window carries one")
	}
	if want := now.Add(20 * 24 * time.Hour); !got.at.Equal(want) {
		t.Errorf("weeklyResetOf() = %v, want %v", got.at, want)
	}
}

// Every Claude window reports no length of its own, so both sites fall back to
// the name and answer exactly as they did. This is the regression the two edits
// are most likely to take out, and it is the reason the fallback exists.
func TestTheClaudeWindowsStillAnswerOffTheirNames(t *testing.T) {
	s := snap(win(10, time.Hour), win(85, 48*time.Hour))

	h := HeadroomOf(s, thr())
	if !h.HasFloor || h.Floor != usage.WindowSevenDay {
		t.Fatalf("Floor = (%q, %v), want (seven_day, true)", h.Floor, h.HasFloor)
	}
	got := weeklyResetOf(s, opts().Model, thr())
	if want := now.Add(48 * time.Hour); !got.ok || !got.at.Equal(want) {
		t.Fatalf("weeklyResetOf() = (%v, %v), want (%v, true)", got.at, got.ok, want)
	}
}
