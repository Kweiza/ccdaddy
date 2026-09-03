package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// `ccdad list` has no window column at any width, so on a row whose LEFT and
// RESETS IN describe two windows nothing on the row names either. This is the
// live shape that made it the normal case: an all-model week with a fifth left
// beside a model-scoped cap with nothing.
func TestListNamesBothWindowsWhenTheRowsFiguresComeFromTwo(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "split@example.com")
	at := statusNow.Add(40 * time.Hour)
	gone := 100.0
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-30 * time.Second),
		Snapshot: &usage.Snapshot{
			FiveHour: window(0, statusNow.Add(3*time.Hour)),
			SevenDay: window(80, at),
			Limits: []usage.Limit{usage.LimitFor(usage.LimitInput{
				Kind: "weekly_scoped", Model: "Fable", Percent: &gone, ResetsAt: &at,
			})},
		},
	})

	code, stdout, _, top := runRoot(t, "list")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 (%s)\n%s", code, top, stdout)
	}
	for _, want := range []string{"weekly_scoped:model:Fable", "spent", "seven_day"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the listing does not mention %q; a reader cannot tell which window either figure is about:\n%s", want, stdout)
		}
	}
}

// And it stays quiet on a row where one window answers both figures, so the
// note cannot be produced by printing it unconditionally.
func TestListSaysNothingExtraWhenOneWindowAnswersBothFigures(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "plain@example.com")
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-30 * time.Second),
		Snapshot: &usage.Snapshot{
			FiveHour: window(10, statusNow.Add(3*time.Hour)),
			SevenDay: window(20, statusNow.Add(40*time.Hour)),
		},
	})

	_, stdout, _, _ := runRoot(t, "list")
	if strings.Contains(stdout, "left on") {
		t.Errorf("the listing carries a split note on a row with no split:\n%s", stdout)
	}
}
