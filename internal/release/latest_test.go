package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Location header is remote-controlled input that ends up in status.json
// and on a terminal, so this is a table over what an origin can send rather
// than a happy-path check.
func TestLatestReadsTheRedirect(t *testing.T) {
	for _, c := range []struct {
		name     string
		code     int
		location string
		want     string // "" means the call must fail
	}{
		{"302", http.StatusFound, "/Kweiza/ccdaddy/releases/tag/v0.7.0", "v0.7.0"},
		{"301", http.StatusMovedPermanently, "/Kweiza/ccdaddy/releases/tag/v0.7.0", "v0.7.0"},
		{"307", http.StatusTemporaryRedirect, "/Kweiza/ccdaddy/releases/tag/v0.7.0", "v0.7.0"},
		{"308", http.StatusPermanentRedirect, "/Kweiza/ccdaddy/releases/tag/v0.7.0", "v0.7.0"},
		{"absolute location", http.StatusFound, "https://example.test/x/releases/tag/v1.2.3", "v1.2.3"},
		{"a tag with no v is normalized", http.StatusFound, "/releases/tag/1.2.3", "v1.2.3"},
		{"a prerelease tag", http.StatusFound, "/releases/tag/v1.2.3-rc.1", "v1.2.3-rc.1"},
		{"no redirect at all", http.StatusOK, "", ""},
		{"a redirect with no Location", http.StatusFound, "", ""},
		{"no /tag/ segment", http.StatusFound, "/releases/latest", ""},
		{"a segment merely containing tag", http.StatusFound, "/releases/nottag/v1.2.3", ""},
		{"a last segment that is not a version", http.StatusFound, "/releases/tag/latest", ""},
		{"a last segment that is a path", http.StatusFound, "/releases/tag/v1.2.3%2F..%2F..", ""},
		// Pins "unescaped AFTER the split" specifically: a %2F standing in
		// for the segment separator itself. Splitting the DECODED path
		// instead of EscapedPath would read this as ["releases", "tag",
		// "v9.9.9"] — segment before last equal to "tag" — and hand back
		// v9.9.9 as though the origin had actually named a release tag,
		// rather than smuggled one past the segment check with an encoded
		// slash. The two-dots case above does not pin this: decoding it
		// early still leaves the segment before last as "..", which is not
		// "tag" either, so that mutation stays green under it for the wrong
		// reason.
		{"an encoded tag segment separator", http.StatusFound, "/releases/tag%2Fv9.9.9", ""},
		// A 200 that DOES carry a Location must still be refused by the
		// status-range guard, not merely happen to fail because these other
		// 200/500 rows carry no Location at all: with no Location value they
		// would fail on the empty-Location check just as well, so deleting
		// the status-range guard entirely leaves the suite green while a 200
		// carrying a real-looking Location would then be accepted as the
		// published latest release.
		{"a 200 with a Location header is still not a redirect", http.StatusOK, "/releases/tag/v9.9.9", ""},
		{"a 500", http.StatusInternalServerError, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.location != "" {
					w.Header().Set("Location", c.location)
				}
				w.WriteHeader(c.code)
			}))
			t.Cleanup(srv.Close)
			t.Setenv("CCDAD_BASE_URL", srv.URL)

			got, err := NewClient().Latest(context.Background())
			if c.want == "" {
				if err == nil {
					t.Fatalf("Latest() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Latest() error = %v", err)
			}
			if got != c.want {
				t.Errorf("Latest() = %q, want %q", got, c.want)
			}
		})
	}
}

// The redirect is the answer, so the body must never be fetched or read: the
// request is not allowed to follow the hop.
func TestLatestDoesNotFollowTheRedirect(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		http.Redirect(w, r, "/releases/tag/v0.7.0", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CCDAD_BASE_URL", srv.URL)

	if _, err := NewClient().Latest(context.Background()); err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if len(paths) != 1 || paths[0] != "/latest" {
		t.Errorf("origin saw %v, want exactly one request for /latest", paths)
	}
}
