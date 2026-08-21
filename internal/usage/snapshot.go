// Package usage reads GET /api/oauth/usage and normalizes it into a shape the
// auto-switch engine can rank on.
//
// The one thing this package exists to get right is that every number it
// carries is TRI-STATE. A window that could not be read is not an empty
// account, and a reset that was not reported is not "resets now" (spec §7.2).
// cswap got this wrong once and a single expired token parked its engine on the
// account that reset last, so nothing here returns a bare float64.
//
// The other thing is units. The BODY reports utilization as a percent (0-100)
// with an ISO-8601 resets_at; the anthropic-ratelimit-unified-* response HEADERS
// report the same quantity as a fraction (0-1) with resets_at in epoch seconds.
// Claude Code converts the header form to the body form with `n.utilization*100`
// and `new Date(n.resets_at*1000).toISOString()` (function seedUtilization in
// the 2.1.239 bundle), and its own schema documents the body side as
// "Percentage of the window used, 0-100". This package stores the BODY form,
// and WindowFromHeader is the only place the other form is allowed to exist.
// Mixing the two makes a 92%-consumed account read as 0.92% and the engine
// never switches.
package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoUsageFields is a 200 whose body carries none of the eight keys a usage
// response is made of. Claude Code calls this an in-band error and falls back to
// its cached seed rather than reading six unknown windows out of it, because a
// body that parses is not a body that answered the question.
var ErrNoUsageFields = errors.New("the usage endpoint returned a body with no usage fields")

// ErrRateLimited is a rate limit delivered inside a 200, as
// {"error":{"type":"rate_limit_error"}}. It is separated from ErrNoUsageFields
// because the poll policy backs off for one and not the other.
var ErrRateLimited = errors.New("the usage endpoint reported a rate limit in the response body")

// WindowName identifies a window in the usage response.
type WindowName string

// The six windows the 2.1.239 schema names, in its own order.
const (
	WindowFiveHour          WindowName = "five_hour"
	WindowSevenDay          WindowName = "seven_day"
	WindowSevenDayOAuthApps WindowName = "seven_day_oauth_apps"
	WindowSevenDayOpus      WindowName = "seven_day_opus"
	WindowSevenDaySonnet    WindowName = "seven_day_sonnet"
	WindowCinderCove        WindowName = "cinder_cove"
)

// Window is one rate-limit window.
//
// Present records that the response carried the key at all, which is a separate
// question from whether either field could be read: a freshly reset account
// reports {"utilization":null,"resets_at":null} and still HAS the window.
// Claude Code's own seed check is a truthiness test on the object, so a JSON
// null reads as absent here for the same reason.
type Window struct {
	Present bool

	pct     float64
	hasPct  bool
	reset   time.Time
	hasTime bool
}

// Percent is the window's utilization as a percent of 0-100, and whether it was
// reported at all. It is never scaled: the body is already a percent.
func (w Window) Percent() (float64, bool) { return w.pct, w.hasPct }

// Reset is when the window rolls over, and whether it was reported at all. An
// unreported reset is unknown, never "now".
func (w Window) Reset() (time.Time, bool) { return w.reset, w.hasTime }

// NamedWindow pairs a window with the key it arrived under, so a caller that
// ranks windows can name the one that binds.
type NamedWindow struct {
	Name WindowName
	Window
}

// NewWindow builds a present Window from already-normalized values: a percent
// of 0-100 and a reset time, either of which may be nil for "not reported". The
// zero Window is the absent one, so this is the only way to say "present, and
// here is what it said".
//
// It exists because Window's tri-state fields are unexported — which is what
// stops a caller reading a zero out of an unknown — and without a constructor
// every package downstream would have to build its test readings out of JSON.
func NewWindow(pct *float64, resetsAt *time.Time) Window {
	w := Window{Present: true}
	if pct != nil {
		w.pct, w.hasPct = *pct, true
	}
	if resetsAt != nil {
		w.reset, w.hasTime = resetsAt.UTC(), true
	}
	return w
}

// ExtraUsageFor builds a present ExtraUsage, for the same reason NewWindow
// exists. The zero value stays the absent one.
func ExtraUsageFor(state ExtraUsageState, disabledReason string, monthlyLimit, usedCredits *float64) ExtraUsage {
	e := ExtraUsage{Present: true, State: state, DisabledReason: disabledReason}
	if monthlyLimit != nil {
		e.limit, e.hasLimit = *monthlyLimit, true
	}
	if usedCredits != nil {
		e.used, e.hasUsed = *usedCredits, true
	}
	return e
}

// WindowFromHeader builds a Window from the anthropic-ratelimit-unified-*
// response headers, which use the OTHER representation: utilization is a 0-1
// fraction and resets_at is an epoch second. This is the single conversion
// boundary; nothing else in the package may accept the header form.
func WindowFromHeader(fraction float64, resetsAtUnix int64) Window {
	return Window{
		Present: true,
		pct:     fraction * 100,
		hasPct:  true,
		reset:   time.Unix(resetsAtUnix, 0).UTC(),
		hasTime: true,
	}
}

// ExtraUsageState is how an account's overage credits stand. Claude Code reads
// four states, not two, and the difference between them decides money: Blocked
// is an org or seat policy refusal (org_spend_cap_reached, out_of_credits,
// seat_tier_zero_credit_limit and friends), which is not the same as an account
// that simply has overage switched off, and neither is the same as not knowing.
type ExtraUsageState uint8

const (
	// ExtraUsageUnknown is the zero value deliberately: an unread account must
	// not present as one with credit room.
	ExtraUsageUnknown ExtraUsageState = iota
	ExtraUsageEnabled
	ExtraUsageDisabled
	ExtraUsageBlocked
)

// ParseExtraUsageState is String's inverse, for reading a persisted state back.
//
// An unrecognized name — a file written before the field existed, a typo, or a
// state a future release adds — reads as unknown for the same reason Classify's
// default leans the way it does: unknown is the side that does not spend.
func ParseExtraUsageState(name string) ExtraUsageState {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "enabled":
		return ExtraUsageEnabled
	case "disabled":
		return ExtraUsageDisabled
	case "blocked":
		return ExtraUsageBlocked
	}
	return ExtraUsageUnknown
}

func (s ExtraUsageState) String() string {
	switch s {
	case ExtraUsageEnabled:
		return "enabled"
	case ExtraUsageDisabled:
		return "disabled"
	case ExtraUsageBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// ExtraUsage is the credit axis: extra_usage from the response body.
type ExtraUsage struct {
	Present bool
	State   ExtraUsageState
	// DisabledReason is why the org or seat blocked overage. It is the string
	// the gate's notification names, so it is kept verbatim.
	DisabledReason string
	Currency       string

	limit    float64
	hasLimit bool
	used     float64
	hasUsed  bool
	pct      float64
	hasPct   bool
}

// MonthlyLimit is the account's own spend cap, and whether one was reported. A
// null limit means unlimited, which §7.3 reads as "no account cap" and falls
// back to the configured ceiling — it does not mean a cap of zero.
func (e ExtraUsage) MonthlyLimit() (float64, bool) { return e.limit, e.hasLimit }

// UsedCredits is the money already spent, and whether it could be read at all.
// §7.3 refuses to switch when this is unknown: fail closed on money.
func (e ExtraUsage) UsedCredits() (float64, bool) { return e.used, e.hasUsed }

// Percent is extra_usage.utilization, a percent of 0-100.
func (e ExtraUsage) Percent() (float64, bool) { return e.pct, e.hasPct }

// Limit is one entry of the limits[] array: a per-model or per-surface weekly
// window the server reports alongside the fixed six.
type Limit struct {
	Kind    string
	Group   string
	Percent float64

	ModelDisplayName   string
	SurfaceDisplayName string

	reset   time.Time
	hasTime bool
}

// Reset is when this limit rolls over, and whether it was reported.
func (l Limit) Reset() (time.Time, bool) { return l.reset, l.hasTime }

// Snapshot is one reading of an account's usage.
type Snapshot struct {
	FiveHour          Window
	SevenDay          Window
	SevenDayOAuthApps Window
	SevenDayOpus      Window
	SevenDaySonnet    Window
	// CinderCove is the one-time "Claude Code and Cowork credit" grant. Its
	// resets_at is an EXPIRY, not a recurring reset, which is why
	// RateLimitWindows deliberately leaves it out: ranking it as a window would
	// have the engine wait for a rollover that never comes.
	CinderCove Window

	ExtraUsage ExtraUsage
	Limits     []Limit
}

// RateLimitWindows is the five recurring windows, in the schema's order. It
// excludes cinder_cove; see Snapshot.CinderCove for why.
func (s *Snapshot) RateLimitWindows() []NamedWindow {
	return []NamedWindow{
		{WindowFiveHour, s.FiveHour},
		{WindowSevenDay, s.SevenDay},
		{WindowSevenDayOAuthApps, s.SevenDayOAuthApps},
		{WindowSevenDayOpus, s.SevenDayOpus},
		{WindowSevenDaySonnet, s.SevenDaySonnet},
	}
}

// HasSubscriptionWindows reports whether the account carried any of the
// recurring windows a plan is metered by.
//
// It counts the model-specific weekly windows too, not just five_hour and
// seven_day. They are plan limits — an account only has an Opus or Sonnet weekly
// cap because a subscription gives it one — and an Opus-limited account whose
// five_hour and seven_day keys happen not to come back would otherwise classify
// as having no subscription evidence at all, which is the side of the credit
// gate that spends money.
//
// cinder_cove is excluded for the opposite reason: it is a one-time credit
// grant, not a plan window, so it is evidence of the credit axis rather than of
// a subscription.
func (s *Snapshot) HasSubscriptionWindows() bool {
	for _, w := range s.RateLimitWindows() {
		if w.Present {
			return true
		}
	}
	return false
}

// usageFields are the eight keys a usage response is made of, from the 2.1.239
// bundle's own list. A 200 whose object carries none of them is an in-band
// error, not a reading — see ErrNoUsageFields.
var usageFields = [...]string{
	"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus",
	"seven_day_sonnet", "cinder_cove", "extra_usage", "limits",
}

// transientOverageRefusals are the disabled_reason values that mean "refused
// now, may come back", verbatim from the bundle's predicate
// `e==="org_level_disabled_until"||e==="org_spend_cap_reached"||e==="out_of_credits"`.
// Every other reason is a structural "this organization does not do overage",
// which is ExtraUsageDisabled rather than ExtraUsageBlocked.
var transientOverageRefusals = map[string]bool{
	"org_level_disabled_until": true,
	"org_spend_cap_reached":    true,
	"out_of_credits":           true,
}

// windowWire mirrors the schema's window object. Both fields are pointers
// because the schema declares both nullable and the difference between null and
// a value is the whole point of this package.
type windowWire struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type extraUsageWire struct {
	IsEnabled      *bool    `json:"is_enabled"`
	MonthlyLimit   *float64 `json:"monthly_limit"`
	UsedCredits    *float64 `json:"used_credits"`
	Utilization    *float64 `json:"utilization"`
	Currency       *string  `json:"currency"`
	DisabledReason *string  `json:"disabled_reason"`
}

type displayNameWire struct {
	DisplayName string `json:"display_name"`
}

type limitWire struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	ResetsAt *string `json:"resets_at"`
	Scope    *struct {
		Model   *displayNameWire `json:"model"`
		Surface *displayNameWire `json:"surface"`
	} `json:"scope"`
}

// wire mirrors the whole response. Every unknown key is ignored, which is what
// the schema's zod passthrough means on the read side.
type wire struct {
	FiveHour          *windowWire     `json:"five_hour"`
	SevenDay          *windowWire     `json:"seven_day"`
	SevenDayOAuthApps *windowWire     `json:"seven_day_oauth_apps"`
	SevenDayOpus      *windowWire     `json:"seven_day_opus"`
	SevenDaySonnet    *windowWire     `json:"seven_day_sonnet"`
	CinderCove        *windowWire     `json:"cinder_cove"`
	ExtraUsage        *extraUsageWire `json:"extra_usage"`
	Limits            []limitWire     `json:"limits"`
}

// rateLimitEnvelope reports whether the body is a rate limit delivered inside a
// 200. Claude Code looks for exactly this shape and labels it "envelope".
func rateLimitEnvelope(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var e struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return false
	}
	return e.Type == "rate_limit_error"
}

// parseReset reads an ISO-8601 resets_at. A string that is not a timestamp is
// drift in one field — an epoch second sent where an ISO string belongs, say —
// and reads as an unknown reset rather than failing a response whose other five
// windows are perfectly good.
func parseReset(s *string) (time.Time, bool) {
	if s == nil || *s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// toWindow normalizes one window. A nil w is either an absent key or an explicit
// null, and Claude Code's own check cannot tell those apart either: both are
// falsy to it, so both are "not present" here.
func toWindow(w *windowWire) Window {
	if w == nil {
		return Window{}
	}
	out := Window{Present: true}
	if w.Utilization != nil {
		// Stored verbatim. The body is already a percent, so scaling it by
		// magnitude — "0.92 looks like a fraction" — is the exact bug.
		out.pct, out.hasPct = *w.Utilization, true
	}
	out.reset, out.hasTime = parseReset(w.ResetsAt)
	return out
}

func toExtraUsage(e *extraUsageWire) ExtraUsage {
	if e == nil {
		return ExtraUsage{}
	}
	out := ExtraUsage{Present: true}
	if e.DisabledReason != nil {
		out.DisabledReason = *e.DisabledReason
	}
	if e.Currency != nil {
		out.Currency = *e.Currency
	}
	if e.MonthlyLimit != nil {
		out.limit, out.hasLimit = *e.MonthlyLimit, true
	}
	if e.UsedCredits != nil {
		out.used, out.hasUsed = *e.UsedCredits, true
	}
	if e.Utilization != nil {
		out.pct, out.hasPct = *e.Utilization, true
	}

	switch {
	case e.IsEnabled != nil && *e.IsEnabled:
		out.State = ExtraUsageEnabled
	case transientOverageRefusals[out.DisabledReason]:
		out.State = ExtraUsageBlocked
	case e.IsEnabled != nil:
		out.State = ExtraUsageDisabled
	default:
		// is_enabled is required by the schema, so its absence is drift. It
		// reads as unknown, which is the side that does not spend.
		out.State = ExtraUsageUnknown
	}
	return out
}

func toLimits(in []limitWire) []Limit {
	if len(in) == 0 {
		return nil
	}
	out := make([]Limit, 0, len(in))
	for _, l := range in {
		item := Limit{Kind: l.Kind, Group: l.Group, Percent: l.Percent}
		item.reset, item.hasTime = parseReset(l.ResetsAt)
		if l.Scope != nil {
			if l.Scope.Model != nil {
				item.ModelDisplayName = l.Scope.Model.DisplayName
			}
			if l.Scope.Surface != nil {
				item.SurfaceDisplayName = l.Scope.Surface.DisplayName
			}
		}
		out = append(out, item)
	}
	return out
}

// Parse turns a usage response body into a Snapshot.
//
// It refuses two kinds of body that a plain unmarshal would accept. A body that
// is not a JSON object is not a usage response at all; and an object carrying
// none of the eight known keys is what Claude Code calls an in-band error, which
// must not read as six unknown windows. A value that is present but unreadable
// — a null utilization, a resets_at that is not a timestamp — is unknown rather
// than an error, because refusing the whole response over one field would throw
// away the windows that were fine.
func Parse(body []byte) (*Snapshot, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		return nil, fmt.Errorf("the usage endpoint returned a body that is not a JSON object")
	}
	// A literal `null` unmarshals into a nil map without error.
	if keys == nil {
		return nil, fmt.Errorf("the usage endpoint returned a null body")
	}

	known := false
	for _, k := range usageFields {
		if _, ok := keys[k]; ok {
			known = true
			break
		}
	}
	if !known {
		if rateLimitEnvelope(keys["error"]) {
			return nil, ErrRateLimited
		}
		return nil, ErrNoUsageFields
	}

	var w wire
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("the usage endpoint returned a body ccdad could not read")
	}

	return &Snapshot{
		FiveHour:          toWindow(w.FiveHour),
		SevenDay:          toWindow(w.SevenDay),
		SevenDayOAuthApps: toWindow(w.SevenDayOAuthApps),
		SevenDayOpus:      toWindow(w.SevenDayOpus),
		SevenDaySonnet:    toWindow(w.SevenDaySonnet),
		CinderCove:        toWindow(w.CinderCove),
		ExtraUsage:        toExtraUsage(w.ExtraUsage),
		Limits:            toLimits(w.Limits),
	}, nil
}

// ---- JSON codec -------------------------------------------------------------
//
// A Snapshot encodes back into the endpoint's own eight-key shape. That is what
// makes the on-disk cache and a live response the same document, read by one
// parser — and it is what Claude Code does too, persisting the raw utilization
// object rather than a private projection of it.
//
// Absent windows encode as an explicit null rather than being omitted. The two
// are the same thing to this package (Claude Code's own check cannot tell them
// apart either), and always writing the key keeps the eight-key probe happy on
// the way back in.

func fromTime(t time.Time, ok bool) *string {
	if !ok {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func fromFloat(v float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return &v
}

func (w Window) toWire() *windowWire {
	if !w.Present {
		return nil
	}
	return &windowWire{
		Utilization: fromFloat(w.pct, w.hasPct),
		ResetsAt:    fromTime(w.reset, w.hasTime),
	}
}

func (e ExtraUsage) toWire() *extraUsageWire {
	if !e.Present {
		return nil
	}
	out := &extraUsageWire{
		MonthlyLimit: fromFloat(e.limit, e.hasLimit),
		UsedCredits:  fromFloat(e.used, e.hasUsed),
		Utilization:  fromFloat(e.pct, e.hasPct),
	}
	if e.Currency != "" {
		c := e.Currency
		out.Currency = &c
	}
	if e.DisabledReason != "" {
		r := e.DisabledReason
		out.DisabledReason = &r
	}
	// ExtraUsageUnknown leaves is_enabled absent, which is exactly the drift
	// that produced it — so the state survives the round trip rather than being
	// rounded to the nearest boolean.
	switch e.State {
	case ExtraUsageEnabled:
		v := true
		out.IsEnabled = &v
	case ExtraUsageDisabled, ExtraUsageBlocked:
		v := false
		out.IsEnabled = &v
	}
	return out
}

func (l Limit) toWire() limitWire {
	out := limitWire{
		Kind:     l.Kind,
		Group:    l.Group,
		Percent:  l.Percent,
		ResetsAt: fromTime(l.reset, l.hasTime),
	}
	if l.ModelDisplayName != "" || l.SurfaceDisplayName != "" {
		out.Scope = &struct {
			Model   *displayNameWire `json:"model"`
			Surface *displayNameWire `json:"surface"`
		}{}
		if l.ModelDisplayName != "" {
			out.Scope.Model = &displayNameWire{DisplayName: l.ModelDisplayName}
		}
		if l.SurfaceDisplayName != "" {
			out.Scope.Surface = &displayNameWire{DisplayName: l.SurfaceDisplayName}
		}
	}
	return out
}

// MarshalJSON writes the endpoint's own shape.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	w := wire{
		FiveHour:          s.FiveHour.toWire(),
		SevenDay:          s.SevenDay.toWire(),
		SevenDayOAuthApps: s.SevenDayOAuthApps.toWire(),
		SevenDayOpus:      s.SevenDayOpus.toWire(),
		SevenDaySonnet:    s.SevenDaySonnet.toWire(),
		CinderCove:        s.CinderCove.toWire(),
		ExtraUsage:        s.ExtraUsage.toWire(),
	}
	if len(s.Limits) > 0 {
		w.Limits = make([]limitWire, 0, len(s.Limits))
		for _, l := range s.Limits {
			w.Limits = append(w.Limits, l.toWire())
		}
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads a snapshot with the same parser a live response goes
// through, tri-state rules and eight-key probe included: a cache file that has
// been hand-edited into nonsense is refused for the same reasons a nonsense
// response is.
func (s *Snapshot) UnmarshalJSON(data []byte) error {
	parsed, err := Parse(data)
	if err != nil {
		return err
	}
	*s = *parsed
	return nil
}
