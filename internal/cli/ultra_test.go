package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// seedWeekOnlySeat is a verified Ultra seat: five_hour null and the week measured
// flat, so the fleet holds and the summary line is the one under test.
func seedWeekOnlySeat(t *testing.T, uuid, email string) {
	t.Helper()
	seedAccountWithTier(t, uuid, email, runwayTier, runwayAddedAt)
	seedUsageEntry(t, uuid, usage.Entry{
		FetchedAt: statusNow,
		Snapshot:  &usage.Snapshot{SevenDay: window(10, runwayWeeklyReset)},
	})
	var samples []history.Sample
	for i := 4; i >= 0; i-- {
		samples = append(samples, history.Sample{
			At: statusNow.Add(-time.Duration(i) * 30 * time.Minute),
			Windows: map[usage.WindowName]history.Reading{
				usage.WindowSevenDay: {Pct: 10, Reset: runwayWeeklyReset},
			},
		})
	}
	seedHistory(t, uuid, samples...)
}

// The table, the summary line and the payload all have to agree that this fleet has
// one limit. Printed as a five-hour row of "-", or left in the payload with no
// `holds`, it reads as an axis whose evidence disagreed.
func TestRunwayOfAWeekOnlyFleetHasNoFiveHourAxisAnywhere(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedWeekOnlySeat(t, "uuid-a", "a@example.com")
	seedWeekOnlySeat(t, "uuid-b", "b@example.com")

	_, out, _, top := runRoot(t, "runway")
	if strings.Contains(out, "5-hour") {
		t.Errorf("runway (%s) printed a five-hour row:\n%s", top, out)
	}
	if !hasRow(squeezedLines(out), "7-day ") {
		t.Errorf("runway (%s) lost the weekly row:\n%s", top, out)
	}

	_, statusOut, _, _ := runRoot(t, "status")
	line := runwaySummaryLine(t, statusOut)
	if strings.Contains(line, "5h") || !strings.Contains(line, "holds at this rate") {
		t.Errorf("status summary = %q", line)
	}

	_, jsonOut, _, _ := runRoot(t, "runway", "--json")
	var payload struct {
		Forecast struct {
			Axes map[string]json.RawMessage `json:"axes"`
		} `json:"forecast"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &payload); err != nil {
		t.Fatalf("runway --json: %v\n%s", err, jsonOut)
	}
	axes := payload.Forecast.Axes
	if _, has := axes[axisKeyWeekly]; !has {
		t.Fatalf("payload lost the weekly axis: %s", jsonOut)
	}
	if _, has := axes[axisKeyFiveHour]; has {
		t.Errorf("payload carries a five_hour axis: %s", axes[axisKeyFiveHour])
	}
}

// A seat whose refresh token is dead is one no switch can reach, and it stays that
// way until someone logs in. Counting it reported "holds" over a fleet with one
// working seat.
func TestRunwayLeavesAQuarantinedAccountOutOfTheFleet(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)
	if err := strategy.WithState(5*time.Second, func(st *strategy.State) error {
		st.Quarantine("uuid-b", statusNow, time.Hour, "dead refresh token")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, out, _, top := runRoot(t, "runway")
	for _, want := range []string{
		"Fleet:   52 of 100 points left on the weekly axis",
		"1 not in rotation",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("runway (%s) carries no %q:\n%s", top, want, out)
		}
	}
}

// A quarantine that has run out holds nobody out: it is being retried.
func TestRunwayCountsAnAccountWhoseQuarantineHasLifted(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)
	if err := strategy.WithState(5*time.Second, func(st *strategy.State) error {
		st.Quarantine("uuid-b", statusNow.Add(-2*time.Hour), time.Hour, "dead refresh token")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, out, _, top := runRoot(t, "runway")
	if !strings.Contains(out, "of 200 points left") {
		t.Errorf("runway (%s) left a lifted quarantine out:\n%s", top, out)
	}
}

// The default target is a window the account has. A week-only seat has no five-hour
// window, so the old default found no reset, spent a turn waking nothing, and
// recorded a strike against a window that does not exist.
func TestAProbeAimsAtAWindowTheAccountHas(t *testing.T) {
	week := &usage.Snapshot{SevenDay: window(60, probeNow.Add(time.Hour))}
	both := &usage.Snapshot{FiveHour: unusedWindow(0), SevenDay: window(60, probeNow.Add(time.Hour))}
	for _, tc := range []struct {
		name  string
		model string
		s     *usage.Snapshot
		want  usage.WindowName
	}{
		{"week-only seat", "", week, usage.WindowSevenDay},
		{"seat with both", "", both, usage.WindowFiveHour},
		{"nobody has read it", "", nil, usage.WindowFiveHour},
		{"a reading that carried neither", "", &usage.Snapshot{}, usage.WindowFiveHour},
		{"an unknown model on a week-only seat", "gpt", week, usage.WindowSevenDay},
		{"a family with its own window", "opus", week, usage.WindowSevenDayOpus},
	} {
		if got := probeWindow(tc.model, tc.s); got != tc.want {
			t.Errorf("%s: probeWindow = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// End to end: nothing is spent on a week-only seat whose clock is already running.
func TestAProbeOfAWeekOnlySeatSpendsNothing(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedAccount(t, "u-1", "a@example.com")
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: probeNow.Add(-10 * time.Minute),
		Snapshot:  &usage.Snapshot{SevenDay: window(60, probeNow.Add(time.Hour))},
	})
	stub := stubClaude(t, ExitOK)

	code, _, errOut, _ := runRoot(t, "probe", "1")
	if code != ExitNothingToDo {
		t.Fatalf("code = %v, want ExitNothingToDo (%s)", code, errOut)
	}
	if !strings.Contains(errOut, "seven_day clock is already running") {
		t.Errorf("stderr = %q", errOut)
	}
	if len(stub.spec.Args) != 0 {
		t.Errorf("claude was started on a seat with nothing to wake: %v", stub.spec.Args)
	}
}
