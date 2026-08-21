package usage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/oauth"
)

const (
	// usageTimeout is what Claude Code gives this call — five seconds, not the
	// ten it gives the profile call. The two are different calls in the bundle
	// (`eWe` versus `i$t`) and copying the profile's budget here would let one
	// poll outlive the cadence that scheduled it.
	usageTimeout = 5 * time.Second

	// maxUsageBytes caps the body. The endpoint is upstream and this process is
	// a long-lived daemon; an unbounded ReadAll is an unbounded allocation.
	maxUsageBytes = 1 << 20

	// BetaHeader is the anthropic-beta value Claude Code's OAuth request path
	// attaches to this call (`NI` in the 2.1.239 bundle). It is set by Claude
	// Code's own code, not by axios beneath it, so ccdad matches it.
	BetaHeader = "oauth-2025-04-20"

	usagePath = "/api/oauth/usage"
)

// ErrUnauthorized means the usage endpoint rejected the credential itself.
//
// It is deliberately this package's own sentinel and not identity's: they are
// different endpoints, and a caller that fetches usage checks the error the
// usage client returns. What it means here is narrower than "the account is
// dead" — Claude Code answers a 401 on this call by refreshing the token and
// retrying once (`refreshOAuth:true`, then `Uzd` -> "retry"), so a caller must
// read this as "refresh and try again", never as "quarantine the account".
var ErrUnauthorized = errors.New("the usage endpoint rejected the token")

// StatusError is a non-200 from the usage endpoint. It carries the status and
// the Retry-After the endpoint offered, and nothing else: the token is a live
// credential and the body is upstream text.
type StatusError struct {
	Status int

	retryAfter    time.Duration
	hasRetryAfter bool
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("the usage endpoint refused the request (HTTP %d)", e.Status)
}

// RetryAfter is the wait the endpoint asked for, and whether it asked at all.
// §7.4's AIMD backoff needs the difference: an absent header is not a zero wait.
func (e *StatusError) RetryAfter() (time.Duration, bool) {
	return e.retryAfter, e.hasRetryAfter
}

// Unwrap reports the two conditions a caller acts on differently. A rejected
// credential means refresh and retry; a 429 means the shared per-identity budget
// is saturated and the poll policy must back off. Everything else is just a bad
// day upstream and unwraps to nothing.
func (e *StatusError) Unwrap() error {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusTooManyRequests:
		return ErrRateLimited
	}
	return nil
}

// Client calls the usage endpoint.
type Client struct {
	HTTP    *http.Client
	BaseURL string
}

// NewClient returns a Client with a bounded timeout and the stdlib transport, so
// proxy environment variables keep working.
func NewClient() *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	return &Client{
		HTTP:    &http.Client{Transport: tr, Timeout: usageTimeout},
		BaseURL: oauth.APIBaseURL,
	}
}

// parseRetryAfter reads the delay-seconds form of Retry-After. The HTTP-date
// form is legal too, but this endpoint sends seconds and a date parsed against a
// skewed clock is worse than no answer, so only the seconds form is accepted.
func parseRetryAfter(h http.Header) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// FetchUsage reads accessToken's account usage.
//
// Errors name the HTTP status and the network cause, never the token and never
// the response body.
func (c *Client) FetchUsage(ctx context.Context, accessToken string) (*Snapshot, error) {
	// BaseURL is exported and freely assignable, so normalize the join rather
	// than letting a trailing slash change the request path.
	url := strings.TrimRight(c.BaseURL, "/") + usagePath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building the usage request: %w", err)
	}
	// The three headers Claude Code's own usage call ends up sending: the call
	// site (`eWe`) sets Content-Type, and its OAuth client (`Gnr` -> `l$e`)
	// supplies Authorization and anthropic-beta. Cache-Control is NOT among
	// them — that one belongs to the profile call — and Accept is only ever
	// added by axios, so ccdad does not forge it.
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", BetaHeader)

	res, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the usage lookup was cancelled: %w", ctx.Err())
		}
		// A *url.Error carries the method, the URL and the network cause. The
		// token travels in a header, never in the URL, so this leaks nothing.
		return nil, fmt.Errorf("could not reach the Claude usage endpoint: %w", err)
	}
	defer res.Body.Close()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxUsageBytes))
	if err != nil {
		// The body can stop mid-read for the same reasons the request can, and
		// reporting only "reading the usage response" would send a user to look
		// at the endpoint when they cancelled it themselves.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the usage lookup was cancelled: %w", ctx.Err())
		}
		return nil, fmt.Errorf("reading the usage response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		e := &StatusError{Status: res.StatusCode}
		e.retryAfter, e.hasRetryAfter = parseRetryAfter(res.Header)
		return nil, e
	}

	return Parse(data)
}
