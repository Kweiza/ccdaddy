// Package codexusage reads a Codex account's quota.
//
// One unauthenticated-by-nothing GET returns what the Claude side needs two
// calls for: the identity, the tier and both quota windows. It costs NOTHING —
// no inference request, no billed turn, no model call. That matters because the
// obvious alternative, which the largest prior-art proxy in this field actually
// ships, is to POST a minimal real inference request and cancel the body once
// the rate-limit headers arrive. That measures quota with money.
//
// What this package does NOT do, and the reasons are not stylistic:
//
//   - it never spoofs a browser user agent. At least one shipped tool does,
//     with an in-source note that the honest client agent triggers a
//     Cloudflare challenge. ccdad names itself; if an endpoint starts
//     challenging, this feature degrades rather than evades.
//   - it never polls faster than the poll policy's Codex floor. The endpoint
//     advertises no budget: no Retry-After on a good response, no
//     x-ratelimit-* headers, and upstream's own client never polls on a timer
//     at all. A background poller is therefore a traffic shape no official
//     client emits, so the floor is timid by construction and is tightened
//     only against measurement.
package codexusage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// UsageURL is the quota endpoint.
//
// The path prefix is not interchangeable and this is measured, not assumed:
// /backend-api/wham/usage answers 200, and /backend-api/api/codex/usage answers
// a Cloudflare-shaped 403. Upstream picks between the two by whether the base
// URL contains /backend-api, and ccdad only ever speaks to the one that works.
const UsageURL = "https://chatgpt.com/backend-api/wham/usage"

// usageURL is what a request is built against. It is a package var so a test
// can point it at an httptest server; production never reassigns it.
var usageURL = UsageURL

// usageTimeout bounds one poll. It matches the five seconds the Claude-side
// usage client allows: a poll that outlives the cadence that scheduled it is a
// poll that overlaps the next one.
const usageTimeout = 5 * time.Second

// maxUsageBytes caps the body. The measured response is 940 bytes; this is
// three orders of magnitude of headroom, and it is a cap because the process
// reading it may be a long-lived daemon.
const maxUsageBytes = 1 << 20

// Identity is who the reading was about.
//
// UserID is the ACCOUNT and AccountID is the WORKSPACE, and they answer
// different questions: two seats in one team workspace share the second, so
// keying an account on it would let a colleague's login overwrite the first.
type Identity struct {
	UserID    string
	AccountID string
	Email     string
	PlanType  string
}

type wire struct {
	UserID    string         `json:"user_id"`
	AccountID string         `json:"account_id"`
	Email     string         `json:"email"`
	PlanType  string         `json:"plan_type"`
	RateLimit *rateLimitWire `json:"rate_limit"`
}

type rateLimitWire struct {
	PrimaryWindow   *windowWire `json:"primary_window"`
	SecondaryWindow *windowWire `json:"secondary_window"`
}

// windowWire is one window. Every field is a pointer because every one of them
// is nullable and the difference between null and a value is the difference
// between "unknown" and a number a ranking will act on.
type windowWire struct {
	UsedPercent *float64 `json:"used_percent"`
	// LimitWindowSeconds is how long the window runs. It is DATA, not a label:
	// the plans do not agree on it, and a lapsed free tier reports one 30-day
	// window where a paid seat reports five hours and a week.
	LimitWindowSeconds *float64 `json:"limit_window_seconds"`
	// ResetAt is epoch seconds, and it is the field ccdad reads.
	// reset_after_seconds names the same instant as a countdown, so reading it
	// instead would build the reset out of the request's own latency.
	ResetAt *float64 `json:"reset_at"`
}

// usageFields are the keys that make a body a reading. A 200 carrying none of
// them is an in-band error rather than an account with nothing to report, and
// reading it as the latter would report every window unknown while the
// endpoint was plainly saying something else.
var usageFields = [...]string{"user_id", "account_id", "email", "plan_type", "rate_limit"}

// Parse turns a /wham/usage body into a snapshot and the identity beside it.
//
// It is pure so the pinned live capture can be a fixture: the one measurement
// this whole package rests on is a real 200 from a real account, and a parser
// that needed a server to exercise would be tested against a body somebody
// made up instead.
func Parse(body []byte) (*usage.Snapshot, Identity, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		return nil, Identity{}, fmt.Errorf("the codex usage endpoint returned a body that is not a JSON object")
	}
	// A literal `null` unmarshals into a nil map without error.
	if keys == nil {
		return nil, Identity{}, fmt.Errorf("the codex usage endpoint returned a null body")
	}
	known := false
	for _, k := range usageFields {
		if _, ok := keys[k]; ok {
			known = true
			break
		}
	}
	if !known {
		return nil, Identity{}, fmt.Errorf("the codex usage endpoint returned a body with no usage fields: %w", usage.ErrNoUsageFields)
	}

	var w wire
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, Identity{}, fmt.Errorf("the codex usage endpoint returned a body ccdad could not read")
	}

	snap := &usage.Snapshot{}
	if w.RateLimit != nil {
		snap.CodexPrimary = toWindow(w.RateLimit.PrimaryWindow)
		snap.CodexSecondary = toWindow(w.RateLimit.SecondaryWindow)
	}
	return snap, Identity{
		UserID:    w.UserID,
		AccountID: w.AccountID,
		Email:     w.Email,
		// Verbatim. An unrecognized tier is one this build has not seen, not an
		// error: upstream's own type keeps an unknown plan as a string rather
		// than failing the parse.
		PlanType: w.PlanType,
	}, nil
}

// toWindow normalizes one window. A nil w is an absent key or an explicit null,
// and both are "not present".
func toWindow(w *windowWire) usage.Window {
	if w == nil {
		return usage.Window{}
	}
	var pct *float64
	if w.UsedPercent != nil && finite(*w.UsedPercent) {
		// Verbatim. The field is already a percent, so scaling it by magnitude
		// — "0.14 looks like a fraction" — is the exact bug.
		v := *w.UsedPercent
		pct = &v
	}
	var reset *time.Time
	if w.ResetAt != nil && finite(*w.ResetAt) && *w.ResetAt > 0 {
		at := time.Unix(int64(*w.ResetAt), 0).UTC()
		reset = &at
	}
	var length time.Duration
	if w.LimitWindowSeconds != nil && finite(*w.LimitWindowSeconds) && *w.LimitWindowSeconds > 0 {
		length = time.Duration(*w.LimitWindowSeconds) * time.Second
	}
	return usage.NewWindowWithLength(pct, reset, length)
}

// finite is the guard every numeric field shares. JSON cannot carry a NaN, but
// a value that reached here through anything else could — and because every
// NaN comparison is false, a NaN percentage would lose no comparison in the
// ranking and could hold first place while being the one account nobody can
// read.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Fetch reads accessToken's quota.
//
// accountID is the WORKSPACE. Sent, it populates the account-scoped fields;
// omitted, the bearer alone still answers 200 with a slightly smaller body. An
// empty value is therefore not sent at all rather than sent empty.
//
// Errors name the status and the network cause, never the token and never the
// response body.
func Fetch(ctx context.Context, client *http.Client, accessToken, accountID, version string) (*usage.Snapshot, Identity, error) {
	if client == nil {
		// The request carries the bearer in a header, and Go's default client
		// already strips Authorization crossing hosts, so a followed redirect
		// here would not leak the token the way the POST-based clients'
		// bodies would. But this endpoint never legitimately redirects, and
		// following one anyway would let whatever the redirect names answer
		// in this account's name. Returning http.ErrUseLastResponse refuses
		// that and hands the redirect response back unchanged instead.
		client = &http.Client{
			Timeout: usageTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, Identity{}, fmt.Errorf("building the codex usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	// ccdad names itself and its version. It does NOT send codex's own agent:
	// that string names a codex release this binary is not, so it would go
	// stale on codex's next release and would misreport which client made the
	// request.
	req.Header.Set("User-Agent", "ccdad/"+version)

	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, Identity{}, fmt.Errorf("the codex usage lookup was cancelled: %w", ctx.Err())
		}
		// A *url.Error carries the method, the URL and the network cause. The
		// token travels in a header, never in the URL, so this leaks nothing.
		return nil, Identity{}, fmt.Errorf("could not reach the codex usage endpoint: %w", err)
	}
	defer res.Body.Close()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxUsageBytes))
	if err != nil {
		if ctx.Err() != nil {
			return nil, Identity{}, fmt.Errorf("the codex usage lookup was cancelled: %w", ctx.Err())
		}
		return nil, Identity{}, fmt.Errorf("reading the codex usage response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		// The SAME error type the Claude usage client returns, so one poller
		// branches on one 401, one 403 and one 429 whichever provider it just
		// read. The body is dropped: it is upstream text and it can echo a
		// value ccdad sent.
		return nil, Identity{}, usage.StatusErrorFrom(res.StatusCode, res.Header, time.Now())
	}
	return Parse(data)
}
