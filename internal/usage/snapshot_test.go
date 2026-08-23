package usage

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// realBody is a full usage response in the shape Claude Code 2.1.239 parses.
// Every field the bundle's zod object names is present — all eight top-level
// keys, cinder_cove among them, and extra_usage's own disabled_reason — plus
// one unknown key per object so the passthrough tolerance is exercised.
const realBody = `{
  "five_hour":            {"utilization": 92.5, "resets_at": "2026-08-22T09:00:00.000Z"},
  "seven_day":            {"utilization": 41,   "resets_at": "2026-08-27T00:00:00Z", "surprise": 1},
  "seven_day_oauth_apps": {"utilization": null, "resets_at": null},
  "seven_day_opus":       {"utilization": 0,    "resets_at": "2026-08-27T00:00:00Z"},
  "seven_day_sonnet":     {"utilization": 3.25, "resets_at": "2026-08-27T00:00:00Z"},
  "cinder_cove":          {"utilization": 10,   "resets_at": "2026-09-30T00:00:00Z"},
  "extra_usage": {"is_enabled": true, "monthly_limit": 15000, "used_credits": 1250,
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

// The exact cswap bug: a window that cannot be read is not an empty account,
// and a missing reset is not "resets now".
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
	w := WindowFromHeader(ptr(0.925), ptrI(1787389200))

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
	// 15000 and 1250 on the wire are $150.00 and $12.50: the endpoint reports
	// money in the currency's minor unit, and max_auto_spend is in dollars.
	if v, ok := e.MonthlyLimit(); !ok || v != 150 {
		t.Errorf("MonthlyLimit() = %v, %v; want 150 dollars from 15000 cents", v, ok)
	}
	if v, ok := e.UsedCredits(); !ok || v != 12.5 {
		t.Errorf("UsedCredits() = %v, %v; want 12.50 dollars from 1250 cents", v, ok)
	}
	if v, ok := e.Percent(); !ok || v != 8.33 {
		t.Errorf("Percent() = %v, %v; want 8.33", v, ok)
	}
	if e.Currency != "USD" {
		t.Errorf("Currency = %q, want %q", e.Currency, "USD")
	}
}

// The credit gate branches on `s.Used == nil` and must never see a zero
// standing in for a figure the wire did not send: fail closed on money.
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
	if first.Kind != "weekly_scoped" || first.Group != "model" {
		t.Errorf("Limits[0] = %+v; want kind weekly_scoped, group model", first)
	}
	if pct, ok := first.Percent(); !ok || pct != 12.5 {
		t.Errorf("Limits[0].Percent() = %v, %v; want 12.5, true", pct, ok)
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

// Classification asks exactly one thing of a snapshot: did the account report
// the windows a subscription is metered by?
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

func ptr(v float64) *float64 { return &v }
func ptrI(v int64) *int64    { return &v }

// A header that was not sent is not a header that said zero. Claude Code records
// a window only when BOTH halves are present, and half a pair here would mean a
// reset at the 1970 epoch — which reads as "already recovered".
func TestWindowFromHeaderNeedsBothHalves(t *testing.T) {
	cases := []struct {
		name     string
		fraction *float64
		reset    *int64
	}{
		{"no utilization header", nil, ptrI(1787389200)},
		{"no reset header", ptr(0.9), nil},
		{"neither", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := WindowFromHeader(tc.fraction, tc.reset)
			if w.Present {
				t.Error("Present = true; half a header pair is not a window")
			}
			if _, ok := w.Reset(); ok {
				t.Error("Reset() reported a value built from a header that was never sent")
			}
			if _, ok := w.Percent(); ok {
				t.Error("Percent() reported a value built from a header that was never sent")
			}
		})
	}
}

// Number("nonsense") is NaN, which is why Claude Code's own consumer re-checks
// Number.isFinite on this exact path.
func TestWindowFromHeaderRefusesANonFiniteFraction(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if w := WindowFromHeader(&v, ptrI(1787389200)); w.Present {
			t.Errorf("fraction %v: Present = true", v)
		}
	}
}

// A non-finite value is not a reading, wherever it came from. Reporting one as
// KNOWN is worse than reporting it unknown: every NaN comparison is false, so it
// loses no comparison in the ranking and can hold first place.
func TestPercentRefusesANonFiniteValue(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		w := NewWindow(&v, nil)
		if got, ok := w.Percent(); ok {
			t.Errorf("NewWindow(%v).Percent() = %v, ok = true", v, got)
		}
	}
}

// THE OTHER 100x TRAP. extra_usage's two money figures arrive in the currency's
// MINOR unit, while max_auto_spend is written in the major one.
func TestExtraUsageConvertsMoneyOutOfMinorUnits(t *testing.T) {
	cases := []struct {
		name      string
		currency  string
		wireLimit float64
		wireUsed  float64
		wantLimit float64
		wantUsed  float64
	}{
		{"USD", "USD", 15000, 1250, 150, 12.5},
		{"lowercase and padded", " usd ", 5000, 60, 50, 0.6},
		// The zero-decimal case is the one that proves the normalization: a
		// currency compared verbatim would divide " jpy " by 100.
		{"lowercase zero-decimal", " jpy ", 15000, 1250, 15000, 1250},
		// JPY, KRW and VND have no minor unit, so their amounts are not divided.
		// This is Claude Code's own list, consulted before its formatter's /100.
		{"JPY", "JPY", 15000, 1250, 15000, 1250},
		{"KRW", "KRW", 20000, 500, 20000, 500},
		{"VND", "VND", 20000, 500, 20000, 500},
		// An unreported currency is a two-decimal one, as Claude Code assumes.
		{"absent", "", 15000, 1250, 150, 12.5},
		{"unrecognized", "XYZ", 15000, 1250, 150, 12.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := ExtraUsageFor(ExtraUsageInput{State: ExtraUsageEnabled, Currency: tc.currency, MonthlyLimit: &tc.wireLimit, UsedCredits: &tc.wireUsed})
			if v, ok := e.MonthlyLimit(); !ok || v != tc.wantLimit {
				t.Errorf("MonthlyLimit() = %v, %v; want %v", v, ok, tc.wantLimit)
			}
			if v, ok := e.UsedCredits(); !ok || v != tc.wantUsed {
				t.Errorf("UsedCredits() = %v, %v; want %v", v, ok, tc.wantUsed)
			}
		})
	}
}

// extra_usage.utilization is already a percent on the wire, like every other
// utilization in this schema, and must not be dragged through the money
// conversion.
// The constructor has to be able to express every field, including the one only
// a parsed response used to be able to set.
func TestExtraUsageForCarriesEveryField(t *testing.T) {
	e := ExtraUsageFor(ExtraUsageInput{
		State:          ExtraUsageBlocked,
		DisabledReason: "out_of_credits",
		Currency:       "USD",
		MonthlyLimit:   ptr(15000),
		UsedCredits:    ptr(1250),
		Utilization:    ptr(8.33),
	})

	if !e.Present || e.State != ExtraUsageBlocked || e.DisabledReason != "out_of_credits" || e.Currency != "USD" {
		t.Errorf("ExtraUsageFor() = %+v", e)
	}
	if v, ok := e.Percent(); !ok || v != 8.33 {
		t.Errorf("Percent() = %v, %v; want 8.33 — a field the constructor cannot set is a field nothing downstream can test against", v, ok)
	}
	if v, ok := e.MonthlyLimit(); !ok || v != 150 {
		t.Errorf("MonthlyLimit() = %v, %v; want 150", v, ok)
	}
	if v, ok := e.UsedCredits(); !ok || v != 12.5 {
		t.Errorf("UsedCredits() = %v, %v; want 12.5", v, ok)
	}
}

func TestExtraUsagePercentIsNotConverted(t *testing.T) {
	s := mustParse(t, `{"extra_usage": {"is_enabled": true, "monthly_limit": 15000,
	                                    "used_credits": 1250, "utilization": 8.33,
	                                    "currency": "USD"}}`)
	if v, ok := s.ExtraUsage.Percent(); !ok || v != 8.33 {
		t.Errorf("Percent() = %v, %v; want 8.33", v, ok)
	}
}

// The disk keeps the WIRE amount, so a cached reading and a live one are the
// same document and the conversion happens in exactly one place.
func TestExtraUsageRoundTripsTheWireAmountNotTheConvertedOne(t *testing.T) {
	s := mustParse(t, realBody)

	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ExtraUsage struct {
			MonthlyLimit *float64 `json:"monthly_limit"`
			UsedCredits  *float64 `json:"used_credits"`
		} `json:"extra_usage"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExtraUsage.MonthlyLimit == nil || *got.ExtraUsage.MonthlyLimit != 15000 {
		t.Errorf("monthly_limit = %v, want the wire's 15000", got.ExtraUsage.MonthlyLimit)
	}
	if got.ExtraUsage.UsedCredits == nil || *got.ExtraUsage.UsedCredits != 1250 {
		t.Errorf("used_credits = %v, want the wire's 1250", got.ExtraUsage.UsedCredits)
	}
}

// A non-finite figure is not money, and the money path is the one place a NaN
// getting through actually spends something.
func TestExtraUsageMoneyRefusesNonFiniteFigures(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		e := ExtraUsageFor(ExtraUsageInput{State: ExtraUsageEnabled, Currency: "USD", MonthlyLimit: &v, UsedCredits: &v, Utilization: &v})
		if got, ok := e.UsedCredits(); ok {
			t.Errorf("UsedCredits() = %v, ok = true for a wire value of %v", got, v)
		}
		if got, ok := e.MonthlyLimit(); ok {
			t.Errorf("MonthlyLimit() = %v, ok = true for a wire value of %v", got, v)
		}
		if got, ok := e.Percent(); ok {
			t.Errorf("Percent() = %v, ok = true for a wire value of %v", got, v)
		}
	}
}

// ---- limits[] as windows ----------------------------------------------------

func scopedNames(ws []ScopedWindow) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, string(w.Name))
	}
	return out
}

// The whole point of ScopedWindows: a limits[] entry is a rate-limit window with
// a scope, and it carries the same two quantities the fixed six do.
func TestScopedWindowsReadsTheLimitsEntries(t *testing.T) {
	ws := mustParse(t, realBody).ScopedWindows()

	if len(ws) != 2 {
		t.Fatalf("ScopedWindows() = %v, want the two limits[] entries", scopedNames(ws))
	}

	model := ws[0]
	if model.Model != "Fable" || model.Surface != "" {
		t.Errorf("ScopedWindows()[0] scope = model %q surface %q, want model Fable", model.Model, model.Surface)
	}
	if pct, ok := model.Percent(); !ok || pct != 12.5 {
		t.Errorf("ScopedWindows()[0].Percent() = %v, %v; want 12.5, true — percent IS the window's utilization", pct, ok)
	}
	if at, ok := model.Reset(); !ok || !at.Equal(mustTime(t, "2026-08-27T00:00:00Z")) {
		t.Errorf("ScopedWindows()[0].Reset() = %v, %v", at, ok)
	}
	if !model.Present {
		t.Error("ScopedWindows()[0].Present = false; an entry that arrived is a window that is there")
	}

	surface := ws[1]
	if surface.Surface != "Cowork" || surface.Model != "" {
		t.Errorf("ScopedWindows()[1] scope = model %q surface %q, want surface Cowork", surface.Model, surface.Surface)
	}
	if at, ok := surface.Reset(); ok {
		t.Errorf("ScopedWindows()[1].Reset() = %s, ok = true; a null resets_at is unknown", at)
	}
}

// The scope object is nullable and BOTH halves are optional, so an entry naming
// neither is legal wire. It must not panic, and it must not become a window
// under an empty name — which would then be indistinguishable from any other
// unnamed one.
func TestScopedWindowsDropsAnEntryWithNoScopeName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope string
	}{
		{"no scope key at all", ``},
		{"scope null", `, "scope": null`},
		{"scope with neither half", `, "scope": {}`},
		{"scope with both halves null", `, "scope": {"model": null, "surface": null}`},
		{"a display name that is empty", `, "scope": {"model": {"display_name": ""}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"limits": [{"kind": "weekly_scoped", "group": "model", "percent": 99` + tc.scope + `}]}`
			s := mustParse(t, body)
			if len(s.Limits) != 1 {
				t.Fatalf("the entry should still parse; Limits = %+v", s.Limits)
			}
			if ws := s.ScopedWindows(); len(ws) != 0 {
				t.Errorf("ScopedWindows() = %v, want none — an unattributable cap is not a window", scopedNames(ws))
			}
		})
	}
}

// weekly_scoped is the one kind Claude Code reads as a rate-limit window. An
// entry of some other kind may be a one-time grant whose resets_at is an EXPIRY,
// and binding one is the cinder_cove bug in a new costume.
func TestScopedWindowsIgnoresAnUnknownKind(t *testing.T) {
	s := mustParse(t, `{"limits": [
	  {"kind": "one_time_grant", "group": "model", "percent": 99, "resets_at": "2026-12-31T00:00:00Z",
	   "scope": {"model": {"display_name": "Opus 4.5"}}},
	  {"kind": "weekly_scoped", "group": "model", "percent": 10, "resets_at": null,
	   "scope": {"model": {"display_name": "Opus 4.5"}}}]}`)

	ws := s.ScopedWindows()
	if len(ws) != 1 {
		t.Fatalf("ScopedWindows() = %v, want only the weekly_scoped entry", scopedNames(ws))
	}
	if pct, _ := ws[0].Percent(); pct != 10 {
		t.Errorf("the surviving window reports %v%%, so the one_time_grant entry is the one that was kept", pct)
	}
}

// A model and a surface can share a display name. They are two different caps,
// and a caller that looks a binding window back up BY NAME has to find the one
// that bound.
func TestScopedWindowNamesSeparateAModelFromASurface(t *testing.T) {
	s := mustParse(t, `{"limits": [
	  {"kind": "weekly_scoped", "group": "model",   "percent": 10, "resets_at": null,
	   "scope": {"model":   {"display_name": "Cowork"}}},
	  {"kind": "weekly_scoped", "group": "surface", "percent": 20, "resets_at": null,
	   "scope": {"surface": {"display_name": "Cowork"}}}]}`)

	ws := s.ScopedWindows()
	if len(ws) != 2 {
		t.Fatalf("ScopedWindows() = %v, want both", scopedNames(ws))
	}
	if ws[0].Name == ws[1].Name {
		t.Fatalf("both windows are named %q; a model and a surface sharing a display name would collide", ws[0].Name)
	}
	for _, w := range ws {
		if !w.Name.Scoped() {
			t.Errorf("Name %q does not report as scoped", w.Name)
		}
	}
	for _, n := range []WindowName{WindowFiveHour, WindowSevenDayOpus, WindowCinderCove} {
		if n.Scoped() {
			t.Errorf("the fixed window %q reports as scoped", n)
		}
	}
}

// The schema writes percent as a non-null number, so this is drift — and it is
// drift in the one direction that hides a spent account: a missing key
// unmarshals to 0 in Go, which reads as a window with everything left.
func TestLimitPercentIsUnknownRatherThanZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"the key is missing", `{"limits": [{"kind": "weekly_scoped", "group": "model",
		  "resets_at": null, "scope": {"model": {"display_name": "Fable"}}}]}`},
		{"the key is null", `{"limits": [{"kind": "weekly_scoped", "group": "model", "percent": null,
		  "resets_at": null, "scope": {"model": {"display_name": "Fable"}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustParse(t, tc.body)
			if len(s.Limits) != 1 {
				t.Fatalf("Limits = %+v, want the one entry", s.Limits)
			}
			if pct, ok := s.Limits[0].Percent(); ok {
				t.Errorf("Percent() = %v, true; an unread percent must not read as %v%% used", pct, pct)
			}
			ws := s.ScopedWindows()
			if len(ws) != 1 {
				t.Fatalf("ScopedWindows() = %v; the entry is still a window that exists", scopedNames(ws))
			}
			if _, ok := ws[0].Percent(); ok {
				t.Error("the window reports a known utilization built out of nothing")
			}
		})
	}
}

// The same guard Window.Percent carries, and for the same reason: a NaN reported
// as KNOWN loses no comparison in the ranking and can hold first place.
func TestLimitPercentRefusesANonFiniteValue(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		l := LimitFor(LimitInput{Kind: "weekly_scoped", Model: "Fable", Percent: &v})
		if pct, ok := l.Percent(); ok {
			t.Errorf("LimitFor(%v).Percent() = %v, true", v, pct)
		}
	}
}

func TestLimitForRoundTripsThroughTheCodec(t *testing.T) {
	pct, at := 42.5, mustTime(t, "2026-08-27T00:00:00Z")
	in := &Snapshot{Limits: []Limit{
		LimitFor(LimitInput{Kind: "weekly_scoped", Group: "model", Model: "Fable", Percent: &pct, ResetsAt: &at}),
		LimitFor(LimitInput{Kind: "weekly_scoped", Group: "surface", Surface: "Cowork"}),
	}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Snapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if len(out.Limits) != 2 {
		t.Fatalf("Limits = %+v, want 2\n%s", out.Limits, raw)
	}
	if got, ok := out.Limits[0].Percent(); !ok || got != pct {
		t.Errorf("Limits[0].Percent() = %v, %v; want %v, true", got, ok, pct)
	}
	if got, ok := out.Limits[0].Reset(); !ok || !got.Equal(at) {
		t.Errorf("Limits[0].Reset() = %v, %v", got, ok)
	}
	// The unknown one has to survive as unknown, not as a zero.
	if got, ok := out.Limits[1].Percent(); ok {
		t.Errorf("Limits[1].Percent() = %v, true; an unknown percent came back known\n%s", got, raw)
	}
	if out.Limits[1].SurfaceDisplayName != "Cowork" {
		t.Errorf("Limits[1].SurfaceDisplayName = %q", out.Limits[1].SurfaceDisplayName)
	}
}

func TestAllWindowsIsTheFixedFivePlusTheScopedOnes(t *testing.T) {
	s := mustParse(t, realBody)

	all := s.AllWindows()
	if len(all) != 7 {
		t.Fatalf("AllWindows() has %d windows, want 5 fixed + 2 scoped", len(all))
	}
	for i, w := range s.RateLimitWindows() {
		if all[i].Name != w.Name {
			t.Fatalf("AllWindows()[%d] = %q, want the fixed windows first in schema order", i, all[i].Name)
		}
	}
	if !all[5].Name.Scoped() || !all[6].Name.Scoped() {
		t.Errorf("AllWindows() tail = %q, %q; want the scoped ones", all[5].Name, all[6].Name)
	}
	// cinder_cove is a one-time grant, not a window to rank on. It stays out of
	// the widened set for the same reason it stays out of the narrow one.
	for _, w := range all {
		if w.Name == WindowCinderCove {
			t.Error("AllWindows() carries cinder_cove")
		}
	}

	if got := (&Snapshot{}).AllWindows(); len(got) != 5 {
		t.Errorf("a snapshot with no limits has %d windows, want the fixed five", len(got))
	}
	var nilSnap *Snapshot
	if got := nilSnap.AllWindows(); got != nil {
		t.Errorf("nil.AllWindows() = %v, want nil", got)
	}
	if got := nilSnap.ScopedWindows(); got != nil {
		t.Errorf("nil.ScopedWindows() = %v, want nil", got)
	}
}

// Claude Code's own filter requires scope.model, so an entry carrying both is a
// model window that also names where it applies.
func TestScopedWindowPrefersTheModelWhenBothScopesAreSet(t *testing.T) {
	s := mustParse(t, `{"limits": [{"kind": "weekly_scoped", "group": "model", "percent": 50, "resets_at": null,
	  "scope": {"model": {"display_name": "Opus 4.5"}, "surface": {"display_name": "Cowork"}}}]}`)

	ws := s.ScopedWindows()
	if len(ws) != 1 {
		t.Fatalf("ScopedWindows() = %v", scopedNames(ws))
	}
	if ws[0].Model != "Opus 4.5" || ws[0].Surface != "Cowork" {
		t.Errorf("scope = model %q surface %q; both halves are kept", ws[0].Model, ws[0].Surface)
	}
	if name := string(ws[0].Name); !strings.Contains(name, "Opus 4.5") {
		t.Errorf("Name = %q, want it named for the model", name)
	}
}
