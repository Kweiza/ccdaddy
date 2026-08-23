package oauth

import (
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func parseAuthorize(t *testing.T, raw string) (*url.URL, url.Values) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("AuthorizeURL produced an unparseable URL %q: %v", raw, err)
	}
	return u, u.Query()
}

func TestAuthorizeURLClaudeAISurface(t *testing.T) {
	got := AuthorizeURL(AuthorizeParams{
		Surface:     SurfaceClaudeAI,
		Challenge:   "CHALLENGE",
		State:       "STATE",
		RedirectURI: "http://localhost:1234/callback",
	})
	u, q := parseAuthorize(t, got)

	if want := "https://claude.com/cai/oauth/authorize"; u.Scheme+"://"+u.Host+u.Path != want {
		t.Fatalf("endpoint = %q, want %q", u.Scheme+"://"+u.Host+u.Path, want)
	}
	checks := map[string]string{
		"code":                  "true",
		"client_id":             ClientID,
		"response_type":         "code",
		"redirect_uri":          "http://localhost:1234/callback",
		"code_challenge":        "CHALLENGE",
		"code_challenge_method": "S256",
		"state":                 "STATE",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// The complete key set, not just the values. Claude Code's builder appends
// exactly these eight unconditionally; its orgUUID, login_hint and login_method
// each sit behind an `if`, so they are not part of what ccdad sends. Nothing
// else in this file would notice a ninth key: the per-key checks only look up
// names they already know, and the manual-vs-loopback comparison passes when a
// stray parameter is on BOTH URLs.
func TestAuthorizeURLSendsExactlyClaudeCodesParameterSet(t *testing.T) {
	want := []string{
		"client_id",
		"code",
		"code_challenge",
		"code_challenge_method",
		"redirect_uri",
		"response_type",
		"scope",
		"state",
	}
	for _, surface := range []Surface{SurfaceClaudeAI, SurfaceConsole} {
		_, q := parseAuthorize(t, AuthorizeURL(AuthorizeParams{
			Surface: surface, Challenge: "c", State: "s", RedirectURI: "r",
		}))
		got := slices.Sorted(maps.Keys(q))
		if !slices.Equal(got, want) {
			t.Errorf("surface %v: parameters = %q, want exactly %q", surface, got, want)
		}
	}
}

// Claude Code appends code=true to every authorize request, loopback and manual
// alike; it selects the CLI code flow.
func TestAuthorizeURLAlwaysSetsCodeTrue(t *testing.T) {
	for _, s := range []Surface{SurfaceClaudeAI, SurfaceConsole} {
		got := AuthorizeURL(AuthorizeParams{Surface: s, Challenge: "c", State: "s", RedirectURI: "r"})
		_, q := parseAuthorize(t, got)
		if q.Get("code") != "true" {
			t.Errorf("surface %v: code = %q, want \"true\"", s, q.Get("code"))
		}
	}
}

func TestAuthorizeURLConsoleSurface(t *testing.T) {
	got := AuthorizeURL(AuthorizeParams{Surface: SurfaceConsole, Challenge: "c", State: "s", RedirectURI: "r"})
	u, _ := parseAuthorize(t, got)
	if want := "https://platform.claude.com/oauth/authorize"; u.Scheme+"://"+u.Host+u.Path != want {
		t.Fatalf("endpoint = %q, want %q", u.Scheme+"://"+u.Host+u.Path, want)
	}
}

// This pins the separator and the round-trip through url.Values, which encodes a
// space as '+'. It cannot see a reorder or a typo — both sides derive from the
// same value — so it is paired with the literal-pinned test below.
func TestAuthorizeURLScopeIsSpaceJoined(t *testing.T) {
	got := AuthorizeURL(AuthorizeParams{Surface: SurfaceClaudeAI, Challenge: "c", State: "s", RedirectURI: "r"})
	_, q := parseAuthorize(t, got)

	want := strings.Join(Scopes, " ")
	if q.Get("scope") != want {
		t.Fatalf("scope = %q, want %q", q.Get("scope"), want)
	}
}

// Written out again on purpose: this is what actually goes on the wire, and a
// reorder, a drop, or a typo in const.go must fail here.
func TestAuthorizeScopeParamIsTheExactClaudeCodeString(t *testing.T) {
	const want = "org:create_api_key user:profile user:inference " +
		"user:sessions:claude_code user:mcp_servers user:file_upload"
	_, q := parseAuthorize(t, AuthorizeURL(AuthorizeParams{
		Surface: SurfaceClaudeAI, Challenge: "c", State: "s", RedirectURI: "r",
	}))
	if got := q.Get("scope"); got != want {
		t.Fatalf("scope = %q, want %q", got, want)
	}
}

// Scopes is exported and mutable; the builder must not read it live.
func TestAuthorizeURLDoesNotDependOnTheExportedScopesSlice(t *testing.T) {
	before := slices.Clone(Scopes)
	t.Cleanup(func() { Scopes = before })

	Scopes = []string{"evil"}
	_, q := parseAuthorize(t, AuthorizeURL(AuthorizeParams{
		Surface: SurfaceClaudeAI, Challenge: "c", State: "s", RedirectURI: "r",
	}))
	if got := q.Get("scope"); strings.Contains(got, "evil") {
		t.Fatalf("scope = %q; the builder read the mutable Scopes slice", got)
	}
}

func TestLoopbackRedirectURI(t *testing.T) {
	if got, want := LoopbackRedirectURI(51234), "http://localhost:51234/callback"; got != want {
		t.Fatalf("LoopbackRedirectURI(51234) = %q, want %q", got, want)
	}
}

// A zero port means the loopback listener never bound, so the login degrades to
// manual-only. Building the URL anyway would send http://localhost:0/callback
// and turn a local bug into an opaque 400 from the authorization server.
func TestLoopbackRedirectURIRejectsAnUnboundPort(t *testing.T) {
	for _, port := range []int{0, -1, 65536} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("LoopbackRedirectURI(%d) returned normally, want a panic", port)
				}
			}()
			_ = LoopbackRedirectURI(port)
		}()
	}
}

// The manual and loopback URLs for one attempt must differ ONLY in redirect_uri:
// they share a verifier, a challenge, and a state, and the exchange picks the
// matching redirect_uri from whichever path won.
func TestManualAndLoopbackDifferOnlyInRedirectURI(t *testing.T) {
	base := AuthorizeParams{Surface: SurfaceClaudeAI, Challenge: "c", State: "s"}
	manual, loopback := base, base
	manual.RedirectURI = ManualRedirectURL
	loopback.RedirectURI = LoopbackRedirectURI(4000)

	_, mq := parseAuthorize(t, AuthorizeURL(manual))
	_, lq := parseAuthorize(t, AuthorizeURL(loopback))

	if got := mq.Get("redirect_uri"); got != ManualRedirectURL {
		t.Errorf("manual redirect_uri = %q, want %q", got, ManualRedirectURL)
	}
	if got, want := lq.Get("redirect_uri"), "http://localhost:4000/callback"; got != want {
		t.Errorf("loopback redirect_uri = %q, want %q", got, want)
	}
	delete(mq, "redirect_uri")
	delete(lq, "redirect_uri")
	// Compare the whole maps, not just the manual side's keys: a parameter
	// present on only one of the two URLs must fail too.
	if !maps.EqualFunc(mq, lq, slices.Equal) {
		t.Fatalf("queries differ outside redirect_uri:\n manual   %v\n loopback %v", mq, lq)
	}
}

func TestSurfaceString(t *testing.T) {
	for _, c := range []struct {
		s    Surface
		want string
	}{
		{SurfaceClaudeAI, "claude.ai"},
		{SurfaceConsole, "console"},
		{Surface(99), "Surface(99)"},
	} {
		if got := c.s.String(); got != c.want {
			t.Errorf("Surface(%d).String() = %q, want %q", int(c.s), got, c.want)
		}
	}
}
