package usage

import (
	"errors"
	"math"
	"strconv"
	"testing"
	"time"
)

// realBody is a full usage response in the shape Claude Code 2.1.239 parses.
// Every field the bundle's zod object names is present, including the two the
// spec's four-field summary omits (cinder_cove and extra_usage.disabled_reason),
// plus one unknown key per object so the passthrough tolerance is exercised.
const realBody = `{
  "five_hour":            {"utilization": 92.5, "resets_at": "2026-08-22T09:00:00.000Z"},
  "seven_day":            {"utilization": 41,   "resets_at": "2026-08-27T00:00:00Z", "surprise": 1},
  "seven_day_oauth_apps": {"utilization": null, "resets_at": null},
  "seven_day_opus":       {"utilization": 0,    "resets_at": "2026-08-27T00:00:00Z"},
  "seven_day_sonnet":     {"utilization": 3.25, "resets_at": "2026-08-27T00:00:00Z"},
  "cinder_cove":          {"utilization": 10,   "resets_at": "2026-09-30T00:00:00Z"},
  "extra_usage": {"is_enabled": true, "monthly_limit": 150, "used_credits": 12.5,
                  "utilization": 8.33, "currency": "USD", "disabled_reason": null},
  "limits": [
    {"kind": "weekly_scoped", "group": "model", "percent": 12.5,
     "resets_at": "2026-08-27T00:00:00Z",
     "scope": {"model": {"display_name": "Fable"}}},
    {"kind": "weekly_scoped", "group": "surface", "percent": 0,
     "resets_at": null,
     "scope": {"surface": {"display_name": "Cowork"}}}
  ],
  "unknown_future_window": {"utilization": 1, "resets_at": null}
}`

func mustParse(t *testing.T, body string) *Snapshot {
	t.Helper()
	s, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse() returned an unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("Parse() returned a nil snapshot and a nil error")
	}
	return s
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return v
}

func TestParseRoundTripsEveryWindow(t *testing.T) {
	s := mustParse(t, realBody)

	cases := []struct {
		name string
		got  Window
		pct  float64
		at   string
	}{
		{"five_hour", s.FiveHour, 92.5, "2026-08-22T09:00:00Z"},
		{"seven_day", s.SevenDay, 41, "2026-08-27T00:00:00Z"},
		{"seven_day_opus", s.SevenDayOpus, 0, "2026-08-27T00:00:00Z"},
		{"seven_day_sonnet", s.SevenDaySonnet, 3.25, "2026-08-27T00:00:00Z"},
		{"cinder_cove", s.CinderCove, 10, "2026-09-30T00:00:00Z"},
	}
	for _, tc := range cases {
		if !tc.got.Present {
			t.Errorf("%s: Present = false, want true", tc.name)
			continue
		}
		pct, ok := tc.got.Percent()
		if !ok {
			t.Errorf("%s: Percent() reported unknown, want %v", tc.name, tc.pct)
		} else if pct != tc.pct {
			t.Errorf("%s: Percent() = %v, want %v", tc.name, pct, tc.pct)
		}
		at, ok := tc.got.Reset()
		if !ok {
			t.Errorf("%s: Reset() reported unknown, want %s", tc.name, tc.at)
		} else if !at.Equal(mustTime(t, tc.at)) {
			t.Errorf("%s: Reset() = %s, want %s", tc.name, at, tc.at)
		}
	}
}

// A present window whose fields are both null is still evidence the account HAS
// that window — Claude Code's own seed check is a truthiness test on the object,
// not on its fields. Collapsing "present but empty" into "absent" would file a
// freshly reset account as having no subscription windows at all.
func TestParseKeepsAPresentButEmptyWindowPresent(t *testing.T) {
	s := mustParse(t, realBody)

	w := s.SevenDayOAuthApps
	if !w.Present {
		t.Fatal("seven_day_oauth_apps: Present = false, want true — the key was in the body")
	}
	if _, ok := w.Percent(); ok {
		t.Error("seven_day_oauth_apps: Percent() reported a value, want unknown")
	}
	if _, ok := w.Reset(); ok {
		t.Error("seven_day_oauth_apps: Reset() reported a value, want unknown")
	}
}

// The exact cswap bug spec §7.2 names: a window that cannot be read is not an
// empty account, and a missing reset is not "resets now".
func TestParseNeverReadsUnknownAsZero(t *testing.T) {
	s := mustParse(t, `{"five_hour": {"utilization": null, "resets_at": null}}`)

	if pct, ok := s.FiveHour.Percent(); ok {
		t.Errorf("Percent() = %v, ok = true; a null utilization must read as unknown, not %v", pct, pct)
	}
	if at, ok := s.FiveHour.Reset(); ok {
		t.Errorf("Reset() = %s, ok = true; a null resets_at must read as unknown, not a timestamp", at)
	}
}

// An absent window is unknown too, and must not masquerade as present.
func TestParseTreatsAnAbsentWindowAsAbsent(t *testing.T) {
	s := mustParse(t, `{"five_hour": {"utilization": 5, "resets_at": null}}`)

	if s.SevenDay.Present {
		t.Error("seven_day: Present = true, want false — the body never carried the key")
	}
	if _, ok := s.SevenDay.Percent(); ok {
		t.Error("seven_day: Percent() reported a value for a window that was not in the body")
	}
}

// A JSON null for the window itself is falsy in Claude Code's check, so it reads
// as absent here for the same reason.
func TestParseTreatsANullWindowAsAbsent(t *testing.T) {
	s := mustParse(t, `{"five_hour": null, "seven_day": {"utilization": 1, "resets_at": null}}`)

	if s.FiveHour.Present {
		t.Error("five_hour: Present = true, want false — the body carried an explicit null")
	}
}

// THE 100x TRAP. The body's utilization is a PERCENT and is stored verbatim.
// A value that happens to look like a fraction is still a percent: rescaling by
// magnitude is exactly the bug, so 0.92 must survive as 0.92 and 92 as 92.
func TestParseStoresUtilizationAsAPercentWithoutRescaling(t *testing.T) {
	for _, want := range []float64{0, 0.92, 1, 92, 92.5, 100} {
		s := mustParse(t, `{"five_hour": {"utilization": `+ftoa(want)+`, "resets_at": null}}`)
		got, ok := s.FiveHour.Percent()
		if !ok {
			t.Fatalf("utilization %v: Percent() reported unknown", want)
		}
		if got != want {
			t.Errorf("utilization %v: Percent() = %v — the body is already a percent, so it must not be scaled", want, got)
		}
	}
}

// The header form is the other representation and it is NOT what this endpoint
// returns: headers carry a 0-1 fraction with an epoch-second reset. The
// conversion lives at that boundary and is pinned here so the two forms can
// never be mixed silently.
func TestPercentFromHeaderFractionConvertsBothAxes(t *testing.T) {
	w := WindowFromHeader(0.925, 1787389200)

	pct, ok := w.Percent()
	if !ok || pct != 92.5 {
		t.Errorf("Percent() = %v, %v; want 92.5 — a 0-1 header fraction is a percent times 100", pct, ok)
	}
	at, ok := w.Reset()
	if !ok || !at.Equal(time.Unix(1787389200, 0).UTC()) {
		t.Errorf("Reset() = %v, %v; want the epoch second read as a time, not as an ISO string", at, ok)
	}
	if !w.Present {
		t.Error("Present = false, want true")
	}
}

// A resets_at that is not a timestamp is wire drift in one field. It reads as an
// unknown reset rather than failing the whole snapshot, because every other
// object in this schema is a passthrough and refusing the lot would throw away
// five good windows over one bad string.
func TestParseReadsAnUnparsableResetAsUnknown(t *testing.T) {
	s := mustParse(t, `{"five_hour": {"utilization": 5, "resets_at": "1787389200"}}`)

	if at, ok := s.FiveHour.Reset(); ok {
		t.Errorf("Reset() = %s; an epoch-second string is not an ISO-8601 reset and must read as unknown", at)
	}
	if pct, ok := s.FiveHour.Percent(); !ok || pct != 5 {
		t.Errorf("Percent() = %v, %v; the good half of the window must survive the bad half", pct, ok)
	}
}

func TestParseRoundTripsExtraUsage(t *testing.T) {
	s := mustParse(t, realBody)
	e := s.ExtraUsage

	if !e.Present {
		t.Fatal("ExtraUsage.Present = false, want true")
	}
	if e.State != ExtraUsageEnabled {
		t.Errorf("State = %v, want %v", e.State, ExtraUsageEnabled)
	}
	if v, ok := e.MonthlyLimit(); !ok || v != 150 {
		t.Errorf("MonthlyLimit() = %v, %v; want 150", v, ok)
	}
	if v, ok := e.UsedCredits(); !ok || v != 12.5 {
		t.Errorf("UsedCredits() = %v, %v; want 12.5", v, ok)
	}
	if v, ok := e.Percent(); !ok || v != 8.33 {
		t.Errorf("Percent() = %v, %v; want 8.33", v, ok)
	}
	if e.Currency != "USD" {
		t.Errorf("Currency = %q, want %q", e.Currency, "USD")
	}
}

// §7.3 branches on `s.Used == nil` and must never see a zero standing in for a
// figure the wire did not send: fail closed on money.
func TestParseKeepsUnreadableSpendUnknown(t *testing.T) {
	s := mustParse(t, `{"extra_usage": {"is_enabled": true, "monthly_limit": null,
	                                    "used_credits": null, "utilization": null}}`)
	e := s.ExtraUsage

	if v, ok := e.UsedCredits(); ok {
		t.Errorf("UsedCredits() = %v, ok = true; a null must not read as 0 spent", v)
	}
	if v, ok := e.MonthlyLimit(); ok {
		t.Errorf("MonthlyLimit() = %v, ok = true; a null limit means unlimited, not a cap of %v", v, v)
	}
}

// Claude Code reads extra_usage as four states, not two: is_enabled false with a
// disabled_reason is BLOCKED by org or seat policy, which is not the same as an
// account that simply has overage switched off.
func TestParseReadsTheFourExtraUsageStates(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ExtraUsageState
	}{
		{"enabled", `{"extra_usage": {"is_enabled": true, "monthly_limit": null, "used_credits": null, "utilization": null}}`, ExtraUsageEnabled},
		{"disabled", `{"extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null}}`, ExtraUsageDisabled},
		{"blocked", `{"extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null, "disabled_reason": "org_spend_cap_reached"}}`, ExtraUsageBlocked},
		{"absent", `{"five_hour": null}`, ExtraUsageUnknown},
		{"null", `{"extra_usage": null}`, ExtraUsageUnknown},
		// is_enabled is required by the schema, so an object without it is
		// drift. It must read as unknown — the side that does not spend — and
		// this case is the only one that reaches the state switch's default.
		{"present without is_enabled", `{"extra_usage": {"monthly_limit": 150, "used_credits": 1, "utilization": null}}`, ExtraUsageUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustParse(t, tc.body)
			if got := s.ExtraUsage.State; got != tc.want {
				t.Errorf("State = %v, want %v", got, tc.want)
			}
		})
	}
}

// Only three reasons mean "refused now, may come back": Claude Code's own
// predicate is `e==="org_level_disabled_until"||e==="org_spend_cap_reached"||
// e==="out_of_credits"`. Every other reason is a structural "this org does not
// do overage", which reads as disabled — the reason string is kept either way.
func TestParseReadsAStructuralReasonAsDisabledNotBlocked(t *testing.T) {
	s := mustParse(t, `{"extra_usage": {"is_enabled": false, "monthly_limit": null,
	                                    "used_credits": null, "utilization": null,
	                                    "disabled_reason": "seat_tier_level_disabled"}}`)

	if got := s.ExtraUsage.State; got != ExtraUsageDisabled {
		t.Errorf("State = %v, want %v — seat_tier_level_disabled is not one of the three transient refusals", got, ExtraUsageDisabled)
	}
	if got := s.ExtraUsage.DisabledReason; got != "seat_tier_level_disabled" {
		t.Errorf("DisabledReason = %q, want it kept verbatim", got)
	}
}

func TestParseKeepsTheBlockedReason(t *testing.T) {
	s := mustParse(t, `{"extra_usage": {"is_enabled": false, "monthly_limit": null,
	                                    "used_credits": null, "utilization": null,
	                                    "disabled_reason": "out_of_credits"}}`)
	if got := s.ExtraUsage.DisabledReason; got != "out_of_credits" {
		t.Errorf("DisabledReason = %q, want %q — the gate's notification names it", got, "out_of_credits")
	}
}

func TestParseRoundTripsLimits(t *testing.T) {
	s := mustParse(t, realBody)

	if len(s.Limits) != 2 {
		t.Fatalf("len(Limits) = %d, want 2", len(s.Limits))
	}
	first := s.Limits[0]
	if first.Kind != "weekly_scoped" || first.Group != "model" || first.Percent != 12.5 {
		t.Errorf("Limits[0] = %+v; want kind weekly_scoped, group model, percent 12.5", first)
	}
	if first.ModelDisplayName != "Fable" {
		t.Errorf("Limits[0].ModelDisplayName = %q, want %q", first.ModelDisplayName, "Fable")
	}
	if at, ok := first.Reset(); !ok || !at.Equal(mustTime(t, "2026-08-27T00:00:00Z")) {
		t.Errorf("Limits[0].Reset() = %v, %v", at, ok)
	}

	second := s.Limits[1]
	if second.SurfaceDisplayName != "Cowork" {
		t.Errorf("Limits[1].SurfaceDisplayName = %q, want %q", second.SurfaceDisplayName, "Cowork")
	}
	if at, ok := second.Reset(); ok {
		t.Errorf("Limits[1].Reset() = %s, ok = true; a null resets_at is unknown", at)
	}
}

// cinder_cove is a one-time "Claude Code and Cowork credit" whose resets_at is an
// EXPIRY, not a recurring reset. Ranking a one-time grant as a rate-limit window
// would have the engine wait for a reset that never comes, so the rate-limit
// iterator must not hand it out.
func TestRateLimitWindowsExcludesCinderCove(t *testing.T) {
	s := mustParse(t, realBody)

	names := map[WindowName]bool{}
	for _, w := range s.RateLimitWindows() {
		names[w.Name] = true
	}
	if names[WindowCinderCove] {
		t.Error("RateLimitWindows() handed out cinder_cove; it is a one-time credit, not a rate-limit window")
	}
	for _, want := range []WindowName{WindowFiveHour, WindowSevenDay, WindowSevenDayOAuthApps, WindowSevenDayOpus, WindowSevenDaySonnet} {
		if !names[want] {
			t.Errorf("RateLimitWindows() omitted %s", want)
		}
	}
}

// Classification (task 29) asks exactly one thing of a snapshot: did the account
// report the windows a subscription is metered by?
func TestHasSubscriptionWindows(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"five_hour only", `{"five_hour": {"utilization": null, "resets_at": null}}`, true},
		{"seven_day only", `{"seven_day": {"utilization": null, "resets_at": null}}`, true},
		{"credit account", `{"extra_usage": {"is_enabled": true, "monthly_limit": null, "used_credits": null, "utilization": null}}`, false},
		// A model-specific weekly cap is a plan limit: an account only has one
		// because a subscription gives it one. Reading this as "no subscription
		// evidence" would put an Opus-limited account on the money-spending
		// side of the credit gate.
		{"opus window alone", `{"seven_day_opus": {"utilization": 1, "resets_at": null}}`, true},
		{"sonnet window alone", `{"seven_day_sonnet": {"utilization": null, "resets_at": null}}`, true},
		// cinder_cove is a one-time credit grant, not a plan window.
		{"cinder_cove alone", `{"cinder_cove": {"utilization": 1, "resets_at": null}}`, false},
		{"nulls", `{"five_hour": null, "seven_day": null}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustParse(t, tc.body)
			if got := s.HasSubscriptionWindows(); got != tc.want {
				t.Errorf("HasSubscriptionWindows() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A 200 is not proof of a usage body. Claude Code treats an object carrying none
// of the eight known keys as an in-band error and falls back to its seed rather
// than reading six unknown windows out of it.
func TestParseRefusesABodyWithNoKnownField(t *testing.T) {
	for _, body := range []string{`{}`, `{"foo": 1}`, `{"detail": "nope"}`} {
		_, err := Parse([]byte(body))
		if !errors.Is(err, ErrNoUsageFields) {
			t.Errorf("Parse(%s) error = %v, want ErrNoUsageFields", body, err)
		}
	}
}

// A rate limit delivered inside a 200 body. It is a different condition from a
// fieldless body — the poll policy backs off for it — so it gets its own error.
func TestParseReportsAnInBandRateLimitEnvelope(t *testing.T) {
	body := `{"error": {"type": "rate_limit_error", "message": "slow down"}}`

	_, err := Parse([]byte(body))
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("Parse() error = %v, want ErrRateLimited", err)
	}
	if errors.Is(err, ErrNoUsageFields) {
		t.Error("an in-band rate limit must not also read as a fieldless body; the two drive different backoff")
	}
}

// Only rate_limit_error is the envelope; any other error object is just a body
// with no usage fields.
func TestParseReadsAnUnrelatedErrorObjectAsFieldless(t *testing.T) {
	_, err := Parse([]byte(`{"error": {"type": "invalid_request_error"}}`))
	if !errors.Is(err, ErrNoUsageFields) {
		t.Errorf("Parse() error = %v, want ErrNoUsageFields", err)
	}
	if errors.Is(err, ErrRateLimited) {
		t.Error("a non-rate-limit error object must not read as a rate limit")
	}
}

func TestParseRefusesANonObjectBody(t *testing.T) {
	for _, body := range []string{`[]`, `7`, `"five_hour"`, `null`} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("Parse(%s) returned no error; a usage body is a JSON object", body)
		}
	}
}

func TestParseRefusesInvalidJSON(t *testing.T) {
	if _, err := Parse([]byte(`{"five_hour":`)); err == nil {
		t.Error("Parse() accepted a truncated body")
	}
}

// Every object in the schema is a zod passthrough, so an unknown key is drift to
// tolerate, never a reason to refuse a response that is otherwise complete.
func TestParseToleratesUnknownKeys(t *testing.T) {
	s := mustParse(t, realBody)
	if pct, ok := s.FiveHour.Percent(); !ok || pct != 92.5 {
		t.Errorf("an unknown sibling key changed the parse: Percent() = %v, %v", pct, ok)
	}
}

// A NaN or an infinity cannot appear in valid JSON, but a percent that arrives
// outside 0-100 is wire drift the ranking axis would read as impossible
// headroom. It is kept verbatim rather than clamped, so callers can see it.
func TestParseKeepsAnOutOfRangePercentVerbatim(t *testing.T) {
	s := mustParse(t, `{"five_hour": {"utilization": 140, "resets_at": null}}`)
	pct, ok := s.FiveHour.Percent()
	if !ok || pct != 140 {
		t.Errorf("Percent() = %v, %v; want 140 kept as read", pct, ok)
	}
	if math.IsNaN(pct) {
		t.Error("Percent() returned NaN")
	}
}

// ftoa renders a float the way JSON does, so a table of numbers can be spliced
// into a body without a fixture file per value.
func ftoa(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// ParseExtraUsageState is what reads a persisted state back, so every name
// String() writes has to come back as the state it was written from.
func TestExtraUsageStateNamesRoundTrip(t *testing.T) {
	for _, want := range []ExtraUsageState{ExtraUsageUnknown, ExtraUsageEnabled, ExtraUsageDisabled, ExtraUsageBlocked} {
		if got := ParseExtraUsageState(want.String()); got != want {
			t.Errorf("ParseExtraUsageState(%q) = %v, want %v", want.String(), got, want)
		}
	}
}

// An unrecognized name reads as unknown, which is the side that does not spend.
func TestParseExtraUsageStateIsForwardCompatible(t *testing.T) {
	for _, name := range []string{"", "  ", "ENABLED_SOMEDAY", "credit", "yes"} {
		if got := ParseExtraUsageState(name); got != ExtraUsageUnknown {
			t.Errorf("ParseExtraUsageState(%q) = %v, want unknown", name, got)
		}
	}
}

// Case and surrounding space are tolerated, as ParseKind tolerates them.
func TestParseExtraUsageStateTolerAtesCaseAndSpace(t *testing.T) {
	if got := ParseExtraUsageState("  BLOCKED "); got != ExtraUsageBlocked {
		t.Errorf("ParseExtraUsageState = %v, want blocked", got)
	}
}
