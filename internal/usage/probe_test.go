package usage

import (
	"errors"
	"testing"
	"time"
)

// The six-hour gate, and the two edges that decide whether it is a gate at all.
func TestMayProbeHoldsForSixHoursAfterAnyAttempt(t *testing.T) {
	now := mustTime(t, "2026-08-24T12:00:00Z")
	for _, tc := range []struct {
		name string
		last time.Time
		want bool
	}{
		{"never probed", time.Time{}, true},
		{"a moment ago", now.Add(-time.Second), false},
		{"just under six hours ago", now.Add(-ProbeRetryAfter + time.Second), false},
		{"exactly six hours ago", now.Add(-ProbeRetryAfter), true},
		{"stamped in the future by a clock that moved back", now.Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Entry{Probe: ProbeState{LastAttemptAt: tc.last}}
			if got := e.MayProbe(now); got != tc.want {
				t.Errorf("MayProbe = %v, want %v", got, tc.want)
			}
		})
	}
}

// A window that has never been used and a window that is simply not there answer
// the same, and neither answers "now". Reading a missing reset as the zero time
// would put every such rollover in 1970.
func TestResetForSeparatesAReportedResetFromNoneAtAll(t *testing.T) {
	at := mustTime(t, "2026-08-24T17:00:00Z")
	pct := 40.0
	s := &Snapshot{
		FiveHour: NewWindow(&pct, &at),
		SevenDay: NewWindow(&pct, nil),
	}
	if got, ok := s.ResetFor(WindowFiveHour); !ok || !got.Equal(at) {
		t.Errorf("ResetFor(five_hour) = %s, %v; want %s, true", got, ok, at)
	}
	if _, ok := s.ResetFor(WindowSevenDay); ok {
		t.Error("a window with no resets_at reported one")
	}
	if _, ok := s.ResetFor(WindowSevenDayOpus); ok {
		t.Error("a window the response never carried reported a reset")
	}
	var nilSnap *Snapshot
	if _, ok := nilSnap.ResetFor(WindowFiveHour); ok {
		t.Error("a reading that does not exist reported a reset")
	}
}

// The two things a stamp has to do at once: consume the six-hour budget, and
// push the poll that will read what the probe woke out to ProbePollDelay. It
// must not touch the reading that said a probe was needed.
func TestRecordProbeStampsTheAttemptAndHoldsTheNextPollOff(t *testing.T) {
	isolate(t)
	at := mustTime(t, "2026-08-24T12:00:00Z")
	pct := 0.0
	if err := WithCache(time.Second, func(c *Cache) error {
		c.Put("u-1", Entry{FetchedAt: at.Add(-time.Hour), Snapshot: &Snapshot{FiveHour: NewWindow(&pct, nil)}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := RecordProbe(time.Second, "u-1", at, errors.New("claude exited 1")); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.Get("u-1")
	if !ok {
		t.Fatal("RecordProbe dropped the entry it was stamping")
	}
	if !got.Probe.LastAttemptAt.Equal(at) {
		t.Errorf("LastAttemptAt = %s, want %s", got.Probe.LastAttemptAt, at)
	}
	if got.Probe.LastError == "" {
		t.Error("a failed probe left no record of itself, and a detached probe reports to nobody else")
	}
	if want := at.Add(ProbePollDelay); !got.NextPollAt.Equal(want) {
		t.Errorf("NextPollAt = %s, want %s — a poll now would spend the usage budget on top of the "+
			"inference budget the probe just spent", got.NextPollAt, want)
	}
	if got.Snapshot == nil {
		t.Error("RecordProbe erased the reading that said a probe was needed")
	}
}
