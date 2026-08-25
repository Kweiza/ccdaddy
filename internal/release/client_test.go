package release

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
)

// serve stands up an origin, points CCDAD_BASE_URL at it, and returns a Client
// built the way production builds one. The environment variable is the seam —
// there is no second one — so a test that reaches the client reaches the same
// code path `ccdad update` does.
func serve(t *testing.T, h http.HandlerFunc) (*Client, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("CCDAD_BASE_URL", srv.URL)
	return NewClient(), srv.URL
}

func TestGetReadsABody(t *testing.T) {
	c, base := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})
	body, err := c.Get(context.Background(), base+"/x", 1<<20)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("Get() = %q, want %q", body, "hello")
	}
}

// The status has to survive as a number, because ONE of them means something
// specific downstream: a 404 on the signature is a release that genuinely
// carries none, and a 500 says nothing about the release at all.
func TestGetCarriesTheStatus(t *testing.T) {
	for _, code := range []int{404, 500, 403, 301} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			c, base := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			})
			_, err := c.Get(context.Background(), base+"/x", 1<<20)
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("Get() error = %v, want a *StatusError", err)
			}
			if se.Status != code {
				t.Errorf("StatusError.Status = %d, want %d", se.Status, code)
			}
		})
	}
}

// A cap that TRUNCATES hands a prefix of an over-long body back to a parser
// that will treat it as the whole thing. This one refuses.
func TestGetRefusesAnOverLongBodyRatherThanTruncatingIt(t *testing.T) {
	c, base := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	})
	if _, err := c.Get(context.Background(), base+"/x", 99); err == nil {
		t.Fatal("Get() accepted a body larger than its limit")
	}
	if _, err := c.Get(context.Background(), base+"/x", 100); err != nil {
		t.Fatalf("Get() refused a body exactly at its limit: %v", err)
	}
}

// GitHub bounces an asset to another host, so redirects are followed here even
// though the oauth client in this tree refuses them all. What is kept from that
// refusal is the half that still matters with no credential on the wire.
func TestRedirectsMayNotChangeTheScheme(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/end", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("arrived"))
	}))
	t.Cleanup(srv.Close)

	// An http base: an http hop is the same scheme, so it is allowed.
	t.Setenv("CCDAD_BASE_URL", srv.URL)
	if _, err := NewClient().Get(context.Background(), srv.URL+"/start", 1<<20); err != nil {
		t.Fatalf("an http base refused an http hop: %v", err)
	}

	// An https base: the SAME http hop must now be refused. Nothing dials the
	// https base — the rule is about the hop, and the first request's scheme is
	// the base's by construction.
	t.Setenv("CCDAD_BASE_URL", "https://releases.example.invalid")
	_, err := NewClient().Get(context.Background(), srv.URL+"/start", 1<<20)
	if err == nil {
		t.Fatal("an https base followed a redirect down to http")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error = %v, want it to name the redirect it refused", err)
	}
}

// err == nil is not enough here on its own: deleting the cap turns this into
// an infinite redirect loop rather than an error, which this test would only
// notice via go test's own multi-minute timeout taking the whole package down
// with it — a real failure, but not one this test would get credit for
// catching on its own terms. What actually distinguishes "the configured cap
// fired" from "some other error happened to fire first" is the request count
// the origin saw and the number named in the message: a cap of 1 paired with
// an unrelated message would satisfy err != nil alone, so both are asserted.
func TestRedirectsAreCapped(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CCDAD_BASE_URL", srv.URL)

	_, err := NewClient().Get(context.Background(), srv.URL+"/start", 1<<20)
	if err == nil {
		t.Fatal("Get() followed a redirect loop forever")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d redirects", maxRedirects)) {
		t.Errorf("error = %v, want it to name %d redirects", err, maxRedirects)
	}
	// CheckRedirect is called before the (maxRedirects+1)th request is ever
	// sent, so exactly maxRedirects requests reach the origin when the cap is
	// the one enforcing this and nothing else is.
	if hops != maxRedirects {
		t.Errorf("origin saw %d requests, want exactly %d", hops, maxRedirects)
	}
}

// The scheme rule's fallback is meant to fail CLOSED — see schemeOf's own
// comment — and nothing exercised either half of that before now: neither a
// base url.Parse itself refuses, nor a base that parses but names no scheme
// at all (which is what CCDAD_BASE_URL=host.example.com without a scheme
// produces, since BaseURL only trims a trailing slash and does not require
// one).
func TestSchemeOf(t *testing.T) {
	for _, c := range []struct{ name, base, want string }{
		{"a normal https base", "https://releases.example.invalid", "https"},
		{"a normal http base", "http://127.0.0.1:1234", "http"},
		// The mutation "return u.Scheme unconditionally" answers "" here
		// instead of "https", and an empty scheme matches no hop's scheme
		// ever, which silently refuses every redirect rather than announcing
		// that the base is malformed.
		{"a base with no scheme at all", "releases.example.com", "https"},
		// A base url.Parse itself refuses, exercising the err != nil half of
		// the fallback separately from the empty-scheme half above.
		{"an unparseable base", "://bad", "https"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := schemeOf(c.base); got != c.want {
				t.Errorf("schemeOf(%q) = %q, want %q", c.base, got, c.want)
			}
		})
	}
}

// userAgent's doc comment argues at length that it must carry the version and
// nothing else; nothing in the package inspected the header the client
// actually sends, so that argument had no executable form and a hostname or
// an install id could have been appended without turning anything red.
func TestUserAgentNamesOnlyTheVersion(t *testing.T) {
	var got string
	c, base := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
	})
	if _, err := c.Get(context.Background(), base+"/x", 1<<20); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if want := "ccdad/" + buildinfo.Version; got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
}

// fakeRoundTripper stands in for "something else in the process replaced
// http.DefaultTransport with a RoundTripper that is not *http.Transport" —
// the one case newTransport's type assertion cannot Clone().
type fakeRoundTripper struct{}

func (fakeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("fakeRoundTripper: no real network here")
}

// internal/oauth/token.go faces the identical hazard and its own comment
// explains why it leaves Transport nil rather than asserting and panicking.
// The request is exercised here too, not just NewClient() itself: a fix that
// left Transport holding a typed-nil *http.Transport (a non-nil
// http.RoundTripper wrapping a nil pointer) would still let NewClient()
// return cleanly and only panic one call later, on the first Do — which
// construction alone would not catch.
func TestNewClientSurvivesAReplacedDefaultTransport(t *testing.T) {
	old := http.DefaultTransport
	http.DefaultTransport = fakeRoundTripper{}
	t.Cleanup(func() { http.DefaultTransport = old })

	if _, err := NewClient().Get(context.Background(), "http://release.example.invalid/x", 1<<20); err == nil {
		t.Fatal("Get() through a fake DefaultTransport reported success, want the fake's own error")
	}
}
