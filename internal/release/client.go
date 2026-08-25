package release

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
)

// maxRedirects caps the hop chain. A real asset download is one hop —
// github.com bounces it to objects.githubusercontent.com — and ten is the
// stdlib's own default, kept so a mirror with a chain of its own still works
// while a loop still ends.
const maxRedirects = 10

// StatusError is a non-200 answer, carrying the code because one of them means
// something specific: a 404 on sha256sums.txt.minisig is a release that
// genuinely carries no signature, which is a refusal a user can act on, while a
// 500, a timeout or a DNS failure says nothing about the release at all.
// Reporting the second as a tamper verdict would let a flaky origin manufacture
// a permanent-looking one.
type StatusError struct {
	URL    string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s answered HTTP %d", e.URL, e.Status)
}

// Client fetches release metadata and assets.
type Client struct {
	base string
	// follow is for assets and metadata; noFollow is for the one request whose
	// ANSWER is the redirect.
	follow   *http.Client
	noFollow *http.Client
}

// NewClient builds a client pinned to the configured origin.
func NewClient() *Client {
	c := &Client{base: BaseURL()}
	c.follow = &http.Client{
		Transport: newTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			// internal/oauth's client refuses EVERY redirect, and that is a
			// credential-leak guard; copying it here would break every
			// download, because the asset lives on another host. What is kept
			// is the half that still matters with no credential on the wire:
			// no hop may change the scheme the configured base uses. With the
			// default base that is https-only, and with a CCDAD_BASE_URL of
			// http://127.0.0.1:… a test origin can still redirect in
			// plaintext — but no configuration can silently walk an https base
			// down to http.
			if got, want := req.URL.Scheme, schemeOf(c.base); got != want {
				return fmt.Errorf("refusing a redirect from %s to %s: a %s base may not be walked down", want, got, want)
			}
			return nil
		},
	}
	c.noFollow = &http.Client{
		Transport:     newTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// newTransport clones the stdlib transport, which is the idiom internal/usage
// already uses and it is there so HTTPS_PROXY keeps working.
//
// http.DefaultTransport.Clone() does not compile: DefaultTransport is declared
// as a RoundTripper, so the type assertion is part of the idiom rather than
// defensive noise.
func newTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	// Stated rather than inherited. The stdlib's floor has moved before, and a
	// channel that hands this process a new executable is not the place to find
	// out which way it moved next.
	tr.TLSClientConfig.MinVersion = tls.VersionTLS12
	return tr
}

// schemeOf is the scheme of a base URL, defaulting to https.
//
// Failing CLOSED: a base nobody can parse must not be the thing that switches
// the scheme rule off.
func schemeOf(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		return "https"
	}
	return u.Scheme
}

// userAgent names the version and nothing else.
//
// The version IS the question a version check asks. An OS, an architecture, a
// hostname, an install id or an account count would each turn one request into
// a fingerprint, and there is no jar on either client — the live origin does
// set cookies, a nil jar discards them, and a jar added later would quietly
// make a once-a-day check a tracked session.
func userAgent() string { return "ccdad/" + buildinfo.Version }

// Get reads a whole small body.
//
// limit is a REFUSAL, not a truncation. An io.LimitReader on its own hands back
// a prefix of an over-long body, and every caller here goes on to parse what it
// got as though it were the whole file.
func (c *Client) Get(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building a request for %s: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := c.follow.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{URL: rawURL, Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", rawURL, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes, which is not the file it claims to be", rawURL, limit)
	}
	return body, nil
}

// trimmedBase is the configured origin with no trailing slash, for the callers
// in this file that join onto it.
func (c *Client) trimmedBase() string { return strings.TrimRight(c.base, "/") }

// Latest resolves the published latest release to its tag.
//
// GET <base>/latest with redirects NOT followed: the Location header is the
// answer. api.github.com/repos/.../releases/latest is deliberately not used,
// and the reason is already written down in install.sh — sixty unauthenticated
// requests an hour turns a corporate NAT or a CI runner into a mystery failure.
// Reintroducing it here would need that argument re-made, not merely forgotten.
//
// The Location is remote-controlled input that ends up in a status document and
// on a terminal. Only a …/tag/<version> shape is accepted, it is re-parsed, and
// the canonical re-stringified tag is what comes back — never the bytes the
// origin sent.
//
// The tag arriving over an unauthenticated channel is safe ONLY because the
// signature's trusted comment names the release too: the origin chooses the
// name, and the signature decides whether the bytes are the ones published
// under it. Neither half stands alone.
func (c *Client) Latest(ctx context.Context) (string, error) {
	u := c.trimmedBase() + "/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("building a request for %s: %w", u, err)
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := c.noFollow.Do(req)
	if err != nil {
		return "", fmt.Errorf("asking %s which release is latest: %w", u, err)
	}
	// The body is never read. A redirect's body is decoration, and reading it
	// would be an unbounded read of something nothing looks at.
	defer resp.Body.Close()

	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("%s answered HTTP %d instead of redirecting to a release", u, resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("%s redirected with no Location header", u)
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("%s redirected to an unparseable location: %w", u, err)
	}
	// A relative Location is legal and GitHub has sent both forms; resolving
	// against the request URL is what the stdlib's own redirect handling does.
	target := resp.Request.URL.ResolveReference(ref)

	// Whole SEGMENTS, never strings.Contains. The origin chooses the whole
	// path, so /releases/nottag/v1.2.3 must not answer this question.
	seg := strings.Split(strings.Trim(target.EscapedPath(), "/"), "/")
	if len(seg) < 2 || seg[len(seg)-2] != "tag" {
		return "", fmt.Errorf("%s redirected to %s, which does not name a release tag", u, target.Path)
	}
	// Unescaped AFTER the split, so an encoded separator cannot manufacture a
	// segment boundary that was not in the path.
	last, err := url.PathUnescape(seg[len(seg)-1])
	if err != nil {
		return "", fmt.Errorf("%s redirected to %s, whose last segment does not decode", u, target.Path)
	}
	v, ok := ParseTag(last)
	if !ok {
		return "", fmt.Errorf("%s redirected to %s, whose last segment is not a version", u, target.Path)
	}
	return v.Tag(), nil
}
