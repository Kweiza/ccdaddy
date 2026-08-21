// Package identity resolves an OAuth token to the account it belongs to and
// classifies that account.
//
// One call to GET /api/oauth/profile answers both questions at once: it carries
// the email that labels the account AND the organization fields that decide
// whether the account is subscription-billed or credit-billed.
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/oauth"
)

const (
	profileTimeout  = 10 * time.Second
	maxProfileBytes = 1 << 20
)

// ErrUnauthorized means the endpoint rejected the credential itself, as opposed
// to failing for a reason a retry could fix. A dead token must not be stored as
// an account.
var ErrUnauthorized = errors.New("the profile endpoint rejected the token")

// StatusError is a non-200 from the profile endpoint. It carries the status and
// nothing else: the token is a live credential and the body is upstream text.
type StatusError struct{ Status int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("the profile endpoint refused the request (HTTP %d)", e.Status)
}

// Unwrap reports a rejected credential as ErrUnauthorized so a caller can tell
// "this token is dead" from "the endpoint is having a bad day" without
// inspecting the number.
func (e *StatusError) Unwrap() error {
	if e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden {
		return ErrUnauthorized
	}
	return nil
}

// Profile is what /api/oauth/profile tells us about a token's owner.
type Profile struct {
	// AccountUUID is account.uuid from the profile endpoint. The primary key.
	AccountUUID      string
	Email            string
	OrganizationUUID string

	// OrganizationType maps to the subscription tier: pro, claude_max, team,
	// enterprise.
	OrganizationType string
	RateLimitTier    string
	SeatTier         string

	// BillingType is the one profile field that is evidence of how the account
	// is metered; see Classify.
	BillingType string
	// HasExtraUsage is the organization's has_extra_usage_enabled overage
	// switch. It is recorded as a secondary axis and is deliberately NOT
	// classification evidence: subscription organizations turn it on too.
	HasExtraUsage bool
}

// Client calls the profile endpoint.
type Client struct {
	HTTP    *http.Client
	BaseURL string
}

// NewClient returns a Client with a bounded timeout and the stdlib transport,
// so proxy environment variables keep working.
func NewClient() *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	return &Client{
		HTTP:    &http.Client{Transport: tr, Timeout: profileTimeout},
		BaseURL: oauth.APIBaseURL,
	}
}

// wire mirrors the endpoint's JSON shape.
type wire struct {
	Account struct {
		UUID  string `json:"uuid"`
		Email string `json:"email"`
	} `json:"account"`
	Organization struct {
		UUID                 string `json:"uuid"`
		OrganizationType     string `json:"organization_type"`
		RateLimitTier        string `json:"rate_limit_tier"`
		SeatTier             string `json:"seat_tier"`
		HasExtraUsageEnabled bool   `json:"has_extra_usage_enabled"`
		BillingType          string `json:"billing_type"`
	} `json:"organization"`
}

// FetchProfile resolves accessToken to its account.
//
// Errors name the HTTP status and the network cause, never the token and never
// the response body: the token is a live credential and the body is upstream
// text, and neither belongs in a message that reaches stderr or a log file.
func (c *Client) FetchProfile(ctx context.Context, accessToken string) (*Profile, error) {
	// BaseURL is exported and freely assignable, so normalize the join rather
	// than letting a trailing slash change the request path.
	url := strings.TrimRight(c.BaseURL, "/") + "/api/oauth/profile"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building the profile request: %w", err)
	}
	// These are exactly the three headers Claude Code's own profile call sets
	// (function KLt in the 2.1.238 bundle: `ui.get(t, {headers:{Authorization,
	// "Content-Type":"application/json", "Cache-Control":"no-cache"},
	// timeout:1e4})`). Content-Type is meaningless on a bodyless GET, but spec
	// §3.2's rule is to match the headers Claude Code sets in its own code and
	// not to forge the ones axios adds beneath it — so it stays, and Accept
	// (which only axios contributes) is not sent.
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")

	res, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the profile lookup was cancelled: %w", ctx.Err())
		}
		// A *url.Error carries the method, the URL and the network cause. The
		// token travels in a header, never in the URL, so this leaks nothing.
		return nil, fmt.Errorf("could not reach the Claude profile endpoint: %w", err)
	}
	defer res.Body.Close()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxProfileBytes))
	if err != nil {
		return nil, fmt.Errorf("reading the profile response")
	}
	if res.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: res.StatusCode}
	}

	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("the profile endpoint returned a body that is not valid JSON")
	}
	// The uuid is the account's durable primary key. A response without one is
	// unresolvable rather than partially useful, so refuse it outright instead
	// of storing an account that can never be matched again.
	if w.Account.UUID == "" {
		return nil, fmt.Errorf("the profile response carried no account uuid")
	}

	return &Profile{
		AccountUUID:      w.Account.UUID,
		Email:            w.Account.Email,
		OrganizationUUID: w.Organization.UUID,
		OrganizationType: w.Organization.OrganizationType,
		RateLimitTier:    w.Organization.RateLimitTier,
		SeatTier:         w.Organization.SeatTier,
		BillingType:      w.Organization.BillingType,
		HasExtraUsage:    w.Organization.HasExtraUsageEnabled,
	}, nil
}
