package usage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serve stands up a usage endpoint and returns a Client pointed at it.
func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := NewClient()
	c.BaseURL = srv.URL
	return c
}

func okBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestFetchUsageParsesAResponse(t *testing.T) {
	c := serve(t, okBody(realBody))

	s, err := c.FetchUsage(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if pct, ok := s.FiveHour.Percent(); !ok || pct != 92.5 {
		t.Errorf("FiveHour.Percent() = %v, %v; want 92.5", pct, ok)
	}
	if s.ExtraUsage.State != ExtraUsageEnabled {
		t.Errorf("ExtraUsage.State = %v, want enabled", s.ExtraUsage.State)
	}
}

// The usage call is NOT the profile call. Claude Code's own code sends
// Authorization + Content-Type + anthropic-beta here and Cache-Control there;
// the rule is to match the headers Claude Code sets and forge nothing axios
// adds beneath them.
func TestFetchUsageSendsTheHeadersClaudeCodeSends(t *testing.T) {
	var got http.Header
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, path = r.Header.Clone(), r.URL.Path
		_, _ = w.Write([]byte(realBody))
	}))
	defer srv.Close()

	c := NewClient()
	c.BaseURL = srv.URL
	if _, err := c.FetchUsage(context.Background(), "s3cret-token"); err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}

	if path != "/api/oauth/usage" {
		t.Errorf("path = %q, want /api/oauth/usage", path)
	}
	if v := got.Get("Authorization"); v != "Bearer s3cret-token" {
		t.Errorf("Authorization = %q", v)
	}
	if v := got.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", v)
	}
	// The literal, not the constant FetchUsage also reads: comparing BetaHeader
	// against BetaHeader through an HTTP round trip can never fail for a wrong
	// value. `oauth-2025-04-20` is what Claude Code's own usage call sends.
	if v := got.Get("anthropic-beta"); v != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q, want oauth-2025-04-20", v)
	}
	if BetaHeader != "oauth-2025-04-20" {
		t.Errorf("BetaHeader = %q, want oauth-2025-04-20", BetaHeader)
	}
	if v := got.Get("Cache-Control"); v != "" {
		t.Errorf("Cache-Control = %q; the usage call does not set it — only the profile call does", v)
	}
	if v := got.Get("Accept"); v != "" {
		t.Errorf("Accept = %q; only axios adds it, so ccdad must not forge it", v)
	}
	// Claude Code's own client sets a FOURTH first-party header here,
	// `User-Agent: claude-cli/<version> (external, cli)`. ccdad deliberately
	// does not send it: it names a Claude Code version ccdad is not, and
	// pinning one is the same lie ccdad already refuses to tell for axios's
	// version string.
	if v := got.Get("User-Agent"); strings.Contains(v, "claude-cli") {
		t.Errorf("User-Agent = %q; ccdad must not claim to be a Claude Code build", v)
	}
}

func TestFetchUsageNormalizesATrailingSlashInTheBaseURL(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(realBody))
	}))
	defer srv.Close()

	c := NewClient()
	c.BaseURL = srv.URL + "/"
	if _, err := c.FetchUsage(context.Background(), "tok"); err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if path != "/api/oauth/usage" {
		t.Errorf("path = %q, want /api/oauth/usage", path)
	}
}

// A 401 is "the token is stale, refresh it". Claude Code refreshes and retries
// this call unconditionally on a 401.
func TestFetchUsageReportsAStaleToken(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, err := c.FetchUsage(context.Background(), "tok")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Errorf("error did not carry the status: %v", err)
	}
}

// A 403 is NOT the same condition, and folding it into the 401 would have ccdad
// refresh a perfectly good token forever against an organization that has
// withdrawn access. Claude Code's own retry wrapper refreshes a 401
// unconditionally but retries a 403 only for callers that opt in — and the usage
// call does not, so its 403 is rethrown on the first response.
func TestFetchUsageDoesNotTreatAForbiddenAsRefreshable(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	_, err := c.FetchUsage(context.Background(), "tok")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Error("a 403 also read as ErrUnauthorized; refreshing the token cannot fix a refusal")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusForbidden {
		t.Errorf("error did not carry the status: %v", err)
	}
}

func TestFetchUsageDoesNotReadAServerErrorAsARejectedCredential(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusNotFound} {
		c := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		})
		_, err := c.FetchUsage(context.Background(), "tok")
		if err == nil {
			t.Fatalf("HTTP %d: FetchUsage() returned no error", code)
		}
		if errors.Is(err, ErrUnauthorized) {
			t.Errorf("HTTP %d read as a rejected credential; only 401 and 403 are", code)
		}
	}
}

// A 429 is the poll policy's signal, and it is the same condition as the in-band
// envelope, so it reads as the same error.
func TestFetchUsageReportsAHardRateLimit(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := c.FetchUsage(context.Background(), "tok")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *StatusError", err)
	}
	d, ok := se.RetryAfter()
	if !ok || d != 42*time.Second {
		t.Errorf("RetryAfter() = %v, %v; want 42s", d, ok)
	}
}

// Retry-After is legally an HTTP-date as well as delta-seconds. Accepting only
// the integer form is defensible — a date parsed against a skewed clock is
// worse than no answer — but the parser resolves that by refusing a date
// already in the PAST rather than by refusing the form. Discarding a legal
// header means ignoring a wait the endpoint asked for, and the next request
// earns another 429.
func TestFetchUsageReadsAnHTTPDateRetryAfter(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := c.FetchUsage(context.Background(), "tok")
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *StatusError", err)
	}
	d, ok := se.RetryAfter()
	if !ok {
		t.Fatal("RetryAfter() reported absent for a legal HTTP-date header")
	}
	// Whole seconds, and the round trip costs a few milliseconds of it.
	if d < 85*time.Second || d > 90*time.Second {
		t.Errorf("RetryAfter() = %v, want about 90s", d)
	}
}

func TestFetchUsageLeavesAnAbsentRetryAfterUnknown(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := c.FetchUsage(context.Background(), "tok")
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *StatusError", err)
	}
	if d, ok := se.RetryAfter(); ok {
		t.Errorf("RetryAfter() = %v, ok = true; an absent header must not read as a zero wait", d)
	}
}

// A rate limit can also arrive inside a 200. It must reach the caller as the
// same condition, or the poll policy backs off for one and not the other.
func TestFetchUsageReportsAnInBandRateLimit(t *testing.T) {
	c := serve(t, okBody(`{"error": {"type": "rate_limit_error"}}`))

	if _, err := c.FetchUsage(context.Background(), "tok"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("error = %v, want ErrRateLimited", err)
	}
}

func TestFetchUsageReportsAFieldlessBody(t *testing.T) {
	c := serve(t, okBody(`{"detail": "nope"}`))

	if _, err := c.FetchUsage(context.Background(), "tok"); !errors.Is(err, ErrNoUsageFields) {
		t.Errorf("error = %v, want ErrNoUsageFields", err)
	}
}

// The token is a live credential and the body is upstream text. Neither belongs
// in a message that reaches stderr or a log file — and that has to hold on every
// path that can produce an error, transport failures included, not only on the
// ones that read a response.
func TestFetchUsageErrorsNameNeitherTheTokenNorTheBody(t *testing.T) {
	const token = "sk-ant-oat01-do-not-log-me"
	const secret = "internal-hostname-and-a-stack-trace"
	secretBody := `{"detail":"` + secret + `"}`

	// Each case returns a client and the context to call it with, so a case can
	// fail the request at the transport rather than in the response.
	cases := []struct {
		name string
		call func(t *testing.T) (*Client, context.Context)
	}{
		{"401", handlerCase(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(secretBody))
		})},
		{"500", handlerCase(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(secretBody))
		})},
		{"fieldless 200", handlerCase(okBody(secretBody))},
		{"bad json", handlerCase(okBody(`{"five_hour":`))},
		// Passes the eight-key probe and fails on the typed read, which is the
		// only path whose message is built from the body itself.
		{"known key, wrong type", handlerCase(okBody(`{"five_hour": {"utilization": "` + secret + `"}}`))},
		// A body that is not an object at all, carrying the secret as its whole
		// value.
		{"non-object body", handlerCase(okBody(`"` + secret + `"`))},
		{"unreachable endpoint", func(t *testing.T) (*Client, context.Context) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			url := srv.URL
			srv.Close()
			c := NewClient()
			c.BaseURL = url
			return c, context.Background()
		}},
		{"cancelled context", func(t *testing.T) (*Client, context.Context) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-release
			}))
			t.Cleanup(func() { close(release); srv.Close() })
			c := NewClient()
			c.BaseURL = srv.URL
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return c, ctx
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := tc.call(t)
			_, err := c.FetchUsage(ctx, token)
			if err == nil {
				t.Fatal("FetchUsage() returned no error")
			}
			msg := err.Error()
			if strings.Contains(msg, token) {
				t.Errorf("the error names the token: %q", msg)
			}
			if strings.Contains(msg, secret) {
				t.Errorf("the error quotes upstream text: %q", msg)
			}
		})
	}
}

// handlerCase adapts a plain handler to the leak table's client factory.
func handlerCase(h http.HandlerFunc) func(*testing.T) (*Client, context.Context) {
	return func(t *testing.T) (*Client, context.Context) {
		return serve(t, h), context.Background()
	}
}

// An endpoint that streams forever must not be able to exhaust this process.
// The body is capped, and a body past the cap is truncated into a parse failure
// rather than being read to the end.
func TestFetchUsageCapsTheBodyItReads(t *testing.T) {
	pad := strings.Repeat("a", 2<<20)
	body := `{"five_hour":{"utilization":42,"resets_at":null},"pad":"` + pad + `"}`
	c := serve(t, okBody(body))

	s, err := c.FetchUsage(context.Background(), "tok")
	if err == nil {
		pct, _ := s.FiveHour.Percent()
		t.Fatalf("FetchUsage() read a %d-byte body to the end (got utilization %v); it must stop at the cap", len(body), pct)
	}
}

func TestFetchUsageHonoursACancelledContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := NewClient()
	c.BaseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.FetchUsage(ctx, "tok")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

func TestFetchUsageReportsAnUnreachableEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient()
	c.BaseURL = url

	if _, err := c.FetchUsage(context.Background(), "tok"); err == nil {
		t.Error("FetchUsage() returned no error for a closed endpoint")
	}
}

// Claude Code gives this call five seconds, not the ten it gives the profile
// call. A poll that outlives its own cadence is a poll that queues behind
// itself.
func TestNewClientUsesTheTimeoutClaudeCodeUses(t *testing.T) {
	if got := NewClient().HTTP.Timeout; got != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", got)
	}
}

func TestNewClientPointsAtTheAnthropicAPI(t *testing.T) {
	if got := NewClient().BaseURL; got == "" || !strings.HasPrefix(got, "https://") {
		t.Errorf("BaseURL = %q", got)
	}
}
