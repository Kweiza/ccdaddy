package switcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// seedWindows caches a reading with a week at weekPct, and a five-hour window at
// fivePct when short.
func seedWindows(t *testing.T, uuid string, short bool, fivePct, weekPct float64, weekIn time.Duration, now time.Time) {
	t.Helper()
	weekAt := now.Add(weekIn)
	snap := &usage.Snapshot{SevenDay: usage.NewWindow(&weekPct, &weekAt)}
	if short {
		fiveAt := now.Add(3 * time.Hour)
		snap.FiveHour = usage.NewWindow(&fivePct, &fiveAt)
	}
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(uuid, usage.Entry{Snapshot: snap, FetchedAt: now, NextPollAt: now.Add(3 * time.Minute)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The daemon's pass reads the work's rest off Claude Code's transcripts: in a
// short week, an hour with no transcript written takes the session off the
// live Ultra seat and back to the Max, and a write a minute ago keeps it there.
// The activity has no other way into the engine, so a pass that dropped it
// would keep the seat in every case and every other test would stay green.
func TestThePassReadsTheRestOffTheTranscripts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		written time.Duration
		action  strategy.Action
		reason  strategy.Reason
	}{
		{"resting", 2 * time.Hour, strategy.ActionSwitch, strategy.ReasonRestReturn},
		{"working", time.Minute, strategy.ActionStay, strategy.ReasonStayUntilLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seed(t, "u-max", "max@example.com")
			seed(t, "u-live", "live@example.com")
			seed(t, "u-spent", "spent@example.com")
			liveAs(t, "u-live")
			now := time.Now().UTC()
			seedWindows(t, "u-max", true, 10, 50, 4*24*time.Hour, now)
			seedWindows(t, "u-live", false, 0, 40, 2*24*time.Hour, now)
			seedWindows(t, "u-spent", false, 0, 99, 3*24*time.Hour, now)

			transcript := filepath.Join(mustPath(ccpath.ConfigHome()), "projects", "-p", "s.jsonl")
			if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			written := now.Add(-tc.written)
			if err := os.Chtimes(transcript, written, written); err != nil {
				t.Fatal(err)
			}

			hover := config.Defaults()
			hover.Hover = true
			ev, err := Evaluate(openStore(t), EvalOptions{Now: now,
				Config: func() (config.Config, error) { return hover, nil }})
			if err != nil {
				t.Fatal(err)
			}
			if ev.Plan.Action != tc.action || ev.Plan.Reason != tc.reason {
				t.Fatalf("Action = %v, Reason = %v; want %v, %v", ev.Plan.Action, ev.Plan.Reason, tc.action, tc.reason)
			}
			if tc.action == strategy.ActionSwitch && ev.Target.UUID != "u-max" {
				t.Fatalf("Target = %q, want u-max", ev.Target.UUID)
			}
		})
	}
}
