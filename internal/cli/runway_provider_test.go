package cli

import (
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// A Codex account is not in the fleet a runway forecasts.
//
// Two reasons, and either alone is enough. Its percentage points are a
// different plan's points, so summing them with Claude's reports a fleet that
// does not exist. And the rotation cannot reach it from here at all: a Claude
// switch can never make a Codex account the live login, so its quota is quota
// this forecast's own eligibility rule says is unreachable.
func TestTheRunwayFleetHoldsNoCodexAccount(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "u-claude", "a@example.com")
	seedCodexAccount(t, "u-codex", "c@example.com")
	// A row is one line per READABLE account, and an account with no snapshot
	// is not readable: the forecast drops it before it looks at anything else.
	// So BOTH accounts get a reading, and the Codex one is then left out for
	// its provider rather than for being blank -- which is the difference
	// between this test proving the filter and proving nothing.
	for _, uuid := range []string{"u-claude", "u-codex"} {
		seedUsageEntry(t, uuid, usage.Entry{
			FetchedAt: statusNow,
			Snapshot: &usage.Snapshot{
				FiveHour: window(30, runwayFiveReset),
				SevenDay: window(40, runwayWeeklyReset),
			},
		})
	}

	cache, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	f, _ := fleetForecast(storeAccounts(t), cache, statusNow)
	for _, r := range f.Rows {
		if r.UUID == "u-codex" {
			t.Fatalf("the forecast measured a Codex account: %+v", r)
		}
	}
	if len(f.Rows) != 1 || f.Rows[0].UUID != "u-claude" {
		t.Fatalf("forecast rows = %+v, want only the Claude account", f.Rows)
	}
}

// The page says the Codex accounts were left out, once, on stderr. Silence
// would read as a fleet that is smaller than it is.
func TestRunwaySaysWhichAccountsAreNotForecast(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-claude", "a@example.com")
	seedCodexAccount(t, "u-codex", "c@example.com")

	_, _, errOut, _ := runRoot(t, "runway")
	if !strings.Contains(errOut, "not forecast") {
		t.Fatalf("runway said nothing about the Codex account:\n%s", errOut)
	}
	if !strings.Contains(errOut, "c@example.com") {
		t.Fatalf("the note does not name the account it left out:\n%s", errOut)
	}
	if strings.Count(errOut, "not forecast") != 1 {
		t.Fatalf("the note is printed more than once:\n%s", errOut)
	}
}

// A store with no Codex account says nothing, because there is nothing to say
// and a line about an absence on every run is noise.
func TestRunwaySaysNothingWhenThereAreNoCodexAccounts(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-claude", "a@example.com")

	_, _, errOut, _ := runRoot(t, "runway")
	if strings.Contains(errOut, "not forecast") {
		t.Fatalf("runway printed the Codex note with no Codex account:\n%s", errOut)
	}
}
