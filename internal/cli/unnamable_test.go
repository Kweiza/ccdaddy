package cli

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// A weekly_scoped entry the wire gave no handle for is dropped everywhere: it is
// not in ScopedWindows, not in UnknownScopeWindows, and therefore not in the
// `windows` object. Dropping it is right — there is no name a threshold could be
// set on. Dropping it SILENTLY is not: the payload would then describe an
// account as having quota it does not have, and nothing anywhere would say a cap
// had been discarded.
func TestTheStatusPayloadReportsAWeeklyCapItCouldNotName(t *testing.T) {
	body := `{"five_hour": {"utilization": 10, "resets_at": null},
	  "limits": [{"kind": "weekly_scoped", "group": "region", "percent": 90, "resets_at": null,
	  "scope": {"region": {"id": "eu-west-1"}}}]}`
	snap, err := usage.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	row := view.Row{HasEntry: true, Entry: usage.Entry{Snapshot: snap, FetchedAt: time.Unix(0, 0).UTC()}}

	out := usageJSON(row, time.Unix(0, 0).UTC())
	if got, ok := out["unnamableWeeklyCaps"]; !ok || got != 1 {
		t.Errorf("unnamableWeeklyCaps = %v, %v; want 1, true — a cap that vanishes with nothing saying so is quota the ranking spends without knowing it exists", got, ok)
	}
	if w, ok := out["windows"].(map[string]any); ok {
		if _, listed := w["weekly_scoped:region:"]; listed {
			t.Error("windows carries a bare-prefix name; a window with no display half is a key no threshold can be set on")
		}
	}
}

// The field is absent, not zero, on an ordinary reading — an always-present
// counter that always reads 0 is noise in every payload a script parses.
func TestAnOrdinaryReadingCarriesNoUnnamableCount(t *testing.T) {
	body := `{"five_hour": {"utilization": 10, "resets_at": null},
	  "limits": [{"kind": "weekly_scoped", "group": "model", "percent": 20, "resets_at": null,
	  "scope": {"model": {"display_name": "Fable"}}}]}`
	snap, err := usage.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	row := view.Row{HasEntry: true, Entry: usage.Entry{Snapshot: snap, FetchedAt: time.Unix(0, 0).UTC()}}

	if _, ok := usageJSON(row, time.Unix(0, 0).UTC())["unnamableWeeklyCaps"]; ok {
		t.Error("unnamableWeeklyCaps is present on a reading with nothing to report")
	}
}
