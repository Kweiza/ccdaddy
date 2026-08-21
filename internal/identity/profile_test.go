package identity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient()
	c.BaseURL = srv.URL
	return c
}

func TestFetchProfileParsesAccountAndOrganization(t *testing.T) {
	var gotAuth, gotPath, gotCacheControl, gotMethod, gotContentType string

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotCacheControl = r.Header.Get("Cache-Control")
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		io.WriteString(w, `{
			"account":{"uuid":"acct-1","email":"user@example.com"},
			"organization":{
				"uuid":"org-1","organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x",
				"seat_tier":"standard","has_extra_usage_enabled":true,"billing_type":"subscription"
			}
		}`)
	})

	got, err := c.FetchProfile(context.Background(), "TOKEN")
	if err != nil {
		t.Fatalf("FetchProfile() = %v, want nil", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET (spec §3.2)", gotMethod)
	}
	if gotPath != "/api/oauth/profile" {
		t.Fatalf("path = %q, want /api/oauth/profile", gotPath)
	}
	if gotAuth != "Bearer TOKEN" {
		t.Fatalf("Authorization = %q, want Bearer TOKEN", gotAuth)
	}
	if gotCacheControl != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", gotCacheControl)
	}
	// Claude Code's own profile call sets Content-Type on this GET (bundle
	// function KLt), so ccdad sends the same three headers it does.
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	if got.AccountUUID != "acct-1" || got.Email != "user@example.com" {
		t.Fatalf("account = %+v", got)
	}
	if got.OrganizationType != "claude_max" || got.RateLimitTier != "default_claude_max_20x" {
		t.Fatalf("organization = %+v", got)
	}
	// OrganizationUUID is load-bearing: the ambiguous-email error in spec §5.1
	// lists each candidate's organization, so dropping it degrades that error
	// with nothing else failing.
	if got.OrganizationUUID != "org-1" {
		t.Fatalf("OrganizationUUID = %q, want org-1", got.OrganizationUUID)
	}
	if got.SeatTier != "standard" {
		t.Fatalf("SeatTier = %q, want standard", got.SeatTier)
	}
	if !got.HasExtraUsage {
		t.Fatal("HasExtraUsage = false, want true")
	}
	if got.BillingType != "subscription" {
		t.Fatalf("BillingType = %q, want subscription", got.BillingType)
	}
}

// BaseURL is exported and freely assignable, so a trailing slash must not
// change the request path.
func TestFetchProfileNormalizesATrailingSlashInBaseURL(t *testing.T) {
	var gotPath string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"account":{"uuid":"acct-1"}}`)
	})
	c.BaseURL += "/"

	if _, err := c.FetchProfile(context.Background(), "TOKEN"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/oauth/profile" {
		t.Fatalf("path = %q, want /api/oauth/profile", gotPath)
	}
}

// A response with no usable account uuid is unresolvable, not a partial success.
func TestFetchProfileRequiresAccountUUID(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"account":{"email":"user@example.com"}}`)
	})

	if _, err := c.FetchProfile(context.Background(), "TOKEN"); err == nil {
		t.Fatal("FetchProfile() = nil, want an error when account.uuid is missing")
	}
}

// A rejected credential must be distinguishable from an endpoint having a bad
// day: a caller that stores an account anyway on any error would persist a dead
// token under an unmatchable identifier.
func TestFetchProfile401IsUnauthorized(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, err := c.FetchProfile(context.Background(), "TOKEN")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want it to unwrap to ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %q, want it to name the status", err)
	}
}

func TestFetchProfile403IsUnauthorized(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	if _, err := c.FetchProfile(context.Background(), "TOKEN"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want it to unwrap to ErrUnauthorized", err)
	}
}

func TestFetchProfile500IsNotUnauthorized(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := c.FetchProfile(context.Background(), "TOKEN")
	if err == nil {
		t.Fatal("FetchProfile() = nil, want an error")
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, a 500 must not read as a dead token", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %q, want it to name the status", err)
	}
}

// The bearer token must never reach an error message: errors go to stderr and
// to the log.
func TestFetchProfileErrorWithholdsToken(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "upstream detail")
	})

	_, err := c.FetchProfile(context.Background(), "SECRET-TOKEN-VALUE")
	if err == nil {
		t.Fatal("FetchProfile() = nil, want an error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN-VALUE") {
		t.Fatalf("error leaked the token: %q", err)
	}
	if strings.Contains(err.Error(), "upstream detail") {
		t.Fatalf("error leaked the upstream body: %q", err)
	}
}

// A hostile or broken endpoint must not be able to make ccdad read an unbounded
// body into memory. Past the cap the JSON is truncated, so it cannot parse.
func TestFetchProfileCapsTheResponseBody(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"account":{"uuid":"acct-1","email":"`+strings.Repeat("a", 2<<20)+`"}}`)
	})

	if _, err := c.FetchProfile(context.Background(), "TOKEN"); err == nil {
		t.Fatal("FetchProfile() = nil, want an error for a body over the 1 MiB cap")
	}
}

func TestFetchProfileCancelledContext(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.FetchProfile(ctx, "SECRET-TOKEN-VALUE")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	// A cancelled lookup is not an unreachable endpoint, and the message has
	// to say so: "could not reach the Claude profile endpoint" would send a
	// user to debug their network over a context they closed themselves.
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %q, want it to say the lookup was cancelled", err)
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN-VALUE") {
		t.Fatalf("error leaked the token: %q", err)
	}
}

// The client carries its own deadline so a caller passing context.Background()
// cannot hang forever on a stalled endpoint.
func TestNewClientIsBounded(t *testing.T) {
	if got := NewClient().HTTP.Timeout; got <= 0 {
		t.Fatalf("NewClient().HTTP.Timeout = %v, want a bounded deadline", got)
	}
}

// The body can stop mid-read for the same reasons the request can. Reporting
// only "reading the profile response" sends a user to look at the endpoint when
// they cancelled it themselves.
func TestFetchProfileCancelledDuringTheBodyRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			io.WriteString(w, `{"account":{"uuid":"acct-1"`)
			f.Flush()
		}
		cancel()
		<-ctx.Done()
	})

	_, err := c.FetchProfile(ctx, "SECRET-TOKEN-VALUE")
	if err == nil {
		t.Fatal("FetchProfile() = nil, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %q, want it to say the lookup was cancelled", err)
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN-VALUE") {
		t.Fatalf("error leaked the token: %q", err)
	}
}
