// Package usage reads GET /api/oauth/usage and normalizes it into a shape the
// auto-switch engine can rank on.
//
// The one thing this package exists to get right is that every number it
// carries is TRI-STATE. A window that could not be read is not an empty
// account, and a reset that was not reported is not "resets now".
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
	"maps"
	"math"
	"slices"
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

// Scoped reports whether this name belongs to a limits[] entry rather than to
// one of the six keys the schema names. It is how a caller that has only a name
// tells a per-model or per-surface weekly window from a fixed one.
func (n WindowName) Scoped() bool {
	return strings.HasPrefix(string(n), weeklyScopedKind+":")
}

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
//
// A value that is not finite is not a reading. JSON cannot carry one, but a
// header parsed with Number()/ParseFloat can, and Claude Code's own consumer
// re-checks `Number.isFinite(l.utilization)` on exactly that path. Without this
// guard a NaN would report as KNOWN — and because every NaN comparison is
// false, it would then lose no comparison in the ranking and could hold first
// place while being the one account nobody could read.
func (w Window) Percent() (float64, bool) {
	if !w.hasPct || !isFinite(w.pct) {
		return 0, false
	}
	return w.pct, true
}

// isFinite is the guard every tri-state accessor shares.
func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

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

// ExtraUsageInput is what ExtraUsageFor takes, for the same reason NewWindow
// exists: the zero ExtraUsage stays the absent one, so a present-but-empty
// reading needs a constructor.
//
// It takes every field the type carries, so there is no part of an ExtraUsage
// that only a parsed response can express -- an unconstructible field is a field
// nothing downstream can test against.
//
// MonthlyLimit and UsedCredits are WIRE amounts, in the currency's minor unit,
// because that is what this type stores and what its JSON codec writes back; the
// accessors convert, see majorUnits. Utilization is already a percent and is
// not converted. An empty Currency reads as a two-decimal one.
type ExtraUsageInput struct {
	State          ExtraUsageState
	DisabledReason string
	Currency       string
	MonthlyLimit   *float64
	UsedCredits    *float64
	Utilization    *float64
}

// ExtraUsageFor builds a present ExtraUsage from already-normalized values.
func ExtraUsageFor(in ExtraUsageInput) ExtraUsage {
	e := ExtraUsage{
		Present:        true,
		State:          in.State,
		DisabledReason: in.DisabledReason,
		Currency:       in.Currency,
	}
	if in.MonthlyLimit != nil {
		e.limit, e.hasLimit = *in.MonthlyLimit, true
	}
	if in.UsedCredits != nil {
		e.used, e.hasUsed = *in.UsedCredits, true
	}
	if in.Utilization != nil {
		e.pct, e.hasPct = *in.Utilization, true
	}
	return e
}

// WindowFromHeader builds a Window from the anthropic-ratelimit-unified-*
// response headers, which use the OTHER representation: utilization is a 0-1
// fraction and resets_at is an epoch second. This is the single conversion
// boundary; nothing else in the package may accept the header form.
//
// Both halves are pointers, and a missing or non-finite half yields the ABSENT
// window rather than a half-filled one. That is Claude Code's own rule: `y9p`
// records a window only `if(o!==null&&i!==null)`, and its consumer re-checks
// both with Number.isFinite before trusting the record. Filling in the other
// half would mean a header that was never sent arriving as "0% used" or as a
// reset at the 1970 epoch — which reads as "already recovered" and puts the one
// account nobody could measure at the front of the recovery queue.
func WindowFromHeader(fraction *float64, resetsAtUnix *int64) Window {
	if fraction == nil || resetsAtUnix == nil || !isFinite(*fraction) {
		return Window{}
	}
	return Window{
		Present: true,
		pct:     *fraction * 100,
		hasPct:  true,
		reset:   time.Unix(*resetsAtUnix, 0).UTC(),
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

// zeroDecimalCurrencies have no minor unit — their smallest unit IS the major
// one — so an amount in them is not divided. The set is Claude Code's own
// (`OpE=new Set(["JPY","KRW","VND"])`, consulted by its currency formatter `Zm`
// before that formatter's `e/100`).
var zeroDecimalCurrencies = map[string]bool{"JPY": true, "KRW": true, "VND": true}

// majorUnits converts a wire amount to the unit max_auto_spend is written in.
//
// THE OTHER 100x TRAP, and it is the one that spends money. extra_usage's
// monthly_limit and used_credits arrive in the currency's MINOR unit — cents for
// USD — while the credit gate's max_auto_spend (README, "Configuration") is
// dollars. Claude Code's formatter `Zm` proves it by dividing by 100 before
// rendering either figure, and its own test double maps
// `spendLimitCents -> monthly_limit` and `usedCents -> used_credits` with no
// conversion at all. Comparing the two units directly makes a $0.60 spend look
// like $60 against a $50 ceiling and blocks the engine at 1.2% of the
// authorized budget; in the other direction an account's own cap stops binding.
//
// CONFIRMED against a live account on 2026-08-25, which until then it had not
// been: an account whose holder puts its cap at $466.00 reports monthly_limit
// 46600 and used_credits 46600. The reading itself cannot prove this -- used
// equals the limit there, so the ratio is 1 either way -- and the confirmation
// comes from the person who set the cap. TestTheLiveCreditReadingParsesAsThisPackageAssumed
// pins the reading.
//
// An unreported or unrecognized currency is treated as a two-decimal one, which
// is what Claude Code does (`tse.currency ?? "USD"`).
func (e ExtraUsage) majorUnits(minor float64) float64 {
	if zeroDecimalCurrencies[strings.ToUpper(strings.TrimSpace(e.Currency))] {
		return minor
	}
	return minor / 100
}

// MonthlyLimit is the account's own spend cap IN MAJOR UNITS — dollars for USD —
// and whether one was reported. A null limit means unlimited, which the credit
// gate reads as "no account cap" and falls back to the configured ceiling; it
// does not mean a cap of zero.
func (e ExtraUsage) MonthlyLimit() (float64, bool) {
	if !e.hasLimit || !isFinite(e.limit) {
		return 0, false
	}
	return e.majorUnits(e.limit), true
}

// UsedCredits is the money already spent, in major units, and whether it could
// be read at all. The credit gate refuses to switch when this is unknown: fail
// closed on money.
func (e ExtraUsage) UsedCredits() (float64, bool) {
	if !e.hasUsed || !isFinite(e.used) {
		return 0, false
	}
	return e.majorUnits(e.used), true
}

// Percent is extra_usage.utilization, a percent of 0-100. Unlike the two money
// figures it is already a percent on the wire and is not converted.
func (e ExtraUsage) Percent() (float64, bool) {
	if !e.hasPct || !isFinite(e.pct) {
		return 0, false
	}
	return e.pct, true
}

// AmountString renders a MAJOR-UNIT figure — used, limit, or their difference —
// the way this account's own currency writes amounts: two decimals, except the
// zero-decimal currencies (see zeroDecimalCurrencies), which never had a minor
// unit to round to. It carries no currency code of its own, so a caller
// printing several figures side by side names the currency once.
func (e ExtraUsage) AmountString(major float64) string {
	if zeroDecimalCurrencies[strings.ToUpper(strings.TrimSpace(e.Currency))] {
		return fmt.Sprintf("%.0f", major)
	}
	return fmt.Sprintf("%.2f", major)
}

// CurrencyCode is the ISO code AmountString's figures are in, defaulting to USD
// for the same reason majorUnits does: an unreported currency is Claude Code's
// own default (`tse.currency ?? "USD"`), not evidence of a two-decimal one only.
func (e ExtraUsage) CurrencyCode() string {
	c := strings.ToUpper(strings.TrimSpace(e.Currency))
	if c == "" {
		return "USD"
	}
	return c
}

// Limit is one entry of the limits[] array: a per-model or per-surface weekly
// window the server reports alongside the fixed six.
type Limit struct {
	Kind  string
	Group string

	ModelDisplayName   string
	SurfaceDisplayName string
	// OtherScopes is every scope key this build does not name, by key, with the
	// scope's display name for a value. Claude Code types the scope object as
	// {model?, surface?} and names no third key, but every level of that schema
	// is a passthrough, so a key added server-side is legal wire. Dropping one at
	// the decode would make a weekly cap the session is subject to invisible
	// rather than merely unranked, and the ranking would spend against quota it
	// could not see.
	OtherScopes map[string]string

	pct     float64
	hasPct  bool
	reset   time.Time
	hasTime bool
}

// Percent is this limit's utilization as a percent of 0-100, and whether it was
// reported. It is the same quantity a Window carries under `utilization`, in the
// same unit: Claude Code's own projection of a limits[] entry is
// `{utilization: n.percent, resets_at: n.resets_at}`.
//
// The schema writes `percent` as a plain non-null number, so on the WIRE this
// value's tri-state is the presence of the entry rather than the nullability of
// the field. It is still read tri-state here, for two reasons. A body that omits
// the key unmarshals to 0 in Go, which reads as "0% used" — the one direction
// that makes a spent account look fresh, and this is a value the ranking takes a
// minimum over, so a fresh-looking entry is invisible rather than loud. And
// Claude Code null-guards the value it derives from percent on both of its own
// paths (`a.utilization === null` in formatRateLimits, `?? null` in the
// model_scoped projection), so the null case is real one projection downstream.
func (l Limit) Percent() (float64, bool) {
	if !l.hasPct || !isFinite(l.pct) {
		return 0, false
	}
	return l.pct, true
}

// Reset is when this limit rolls over, and whether it was reported.
func (l Limit) Reset() (time.Time, bool) { return l.reset, l.hasTime }

// LimitInput is what LimitFor takes, for the same reason NewWindow exists: the
// tri-state fields are unexported, so a reading that did not come from a parsed
// response needs a constructor. Percent is already a percent of 0-100 and is
// not converted.
type LimitInput struct {
	Kind    string
	Group   string
	Model   string
	Surface string
	// OtherScopes is the scope keys this build does not name; see
	// Limit.OtherScopes.
	OtherScopes map[string]string

	Percent  *float64
	ResetsAt *time.Time
}

// LimitFor builds a Limit from already-normalized values.
func LimitFor(in LimitInput) Limit {
	l := Limit{
		Kind:               in.Kind,
		Group:              in.Group,
		ModelDisplayName:   in.Model,
		SurfaceDisplayName: in.Surface,
	}
	if len(in.OtherScopes) > 0 {
		l.OtherScopes = maps.Clone(in.OtherScopes)
	}
	if in.Percent != nil {
		l.pct, l.hasPct = *in.Percent, true
	}
	if in.ResetsAt != nil {
		l.reset, l.hasTime = in.ResetsAt.UTC(), true
	}
	return l
}

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

// weeklyScopedKind is the one limits[] kind Claude Code identifies as a
// rate-limit window: its projection filters `kind === "weekly_scoped"` before it
// reads percent and resets_at off an entry. Every other kind is ignored here,
// and that is the cinder_cove rule one level down — an entry of an unknown kind
// may be a one-time grant whose resets_at is an EXPIRY, and binding one would
// park the engine waiting for a rollover that never comes.
const weeklyScopedKind = "weekly_scoped"

// ScopedWindow is one limits[] entry read as a window: a weekly cap the server
// scopes to one model or one surface.
//
// Model and Surface are the scope's DISPLAY names, kept verbatim because that
// is the only handle the wire gives them — there is no stable identifier in the
// scope object. Both may be set; the entry is then named for its model, which is
// the half Claude Code's own filter requires (`n.scope?.model`).
type ScopedWindow struct {
	NamedWindow
	Model   string
	Surface string
	// Scope is the scope KEY the entry was filed under and the name was built
	// from: ScopeModel, ScopeSurface, or — for a window that came back from
	// UnknownScopeWindows — a key this build does not name. It is what tells the
	// two apart once only the window is in hand.
	Scope string
}

// ScopedWindows is the limits[] entries that can bind, in wire order.
//
// An entry with no scope name is dropped rather than kept under an empty one.
// The scope object is nullable and BOTH of its halves are optional, so an entry
// naming neither is legal wire; it says a weekly cap exists without saying what
// it caps, which is nothing a ranking can attribute to a session.
//
// The synthetic name carries the scope's own kind as well as its display name,
// so a model and a surface that share a display name stay two windows and a
// caller looking a binding window back up by name finds the one that bound. It
// is built by ScopedWindowName, which is the same builder ValidWindowName
// recognizes a name by, so nothing here can produce a name a caller holding
// only the name would reject.
func (s *Snapshot) ScopedWindows() []ScopedWindow {
	if s == nil || len(s.Limits) == 0 {
		return nil
	}
	out := make([]ScopedWindow, 0, len(s.Limits))
	for _, l := range s.Limits {
		if l.Kind != weeklyScopedKind {
			continue
		}
		scope, display, ok := namedScopeOf(l)
		if !ok {
			continue
		}
		out = append(out, scopedWindowOf(l, scope, display))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// namedScopeOf is the scope this build names an entry by, in the order the scope
// table declares: a model display name wins when an entry carries both. It
// answers false for an entry naming no scope this build knows, which is the one
// question ScopedWindows and UnknownScopeWindows split on, so neither can claim
// an entry the other also claims.
func namedScopeOf(l Limit) (scope, display string, ok bool) {
	for _, sc := range scopedWindowScopes {
		if d := sc.Display(l); d != "" {
			return sc.Name, d, true
		}
	}
	return "", "", false
}

// scopedWindowOf reads one limits[] entry as a window under the given scope.
func scopedWindowOf(l Limit, scope, display string) ScopedWindow {
	return ScopedWindow{
		NamedWindow: NamedWindow{
			Name: ScopedWindowName(scope, display),
			// Present is the entry's own existence. A scoped window whose
			// percent could not be read is still a window that IS there,
			// which is the same distinction Window.Present draws.
			Window: Window{
				Present: true,
				pct:     l.pct,
				hasPct:  l.hasPct,
				reset:   l.reset,
				hasTime: l.hasTime,
			},
		},
		Model:   l.ModelDisplayName,
		Surface: l.SurfaceDisplayName,
		Scope:   scope,
	}
}

// UnknownScopeWindows is the limits[] entries this build cannot attribute: a
// weekly cap filed under a scope key it does not name, and under no key it does.
//
// They are kept OUT of ScopedWindows, and therefore out of the set the engine
// binds on, for the reason an entry naming no scope at all has always been
// dropped — a cap ccdad cannot describe is not one it can tell a user it
// switched away for. But they are not discarded either: a user who knows what
// the scope means can set a threshold on the name and put it into the ranking,
// and nothing can be tuned that cannot first be seen.
//
// One ENTRY is one window even when it carries several unnamed scopes, because
// one entry is one cap; ranking it twice would double-count the same quota. The
// key is chosen in sorted order rather than in map order, so the same reading
// names the same window on every call — the ranking ties on the first window in
// order, and a tie that moved between calls would move the answer with it.
func (s *Snapshot) UnknownScopeWindows() []ScopedWindow {
	if s == nil || len(s.Limits) == 0 {
		return nil
	}
	var out []ScopedWindow
	for _, l := range s.Limits {
		if l.Kind != weeklyScopedKind {
			continue
		}
		if _, _, ok := namedScopeOf(l); ok {
			continue
		}
		if scope, display, ok := unknownScopeOf(l); ok {
			out = append(out, scopedWindowOf(l, scope, display))
		}
	}
	return out
}

// unknownScopeOf is the scope an entry is named by when this build names none of
// its scopes: the first, in sorted key order, a window can actually be ADDRESSED
// under. A key with no display half, or one carrying a colon, or an empty one
// builds a string no threshold can be set on, so it is passed over rather than
// used — and an entry offering no usable key at all answers false, which is the
// entry UnnamableLimits counts.
//
// Sorted rather than map order because the choice has to be the same on every
// call far more than it has to be any particular one: the ranking ties on the
// first window in order, and a name that moved between calls would move the
// answer with it.
func unknownScopeOf(l Limit) (scope, display string, ok bool) {
	for _, key := range slices.Sorted(maps.Keys(l.OtherScopes)) {
		if d := l.OtherScopes[key]; d != "" && namableScopeKey(key) {
			return key, d, true
		}
	}
	return "", "", false
}

// UnnamableLimits is how many weekly_scoped entries produced no window at all:
// the wire said a weekly cap exists and gave nothing to name it by, so there is
// no key a threshold could be set on and no row a report could carry.
//
// It is a count and not a list because there is nothing to list — the entries
// have no names, which is the whole of what is wrong with them.
//
// Every part of dropping such an entry is deliberate. ScopedWindows has always
// left out a cap it cannot attribute, because a cap ccdad cannot describe is not
// one it can tell a user it switched away for. But deliberate and INVISIBLE are
// different things, and a weekly cap the ranking cannot see is the exact failure
// the rest of this file exists to prevent. A non-zero count is the operator's
// only sign that the wire is carrying quota this build has no handle on.
func (s *Snapshot) UnnamableLimits() int {
	if s == nil {
		return 0
	}
	n := 0
	for _, l := range s.Limits {
		if l.Kind != weeklyScopedKind {
			continue
		}
		if _, _, ok := namedScopeOf(l); ok {
			continue
		}
		if _, _, ok := unknownScopeOf(l); ok {
			continue
		}
		n++
	}
	return n
}

// AllWindows is every window an account can be ranked on: the five recurring
// ones and the scoped weekly windows. It is what a caller holding a binding
// window's NAME looks it up in — the ranking may narrow which of these bind for
// a given session, but it never binds on a window that is not in here.
func (s *Snapshot) AllWindows() []NamedWindow {
	if s == nil {
		return nil
	}
	fixed := s.RateLimitWindows()
	scoped := s.ScopedWindows()
	// The unnamed-scope windows are in here even though they do not bind by
	// default. This is the set a caller holding only a NAME looks a window up
	// in, and the opt-in can put one of them into the ranking — leaving them out
	// would have status report a binding window its own renderer cannot find.
	unknown := s.UnknownScopeWindows()
	if len(scoped) == 0 && len(unknown) == 0 {
		return fixed
	}
	out := make([]NamedWindow, 0, len(fixed)+len(scoped)+len(unknown))
	out = append(out, fixed...)
	for _, w := range scoped {
		out = append(out, w.NamedWindow)
	}
	for _, w := range unknown {
		out = append(out, w.NamedWindow)
	}
	return out
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
	Kind     string     `json:"kind"`
	Group    string     `json:"group"`
	Percent  *float64   `json:"percent"`
	ResetsAt *string    `json:"resets_at"`
	Scope    *scopeWire `json:"scope"`
}

// scopeWire is a limits[] entry's scope object. It is decoded key by key rather
// than by field tags because the schema is a passthrough: a scope key this build
// does not name is legal wire, and a decode that knows only two fields discards
// it without a trace.
//
// A key whose value is not an object carrying a non-empty display_name is left
// out rather than failing the document. It names nothing, and one unreadable
// scope must not cost the five windows that parsed.
type scopeWire struct {
	Model   *displayNameWire
	Surface *displayNameWire
	Other   map[string]string
}

func (w *scopeWire) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for key, v := range raw {
		switch key {
		case ScopeModel, ScopeSurface:
			// A key this build NAMES is held to the schema, and a shape it
			// cannot read fails the document rather than reading as absent.
			// That is not tidiness: on the wire an unreadable scope says a cap
			// exists and refuses to say what it caps, and reading it as absent
			// is the one direction that makes a spent account look fresh. The
			// tolerance below is for keys this build has no schema for.
			var d displayNameWire
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			if d.DisplayName == "" {
				continue
			}
			if key == ScopeModel {
				w.Model = &displayNameWire{DisplayName: d.DisplayName}
			} else {
				w.Surface = &displayNameWire{DisplayName: d.DisplayName}
			}
		default:
			display, ok := unknownScopeDisplay(v)
			if !ok || !namableScopeKey(key) {
				continue
			}
			if w.Other == nil {
				w.Other = make(map[string]string, len(raw))
			}
			w.Other[key] = display
		}
	}
	return nil
}

// unknownScopeDisplay is the handle a scope key this build has no schema for
// offers, and whether it offers one at all.
//
// Two shapes are read. {"display_name": "..."} is the one both named scopes use,
// and a key added later is likeliest to follow its neighbours. A bare string is
// the other shape a scope has ever plausibly taken, and it is just as usable a
// handle. Anything else — an object with no display name, a number, a list —
// gives nothing to name a window by, and a window with no name is one no
// threshold can ever address, so it is left out rather than carried nameless.
//
// Failing the document over one of these is the thing NOT to do: the schema is a
// passthrough, so a shape this build has never seen is legal wire, and throwing
// away the five windows that parsed to punish it is the larger mistake.
func unknownScopeDisplay(v json.RawMessage) (string, bool) {
	var d displayNameWire
	if err := json.Unmarshal(v, &d); err == nil && d.DisplayName != "" {
		return d.DisplayName, true
	}
	var bare string
	if err := json.Unmarshal(v, &bare); err == nil && bare != "" {
		return bare, true
	}
	return "", false
}

// namableScopeKey reports whether a window named under this scope key can be
// read back out of its own name. ScopedWindowName joins the key and the display
// half with a colon and ValidWindowName splits on the FIRST one, so a key
// carrying a colon of its own produces a name that means something else on the
// way back — {"model:x": {"display_name": "y"}} and a model called "x:y" build
// the identical string, and one threshold would then govern two caps. An empty
// key splits to an empty scope, which is not a name either.
func namableScopeKey(key string) bool {
	return key != "" && !strings.Contains(key, ":")
}

// MarshalJSON writes the scope back as the object it arrived as. Go renders a
// map in sorted key order, so the encoding is byte-stable and a cached entry
// re-read is the entry that was written.
func (w scopeWire) MarshalJSON() ([]byte, error) {
	out := make(map[string]displayNameWire, 2+len(w.Other))
	if w.Model != nil {
		out[ScopeModel] = *w.Model
	}
	if w.Surface != nil {
		out[ScopeSurface] = *w.Surface
	}
	for k, v := range w.Other {
		out[k] = displayNameWire{DisplayName: v}
	}
	return json.Marshal(out)
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
		item := Limit{Kind: l.Kind, Group: l.Group}
		if l.Percent != nil {
			// Stored verbatim, for the same reason toWindow stores utilization
			// verbatim: the body is already a percent.
			item.pct, item.hasPct = *l.Percent, true
		}
		item.reset, item.hasTime = parseReset(l.ResetsAt)
		if l.Scope != nil {
			if l.Scope.Model != nil {
				item.ModelDisplayName = l.Scope.Model.DisplayName
			}
			if l.Scope.Surface != nil {
				item.SurfaceDisplayName = l.Scope.Surface.DisplayName
			}
			if len(l.Scope.Other) > 0 {
				item.OtherScopes = maps.Clone(l.Scope.Other)
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
		Percent:  fromFloat(l.pct, l.hasPct),
		ResetsAt: fromTime(l.reset, l.hasTime),
	}
	if l.ModelDisplayName != "" || l.SurfaceDisplayName != "" || len(l.OtherScopes) > 0 {
		out.Scope = &scopeWire{}
		if l.ModelDisplayName != "" {
			out.Scope.Model = &displayNameWire{DisplayName: l.ModelDisplayName}
		}
		if l.SurfaceDisplayName != "" {
			out.Scope.Surface = &displayNameWire{DisplayName: l.SurfaceDisplayName}
		}
		if len(l.OtherScopes) > 0 {
			out.Scope.Other = maps.Clone(l.OtherScopes)
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
