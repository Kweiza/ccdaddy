package daemon

import (
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// TestTheSwitchLogLineSaysWhyAndNotOnlyWhat exists because a real incident was
// undiagnosable after the fact for exactly this reason.
//
// A daemon alternated between two accounts every 121 seconds for twenty-five
// minutes. 121 s is HoverCooldown plus one tick, which says the ranking wanted
// to move on every single evaluation — but the log recorded only "switched to
// X", so WHICH margin kept clearing, and on what numbers, was not recoverable.
// The readings behind it aged out of the series before anyone looked.
//
// Everything needed was one line away in the same scope: the plan carries the
// reason and the binding window's slack against the threshold it was measured
// on, and the evaluation carries the account being left. A log line that omits
// them turns a five-minute diagnosis into an unanswerable one.
func TestTheSwitchLogLineSaysWhyAndNotOnlyWhat(t *testing.T) {
	ev := switcher.Evaluation{
		Decided: true,
		Live:    store.Account{Email: "from@example.com"},
		Plan: strategy.Plan{
			Action: strategy.ActionSwitch,
			Reason: strategy.ReasonBetterTarget,
			Target: strategy.Ranked{
				UUID: "u-2",
				Headroom: strategy.Headroom{
					Known: true, Binding: usage.WindowSevenDay,
					Pct: 42, Slack: 7.5, Threshold: 64,
				},
			},
		},
	}
	line := switchLogLine(ev, store.Account{Email: "to@example.com"})

	for _, want := range []string{"to@example.com", "from@example.com"} {
		if !strings.Contains(line, want) {
			t.Errorf("line = %q; it must name both ends of the move (%q)", line, want)
		}
	}
	if !strings.Contains(line, strategy.ReasonBetterTarget.String()) {
		t.Errorf("line = %q; it must carry the reason the ranking gave", line)
	}
	// The numbers are the half that makes a repeat diagnosable: a flap is a
	// margin that keeps clearing, and only slack against its threshold says
	// whether it cleared by a hair or by a mile.
	for _, want := range []string{"seven_day", "7.5", "64"} {
		if !strings.Contains(line, want) {
			t.Errorf("line = %q; it must carry %q — the figure the margin was judged on", line, want)
		}
	}
}

// TestTheSwitchLogLineSurvivesAnUnreadableHeadroom is the guard. A credit seat
// binds on a window AllWindows does not carry, and a switch made with no
// baseline has no live account to name. Neither may turn a log line into a row
// of zeroes that reads like a measurement.
func TestTheSwitchLogLineSurvivesAnUnreadableHeadroom(t *testing.T) {
	ev := switcher.Evaluation{
		Decided: true,
		Plan: strategy.Plan{
			Action: strategy.ActionSwitch,
			Reason: strategy.ReasonBetterTarget,
			Target: strategy.Ranked{UUID: "u-2"},
		},
	}
	line := switchLogLine(ev, store.Account{Email: "to@example.com"})

	if strings.Contains(line, "slack=0") || strings.Contains(line, "thr=0") {
		t.Errorf("line = %q; an unknown headroom must not render as zero", line)
	}
	if !strings.Contains(line, "to@example.com") {
		t.Errorf("line = %q; it must still name the target", line)
	}
}
