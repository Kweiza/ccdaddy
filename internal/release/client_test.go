package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestRedirectsAreCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CCDAD_BASE_URL", srv.URL)

	if _, err := NewClient().Get(context.Background(), srv.URL+"/start", 1<<20); err == nil {
		t.Fatal("Get() followed a redirect loop forever")
	}
}
